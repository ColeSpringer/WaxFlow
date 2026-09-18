package mpa_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

func mp3Fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// frameOffsets walks the run's frame headers the way the demuxer does, so a
// test can cut a file exactly on a boundary rather than into a frame. The
// first entry is the metadata frame.
func frameOffsets(t *testing.T, raw []byte) []int64 {
	t.Helper()
	var offs []int64
	for off := int64(0); off < int64(len(raw)); {
		h, err := mp3.ParseHeader(raw[off:])
		if err != nil || h.Size() == 0 || off+int64(h.Size()) > int64(len(raw)) {
			break
		}
		offs = append(offs, off)
		off += int64(h.Size())
	}
	if len(offs) < 4 {
		t.Fatalf("found %d frames, want a run to cut", len(offs))
	}
	return offs
}

// decodeLength reads raw to the end through the whole stack and reports the
// samples it delivered: the independent measurement a settled length is a
// claim about.
func decodeLength(t *testing.T, raw []byte) int64 {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	dst := audio.Get(med.Info().Default().Fmt, 4096)
	defer audio.Put(dst)
	var got int64
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		got += int64(dst.N)
	}
}

func walked(t *testing.T, raw []byte, strict bool) (container.Track, []container.Warning, error) {
	t.Helper()
	d, err := mpa.NewDemuxer(container.BytesSource(raw), &mpa.DemuxerOptions{Strict: strict})
	if err != nil {
		return container.Track{}, nil, err
	}
	err = d.Walk()
	return d.Tracks()[0], d.Warnings(), err
}

// readToEnd drains every packet, which is the other path to the end of the
// run: the comparison lives in the walker so that a read and a walk of the
// same file reach the same verdict.
func readToEnd(t *testing.T, raw []byte, strict bool) error {
	t.Helper()
	d, err := mpa.NewDemuxer(container.BytesSource(raw), &mpa.DemuxerOptions{Strict: strict})
	if err != nil {
		return err
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func findings(ws []container.Warning, kind container.WarningKind) []string {
	var out []string
	for _, w := range ws {
		if w.Kind == kind {
			out = append(out, w.Msg)
		}
	}
	return out
}

// TestWalkSettlesAnUntaggedRun is the simple half: a stream that states no
// length at all is exactly its frames' worth once the walk has counted them.
func TestWalkSettlesAnUntaggedRun(t *testing.T) {
	raw := mp3Fixture(t, "sine-untagged.mp3")
	track, ws, err := walked(t, raw, true)
	if err != nil {
		t.Fatalf("strict Walk: %v", err)
	}
	if !track.SamplesExact || track.SamplesAdvisory {
		t.Errorf("flags exact=%v advisory=%v, want exact", track.SamplesExact, track.SamplesAdvisory)
	}
	if n := decodeLength(t, raw); track.Samples != n {
		t.Errorf("walked length %d, a decode delivered %d", track.Samples, n)
	}
	if len(ws) != 0 {
		t.Errorf("findings on a clean run: %v", ws)
	}
}

// TestWalkConfirmsAXingCount is the case the walk used to leave alone: a
// tagged stream whose count is right stays at that count, now confirmed
// rather than taken on trust.
func TestWalkConfirmsAXingCount(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	track, ws, err := walked(t, raw, true)
	if err != nil {
		t.Fatalf("strict Walk: %v", err)
	}
	if track.Samples != 22050 || !track.SamplesExact {
		t.Errorf("walked to %d (exact %v), want 22050 exact", track.Samples, track.SamplesExact)
	}
	if track.Delay != 1105 || track.Padding != 1037 {
		t.Errorf("trims moved to %d/%d, want 1105/1037", track.Delay, track.Padding)
	}
	if len(ws) != 0 {
		t.Errorf("findings on an intact tagged run: %v", ws)
	}
	if n := decodeLength(t, raw); n != 22050 {
		t.Errorf("a decode delivered %d, want 22050", n)
	}
}

// TestTruncatedTaggedRunShrinks is the request that started this: a Xing
// count is a claim, and a run that cannot fill it shrinks to what its packets
// hold rather than promising audio the file does not have.
func TestTruncatedTaggedRunShrinks(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	cut := raw[:len(raw)-100] // into the final frame's payload

	track, ws, err := walked(t, cut, false)
	if err != nil {
		t.Fatalf("tolerant Walk: %v", err)
	}
	if n := decodeLength(t, cut); track.Samples != n {
		t.Errorf("settled to %d, a decode of the same bytes delivers %d", track.Samples, n)
	}
	if track.Samples >= 22050 {
		t.Errorf("settled to %d, want less than the declared 22050", track.Samples)
	}
	if !track.SamplesExact {
		t.Error("a finished walk must report its measurement exact")
	}
	if got := findings(ws, container.Damage); len(got) != 2 {
		t.Errorf("damage = %v, want the dropped frame and the shortfall", got)
	}
	if _, _, err := walked(t, cut, true); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict Walk = %v, want malformed", err)
	}
	if err := readToEnd(t, cut, true); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict read = %v, want malformed", err)
	}
}

// TestAShortfallOnACleanCutIsFoundByBothPaths is why the comparison sits in
// the walker. Cut exactly on a frame boundary there is no truncated frame and
// no trailing junk, so a read has nothing of its own to notice: without the
// walker-side check a strict walk would refuse a file a strict read accepted.
//
// One frame is the tolerance rather than the case, since an encoder that
// counts its own metadata frame produces exactly that on an intact file.
func TestAShortfallOnACleanCutIsFoundByBothPaths(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	offs := frameOffsets(t, raw)

	one := raw[:offs[len(offs)-1]]
	track, ws, err := walked(t, one, true)
	if err != nil {
		t.Fatalf("one frame short must be tolerated even under Strict: %v", err)
	}
	if got := findings(ws, container.Note); len(got) != 1 {
		t.Errorf("notes = %v, want the one-frame shortfall", got)
	}
	if got := findings(ws, container.Damage); len(got) != 0 {
		t.Errorf("damage = %v, want none for a one-frame shortfall", got)
	}
	if n := decodeLength(t, one); track.Samples != n {
		t.Errorf("settled to %d, a decode delivers %d", track.Samples, n)
	}

	two := raw[:offs[len(offs)-2]]
	if _, _, err := walked(t, two, true); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict Walk of a two-frame shortfall = %v, want malformed", err)
	}
	if err := readToEnd(t, two, true); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict read of a two-frame shortfall = %v, want malformed", err)
	}
	track, ws, err = walked(t, two, false)
	if err != nil {
		t.Fatalf("tolerant Walk: %v", err)
	}
	if got := findings(ws, container.Damage); len(got) != 1 {
		t.Errorf("damage = %v, want the shortfall alone (the cut is on a boundary)", got)
	}
	if n := decodeLength(t, two); track.Samples != n {
		t.Errorf("settled to %d, a decode delivers %d", track.Samples, n)
	}
}

// TestARunPastItsCountIsANote is the other direction: the frames the tag
// named are all there, and more behind them. Nothing is missing, so it is the
// file being loose rather than damaged, and the trims still bound the length.
func TestARunPastItsCountIsANote(t *testing.T) {
	raw := mp3Fixture(t, "sine-vbr.mp3")
	offs := frameOffsets(t, raw)
	last := raw[offs[len(offs)-1]:]
	grown := append(append([]byte(nil), raw...), last...)
	grown = append(grown, last...)

	track, ws, err := walked(t, grown, true)
	if err != nil {
		t.Fatalf("strict Walk must tolerate an overrun: %v", err)
	}
	if got := findings(ws, container.Note); len(got) != 1 {
		t.Fatalf("notes = %v, want the overrun", got)
	}
	if got := findings(ws, container.Damage); len(got) != 0 {
		t.Errorf("damage = %v, want none", got)
	}
	if track.Samples != 22050 {
		t.Errorf("settled to %d, want the declared 22050: the cap still bounds it", track.Samples)
	}
	if n := decodeLength(t, grown); track.Samples != n {
		t.Errorf("settled to %d, a decode delivers %d", track.Samples, n)
	}
}
