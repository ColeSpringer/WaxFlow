package flacn_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestSeekProbeRefusalDoesNotStick: a SEEKTABLE point at a damaged frame is
// probed under Strict, refused there and dropped, and the seek bisects
// instead. The refusal costs neither the landing nor the reads after it.
func TestSeekProbeRefusalDoesNotStick(t *testing.T) {
	raw := noiseStream(t, 1<<18) // past two windows, so bisection lands past the damage
	frames := testutil.FLACFrames(t, raw)
	raw = testutil.DamageFLACFrames(t, raw, frames, 1)
	first := uint64(frames[0].Off)
	raw = withSeekTable(t, raw, [][3]uint64{
		{0, 0, uint64(frames[0].Dur)},
		{uint64(frames[1].Pos), uint64(frames[1].Off) - first, uint64(frames[1].Dur)},
	})
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	last := frames[len(frames)-1]
	landed, err := d.SeekSample(context.Background(), 0, last.Pos)
	if err != nil {
		t.Fatalf("a strict seek past the damaged frame failed: %v", err)
	}
	if landed != last.Pos {
		t.Fatalf("the seek landed at %d, want the last frame's %d", landed, last.Pos)
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a read after the dropped refusal failed: %v", err)
		}
	}
	if ws := d.Warnings(); len(ws) != 0 {
		t.Errorf("listed %v after the probe, want nothing", ws)
	}
}

// TestRejectedSeekProbeLeavesNoFinding: a tolerant seek whose SEEKTABLE
// point is a damaged frame drops what probing it found, so the seek lists
// and counts nothing a read has not met.
func TestRejectedSeekProbeLeavesNoFinding(t *testing.T) {
	raw := noiseStream(t, 1<<18)
	frames := testutil.FLACFrames(t, raw)
	raw = testutil.DamageFLACFrames(t, raw, frames, 1)
	first := uint64(frames[0].Off)
	raw = withSeekTable(t, raw, [][3]uint64{
		{0, 0, uint64(frames[0].Dur)},
		{uint64(frames[1].Pos), uint64(frames[1].Off) - first, uint64(frames[1].Dur)},
	})
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	before, count := d.Warnings(), d.DamageCount()
	last := frames[len(frames)-1]
	if landed, err := d.SeekSample(context.Background(), 0, last.Pos); err != nil || landed != last.Pos {
		t.Fatalf("the seek landed at %d, %v; want the last frame's %d", landed, err, last.Pos)
	}
	if ws := d.Warnings(); !slices.Equal(ws, before) || d.DamageCount() != count {
		t.Errorf("after the seek listed %v and counted %d, want %v and %d", ws, d.DamageCount(), before, count)
	}
}

// TestRejectedTailCandidateLeavesNoFinding: a false header in the last
// frame is the tail scan's first candidate and is rejected; what it found is
// not the file's, so a tolerant open lists and counts nothing.
func TestRejectedTailCandidateLeavesNoFinding(t *testing.T) {
	d, err := flacn.NewDemuxer(container.BytesSource(falseHeaderInLastFrame(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws := d.Warnings(); len(ws) != 0 || d.DamageCount() != 0 || !d.Tracks()[0].SamplesExact {
		t.Errorf("listed %v, counted %d, exact %v; want nothing, exact", ws, d.DamageCount(), d.Tracks()[0].SamplesExact)
	}
}

// TestOpenMeasurementCountsNothing: the open's measurement of a tail that
// will not confirm leaves nothing behind, so the open counts only what it
// lists, the damage it reports again included once.
func TestOpenMeasurementCountsNothing(t *testing.T) {
	raw, _ := testutil.FLACFixture(t)
	d, err := flacn.NewDemuxer(container.BytesSource(testutil.CutTail(raw, 100)), nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			listed++
		}
	}
	if listed == 0 {
		t.Fatal("the open listed no damage: this pins nothing")
	}
	if n := container.DamageOf(d); n != listed {
		t.Errorf("the open counted %d damage and listed %d: %v", n, listed, d.Warnings())
	}
}

// TestReplayedFindingIsTranslatedOnce: the damage the open's measurement met
// is reported again at the open, behind a leading tag at its offset in the
// file, translated once.
func TestReplayedFindingIsTranslatedOnce(t *testing.T) {
	raw, _ := testutil.FLACFixture(t)
	raw = testutil.CutTail(raw, 100) // the last frame cut short: the tail will not confirm
	const base = 4096
	open := func(opts *container.DemuxerOptions) ([]container.Warning, error) {
		d, err := flacn.NewDemuxer(container.BytesSource(raw), opts)
		if err != nil {
			return nil, err
		}
		return d.Warnings(), nil
	}
	bare, err := open(nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(bare, func(w container.Warning) bool { return w.Kind == container.Damage })
	if i < 0 {
		t.Fatalf("listed %v, want the damaged tail", bare)
	}
	w := bare[i]
	got, err := open(&container.DemuxerOptions{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if want := (container.Warning{Offset: w.Offset + base, Msg: w.Msg}); !slices.Contains(got, want) {
		t.Errorf("behind %d bytes listed %v, want %v", base, got, want)
	}
	_, err = open(&container.DemuxerOptions{Strict: true, Base: base})
	if want := fmt.Sprintf("flac: %s (at offset %d)", w.Msg, w.Offset+base); waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != want {
		t.Errorf("a strict open behind %d bytes failed with %v, want %q", base, err, want)
	}
}

// decoyBehindJunk is the 24-bit fixture with a seek window of junk in front
// of its third frame and that frame's first bytes again near the junk's
// end: the first header a seek's probe meets, with no frame behind it.
func decoyBehindJunk(t *testing.T) (raw []byte, third int64) {
	t.Helper()
	whole := fixture(t, "sine-s24.flac")
	frames := testutil.FLACFrames(t, whole)
	const long = 300 << 10
	return testutil.DecoyOf("decoy", whole, frames[2].Off, long, long-2048, 0xFF).Raw, frames[2].Pos
}

// TestAnAbandonedSeekWalkLeavesNoFinding: a seek whose walk begins on a
// header with no frame behind it starts again further back, and what the
// abandoned walk found goes with it: the seek lists and counts nothing a
// read has not met.
func TestAnAbandonedSeekWalkLeavesNoFinding(t *testing.T) {
	raw, third := decoyBehindJunk(t)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readFrames(t, d); err != nil {
		t.Fatal(err)
	}
	before, count := d.Warnings(), d.DamageCount()
	if count != 1 {
		t.Fatalf("the read counted %d findings, want the junk alone: %v", count, before)
	}
	if landed, err := d.SeekSample(context.Background(), 0, third+3000); err != nil || landed != third {
		t.Fatalf("the seek landed at %d (%v), want the third frame's %d", landed, err, third)
	}
	if ws := d.Warnings(); !slices.Equal(ws, before) || d.DamageCount() != count+1 {
		t.Errorf("after the seek listed %v and counted %d, want %v and %d: the junk again, which its second walk crossed",
			ws, d.DamageCount(), before, count+1)
	}
}

// TestASeekWalkThatLandsKeepsItsFinding: the walk a seek lands by crossed
// the junk, so on an open that has read nothing the seek lists it, once.
func TestASeekWalkThatLandsKeepsItsFinding(t *testing.T) {
	raw, third := decoyBehindJunk(t)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := d.DamageCount(); n != 0 {
		t.Fatalf("the open counted %d findings: %v", n, d.Warnings())
	}
	if landed, err := d.SeekSample(context.Background(), 0, third+3000); err != nil || landed != third {
		t.Fatalf("the seek landed at %d (%v), want the third frame's %d", landed, err, third)
	}
	if got := damageOf(d); len(got) != 1 || d.DamageCount() != 1 || got[0] != "307200 bytes are not a valid frame, dropped: 0 samples lost" {
		t.Errorf("after the seek listed %v and counted %d, want the junk its walk crossed, once", got, d.DamageCount())
	}
}

// TestAStrictSeekRefusesAtTheHeaderItsProbeMeets: under Strict the walk that
// begins on the header with no frame behind it refuses there, as it did,
// and lists nothing.
func TestAStrictSeekRefusesAtTheHeaderItsProbeMeets(t *testing.T) {
	raw, third := decoyBehindJunk(t)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.SeekSample(context.Background(), 0, third+3000)
	if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || !strings.Contains(err.Error(), "7163 bytes are not a valid frame, dropped: 4608 samples lost") {
		t.Fatalf("a strict seek returned %v, want the header with no frame behind it refused", err)
	}
	if len(d.Warnings()) != 0 || d.DamageCount() != 0 {
		t.Errorf("the refusal listed %v and counted %d", d.Warnings(), d.DamageCount())
	}
}
