package flacn_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// farBehindAHole returns raw with frame at damaged and every frame behind it
// numbered far frames on, more than the damaged bytes could have held: the
// closing frame confirms a length that counts them, and a read joins them on
// with nothing lost, so it ends short of that length by the far frames and
// the damaged one.
func farBehindAHole(t testing.TB, raw []byte, at, far int) []byte {
	t.Helper()
	frames := testutil.FLACFrames(t, raw)
	out := testutil.DamageFLACFrames(t, raw, frames, at)[:frames[at+1].Off]
	for k := at + 1; k < len(frames); k++ {
		f := frames[k]
		out = append(out, testutil.SetFLACFrameNumber(t, raw[f.Off:f.End], false, uint64(k+far))...)
	}
	return out
}

// strayBehindAHole returns raw with frame at damaged and the two frames
// behind it numbered far past the stream's end, in front of frames that
// carry on as they were.
func strayBehindAHole(t *testing.T, raw []byte, at int) []byte {
	t.Helper()
	frames := testutil.FLACFrames(t, raw)
	out := testutil.DamageFLACFrames(t, raw, frames, at)[:frames[at+1].Off]
	for k := at + 1; k < at+3; k++ {
		f := frames[k]
		out = append(out, testutil.SetFLACFrameNumber(t, raw[f.Off:f.End], false, uint64(k+2*len(frames)))...)
	}
	return append(out, raw[frames[at+3].Off:]...)
}

// reframed returns raw with frame, a whole frame of the same stream shape,
// put in front of frame at and numbered for that place, and every frame
// behind it numbered one on.
func reframed(t *testing.T, raw []byte, at int, frame []byte) []byte {
	t.Helper()
	frames := testutil.FLACFrames(t, raw)
	out := append([]byte(nil), raw[:frames[at].Off]...)
	out = append(out, testutil.SetFLACFrameNumber(t, frame, false, uint64(at))...)
	for k := at; k < len(frames); k++ {
		f := frames[k]
		out = append(out, testutil.SetFLACFrameNumber(t, raw[f.Off:f.End], false, uint64(k+1))...)
	}
	return out
}

// opened opens raw tolerant and holds its length exact at samples.
func opened(t *testing.T, raw []byte, samples int64) *flacn.Demuxer {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, samples)
	return d
}

func exactAt(t *testing.T, d *flacn.Demuxer, samples int64) {
	t.Helper()
	if tr := d.Tracks()[0]; tr.Samples != samples || !tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d exact", tr.Samples, tr.SamplesExact, samples)
	}
}

// readCount reads d to its end and returns what its packets hold: their
// durations and the samples holes took.
func readCount(t *testing.T, d *flacn.Demuxer) int64 {
	t.Helper()
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatalf("the read ended %v", err)
	}
	var n int64
	for _, f := range frames {
		n += f.dur + f.lost
	}
	return n
}

// settled fails unless d's track says samples and no longer calls them
// exact, counts truncated past them, and lists one Damage naming want at
// the first frame.
func settled(t *testing.T, d *flacn.Demuxer, samples, truncated int64, want string) {
	t.Helper()
	if tr := d.Tracks()[0]; tr.Samples != samples || tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want %d and not exact", tr.Samples, tr.SamplesExact, samples)
	}
	if got := container.TruncatedOf(d, 0); got != truncated {
		t.Errorf("%d samples counted truncated, want %d", got, truncated)
	}
	n := 0
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, want) {
			n++
			if w.Kind != container.Damage || w.Offset != d.FirstFrameForTest() {
				t.Errorf("the shortfall is %+v, want Damage at the first frame, %d", w, d.FirstFrameForTest())
			}
		}
	}
	if n != 1 {
		t.Errorf("warnings %+v, want one naming %q", d.Warnings(), want)
	}
}

// TestAReadEndingShortOfTheLengthSettlesIt: a read whose clean end falls short
// of the exact length the open stated reports the shortfall, once, adopts
// what the packets hold, and counts the rest truncated. The length is no
// longer called exact: a seek may reach what the read did not count.
func TestAReadEndingShortOfTheLengthSettlesIt(t *testing.T) {
	whole := unseekableTwin(t, 4)
	const at, far = 20, 5000
	total := streamSamples(t, whole)
	stated, reached := total+far*4096, total-4096
	raw := farBehindAHole(t, whole, at, far)
	d := opened(t, raw, stated)
	if n := readCount(t, d); n != reached {
		t.Fatalf("read %d samples, want the %d of every frame but the damaged one", n, reached)
	}
	want := fmt.Sprintf("the closing frame ends at sample %d but the frames a read reaches end at %d", stated, reached)
	settled(t, d, reached, stated-reached, want)
	si, err := flac.ParseStreamInfo(d.Tracks()[0].CodecConfig)
	if err != nil || si.Samples != reached || si.MD5 != [16]byte{} {
		t.Errorf("the STREAMINFO handed on states %d samples and MD5 %x (%v), want %d and none", si.Samples, si.MD5, err, reached)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != io.EOF {
		t.Fatalf("a read past the end returned %v", err)
	}
	settled(t, d, reached, stated-reached, want)

	// A Strict read refuses sooner, at the bytes the read drops.
	ds, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readFrames(t, ds); err == nil || !strings.Contains(err.Error(), "bytes are not a valid frame, dropped") {
		t.Fatalf("a Strict read ended %v, want the dropped bytes refused", err)
	}
}

// streamSamples is the length raw, an intact stream, opens at.
func streamSamples(t *testing.T, raw []byte) int64 {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	return d.Tracks()[0].Samples
}

// headless returns raw with the header of the frame at off cleared, so the
// frame in front of it is whole and the one behind it is the next a read
// finds.
func headless(raw []byte, off int) []byte {
	out := append([]byte(nil), raw...)
	clear(out[off : off+8])
	return out
}

// TestASeekThenAReadToTheEndSettlesTheSame: where the stream ends does not
// depend on where the read began, when the seek's walk began at the first
// frame and counted as a read does.
func TestASeekThenAReadToTheEndSettlesTheSame(t *testing.T) {
	whole := unseekableTwin(t, 4)
	const at, far = 20, 5000
	total := streamSamples(t, whole)
	stated, reached := total+far*4096, total-4096
	short := farBehindAHole(t, whole, at, far)
	first := testutil.FLACFrames(t, whole)[0].Off
	want := fmt.Sprintf("the closing frame ends at sample %d but the frames a read reaches end at %d", stated, reached)
	for _, tc := range []struct {
		name           string
		raw            []byte
		target, landed int64
		findings       int
	}{
		{"to the middle", short, 10*4096 + 7, 10 * 4096, 2},
		{"into the hole the stream starts with", headless(short, first), 100, 0, 3},
		{"behind the hole the stream starts with", headless(short, first), 10*4096 + 7, 10 * 4096, 3},
	} {
		d := opened(t, tc.raw, stated)
		if landed, err := d.SeekSample(context.Background(), 0, tc.target); err != nil || landed != tc.landed {
			t.Fatalf("a seek %s landed at %d (%v), want %d", tc.name, landed, err, tc.landed)
		}
		if n := readCount(t, d); n != reached-tc.landed {
			t.Fatalf("a seek %s: read %d samples, want the %d behind it", tc.name, n, reached-tc.landed)
		}
		settled(t, d, reached, stated-reached, want)
		if n := container.DamageOf(d); n != tc.findings {
			t.Errorf("a seek %s: %d findings, want %d: %v", tc.name, n, tc.findings, damageOf(d))
		}
	}
}

// decodeRest reads med to its end and returns the samples it delivered.
func decodeRest(t *testing.T, med format.Media) int64 {
	t.Helper()
	f := med.Info().Default().Fmt
	var n int64
	for {
		buf := audio.Get(f, audio.StandardChunk)
		err := med.ReadChunk(buf)
		n += int64(buf.N)
		audio.Put(buf)
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestALengthAReadFallsShortOfIsNotHeldAgainstASeek: a seek lands by a
// frame's own number, which runs past what a read counted, so the length a
// short read leaves is not one a decode is cut at, in either order.
func TestALengthAReadFallsShortOfIsNotHeldAgainstASeek(t *testing.T) {
	const total, at, far = 1 << 18, 20, 40000
	const stated, reached, target = total + far*4096, total - 4096, (50 + far) * 4096
	const behind = total - 50*4096 // what the frames from the target on hold
	raw := farBehindAHole(t, noiseStream(t, total), at, far)
	if len(raw) < 8*srcwin.Chunk {
		t.Fatalf("%d bytes, want a stream a seek bisects", len(raw))
	}
	want := fmt.Sprintf("the closing frame ends at sample %d but the frames a read reaches end at %d", stated, reached)

	// A read to the end, a walk, then a seek past where the read counted.
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if n := decodeRest(t, med); n != reached {
		t.Fatalf("a decode delivered %d samples, want the %d of every frame but the damaged one", n, reached)
	}
	if info := med.Info(); info.Truncated != stated-reached || info.Damage != 2 {
		t.Errorf("after the read the Media counts %d truncated and %d damage, want %d and 2", info.Truncated, info.Damage, stated-reached)
	}
	if err := format.WalkMedia(context.Background(), med); err != nil {
		t.Fatal(err)
	}
	if tr := med.Info().Default(); tr.Samples != reached || tr.SamplesExact {
		t.Errorf("after the read and a walk the Media says %d samples, exact %v; want %d, not exact", tr.Samples, tr.SamplesExact, reached)
	}
	if pos, err := med.SeekSample(context.Background(), target); err != nil || pos != target {
		t.Fatalf("a seek to %d landed at %d (%v)", target, pos, err)
	}
	if n := decodeRest(t, med); n != behind {
		t.Errorf("a decode from the seek delivered %d samples, want the %d behind it", n, behind)
	}

	// The other order through a Media: a seek and a decode to the end, a
	// decode from the start, twice, a walk, and the seek again.
	other, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for i, step := range []struct{ target, want int64 }{{target, behind}, {0, reached}, {0, reached}, {target, behind}} {
		if i == 3 {
			if err := format.WalkMedia(context.Background(), other); err != nil {
				t.Fatal(err)
			}
		}
		if pos, err := other.SeekSample(context.Background(), step.target); err != nil || pos != step.target {
			t.Fatalf("step %d: a seek to %d landed at %d (%v)", i, step.target, pos, err)
		}
		if n := decodeRest(t, other); n != step.want {
			t.Errorf("step %d: a decode from %d delivered %d samples, want %d", i, step.target, n, step.want)
		}
	}

	// The same order on the demuxer. Only the read from the start finds
	// where the stream ends.
	d := opened(t, raw, stated)
	if _, err := d.SeekSample(context.Background(), 0, target); err != nil {
		t.Fatal(err)
	}
	if n := readCount(t, d); n != behind {
		t.Fatalf("the read from the seek delivered %d samples, want %d", n, behind)
	}
	exactAt(t, d, stated)
	if len(d.Warnings()) != 1 { // the open notes STREAMINFO's own total
		t.Errorf("a read from a seek to the end warned %v", d.Warnings())
	}
	for pass := range 2 {
		if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
			t.Fatal(err)
		}
		if n := readCount(t, d); n != reached {
			t.Fatalf("pass %d from the start delivered %d samples, want %d", pass, n, reached)
		}
		settled(t, d, reached, stated-reached, want)
	}

	// A read that ends short from a bisection's landing is no whole read:
	// where it ends counts from the landing, so it judges nothing.
	d = opened(t, raw, stated)
	if landed, err := d.SeekSample(context.Background(), 0, 10*4096); err != nil || landed != 10*4096 {
		t.Fatalf("the seek landed at %d (%v)", landed, err)
	}
	if n := readCount(t, d); n != reached-10*4096 {
		t.Fatalf("the read from the seek delivered %d samples", n)
	}
	exactAt(t, d, stated)
	if got := container.TruncatedOf(d, 0); got != 0 {
		t.Errorf("%d samples counted truncated by a read from a bisection, want none", got)
	}
}

// TestASecondPassReadsAsTheFirst: a hole stays held to the length the open
// called exact after a read has taken that claim back, so a second read
// places the same frames and loses the same samples.
func TestASecondPassReadsAsTheFirst(t *testing.T) {
	whole := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, whole)
	last := frames[len(frames)-1]
	// The short closing frame again in the middle, so a read ends short;
	// and in front of it a damaged frame with two strays behind it, numbered
	// past the length and within what the damaged bytes could hold.
	raw := strayBehindAHole(t, reframed(t, whole, 30, whole[last.Off:last.End]), 10)
	total := streamSamples(t, whole)
	d := opened(t, raw, total+4096)
	var first []frame
	for pass := range 2 {
		got, err := readFrames(t, d)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tr.SamplesExact || tr.Samples != total+last.Dur {
			t.Fatalf("pass %d: the track says %d samples, exact %v; want %d, not exact", pass, tr.Samples, tr.SamplesExact, total+last.Dur)
		}
		if pass == 0 {
			first = got
		} else if len(got) != len(first) {
			t.Fatalf("the second pass read %d packets, the first %d", len(got), len(first))
		}
		for i := range got {
			if got[i].pts != first[i].pts || got[i].lost != first[i].lost {
				t.Fatalf("pass %d packet %d is at %d owing %d, the first pass had it at %d owing %d", pass, i, got[i].pts, got[i].lost, first[i].pts, first[i].lost)
			}
		}
		if got[10].pts != 13*4096 || got[10].lost != 3*4096 {
			t.Fatalf("pass %d: the frame behind the strays is at %d owing %d, want %d owing %d", pass, got[10].pts, got[10].lost, 13*4096, 3*4096)
		}
		if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAShortfallTheReadAloneFindsSettles: a read that ends short with nothing
// warned on the way still settles, and a Strict read that refuses there
// refuses again on the next call.
func TestAShortfallTheReadAloneFindsSettles(t *testing.T) {
	whole := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, whole)
	last := frames[len(frames)-1]
	if last.Dur >= 4096 {
		t.Fatalf("the closing frame holds %d samples, want a short one", last.Dur)
	}
	// The short closing frame, again, in the middle: every frame checks and
	// carries the next number, and the numbers count one block more.
	raw := reframed(t, whole, 10, whole[last.Off:last.End])
	total := streamSamples(t, whole)
	d := opened(t, raw, total+4096)
	if n := readCount(t, d); n != total+last.Dur {
		t.Fatalf("read %d samples, want %d", n, total+last.Dur)
	}
	settled(t, d, total+last.Dur, 4096-last.Dur, fmt.Sprintf(
		"the closing frame ends at sample %d but the frames a read reaches end at %d", total+4096, total+last.Dur))
	if n := container.DamageOf(d); n != 1 {
		t.Errorf("%d findings, want the shortfall alone: %v", n, damageOf(d))
	}

	ds, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, first := readFrames(t, ds)
	var pkt container.Packet
	if second := ds.ReadPacket(&pkt); first == nil || second == nil || second.Error() != first.Error() {
		t.Fatalf("a Strict read ended %v, and the next call %v; want the shortfall refused twice", first, second)
	}
}

// TestAReadPastTheLengthTakesTheClaimBack: a whole read that delivers more
// than the exact length notes it, as the open notes frames past STREAMINFO's
// total, and leaves the length as it was and no longer exact.
func TestAReadPastTheLengthTakesTheClaimBack(t *testing.T) {
	src := loopedPCM(t, 4)
	defer audio.Put(src)
	var small bytes.Buffer
	encodeStream(t, src, 0, &small, int64(src.N)) // blocks of 1152
	big := unseekableTwin(t, 4)                   // blocks of 4096
	wide := testutil.FLACFrames(t, big)[3]
	raw := reframed(t, small.Bytes(), 10, big[wide.Off:wide.End])
	total := streamSamples(t, small.Bytes())
	d := opened(t, raw, total+1152)
	atOpen := slices.Clone(d.Warnings()) // the open notes its own overrun of STREAMINFO's total
	if n := readCount(t, d); n != total+4096 {
		t.Fatalf("read %d samples, want %d", n, total+4096)
	}
	if tr := d.Tracks()[0]; tr.Samples != total+1152 || tr.SamplesExact || container.TruncatedOf(d, 0) != 0 {
		t.Errorf("the track says %d samples, exact %v, %d truncated; want %d, not exact, none",
			tr.Samples, tr.SamplesExact, container.TruncatedOf(d, 0), total+1152)
	}
	// It notes it, once, no damage, and Strict reads to the end as well.
	want := append(atOpen, container.Warning{Offset: d.FirstFrameForTest(), Kind: container.Note,
		Msg: fmt.Sprintf("the closing frame ends at sample %d but the frames hold %d", total+1152, total+4096)})
	if !slices.Equal(d.Warnings(), want) || container.DamageOf(d) != 0 {
		t.Errorf("warnings %+v, %d damage; want %+v alone", d.Warnings(), container.DamageOf(d), want)
	}
	if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if readCount(t, d); !slices.Equal(d.Warnings(), want) {
		t.Errorf("a second read listed %+v, want the one note", d.Warnings())
	}
	ds, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readFrames(t, ds); err != nil || !slices.Equal(ds.Warnings(), want) {
		t.Errorf("a Strict read ended %v with %+v, want the end and the note", err, ds.Warnings())
	}
}

// TestAWholeStreamSettlesNothing: a read to the end of an intact stream,
// from its start or from a seek, keeps the length exact and warns nothing.
func TestAWholeStreamSettlesNothing(t *testing.T) {
	for _, name := range []string{"sine-s16.flac", "noise-s16.flac", "sine-s24.flac", "sine-mono-s16.flac"} {
		for _, from := range []int64{-1, 5000} {
			d, err := flacn.NewDemuxer(container.BytesSource(fixture(t, name)), nil)
			if err != nil {
				t.Fatal(err)
			}
			want, config := d.Tracks()[0].Samples, d.Tracks()[0].CodecConfig
			exactAt(t, d, want)
			if from >= 0 {
				if _, err := d.SeekSample(context.Background(), 0, from); err != nil {
					t.Fatal(err)
				}
			}
			readCount(t, d)
			exactAt(t, d, want)
			if got := container.TruncatedOf(d, 0); got != 0 || len(d.Warnings()) != 0 || !bytes.Equal(d.Tracks()[0].CodecConfig, config) {
				t.Errorf("%s from %d: %d truncated, warnings %v, STREAMINFO moved %v; want nothing",
					name, from, got, d.Warnings(), !bytes.Equal(d.Tracks()[0].CodecConfig, config))
			}
		}
	}
}

// TestAHoleCountsTowardTheLength: what a hole took is part of what a read
// holds, from the open or from a seek whose walk crossed it or landed in
// front of it, so a stream with holes inside its length settles nothing.
func TestAHoleCountsTowardTheLength(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	spans := frameSpans(t, whole)
	for _, tc := range []struct {
		name    string
		raw     []byte
		seek    int64 // -1 for a read from the open
		landed  int64
		damages int // a walk that lands on the frame in front of a hole finds it, and the read again
	}{
		{"read across a hole", damageFrames(t, whole, spans, 10), -1, 0, 1},
		{"seek across a hole", damageFrames(t, whole, spans, 10), 20 * 4096, 20 * 4096, 1},
		{"seek into a hole", damageFrames(t, whole, spans, 10), 10*4096 + 5, 9 * 4096, 2},
		{"seek across a hole behind a whole frame", headless(whole, spans[10][0]), 20 * 4096, 20 * 4096, 1},
		{"seek behind the hole the stream starts with", headless(whole, spans[0][0]), 20 * 4096, 20 * 4096, 1},
		{"read from a hole at the start", damageFrames(t, whole, spans, 0, 1), -1, 0, 1},
		{"seek behind a hole at the start", damageFrames(t, whole, spans, 0, 1), 20 * 4096, 20 * 4096, 1},
		{"seek into a hole at the start", damageFrames(t, whole, spans, 0, 1), 4096, 0, 1},
	} {
		d := opened(t, tc.raw, total)
		if tc.seek >= 0 {
			if landed, err := d.SeekSample(context.Background(), 0, tc.seek); err != nil || landed != tc.landed {
				t.Fatalf("%s: the seek landed at %d (%v), want %d", tc.name, landed, err, tc.landed)
			}
		}
		if n := readCount(t, d); n != total-tc.landed {
			t.Errorf("%s: read %d samples, want %d", tc.name, n, total-tc.landed)
		}
		exactAt(t, d, total)
		if got := container.TruncatedOf(d, 0); got != 0 || container.DamageOf(d) != tc.damages {
			t.Errorf("%s: %d truncated and findings %v, want none and the hole's", tc.name, got, damageOf(d))
		}
	}
}

// TestALengthNotExactIsLeftAlone: a length the open did not call exact is
// not held against a read, which ends short of it and says nothing more.
func TestALengthNotExactIsLeftAlone(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	// Two closing frames numbered past what the bytes could hold: the open
	// keeps STREAMINFO's total, unmeasured, and a read joins them on.
	raw := append([]byte(nil), whole[:frames[20].Off]...)
	for k := 20; k < 22; k++ {
		raw = append(raw, testutil.SetFLACFrameNumber(t, whole[frames[k].Off:frames[k].End], false, uint64(k+10_000_000))...)
	}
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr, before := d.Tracks()[0], container.DamageOf(d)
	if tr.Samples != total || tr.SamplesExact {
		t.Fatalf("the track says %d samples, exact %v, want the %d STREAMINFO declares and not exact", tr.Samples, tr.SamplesExact, total)
	}
	if n := readCount(t, d); n != 22*4096 {
		t.Fatalf("read %d samples, want %d", n, 22*4096)
	}
	if got := d.Tracks()[0]; got.Samples != total || got.SamplesExact || container.TruncatedOf(d, 0) != 0 || container.DamageOf(d) != before+1 {
		t.Errorf("after the read the track says %d samples, exact %v, %d truncated, findings %v; want it as it opened and the join alone",
			got.Samples, got.SamplesExact, container.TruncatedOf(d, 0), damageOf(d))
	}
}

// TestAReadThatFailsSettlesNothing: a read that ends on a failed read has
// not reached the stream's end, so the length stands.
func TestAReadThatFailsSettlesNothing(t *testing.T) {
	const total = 1 << 18
	whole := noiseStream(t, total)
	const stated = total + 40000*4096
	raw := farBehindAHole(t, whole, 20, 40000)
	frames := testutil.FLACFrames(t, whole)
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: int64(frames[12].Off), To: int64(frames[12].Off + 40)}
	d, err := flacn.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	exactAt(t, d, stated)
	if _, err := readFrames(t, d); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("the read ended %v, want the failed read", err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("a read behind the failed one returned %v", err)
	}
	exactAt(t, d, stated)
	if got := container.TruncatedOf(d, 0); got != 0 || container.DamageOf(d) != 0 {
		t.Errorf("%d truncated and findings %v after a read that failed, want none", got, damageOf(d))
	}
}

// TestAFrameTheStreamCouldHaveIsTheOneReadOnFrom: behind damage, frames
// that check and are numbered where the stream could not have them are
// passed over for the next frame it could have, and lie in the hole.
func TestAFrameTheStreamCouldHaveIsTheOneReadOnFrom(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	const at = 20
	raw := strayBehindAHole(t, whole, at)
	d := opened(t, raw, total)
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(frames)-3 || got[at].pts != (at+3)*4096 || got[at].lost != 3*4096 || !got[at].discont ||
		!bytes.Equal(got[at].data, whole[frames[at+3].Off:frames[at+3].End]) {
		t.Fatalf("read %d packets, the one behind the hole at %d owing %d; want %d packets and frame %d at %d owing %d",
			len(got), got[at].pts, got[at].lost, len(frames)-3, at+3, (at+3)*4096, 3*4096)
	}
	exactAt(t, d, total)
	if msgs := damageOf(d); len(msgs) != 1 || !strings.HasSuffix(msgs[0], "bytes are not a valid frame, dropped: 12288 samples lost") {
		t.Errorf("findings %v, want the one hole", msgs)
	}
	if landed, err := d.SeekSample(context.Background(), 0, (at+1)*4096+9); err != nil || landed != (at-1)*4096 {
		t.Errorf("a seek into the hole landed at %d (%v), want the frame in front of it at %d", landed, err, (at-1)*4096)
	}
}

// TestFramesNoLengthCanHoldAreJoinedOn: where every frame behind damage is
// numbered where the stream could not have it, they are delivered in order
// with nothing reported lost, and a frame's packet is the frame alone
// whatever checks behind it.
func TestFramesNoLengthCanHoldAreJoinedOn(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	const at, far = 20, 5000
	renumbered := func(k int) []byte {
		return testutil.SetFLACFrameNumber(t, whole[frames[k].Off:frames[k].End], false, uint64(k+far))
	}
	joined := farBehindAHole(t, whole, at, far)
	headless := headless(farBehindAHole(t, whole, at+1, far), frames[at+1].Off)
	// Two far frames, then frames numbered from 0 again: they carry on from
	// nothing, and check behind the second far frame as one run of bytes.
	stepped := append(append([]byte(nil), joined[:frames[at+1].Off]...), renumbered(at+1)...)
	stepped = append(append(stepped, renumbered(at+2)...), whole[frames[0].Off:frames[2].Off]...)
	for name, tc := range map[string]struct {
		raw     []byte
		first   int // the first frame behind the hole
		packets int
		exact   bool
	}{
		"a damaged frame":                       {joined, at + 1, len(frames) - 1, true},
		"a damaged header behind a whole frame": {headless, at + 2, len(frames) - 1, true},
		"frames that step back behind them":     {stepped, at + 1, at + 2, false},
	} {
		d, err := flacn.NewDemuxer(container.BytesSource(tc.raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tc.exact && (!tr.SamplesExact || tr.Samples != total+far*4096) {
			t.Fatalf("%s: the track says %d samples, exact %v, want %d exact", name, tr.Samples, tr.SamplesExact, total+far*4096)
		}
		got, err := readFrames(t, d)
		if err != nil {
			t.Fatal(err)
		}
		k := at // the packet the first joined frame is
		if tc.first == at+2 {
			k = at + 1
		}
		at0 := int64(tc.first+far) * 4096
		if len(got) != tc.packets || got[k].pts != at0 || got[k].lost != 0 || !got[k].discont || got[k+1].pts != at0+4096 || got[k+1].discont {
			t.Fatalf("%s: %d packets, the joined ones at %d (lost %d, discont %v) and %d; want %d packets and those at %d and %d, nothing lost",
				name, len(got), got[k].pts, got[k].lost, got[k].discont, got[k+1].pts, tc.packets, at0, at0+4096)
		}
		if !bytes.Equal(got[k+1].data, renumbered(tc.first+1)) {
			t.Errorf("%s: the second joined packet is %d bytes, want the frame alone, %d", name, len(got[k+1].data), len(renumbered(tc.first+1)))
		}
		if msgs := strings.Join(damageOf(d), "; "); !strings.Contains(msgs, "numbered past what the stream holds, joined") ||
			tc.exact == strings.Contains(msgs, "bytes hold frames out of sequence, dropped") {
			t.Errorf("%s: findings %v, want the hole reported as joined, and the frames that step back as trailing bytes", name, msgs)
		}
		if !tc.exact {
			continue // its closing frame is numbered 1, which is where a decode is cut
		}
		// A decode delivers them: nothing stands in for the hole.
		med, err := format.Open(container.BytesSource(tc.raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		var want int64
		for _, f := range got {
			want += f.dur
		}
		if c := decodeRest(t, med); c != want || med.Info().Lost != 0 {
			t.Errorf("%s: a decode delivered %d samples, %d of them silence; want the packets' %d and none", name, c, med.Info().Lost, want)
		}
		med.Close()
		// Strict refuses the hole.
		ds, err := flacn.NewDemuxer(container.BytesSource(tc.raw), &container.DemuxerOptions{Strict: true})
		if err == nil {
			_, err = readFrames(t, ds)
		}
		if err == nil || !strings.Contains(err.Error(), "not a valid frame") {
			t.Errorf("%s: Strict ended %v, want the damage refused", name, err)
		}
	}
}

// TestFramesOutOfSequenceAreNotAFramesPacket: whole frames behind a frame
// that are numbered before its successor check together with it (a frame's
// own checksum leaves zero), and used to ride in its packet. The frame ends
// where they begin, and they are dropped and reported; two frames the other
// way round are read in order.
func TestFramesOutOfSequenceAreNotAFramesPacket(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	frame := func(k int) []byte { return whole[frames[k].Off:frames[k].End] }
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	head, rest := whole[:frames[10].Off], whole[frames[12].Off:]
	for _, tc := range []struct {
		name    string
		raw     []byte
		packets int
		lost    int64
		finding string
	}{
		{"a frame again", join(head, frame(10), frame(10), frame(11), rest), len(frames), 0, "bytes hold frames out of sequence, dropped"},
		{"two frames the other way round", join(head, frame(11), frame(10), rest), len(frames), 0, "bytes hold frames numbered ahead of the one behind them, read in order"},
		{"an earlier frame, then a gap in the numbers", join(head, frame(10), frame(3), rest), len(frames) - 1, 4096, "bytes hold frames out of sequence, dropped; the frames skip from sample 45056 to 49152: 4096 samples lost"},
	} {
		d := opened(t, tc.raw, total)
		got, err := readFrames(t, d)
		if err != nil {
			t.Fatal(err)
		}
		var lost int64
		for i, f := range got {
			lost += f.lost
			if k := int(f.pts / 4096); len(f.data) != len(frame(k)) {
				t.Errorf("%s: packet %d at %d is %d bytes, want frame %d alone, %d", tc.name, i, f.pts, len(f.data), k, len(frame(k)))
			}
		}
		if len(got) != tc.packets || lost != tc.lost {
			t.Errorf("%s: %d packets and %d samples lost, want %d and %d", tc.name, len(got), lost, tc.packets, tc.lost)
		}
		exactAt(t, d, total)
		if msgs := strings.Join(damageOf(d), "; "); !strings.Contains(msgs, tc.finding) {
			t.Errorf("%s: findings %q, want %q", tc.name, msgs, tc.finding)
		}
	}
}

// TestFramesNumberedOffTheGapTheyFillAreReadThere: frames whose numbers skip,
// in front of a frame numbered where the gap they open ends, fill that gap
// exactly and are read there, past the stream's length or inside it. With
// nothing the stream could have behind them they are joined on.
func TestFramesNumberedOffTheGapTheyFillAreReadThere(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	const far = 1 << 30
	out := append([]byte(nil), whole[:frames[10].Off]...)
	for k := 10; k < 12; k++ {
		out = append(out, testutil.SetFLACFrameNumber(t, whole[frames[k].Off:frames[k].End], false, uint64(k+far))...)
	}
	ahead := append([]byte(nil), whole[:frames[10].Off]...)
	for k := 10; k < 12; k++ {
		ahead = append(ahead, testutil.SetFLACFrameNumber(t, whole[frames[k].Off:frames[k].End], false, uint64(k+20))...)
	}
	for name, raw := range map[string][]byte{"past the length": out, "inside it": ahead} {
		d := opened(t, append(raw, whole[frames[12].Off:]...), total)
		got, err := readFrames(t, d)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(frames) {
			t.Fatalf("%s: read %d packets, want all %d", name, len(got), len(frames))
		}
		for i, f := range got {
			renumbered := i == 10 || i == 11 // a longer coded number
			if f.pts != frames[i].Pos || f.lost != 0 || (!renumbered && len(f.data) != frames[i].End-frames[i].Off) {
				t.Fatalf("%s: packet %d is %d bytes at %d owing %d, want frame %d's", name, i, len(f.data), f.pts, f.lost, i)
			}
		}
		exactAt(t, d, total)
		if msgs := damageOf(d); len(msgs) != 1 || msgs[0] != fmt.Sprintf("%d bytes hold frames numbered off the gap they fill, read there", len(raw)-frames[10].Off) {
			t.Errorf("%s: findings %v, want the run read at its gap", name, msgs)
		}
	}
	var got []frame
	var err error
	// With nothing the stream could have behind them, they are joined on.
	d, err := flacn.NewDemuxer(container.BytesSource(out), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = readFrames(t, d); err != nil || len(got) != 12 || got[10].pts != (10+far)*4096 || got[10].lost != 0 {
		t.Fatalf("read %d packets (%v), the eleventh at %d owing %d; want twelve, the strays joined on", len(got), err, got[10].pts, got[10].lost)
	}
	if msgs := strings.Join(damageOf(d), "; "); !strings.Contains(msgs, "past what the stream holds; joined") {
		t.Errorf("findings %q, want the skip reported as joined", msgs)
	}
}

// TestAFrameBehindASkipIsNotGivenUpForALaterOne: a skip in the numbers gives
// way only to a frame numbered inside it. One numbered past the frame the
// skip lands on is no reason to drop that frame, and frames numbered off the
// gap behind it that they fill are read there.
func TestAFrameBehindASkipIsNotGivenUpForALaterOne(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	// Frame 9 gone, and frames 11 and 12 numbered 200 on.
	out := append([]byte(nil), whole[:frames[9].Off]...)
	out = append(out, whole[frames[10].Off:frames[10].End]...)
	for k := 11; k < 13; k++ {
		out = append(out, testutil.SetFLACFrameNumber(t, whole[frames[k].Off:frames[k].End], false, uint64(k+200))...)
	}
	d := opened(t, append(out, whole[frames[13].Off:]...), total)
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(frames)-1 || got[9].pts != 10*4096 || got[9].lost != 4096 || got[10].pts != 11*4096 || got[10].lost != 0 || got[12].pts != 13*4096 {
		t.Fatalf("read %d packets, the tenth at %d owing %d and the eleventh at %d owing %d; want %d packets, frame 10 owing 4096 and frames 11 on in place",
			len(got), got[9].pts, got[9].lost, got[10].pts, got[10].lost, len(frames)-1)
	}
	exactAt(t, d, total)
}

// TestALaterFrameDoesNotEndTheWaitOnASkipPastTheStream: a skip past what the
// stream holds is no gap to take, so a frame numbered past it, out of run,
// does not end the wait for a frame the stream could have. The two strays
// are whole, so each is read at the gap it stands in, as they were read
// before the wait.
func TestALaterFrameDoesNotEndTheWaitOnASkipPastTheStream(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	const far = 1 << 30
	out := append([]byte(nil), whole[:frames[10].Off]...)
	out = append(out, testutil.SetFLACFrameNumber(t, whole[frames[10].Off:frames[10].End], false, 10+far)...)
	out = append(out, testutil.SetFLACFrameNumber(t, whole[frames[11].Off:frames[11].End], false, 10+far+5)...)
	d := opened(t, append(out, whole[frames[12].Off:]...), total)
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(frames) {
		t.Fatalf("read %d packets, want all %d", len(got), len(frames))
	}
	for i, f := range got {
		if f.pts != frames[i].Pos || f.lost != 0 {
			t.Fatalf("packet %d is at %d owing %d, want frame %d in place owing nothing", i, f.pts, f.lost, i)
		}
	}
	exactAt(t, d, total)
}

// TestASeekIntoAJoinedHeadLandsWhereTheReadGoesOn: with the first frame
// damaged and the frames behind it numbered past what its bytes could hold,
// they are joined on with nothing owed, so a seek into the head lands on
// the first of them, where a read from the open goes on, not on sample 0.
func TestASeekIntoAJoinedHeadLandsWhereTheReadGoesOn(t *testing.T) {
	whole := unseekableTwin(t, 2)
	const far = 5000
	raw := farBehindAHole(t, whole, 0, far)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != (1+far)*4096 || pkt.Lost != 0 {
		t.Fatalf("the first packet is at %d owing %d (%v), want frame 1 joined at %d owing nothing", pkt.PTS, pkt.Lost, err, (1+far)*4096)
	}
	for _, target := range []int64{0, 1000, 5000} {
		landed, err := d.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.ReadPacket(&pkt); err != nil || landed != (1+far)*4096 || pkt.PTS-pkt.Lost != landed {
			t.Errorf("a seek to %d landed at %d, then read a packet at %d owing %d (%v); want both at %d",
				target, landed, pkt.PTS, pkt.Lost, err, (1+far)*4096)
		}
	}
}

// TestASeekIntoAHeadHoleBeforeAJoinLandsWhereTheReadPlacesIt: a seek into
// the hole the stream starts with lands where the read behind it places
// what it owes, which in front of frames joined on is not sample 0.
func TestASeekIntoAHeadHoleBeforeAJoinLandsWhereTheReadPlacesIt(t *testing.T) {
	whole := unseekableTwin(t, 2)
	const far = 5000
	frames := testutil.FLACFrames(t, whole)
	raw := headless(farBehindAHole(t, whole, 1, far), frames[0].Off)
	// The seek finds the first frame's end once, for the read behind it: the
	// two report and read what a read from the open does.
	var pkt container.Packet
	cost := func(seek bool) (reads, bytes int64, findings int) {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		if seek {
			if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		return int64(src.Reads), src.Bytes, container.DamageOf(d)
	}
	r0, b0, f0 := cost(false)
	if r1, b1, f1 := cost(true); r1 != r0 || b1 != b0 || f1 != f0 {
		t.Errorf("a seek into the head and a read: %d reads, %d bytes, %d findings; a read from the open: %d, %d, %d", r1, b1, f1, r0, b0, f0)
	}
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != (2+far)*4096 {
		t.Fatalf("the first packet is at %d owing %d (%v), want frame 2 joined at %d", pkt.PTS, pkt.Lost, err, (2+far)*4096)
	}
	want := pkt.PTS - pkt.Lost
	for _, target := range []int64{0, 1000} {
		landed, err := d.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.ReadPacket(&pkt); err != nil || landed != want || pkt.PTS-pkt.Lost != landed {
			t.Errorf("a seek to %d landed at %d, then read a packet at %d owing %d (%v); want both at %d", target, landed, pkt.PTS, pkt.Lost, err, want)
		}
	}
}

// TestFramesOutOfOrderAreReadInOrder: frames whose bytes lie out of their
// numbers' order are read in order, by a read and behind a seek: a swapped
// pair, two, one behind damage, and two frames numbered far off the gap in
// front of the frame that follows them, which they fill.
func TestFramesOutOfOrderAreReadInOrder(t *testing.T) {
	whole := unseekableTwin(t, 4)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	frame := func(k int) []byte { return whole[frames[k].Off:frames[k].End] }
	// order lays the frames out as listed; a negative entry is that frame
	// damaged, zeros 100 zeroed bytes, and an entry of 1000 on is that frame
	// numbered 200 on.
	const zeros = -1000
	order := func(ks ...int) []byte {
		out := append([]byte(nil), whole[:frames[0].Off]...)
		for _, k := range ks {
			switch {
			case k == zeros:
				out = append(out, make([]byte, 100)...)
			case k < 0:
				out = append(out, testutil.DamageFLACFrames(t, whole, frames, -k)[frames[-k].Off:frames[-k].End]...)
			case k >= 1000:
				out = append(out, testutil.SetFLACFrameNumber(t, frame(k-1000), false, uint64(k-1000+200))...)
			default:
				out = append(out, frame(k)...)
			}
		}
		return out
	}
	seq := func(from, to int) []int { // [from, to)
		var ks []int
		for k := from; k < to; k++ {
			ks = append(ks, k)
		}
		return ks
	}
	n := len(frames)
	for _, tc := range []struct {
		name string
		ks   []int
		gone []int // frames a read cannot have
		junk int   // bytes of junk behind the frames
	}{
		{"a swapped pair", slices.Concat(seq(0, 6), []int{7, 6}, seq(8, n)), nil, 0},
		{"two swapped pairs", slices.Concat(seq(0, 6), []int{7, 6}, seq(8, 20), []int{21, 20}, seq(22, n)), nil, 0},
		{"a pair swapped behind damage", slices.Concat(seq(0, 6), []int{-6, 7, 9, 8}, seq(10, n)), []int{6}, 0},
		{"two frames numbered 200 on", slices.Concat(seq(0, 6), []int{1006, 1007}, seq(8, n)), nil, 0},
		{"the last two swapped, junk behind", slices.Concat(seq(0, n-2), []int{n - 1, n - 2}), nil, 64},
		{"a pair swapped behind a frame gone", slices.Concat(seq(0, 6), []int{8, 7}, seq(9, n)), []int{6}, 0},
		{"a frame again, then a pair swapped", slices.Concat(seq(0, 7), []int{6, 8, 7}, seq(9, n)), nil, 0},
		{"zeroed bytes, then two frames numbered 200 on", slices.Concat(seq(0, 6), []int{zeros, 1006, 1007}, seq(8, n)), nil, 0},
		{"two frames numbered 200 on, the frame behind them gone", slices.Concat(seq(0, 6), []int{1006, 1007}, seq(9, n)), []int{8}, 0},
		{"two frames numbered 200 on, then a pair swapped", slices.Concat(seq(0, 6), []int{1006, 1007, 9, 8}, seq(10, n)), nil, 0},
		{"a pair swapped, then two frames numbered 200 on", slices.Concat(seq(0, 6), []int{7, 6, 1008, 1009}, seq(10, n)), nil, 0},
		{"a frame ahead, the one in front of its number gone", slices.Concat(seq(0, 6), []int{9, 6, 7}, seq(10, n)), []int{8}, 0},
	} {
		zeroed := slices.Contains(tc.ks, zeros)
		raw := append(order(tc.ks...), bytes.Repeat([]byte("x"), tc.junk)...)
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tr.Samples != total && tc.junk == 0 {
			t.Fatalf("%s: the track says %d samples, want %d", tc.name, tr.Samples, total)
		}
		want := func(from int) []int {
			var ks []int
			for k := from; k < n; k++ {
				if !slices.Contains(tc.gone, k) {
					ks = append(ks, k)
				}
			}
			return ks
		}
		check := func(how string, from int) {
			got, err := readFrames(t, d)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.name, how, err)
			}
			var ks []int
			for i, f := range got {
				ks = append(ks, int(f.pts/4096))
				// Frames read in order follow no gap.
				if f.discont && i > 0 && f.lost == 0 && tc.gone == nil {
					t.Errorf("%s %s: frame %d is marked discontinuous", tc.name, how, f.pts/4096)
				}
			}
			if !slices.Equal(ks, want(from)) {
				t.Errorf("%s %s: read frames %v, want %v", tc.name, how, ks, want(from))
			}
		}
		check("from the open", 0)
		if msgs := strings.Join(damageOf(d), "; "); zeroed != strings.Contains(msgs, "100 bytes are not a valid frame, dropped") ||
			strings.Contains(msgs, "out of sequence") != strings.HasPrefix(tc.name, "a frame again") {
			t.Errorf("%s: findings %q", tc.name, msgs)
		}
		for _, k := range []int{5, 6, 7, 8, 9, 20, 21} {
			landed, err := d.SeekSample(context.Background(), 0, int64(k)*4096+5)
			if err != nil {
				t.Fatal(err)
			}
			from := int(landed / 4096)
			if landed%4096 != 0 || from > k {
				t.Errorf("%s: a seek into frame %d landed at %d", tc.name, k, landed)
				continue
			}
			check(fmt.Sprintf("behind a seek into frame %d", k), from)
		}
	}
}

// TestASeekWalkFromTheFrameBehindASwapStartsFurtherBack: a walk handed the
// frame that lies behind the one numbered after it meets a skip behind its
// first frame and asks to start further back, where that frame lies. So does
// one handed the frame in front of the pair, which takes a run aside behind
// its first frame; from the frame in front of that it lands and the read
// holds both in order.
func TestASeekWalkFromTheFrameBehindASwapStartsFurtherBack(t *testing.T) {
	whole := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, whole)
	const k = 6
	raw := slices.Concat(whole[:frames[k].Off], whole[frames[k+1].Off:frames[k+1].End],
		whole[frames[k].Off:frames[k].End], whole[frames[k+2].Off:])
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	behind := int64(frames[k].Off + frames[k+1].End - frames[k+1].Off)
	if _, ok, err := d.WalkFromForTest(behind, k*4096+5); err != nil || ok {
		t.Errorf("a walk from frame %d, behind frame %d, landed (%v), want it to start further back", k, k+1, err)
	}
	if _, ok, err := d.WalkFromForTest(int64(frames[k-1].Off), k*4096+5); err != nil || ok {
		t.Errorf("a walk from frame %d, in front of the pair, landed (%v), want it to start further back", k-1, err)
	}
	landed, ok, err := d.WalkFromForTest(int64(frames[k-2].Off), k*4096+5)
	if err != nil || !ok || landed != k*4096 {
		t.Fatalf("a walk from frame %d landed at %d, %v (%v), want %d", k-2, landed, ok, err, k*4096)
	}
	got, err := readFrames(t, d)
	if err != nil || len(got) < 2 || got[0].pts != k*4096 || got[1].pts != (k+1)*4096 || got[1].lost != 0 {
		t.Fatalf("behind the walk: %d packets (%v), want frames %d and %d in order", len(got), err, k, k+1)
	}
}

// TestASeekLetsGoOfAHeldRun: a run held for its numbers is the read's; a seek
// past it starts afresh and reports nothing of it.
func TestASeekLetsGoOfAHeldRun(t *testing.T) {
	whole := unseekableTwin(t, 30) // over two windows, so the seek walks from near its target
	frames := testutil.FLACFrames(t, whole)
	f := func(k int) []byte { return whole[frames[k].Off:frames[k].End] }
	raw := slices.Concat(whole[:frames[6].Off], f(9), f(6), f(7), f(8), whole[frames[10].Off:])
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for range 7 { // frames 0 to 6, frame 9 held
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.SeekSample(context.Background(), 0, 200*4096); err != nil {
		t.Fatal(err)
	}
	readCount(t, d)
	if msgs := strings.Join(damageOf(d), "; "); strings.Contains(msgs, "out of sequence") {
		t.Errorf("a seek past the held run and a read reported it dropped: %q", msgs)
	}
}

// TestAReadThroughASwapCostsNoMoreReads: the window keeps a run held for its
// numbers, so reading it costs no read the stream in order does not.
func TestAReadThroughASwapCostsNoMoreReads(t *testing.T) {
	whole := unseekableTwin(t, 30)
	frames := testutil.FLACFrames(t, whole)
	reads := func(raw []byte) int {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.Reset()
		readCount(t, d)
		return src.Reads
	}
	want := reads(whole)
	for k := 1; k < len(frames)-2; k++ {
		raw := slices.Concat(whole[:frames[k].Off], whole[frames[k+1].Off:frames[k+1].End],
			whole[frames[k].Off:frames[k].End], whole[frames[k+2].Off:])
		if got := reads(raw); got > want {
			t.Fatalf("frames %d and %d swapped: a read took %d reads, the stream in order %d", k, k+1, got, want)
		}
	}
}

// TestARunLongerThanItsGapIsJoinedOn: frames numbered past the stream, more
// than the gap in front of the frame behind them holds, are joined on at
// their own numbers, as they always were.
func TestARunLongerThanItsGapIsJoinedOn(t *testing.T) {
	whole := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, whole)
	raw := append([]byte(nil), whole[:frames[6].Off]...)
	for k := 6; k < 9; k++ {
		raw = append(raw, testutil.SetFLACFrameNumber(t, whole[frames[k].Off:frames[k].End], false, uint64(k+200))...)
	}
	raw = append(raw, whole[frames[7].Off:]...) // frame 7 again, then the rest
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	var at []int64
	for _, f := range got {
		at = append(at, f.pts/4096)
	}
	if len(at) < 9 || !slices.Equal(at[:9], []int64{0, 1, 2, 3, 4, 5, 206, 207, 208}) {
		t.Errorf("read frames %v, want 0 to 5 and the run joined at 206 to 208", at)
	}
	// In front of the successor itself there is no gap to read it in.
	raw = slices.Concat(whole[:frames[6].Off], testutil.SetFLACFrameNumber(t, whole[frames[6].Off:frames[6].End], false, 206), whole[frames[6].Off:])
	if d, err = flacn.NewDemuxer(container.BytesSource(raw), nil); err != nil {
		t.Fatal(err)
	}
	if got, err = readFrames(t, d); err != nil || len(got) < 7 || got[6].pts != 206*4096 {
		t.Errorf("a frame numbered 200 on in front of the successor: %d packets (%v), want it joined at %d", len(got), err, 206*4096)
	}
}
