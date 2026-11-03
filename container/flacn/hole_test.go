package flacn_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestHoleTellsTheReaderWhatItLost: the frame after a hole carries the samples
// the hole took, so a reader can keep its positions where the frames say.
func TestHoleTellsTheReaderWhatItLost(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	d, err := flacn.NewDemuxer(container.BytesSource(damageFrames(t, raw, spans, len(spans)/2)), nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for i, f := range frames {
		want := int64(0)
		if i > 0 {
			want = f.pts - (frames[i-1].pts + frames[i-1].dur)
		}
		if f.lost != want {
			t.Errorf("frame at %d: lost %d, want %d", f.pts, f.lost, want)
		}
		total += f.lost
	}
	if total == 0 {
		t.Fatal("no frame says what the hole took")
	}
}

// TestFrameBeforeADamagedHeaderSurvives: a frame is whole by its own CRC-16,
// so the one before a damaged header is delivered and only the damaged frame
// is lost.
func TestFrameBeforeADamagedHeaderSurvives(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	k := len(spans) / 2
	cd, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := readFrames(t, cd)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), raw...)
	bad[spans[k+1][0]+2] ^= 0xFF // the next header's CRC-8 fails
	d, err := flacn.NewDemuxer(container.BytesSource(bad), nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range frames {
		if f.pts != clean[k].pts {
			continue
		}
		if !bytes.Equal(f.data, clean[k].data) {
			t.Fatal("the frame before the damaged header came back changed")
		}
		if i+1 >= len(frames) || frames[i+1].pts != clean[k+2].pts || frames[i+1].lost != clean[k+1].dur {
			t.Fatalf("after the intact frame: want the frame at %d with %d lost", clean[k+2].pts, clean[k+1].dur)
		}
		return
	}
	t.Fatalf("the intact frame at %d before a damaged header was lost with it", clean[k].pts)
}

// TestStreamInfoFollowsTheFrames: when the frames disagree with STREAMINFO,
// the track's STREAMINFO states the frames' span and no MD5, since a header
// whose total does not describe the frames has a signature that cannot either,
// whatever else it states. An intact file's STREAMINFO is handed on byte for
// byte.
func TestStreamInfoFollowsTheFrames(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	const siAt = 8 // "fLaC" and the block header
	signed := func(raw []byte, total int64) []byte {
		out := append([]byte(nil), raw...)
		si, err := flac.ParseStreamInfo(out[siAt : siAt+flac.StreamInfoLen])
		if err != nil {
			t.Fatal(err)
		}
		si.Samples = total
		for i := range si.MD5 {
			si.MD5[i] = byte(i + 1)
		}
		b, err := si.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		copy(out[siAt:], b)
		return out
	}
	cd, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	span := cd.Tracks()[0].Samples
	frames, err := readFrames(t, cd)
	if err != nil {
		t.Fatal(err)
	}
	const shift = 100
	cut := testutil.RenumberFLACFrames(t, raw, shift)
	origin := shift * frames[0].dur
	// A minimum block size of 8 parses, and no header writer would state it.
	oddBounds := signed(testutil.VariableFLACFrames(t, raw, 0), span)
	oddBounds[siAt], oddBounds[siAt+1] = 0, 8
	cases := []struct {
		name   string
		raw    []byte
		intact bool
	}{
		{"intact", signed(raw, span), true},
		{"truncated", signed(raw, span)[:spans[len(spans)/2][1]], false},
		{"cut keeping its source's total", signed(cut, origin+span), false},
		{"truncated under odd block bounds", oddBounds[:spans[len(spans)/2][1]], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := flacn.NewDemuxer(container.BytesSource(tc.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			tr := d.Tracks()[0]
			if tc.intact {
				if !bytes.Equal(tr.CodecConfig, tc.raw[siAt:siAt+flac.StreamInfoLen]) {
					t.Fatal("an intact file's STREAMINFO was not handed on byte for byte")
				}
				return
			}
			si, err := flac.ParseStreamInfo(tr.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			if si.Samples != tr.Samples {
				t.Errorf("STREAMINFO states %d samples, the frames %d", si.Samples, tr.Samples)
			}
			if si.MD5 != ([16]byte{}) {
				t.Errorf("STREAMINFO keeps an MD5 its frames cannot have")
			}
		})
	}
}

// TestDamagedFLACKeepsItsTimeline: a read fills a hole with silence, so it
// delivers the length the open declared, every sample outside the hole sits
// where the clean file has it, and a seek past the hole agrees with the read.
func TestDamagedFLACKeepsItsTimeline(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	k := len(spans) / 2
	cd, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := readFrames(t, cd)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := int(clean[k].pts), int(clean[k].pts+clean[k].dur)
	want := readMedia(t, raw)
	defer audio.Put(want)
	bad := damageFrames(t, raw, spans, k)
	got := readMedia(t, bad)
	defer audio.Put(got)
	if got.N != want.N {
		t.Fatalf("delivered %d samples, want the declared %d", got.N, want.N)
	}
	for c := range got.Fmt.Channels {
		for i := range got.N {
			g, w := got.ChanI(c)[i], want.ChanI(c)[i]
			if i >= lo && i < hi {
				w = 0
			}
			if g != w {
				t.Fatalf("channel %d sample %d: %d, want %d (hole [%d, %d))", c, i, g, w, lo, hi)
			}
		}
	}
	med, err := format.Open(container.BytesSource(bad), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	target := int64(hi + 3*int(clean[k].dur) + 17)
	if _, err := med.SeekSample(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	buf := audio.Get(med.Info().Default().Fmt, 256)
	defer audio.Put(buf)
	if err := med.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	for i := range buf.N {
		if buf.ChanI(0)[i] != got.ChanI(0)[int(target)+i] {
			t.Fatalf("a seek to %d disagrees with the read at sample %d", target, int(target)+i)
		}
	}
}

// readMedia decodes raw through format.Media to its end.
func readMedia(t *testing.T, raw []byte) *audio.Buffer {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	out := audio.Get(f, 1<<20)
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		audio.CopyFrames(out, out.N, buf, 0, buf.N)
		out.N += buf.N
	}
}

// TestHoleMustFitItsBytes: frame numbers past what the file's bytes could
// hold are not believed. A hole the bytes cannot explain ends the read there
// instead of paying the jump out as silence, and the open does not call the
// frames' claim an exact length.
func TestHoleMustFitItsBytes(t *testing.T) {
	raw := fixture(t, "sine-s16.flac")
	spans := frameSpans(t, raw)
	if len(spans) < 4 {
		t.Fatalf("fixture has %d frames, want at least 4", len(spans))
	}
	out := append([]byte(nil), raw[:spans[1][0]]...)
	out = append(out, damageFrames(t, raw, spans, 1)[spans[1][0]:spans[1][1]]...)
	for i, k := range []int{2, 3} {
		out = append(out, testutil.SetFLACFrameNumber(t, raw[spans[k][0]:spans[k][1]], false, 1<<30+uint64(i))...)
	}
	d, err := flacn.NewDemuxer(container.BytesSource(out), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.SamplesExact && tr.Samples > 15435 {
		t.Errorf("opens as %d samples, exact, from frame numbers no file this size holds", tr.Samples)
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		if f.lost > 1<<24 {
			t.Fatalf("frame at %d says a hole took %d samples", f.pts, f.lost)
		}
	}
	if len(d.Warnings()) == 0 {
		t.Error("the damage raised no warning")
	}
}

// TestZeroedFrameIsAHole: a frame overwritten with zeros keeps the checksum of
// the bytes before it (CRC-16 starts from zero), so it must be told from a gap
// in the numbers by where the frame before it ends by its own parse.
func TestZeroedFrameIsAHole(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	k := len(spans) / 2
	cd, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := readFrames(t, cd)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), raw...)
	clear(bad[spans[k][0]:spans[k][1]])
	d, err := flacn.NewDemuxer(container.BytesSource(bad), nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != len(clean)-1 {
		t.Fatalf("read %d frames, want every frame but the zeroed one (%d)", len(frames), len(clean)-1)
	}
	for i, f := range frames {
		want := clean[i]
		if i >= k {
			want = clean[i+1]
		}
		if f.pts != want.pts || !bytes.Equal(f.data, want.data) {
			t.Fatalf("frame %d: at %d, want the clean frame at %d", i, f.pts, want.pts)
		}
	}
	if frames[k].lost != clean[k].dur || len(d.Warnings()) == 0 {
		t.Fatalf("the frame after the zeroed one: lost %d, warnings %v; want %d lost and a warning",
			frames[k].lost, d.Warnings(), clean[k].dur)
	}
}

// TestJunkTailIsDamageAtOpen: bytes after the last whole frame that are no tag
// are damage the open's measurement meets, so a strict open refuses them and a
// tolerant one lists them.
func TestJunkTailIsDamageAtOpen(t *testing.T) {
	raw := append(fixture(t, "sine-s16.flac"), bytes.Repeat([]byte{0x5A}, 100)...)
	if _, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true}); err == nil {
		t.Fatal("a strict open accepted junk after the last frame")
	}
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Warnings(), func(w container.Warning) bool { return w.Kind == container.Damage }) {
		t.Fatalf("a tolerant open listed no damage: %v", d.Warnings())
	}
}

// damageOf is d's Damage warnings.
func damageOf(d *flacn.Demuxer) []string {
	var out []string
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			out = append(out, w.Msg)
		}
	}
	return out
}

// TestCleanJumpIsAHole: frames gone without their numbers, every frame left
// whole, are a hole the stream's measured length vouches for: the samples the
// numbers skip are lost, with a warning, the frames after sit where their
// numbers put them, and a seek into the gap lands before it.
func TestCleanJumpIsAHole(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	k := len(spans) / 2
	cut := append(append([]byte(nil), raw[:spans[k][0]]...), raw[spans[k+3][0]:]...)
	cd, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := readFrames(t, cd)
	if err != nil {
		t.Fatal(err)
	}
	d, err := flacn.NewDemuxer(container.BytesSource(cut), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; !tr.SamplesExact || tr.Samples != cd.Tracks()[0].Samples {
		t.Fatalf("%d samples (exact %v), want the frames' %d exact", tr.Samples, tr.SamplesExact, cd.Tracks()[0].Samples)
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != len(clean)-3 {
		t.Fatalf("%d frames, want the %d left", len(frames), len(clean)-3)
	}
	for i, f := range frames {
		j, lost := i, int64(0)
		if i >= k {
			j = i + 3
		}
		if i == k {
			lost = clean[k].dur + clean[k+1].dur + clean[k+2].dur
		}
		if f.pts != clean[j].pts || !bytes.Equal(f.data, clean[j].data) || f.lost != lost {
			t.Fatalf("frame %d at %d (lost %d), want frame %d at %d (lost %d)", i, f.pts, f.lost, j, clean[j].pts, lost)
		}
	}
	if dm := damageOf(d); len(dm) != 1 || !strings.Contains(dm[0], "skip") {
		t.Errorf("damage %v, want one warning that the frames skip", dm)
	}
	if landed, err := d.SeekSample(context.Background(), 0, clean[k+1].pts); err != nil || landed != clean[k-1].pts {
		t.Errorf("a seek into the gap landed at %d (%v), want the frame before it at %d", landed, err, clean[k-1].pts)
	}
}

// TestCleanJumpPastTheLengthJoins: numbers that skip past what the file's bytes
// could hold vouch for nothing, so no silence is paid out: the frames after
// the jump are delivered whole and joined on, with a warning.
func TestCleanJumpPastTheLengthJoins(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	far := testutil.RenumberFLACFrames(t, raw, 1<<30)
	farSpans := frameSpans(t, far)
	k := len(spans) / 2
	cut := append(append([]byte(nil), raw[:spans[k][0]]...), far[farSpans[k][0]:]...)
	d, err := flacn.NewDemuxer(container.BytesSource(cut), nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != len(spans) {
		t.Fatalf("%d frames, want all %d whole", len(frames), len(spans))
	}
	for _, f := range frames {
		if f.lost != 0 {
			t.Fatalf("frame at %d says %d samples were lost", f.pts, f.lost)
		}
	}
	if len(damageOf(d)) == 0 {
		t.Error("the jump raised no warning")
	}
}

// TestCleanJumpUnderAnInexactLengthIsAHole: a jump the bytes could hold is a
// hole whether or not the length is exact, so a read places the frames after
// it where their numbers do, as a seek and a measure do.
func TestCleanJumpUnderAnInexactLengthIsAHole(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	k := len(spans) / 2
	cut := append(append([]byte(nil), raw[:spans[k][0]]...), raw[spans[k+3][0]:]...)
	cut = cut[:len(cut)-10] // a damaged tail: the length is not exact
	d, err := flacn.NewDemuxer(container.BytesSource(cut), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tracks()[0].SamplesExact {
		t.Fatal("the damaged tail left the length exact; this cell needs one that is not")
	}
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if f := frames[k]; f.lost != 3*f.dur {
		t.Fatalf("the frame after the jump at %d says %d samples were lost, want %d", f.pts, f.lost, 3*f.dur)
	}
	landed, err := d.SeekSample(context.Background(), 0, frames[k+1].pts)
	if err != nil || landed != frames[k+1].pts {
		t.Errorf("a seek to %d landed at %d (%v), where the read has a frame", frames[k+1].pts, landed, err)
	}
}

// TestDamagedFirstFrameIsAHoleFromZero: a first frame lost to damage, its body
// or its header, is a hole at the stream's start like any other: the stream
// still starts at 0, the next frame owes the samples it took, and a seek
// inside it lands at 0, at or before its target.
func TestDamagedFirstFrameIsAHoleFromZero(t *testing.T) {
	raw := unseekableTwin(t, 4)
	spans := frameSpans(t, raw)
	cd, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := readFrames(t, cd)
	if err != nil {
		t.Fatal(err)
	}
	header := append([]byte(nil), raw...)
	header[spans[0][0]+4] ^= 0xFF // the header's CRC-8 fails
	for name, bad := range map[string][]byte{
		"a damaged body":   damageFrames(t, raw, spans, 0),
		"a damaged header": header,
	} {
		t.Run(name, func(t *testing.T) {
			d, err := flacn.NewDemuxer(container.BytesSource(bad), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.Samples != cd.Tracks()[0].Samples {
				t.Errorf("%d samples, want the stream's %d from 0", tr.Samples, cd.Tracks()[0].Samples)
			}
			frames, err := readFrames(t, d)
			if err != nil {
				t.Fatal(err)
			}
			if f := frames[0]; f.pts != clean[1].pts || f.lost != clean[0].dur {
				t.Fatalf("the first frame read is at %d and owes %d, want %d owing %d", f.pts, f.lost, clean[1].pts, clean[0].dur)
			}
			for _, w := range d.Warnings() {
				if w.Kind == container.Note {
					t.Errorf("note %q: the damage reads as a stream-copy cut", w.Msg)
				}
			}
			landed, err := d.SeekSample(context.Background(), 0, clean[0].dur/2)
			if err != nil || landed != 0 {
				t.Fatalf("a seek into the hole landed at %d (%v), want 0", landed, err)
			}
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != clean[1].pts || pkt.Lost != clean[0].dur {
				t.Errorf("after the seek: packet at %d owing %d (%v), want %d owing %d", pkt.PTS, pkt.Lost, err, clean[1].pts, clean[0].dur)
			}
		})
	}
}

// TestFalseHeaderInTheLastFrameIsNotDamage: a header the last frame's own
// bytes spell is a candidate the tail scan tries and rejects, and what its
// rejection reported is not the file's: the file opens strictly, exact.
func TestFalseHeaderInTheLastFrameIsNotDamage(t *testing.T) {
	raw := append([]byte(nil), unseekableTwin(t, 4)...)
	spans := frameSpans(t, raw)
	n := len(spans)
	prev, last := spans[n-2], spans[n-1]
	hdr := raw[prev[0] : prev[0]+16] // a consistent header, and then some
	at := (last[0] + last[1]) / 2
	if at+len(hdr) > last[1]-2 || at < last[0]+16 {
		t.Fatalf("the last frame of %d bytes is too short to plant a header in", last[1]-last[0])
	}
	copy(raw[at:], hdr)
	crc := flac.CRC16(raw[last[0] : last[1]-2])
	raw[last[1]-2], raw[last[1]-1] = byte(crc>>8), byte(crc)
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict open refused a file whose every frame checks: %v", err)
	}
	if tr := d.Tracks()[0]; !tr.SamplesExact || len(d.Warnings()) != 0 {
		t.Errorf("exact %v, warnings %v; want exact with none", tr.SamplesExact, d.Warnings())
	}
}
