package flacn_test

// A frame whose bytes do not check is damage the read steps over: it resyncs on
// the next frame that checks, which its own number places, as libFLAC and
// ffmpeg do.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// frame is one packet as a read delivered it, its data copied.
type frame struct {
	pts, dur int64
	data     []byte
	discont  bool
	lost     int64
}

// readFrames reads d to its end and returns every packet, with the error that
// ended the read, nil for a clean end.
func readFrames(t testing.TB, d *flacn.Demuxer) ([]frame, error) {
	t.Helper()
	var out []frame
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, frame{pkt.PTS, pkt.Dur, append([]byte(nil), pkt.Data...), pkt.Discont, pkt.Lost})
	}
}

// frameSpans is where each frame of raw, an intact native FLAC, sits.
func frameSpans(t testing.TB, raw []byte) [][2]int {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := readFrames(t, d)
	if err != nil || len(frames) == 0 {
		t.Fatalf("read %d frames: %v", len(frames), err)
	}
	off := bytes.Index(raw, frames[0].data)
	spans := make([][2]int, len(frames))
	for i, f := range frames {
		spans[i] = [2]int{off, off + len(f.data)}
		off += len(f.data)
	}
	return spans
}

// damageFrames returns raw with 16 bytes inverted in the middle of each named
// frame, so its checksum fails and every other frame is untouched.
func damageFrames(t testing.TB, raw []byte, spans [][2]int, frames ...int) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	for _, k := range frames {
		if spans[k][1]-spans[k][0] < 64 {
			t.Fatalf("frame %d is %d bytes, too short to damage inside", k, spans[k][1]-spans[k][0])
		}
		mid := (spans[k][0] + spans[k][1]) / 2
		for i := mid - 8; i < mid+8; i++ {
			out[i] ^= 0xFF
		}
	}
	return out
}

// unseekableTwin is encodedTwin without the SEEKTABLE a seekable encode
// writes, so its seeks bisect.
func unseekableTwin(t *testing.T, seconds int) []byte {
	t.Helper()
	src := loopedPCM(t, seconds)
	defer audio.Put(src)
	var buf bytes.Buffer
	encodeStream(t, src, 5, &buf, int64(src.N))
	return buf.Bytes()
}

// decodeOnTimeline decodes frames into one buffer at their own positions.
func decodeOnTimeline(t *testing.T, tr container.Track, frames []frame) *audio.Buffer {
	t.Helper()
	dec := newDecoder(t, tr)
	defer dec.Release()
	end := frames[len(frames)-1].pts + frames[len(frames)-1].dur
	out := audio.Get(tr.Fmt, int(end))
	out.N = int(end)
	for _, f := range frames {
		if err := dec.Decode(f.data, func(b *audio.Buffer) error {
			audio.CopyFrames(out, int(f.pts), b, 0, b.N)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func newDecoder(t *testing.T, tr container.Track) *flac.Decoder {
	t.Helper()
	si, err := flac.ParseStreamInfo(tr.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := flac.NewDecoder(si, tr.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	return dec
}

// TestReadResyncsPastDamagedFrames: every frame that checks is delivered at
// its own position, decoding to the clean file's samples there, with one
// warning per hole naming where it is and the samples it cost. The open's tail
// read cannot see a hole, so the track is the clean file's.
func TestReadResyncsPastDamagedFrames(t *testing.T) {
	clean := encodedTwin(t, 30)
	spans := frameSpans(t, clean)
	n := len(spans)
	ref, err := flacn.NewDemuxer(container.BytesSource(clean), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := ref.Tracks()[0]
	refFrames, err := readFrames(t, ref)
	if err != nil {
		t.Fatal(err)
	}
	pcm := decodeOnTimeline(t, want, refFrames)
	defer audio.Put(pcm)

	for _, c := range []struct {
		name  string
		holes []int
	}{
		{"one hole", []int{n / 3}},
		{"two holes", []int{n / 3, 2 * n / 3}},
		{"the first frame", []int{0}},
		// The frame after the hole is the last, so it checks at the data end.
		{"the frame before the last", []int{n - 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := damageFrames(t, clean, spans, c.holes...)
			d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.Samples != want.Samples || tr.SamplesExact != want.SamplesExact {
				t.Errorf("%d samples (exact %v), want the clean file's %d (exact %v)",
					tr.Samples, tr.SamplesExact, want.Samples, want.SamplesExact)
			}
			got, err := readFrames(t, d)
			if err != nil {
				t.Fatalf("the read failed after %d frames: %v", len(got), err)
			}

			lost := map[int]bool{}
			for _, k := range c.holes {
				lost[k] = true
			}
			var kept []frame
			var lostSamples int64
			afterHole := map[int64]bool{}
			for i, f := range refFrames {
				if lost[i] {
					lostSamples += f.dur
					continue
				}
				if i > 0 && lost[i-1] {
					afterHole[f.pts] = true
				}
				kept = append(kept, f)
			}
			if len(got) != len(kept) {
				t.Fatalf("delivered %d frames, want the %d of %d that check", len(got), len(kept), n)
			}
			var sum int64
			for i, f := range got {
				w := kept[i]
				if f.pts != w.pts || f.dur != w.dur || !bytes.Equal(f.data, w.data) {
					t.Fatalf("frame %d is [%d, +%d) of %d bytes, want [%d, +%d) of %d", i, f.pts, f.dur, len(f.data),
						w.pts, w.dur, len(w.data))
				}
				if f.discont != afterHole[f.pts] {
					t.Errorf("frame at %d: Discont %v, want %v", f.pts, f.discont, afterHole[f.pts])
				}
				sum += f.dur
			}
			if sum != want.Samples-lostSamples {
				t.Errorf("delivered %d samples, want the clean %d less the %d lost", sum, want.Samples, lostSamples)
			}

			dec := newDecoder(t, want)
			defer dec.Release()
			for _, f := range got {
				if err := dec.Decode(f.data, func(b *audio.Buffer) error {
					for ch := range b.Fmt.Channels {
						if i := testutil.DiffI32(b.ChanI(ch)[:b.N], pcm.ChanI(ch)[f.pts:f.pts+int64(b.N)]); i >= 0 {
							t.Fatalf("frame at %d, channel %d: differs from the clean decode at %d", f.pts, ch, f.pts+int64(i))
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

			ws := d.Warnings()
			if len(ws) != len(c.holes) {
				t.Fatalf("warnings %v, want one per hole", ws)
			}
			for i, k := range c.holes {
				if w := ws[i]; w.Kind != container.Damage || w.Offset != int64(spans[k][0]) ||
					!strings.Contains(w.Msg, fmt.Sprintf("%d samples", refFrames[k].dur)) {
					t.Errorf("warning %+v, want damage at offset %d naming its %d samples", w, spans[k][0], refFrames[k].dur)
				}
			}
		})
	}
}

// TestStrictRefusesAtTheFirstHole: strict refuses the damage where the read
// meets it, after the frames before it.
func TestStrictRefusesAtTheFirstHole(t *testing.T) {
	clean := encodedTwin(t, 30)
	spans := frameSpans(t, clean)
	n := len(spans)
	raw := damageFrames(t, clean, spans, n/3, 2*n/3)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict open refused damage its tail read cannot see: %v", err)
	}
	got, err := readFrames(t, d)
	if len(got) != n/3 {
		t.Errorf("strict delivered %d frames, want the %d before the first hole", len(got), n/3)
	}
	if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || !strings.Contains(err.Error(), fmt.Sprintf("offset %d", spans[n/3][0])) {
		t.Errorf("strict read ended with %v, want malformed input at offset %d", err, spans[n/3][0])
	}
}

// TestSeekPastAHole: a seek past a hole lands where the clean file's does,
// through the SEEKTABLE and by bisection, and some of those seeks walk across
// the hole to get there.
func TestSeekPastAHole(t *testing.T) {
	for _, c := range []struct {
		name string
		twin func(*testing.T, int) []byte
	}{{"seek table", encodedTwin}, {"bisection", unseekableTwin}} {
		t.Run(c.name, func(t *testing.T) {
			clean := c.twin(t, 30)
			spans := frameSpans(t, clean)
			n := len(spans)
			// Past the middle, where bisection's first probe lands, so its
			// walks to the targets below start before the hole.
			h := n/2 + 3
			raw := damageFrames(t, clean, spans, h)
			ref, err := flacn.NewDemuxer(container.BytesSource(clean), nil)
			if err != nil {
				t.Fatal(err)
			}
			refFrames, err := readFrames(t, ref)
			if err != nil {
				t.Fatal(err)
			}
			d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}

			var targets []int64
			for k := h + 1; k < min(n, h+16); k++ {
				targets = append(targets, refFrames[k].pts, refFrames[k].pts+refFrames[k].dur/2)
			}
			last := refFrames[n-1]
			targets = append(targets, refFrames[n-10].pts+77, last.pts+5, last.pts+last.dur+1000)
			var pa, pb container.Packet
			for _, target := range targets {
				la, err := ref.SeekSample(context.Background(), 0, target)
				if err != nil {
					t.Fatal(err)
				}
				lb, err := d.SeekSample(context.Background(), 0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				if lb != la {
					t.Fatalf("seek to %d lands at %d, the clean file's at %d", target, lb, la)
				}
				if err := ref.ReadPacket(&pa); err != nil {
					t.Fatal(err)
				}
				if err := d.ReadPacket(&pb); err != nil {
					t.Fatalf("read after the seek to %d: %v", target, err)
				}
				if pb.PTS != pa.PTS || pb.Dur != pa.Dur || !bytes.Equal(pb.Data, pa.Data) {
					t.Fatalf("seek to %d: the next frame is [%d, +%d), the clean file's [%d, +%d)",
						target, pb.PTS, pb.Dur, pa.PTS, pa.Dur)
				}
			}
			if ws := d.Warnings(); len(ws) != 1 || ws[0].Offset != int64(spans[h][0]) {
				t.Errorf("warnings %v, want the hole at %d once: no seek walked across it, or it was reported twice",
					ws, spans[h][0])
			}
		})
	}
}

// TestSeekIntoAHole: a target the hole took lands on the last frame before
// it, at or before the target as a landing must be, and the read goes on from
// there across the hole as a linear one does. That holds where bisection lands
// on the damaged frame itself, which leaves the walk nothing behind it to land
// on, and where the hole is the first frame, the stream begins after it.
func TestSeekIntoAHole(t *testing.T) {
	for _, c := range []struct {
		name string
		twin func(*testing.T, int) []byte
		hole func(spans [][2]int, size int) int
	}{
		{"seek table", encodedTwin, func(s [][2]int, _ int) int { return len(s) / 2 }},
		{"bisection walks across it", unseekableTwin, func(s [][2]int, _ int) int { return len(s)/2 + 3 }},
		// Bisection's first probe is the first frame past the middle of the
		// frame data, which then holds every target in it.
		{"bisection lands in it", unseekableTwin, func(s [][2]int, size int) int {
			mid := s[0][0] + (size-s[0][0])/2
			k := 0
			for s[k][0] < mid {
				k++
			}
			return k
		}},
		{"the first frame", unseekableTwin, func([][2]int, int) int { return 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			clean := c.twin(t, 30)
			spans := frameSpans(t, clean)
			h := c.hole(spans, len(clean))
			ref, err := flacn.NewDemuxer(container.BytesSource(clean), nil)
			if err != nil {
				t.Fatal(err)
			}
			refFrames, err := readFrames(t, ref)
			if err != nil {
				t.Fatal(err)
			}
			gone, after := refFrames[h], refFrames[h+1]
			landing, want := gone.pts, []frame{after} // the stream begins in the hole
			if h > 0 {
				landing, want = refFrames[h-1].pts, []frame{refFrames[h-1], after}
			}
			for _, target := range []int64{gone.pts, gone.pts + gone.dur/2, gone.pts + gone.dur - 1} {
				d, err := flacn.NewDemuxer(container.BytesSource(damageFrames(t, clean, spans, h)), nil)
				if err != nil {
					t.Fatal(err)
				}
				landed, err := d.SeekSample(context.Background(), 0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				if landed != landing {
					t.Fatalf("seek to %d in the hole lands at %d, want %d", target, landed, landing)
				}
				var pkt container.Packet
				for i, w := range want {
					if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != w.pts || !bytes.Equal(pkt.Data, w.data) {
						t.Fatalf("seek to %d, packet %d: at %d (%v), want the frame at %d", target, i, pkt.PTS, err, w.pts)
					}
					// The read crosses the hole after the landing, or begins in
					// it, owing what it took.
					crossed, owed := w.pts == after.pts, int64(0)
					if crossed {
						owed = gone.dur
					}
					if pkt.Discont != crossed || pkt.Lost != owed {
						t.Errorf("seek to %d, packet %d: Discont %v owing %d, want %v owing %d", target, i, pkt.Discont, pkt.Lost, crossed, owed)
					}
				}
			}
		})
	}
}

// TestHoleWarnsOnce: a read and the seeks after it cross the same hole again
// and again, and it is one hole, so it is one warning.
func TestHoleWarnsOnce(t *testing.T) {
	clean := unseekableTwin(t, 30)
	spans := frameSpans(t, clean)
	n := len(spans)
	h := n / 2
	d, err := flacn.NewDemuxer(container.BytesSource(damageFrames(t, clean, spans, h)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := d.SeekSample(context.Background(), 0, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := readFrames(t, d); err != nil {
			t.Fatal(err)
		}
	}
	var pkt container.Packet
	for k := h - 4; k < h+4; k++ {
		if _, err := d.SeekSample(context.Background(), 0, int64(k)*4096+100); err != nil {
			t.Fatal(err)
		}
		if err := d.ReadPacket(&pkt); err != nil && err != io.EOF {
			t.Fatal(err)
		}
	}
	if ws := d.Warnings(); len(ws) != 1 {
		t.Errorf("warnings %v, want the one hole once", ws)
	}
}

// TestDamageWarningsAreCapped: a file damaged throughout still reads every
// frame that checks, and the list it reports is bounded, at the 64 every
// tolerant demuxer in this tree keeps.
func TestDamageWarningsAreCapped(t *testing.T) {
	clean := unseekableTwin(t, 30)
	spans := frameSpans(t, clean)
	var holes []int
	for k := 1; k < len(spans)-1; k += 3 {
		holes = append(holes, k)
	}
	if len(holes) <= 64 {
		t.Fatalf("%d holes, too few to reach the cap", len(holes))
	}
	d, err := flacn.NewDemuxer(container.BytesSource(damageFrames(t, clean, spans, holes...)), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(spans)-len(holes) {
		t.Errorf("delivered %d frames, want the %d that check", len(got), len(spans)-len(holes))
	}
	if ws := d.Warnings(); len(ws) == 0 || len(ws) > 64 {
		t.Errorf("%d warnings for %d holes, want at most 64", len(ws), len(holes))
	}
}

// TestMeasurementCrossesAHole: an open whose tail does not check measures the
// length by walking the last window, and the walk crosses a hole there as the
// read does, so the length it reports is where a read's frames end.
func TestMeasurementCrossesAHole(t *testing.T) {
	clean := unseekableTwin(t, 30)
	spans := frameSpans(t, clean)
	n := len(spans)
	raw := damageFrames(t, clean, spans, n-4)
	raw = raw[:len(raw)-100] // the closing frame no longer checks either
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	got, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	end := got[len(got)-1].pts + got[len(got)-1].dur
	if want := int64(n-1) * 4096; end != want {
		t.Fatalf("the read's frames end at %d, want %d, where the closing frame begins", end, want)
	}
	if tr.Samples != end || tr.SamplesExact {
		t.Errorf("the open measured %d samples (exact %v), want %d, where the read's frames end, unexact",
			tr.Samples, tr.SamplesExact, end)
	}
}
