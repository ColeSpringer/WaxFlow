package ogg

// A stream may start at a granule other than 0: RFC 7845 section 4.5 for
// Opus, Vorbis I section A.2 for Vorbis, and Ogg FLAC through its mapping's
// reference to Vorbis. Each cell holds such a stream to its twin starting at
// 0. They carry the same packets, so they have the same track, the same packet
// walk and the same seek landings on the raw timeline, which starts at the
// first audio packet.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// requireTwin holds shifted to twin. trim is what the shifted stream discards
// from the front of its twin's audio, which only a negative Vorbis start does.
func requireTwin(t *testing.T, twin, shifted []byte, trim int64) {
	t.Helper()
	holdToTwin(t, twin, shifted, twinWant{trim: trim})
}

// requireRenumberedTwin holds shifted, twin's frames numbered from past 0, to
// twin: the same positions and landings, though not the same frame bytes, and
// a note about the first frame.
func requireRenumberedTwin(t *testing.T, twin, shifted []byte) {
	t.Helper()
	holdToTwin(t, twin, shifted, twinWant{renumbered: true, note: "first frame"})
}

// twinWant is what holdToTwin allows a stream beside its twin: a front trim,
// renumbered frames, and the one note and the one damage warning it carries.
type twinWant struct {
	trim         int64
	renumbered   bool
	note, damage string
}

func holdToTwin(t *testing.T, twin, shifted []byte, w twinWant) {
	t.Helper()
	trim := w.trim
	// A legal stream is read strictly throughout, walk and seeks included, so
	// damage anywhere fails; a damaged one is read tolerantly and held, once
	// the seeks are done, to the one warning it carries.
	strict := &DemuxerOptions{Strict: true}
	ref, err := NewDemuxer(container.BytesSource(twin), strict)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewDemuxer(container.BytesSource(shifted), strict)
	if (err != nil) != (w.damage != "") {
		t.Fatalf("a strict open returned %v; damage expected: %q", err, w.damage)
	}
	opts := strict
	if w.damage != "" {
		opts = nil
	}
	got, err := NewDemuxer(container.BytesSource(shifted), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		var notes, damage []string
		for _, x := range got.Warnings() {
			if x.Kind == container.Note {
				notes = append(notes, x.Msg)
			} else {
				damage = append(damage, x.Msg)
			}
		}
		if !oneOrNone(notes, w.note) || !oneOrNone(damage, w.damage) {
			t.Errorf("warnings %v, want note %q and damage %q", got.Warnings(), w.note, w.damage)
		}
	}()
	want, tr := ref.Tracks()[0], got.Tracks()[0]
	if tr.Samples != want.Samples-trim || !tr.SamplesExact || tr.Delay != want.Delay+trim {
		t.Errorf("shifted track: %d samples (exact %v), delay %d; want %d exact, delay %d",
			tr.Samples, tr.SamplesExact, tr.Delay, want.Samples-trim, want.Delay+trim)
	}
	same := func(a, b []byte) bool { return w.renumbered || bytes.Equal(a, b) }

	a, b := walkPackets(t, ref), walkPackets(t, got)
	if len(a) != len(b) {
		t.Fatalf("the shifted stream walks %d packets, its twin %d", len(b), len(a))
	}
	for i := range a {
		if a[i].PTS != b[i].PTS || a[i].Dur != b[i].Dur || !same(a[i].Data, b[i].Data) {
			t.Fatalf("packet %d: [%d, +%d) shifted, [%d, +%d) twin", i, b[i].PTS, b[i].Dur, a[i].PTS, a[i].Dur)
		}
	}

	// Raw targets: the twin's whole raw run, its last few hundred samples
	// (where the end trim and a front trim's shorter length sit), and past it.
	end := want.Delay + want.Samples
	targets := []int64{0, 1, end - 2*trim - 7, end - trim - 7, end - 1, end, end + 5000}
	for i := int64(1); i < 40; i++ {
		targets = append(targets, end*i/40+i)
	}
	for _, target := range targets {
		if target < 0 {
			continue
		}
		la, err := ref.SeekSample(0, target)
		if err != nil {
			t.Fatalf("twin seek to %d: %v", target, err)
		}
		lb, err := got.SeekSample(0, target)
		if err != nil {
			t.Fatalf("shifted seek to %d: %v", target, err)
		}
		if la != lb {
			t.Fatalf("seek to %d lands at %d, its twin's at %d", target, lb, la)
		}
		var pa, pb container.Packet
		if err := ref.ReadPacket(&pa); err != nil {
			t.Fatalf("twin read after seek to %d: %v", target, err)
		}
		if err := got.ReadPacket(&pb); err != nil {
			t.Fatalf("shifted read after seek to %d: %v", target, err)
		}
		if pa.PTS != pb.PTS || !same(pa.Data, pb.Data) {
			t.Fatalf("seek to %d: the next packet is [%d] shifted, [%d] twin", target, pb.PTS, pa.PTS)
		}
	}
}

// oneOrNone reports whether msgs is empty when want is, and otherwise exactly
// one message naming want.
func oneOrNone(msgs []string, want string) bool {
	if want == "" {
		return len(msgs) == 0
	}
	return len(msgs) == 1 && strings.Contains(msgs[0], want)
}

func walkPackets(t *testing.T, d *Demuxer) []container.Packet {
	t.Helper()
	var out []container.Packet
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		cp := pkt
		cp.Data = append([]byte(nil), pkt.Data...)
		out = append(out, cp)
	}
}

// TestOpusInitialGranule: an Opus stream whose first page says it starts
// 10 s in, as a capture joined mid-broadcast does (RFC 7845 section 4.5).
func TestOpusInitialGranule(t *testing.T) {
	// Several bisection windows long, so a seek narrows on page granules.
	twin, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	requireTwin(t, twin, testutil.ShiftOggGranules(t, twin, 2, 480000), 0)
}

// TestOpusInvalidInitialGranule pins the starts RFC 7845 section 4.5 calls
// invalid, which are damage (a warning, and a strict open refuses), and the two
// it allows on a first page that also ends the stream.
func TestOpusInvalidInitialGranule(t *testing.T) {
	t.Run("granule below its page's samples", func(t *testing.T) {
		// The first page completes 3840 samples and claims to end at 2840.
		// Read tolerantly, the timeline is the pages' own: every later page
		// agrees with the first, so the stream reads as its twin does.
		twin, _, _ := buildSyntheticOpus(312, 4, 400, 512)
		shifted := testutil.ShiftOggGranules(t, twin, 2, -1000)
		requireDamage(t, shifted, "smaller than")
		ref, err := NewDemuxer(container.BytesSource(twin), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := NewDemuxer(container.BytesSource(shifted), nil)
		if err != nil {
			t.Fatal(err)
		}
		if a, b := ref.Tracks()[0], got.Tracks()[0]; a.Samples != b.Samples || a.Delay != b.Delay {
			t.Errorf("read tolerantly: %d samples, delay %d; its twin %d, delay %d", b.Samples, b.Delay, a.Samples, a.Delay)
		}
		for _, target := range []int64{0, 5000, 400000, 1000000} {
			la, err := ref.SeekSample(0, target)
			if err != nil {
				t.Fatal(err)
			}
			lb, err := got.SeekSample(0, target)
			if err != nil {
				t.Fatal(err)
			}
			if la != lb {
				t.Errorf("seek to %d lands at %d, its twin's at %d", target, lb, la)
			}
		}
	})

	// One page holding three 960-sample packets, ending the stream: the
	// granule is the only thing that varies.
	page, _, _ := buildSyntheticOpus(312, 3, 1, 0)
	oneEOSPage := func(t *testing.T, granule int64) []byte {
		return testutil.ShiftOggGranules(t, page, 2, granule-2880)
	}
	t.Run("granule below the pre-skip", func(t *testing.T) {
		raw := oneEOSPage(t, 200)
		requireDamage(t, raw, "pre-skip")
		if tr := openTrack(t, raw); tr.Samples != 0 || !tr.SamplesExact {
			t.Errorf("%d samples (exact %v), want 0", tr.Samples, tr.SamplesExact)
		}
	})
	t.Run("granule below its samples trims the end", func(t *testing.T) {
		raw := oneEOSPage(t, 2000)
		if tr := openTrack(t, raw); tr.Samples != 2000-312 || !tr.SamplesExact {
			t.Errorf("%d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, 2000-312)
		}
		if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err != nil {
			t.Errorf("a strict open refused an end-trimmed stream: %v", err)
		}
	})
	t.Run("granule above its samples starts later", func(t *testing.T) {
		raw := oneEOSPage(t, 2880+1000)
		if tr := openTrack(t, raw); tr.Samples != 2880-312 || !tr.SamplesExact {
			t.Errorf("%d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, 2880-312)
		}
		if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err != nil {
			t.Errorf("a strict open refused a stream starting later: %v", err)
		}
	})
}

func openTrack(t *testing.T, raw []byte) container.Track {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d.Tracks()[0]
}

// requireDamage asserts raw opens tolerantly with one damage warning naming
// want, and a strict open refuses it.
func requireDamage(t *testing.T, raw []byte, want string) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("a tolerant open refused: %v", err)
	}
	var damage []string
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			damage = append(damage, w.Msg)
		}
	}
	if len(damage) != 1 || !strings.Contains(damage[0], want) {
		t.Errorf("warnings %v, want one damage warning about %q", d.Warnings(), want)
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("a strict open accepted it")
	}
}

// vorbisTwin encodes testdata/sine-s16.wav, looped to 45 s, and lays its first
// audio page out as a stream starting anywhere but 0 must: the first two audio
// packets, ending at the second's output (Vorbis I section A.2). It returns the
// stream, which starts at 0, and that first page's granule.
func vorbisTwin(t *testing.T) ([]byte, int64) {
	t.Helper()
	raw := fixture(t, "sine-s16.wav")
	if ch, bits := binary.LittleEndian.Uint16(raw[22:]), binary.LittleEndian.Uint16(raw[34:]); ch != 2 || bits != 16 {
		t.Fatalf("the fixture is %d-channel %d-bit, want 16-bit stereo", ch, bits)
	}
	rate := int(binary.LittleEndian.Uint32(raw[24:]))
	data := testutil.WAVData(t, raw)
	frames := len(data) / 4
	src := [][]float32{make([]float32, 0, 45*rate), make([]float32, 0, 45*rate)}
	for len(src[0]) < 45*rate {
		for i := range frames {
			for c := range 2 {
				v := int16(binary.LittleEndian.Uint16(data[4*i+2*c:]))
				src[c] = append(src[c], float32(v)/32768)
			}
		}
	}
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	packets, cfg, tr := encodeVorbis(t, f, src)
	stream := muxVorbis(t, f, cfg, packets, tr, nil)
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	var p container.Packet
	for range 2 {
		if err := d.ReadPacket(&p); err != nil {
			t.Fatal(err)
		}
	}
	first := p.PTS + p.Dur
	return testutil.RepageOggStart(t, stream, 3, 2, first), first
}

// TestVorbisInitialGranule: both signs Vorbis I section A.2 allows. A positive
// start is a stream joined mid-broadcast; a negative one discards the output
// before time zero, no more than the first two packets' overlap, which this
// tree carries as a front trim (Delay).
func TestVorbisInitialGranule(t *testing.T) {
	twin, first := vorbisTwin(t)
	if first < 2 {
		t.Fatalf("the first two packets lap to %d samples, too few to trim", first)
	}
	// Below the bisection window every seek walks from the first page and
	// lands at 0, where a wrong landing is clamped out of sight.
	if len(twin) < 2*seekWindow {
		t.Fatalf("the stream is %d bytes, want more than %d to bisect", len(twin), 2*seekWindow)
	}
	t.Run("positive", func(t *testing.T) {
		requireTwin(t, twin, testutil.ShiftOggGranules(t, twin, 3, 480000), 0)
	})
	t.Run("negative", func(t *testing.T) {
		requireTwin(t, twin, testutil.ShiftOggGranules(t, twin, 3, -first/2), first/2)
	})
}

// TestOggFLACInitialGranule: Ogg FLAC page granules follow Vorbis's, so a
// stream may start at another granule too. FLAC frames carry their own
// positions and no decoder trims on a granule (libFLAC and ffmpeg decode every
// frame), so a start in either direction moves the timeline the pages state
// and nothing else.
func TestOggFLACInitialGranule(t *testing.T) {
	// One 4096-sample frame per page, long enough that a seek bisects.
	twin := synthOggFLAC(t, 8000)
	if len(twin) < 2*seekWindow {
		t.Fatalf("the stream is %d bytes, want more than %d to bisect", len(twin), 2*seekWindow)
	}
	t.Run("positive", func(t *testing.T) {
		requireTwin(t, twin, testutil.ShiftOggGranules(t, twin, 2, 480000), 0)
	})
	t.Run("negative", func(t *testing.T) {
		requireTwin(t, twin, testutil.ShiftOggGranules(t, twin, 2, -1000), 0)
	})
}

// TestOggFLACSeekPastAWrongFirstGranule: a first page whose granule runs far
// ahead of its frames puts every later page before the stream's start, so
// the bisection runs past the target. The frames still say where they are,
// and a seek lands on the one holding its target.
func TestOggFLACSeekPastAWrongFirstGranule(t *testing.T) {
	raw := synthOggFLAC(t, 8000)
	starts := pageStarts(t, raw)
	at := starts[2] // the first audio page, after the two header pages
	hdr, size := pageAt(t, raw, at)
	page := buildPageRaw(raw[at+5], 1<<40, binary.LittleEndian.Uint32(raw[at+14:]),
		binary.LittleEndian.Uint32(raw[at+18:]), raw[at+27:at+hdr], raw[at+hdr:at+size])
	raw = append(append(append([]byte(nil), raw[:at]...), page...), raw[at+size:]...)

	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 4096*100 + 5, 4096*4000 + 7, 4096*7999 + 1} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		if landed != pkt.PTS || target < pkt.PTS || target >= pkt.PTS+pkt.Dur {
			t.Errorf("seek to %d landed at %d on the frame [%d, %d)", target, landed, pkt.PTS, pkt.PTS+pkt.Dur)
		}
	}
}

// TestOggFLACFramesNumberedPastZero: an Ogg FLAC whose frames are numbered
// from 1000, as a stream-copy cut's and this tree's copy of one are, reads as
// its twin numbered from 0, whether its pages count from 0 or from the frames.
func TestOggFLACFramesNumberedPastZero(t *testing.T) {
	twin := synthOggFLAC(t, 8000)
	renumbered := synthOggFLACFrom(t, 8000, 1000)
	t.Run("pages from 0", func(t *testing.T) {
		requireRenumberedTwin(t, twin, renumbered)
	})
	t.Run("pages from the first frame", func(t *testing.T) {
		requireRenumberedTwin(t, twin, testutil.ShiftOggGranules(t, renumbered, 2, 1000*4096))
	})
}

// TestOggFLACFrameBeforeTheFirst: positions run from the first frame, so a
// frame numbered before it is not this stream's. A read drops it as damage
// rather than hand out a negative position.
func TestOggFLACFrameBeforeTheFirst(t *testing.T) {
	const bs = 4096
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	for i, coded := range []uint64{10, 11, 3, 13} {
		flags := byte(0)
		if i == 3 {
			flags = flagEOS
		}
		stream = append(stream, buildPage(flags, int64(i+1)*bs, 77, uint32(i+2),
			buildFrameBytes(t, coded, bs))...)
	}
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pts []int64
	for _, p := range walkPackets(t, d) {
		pts = append(pts, p.PTS)
	}
	if len(pts) != 3 || pts[0] != 0 || pts[1] != bs || pts[2] != 3*bs {
		t.Errorf("packets at %v, want frames 10, 11 and 13 at 0, %d and %d", pts, bs, 3*bs)
	}
	if damage, _ := countKinds(d.Warnings()); damage != 1 {
		t.Errorf("warnings %v, want one damage warning for frame 3", d.Warnings())
	}
	d, err = NewDemuxer(container.BytesSource(stream), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for err == nil {
		err = d.ReadPacket(&pkt)
	}
	if err == io.EOF {
		t.Error("a strict read delivered frame 3's stream to the end")
	}
}

// relayFirstPage puts raw's first n audio packets on its first audio page,
// the way libvorbis lays a stream out, under the granule they end at.
func relayFirstPage(t *testing.T, raw []byte, headers, n int) []byte {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var p container.Packet
	for range n {
		if err := d.ReadPacket(&p); err != nil {
			t.Fatal(err)
		}
	}
	return testutil.RepageOggStart(t, raw, headers, n, p.PTS+p.Dur)
}

// TestUntrustedFirstGranule: a first page whose granule the next page does
// not back states no start at all, so the stream reads as if its first page
// said where it was, with a note. ffmpeg discards a 0 on a first page holding
// audio for the same reason (its trac ticket 3710); a granule far past the
// rest is the same disagreement the other way. An Ogg FLAC reads its frames
// and no granule, so it reads as its twin with nothing to note.
func TestUntrustedFirstGranule(t *testing.T) {
	opus, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	vorbis, _ := vorbisTwin(t)
	vorbis = relayFirstPage(t, vorbis, 3, 45)
	for _, c := range []struct {
		name    string
		twin    []byte
		headers int
		note    string
		// Opus calls a first granule below its own samples invalid.
		damage string
	}{
		{"opus", opus, 2, "disagrees", "smaller than"},
		{"vorbis", vorbis, 3, "disagrees", ""},
		{"flac", synthOggFLAC(t, 8000), 2, "", ""},
	} {
		first := testutil.OggFirstAudioPage(t, c.twin, c.headers)
		t.Run(c.name+" zero", func(t *testing.T) {
			holdToTwin(t, c.twin, testutil.SetOggGranule(t, c.twin, first, 0),
				twinWant{note: c.note, damage: c.damage})
		})
		t.Run(c.name+" far ahead", func(t *testing.T) {
			holdToTwin(t, c.twin, testutil.SetOggGranule(t, c.twin, first, 1<<40),
				twinWant{note: c.note})
		})
	}
}

// TestVorbisNegativeStartNeedsItsLayout: Vorbis I section A.2 lets a stream
// start before time zero only with its first page holding the first two
// packets and a trim within their overlap. A page holding 45 starts at 0 and
// trims nothing, however well the next page agrees, and its granules count
// from where it starts: the length is every packet's, and a seek lands where
// the read has the same position.
func TestVorbisNegativeStartNeedsItsLayout(t *testing.T) {
	twin, _ := vorbisTwin(t)
	twin = relayFirstPage(t, twin, 3, 45)
	shifted := testutil.ShiftOggGranules(t, twin, 3, -300)
	ref, got := openTrack(t, twin), openTrack(t, shifted)
	if got.Samples != ref.Samples || !got.SamplesExact || got.Delay != 0 {
		t.Errorf("%d samples (exact %v), delay %d; want %d exact, delay 0",
			got.Samples, got.SamplesExact, got.Delay, ref.Samples)
	}
	d, err := NewDemuxer(container.BytesSource(shifted), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if ws := d.Warnings(); len(ws) != 1 || ws[0].Kind != container.Note || !strings.Contains(ws[0].Msg, "A.2") {
		t.Errorf("warnings %v, want one note naming Vorbis I A.2", ws)
	}
	linear := walkPackets(t, d)
	at := make(map[int64][]byte, len(linear))
	for _, p := range linear {
		at[p.PTS] = p.Data
	}
	for _, target := range []int64{got.Samples / 3, 2 * got.Samples / 3} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		var p container.Packet
		for p.Dur == 0 { // the priming packet a seek lands on emits nothing
			if err := d.ReadPacket(&p); err != nil {
				t.Fatal(err)
			}
		}
		if want, ok := at[p.PTS]; !ok || !bytes.Equal(p.Data, want) {
			t.Errorf("a seek to %d landed at %d, then a packet the read does not have at %d", target, landed, p.PTS)
		}
	}
}

// TestOggFLACLoneEndPage: an Ogg FLAC whose one audio page ends the stream is
// timed by its granule less its samples like any first page, the shape a
// stream-copy cut leaves (frames from the cut point's frame, granule the
// cut's own length). No FLAC decoder trims on a granule, so the length is the
// frames'; ffmpeg reads the same.
func TestOggFLACLoneEndPage(t *testing.T) {
	const bs = 4096
	sizes := []int{bs, bs, bs, bs, bs, 1876} // 22356 samples
	var frames [][]byte
	for i, n := range sizes {
		frames = append(frames, buildFrameBytes(t, uint64(48+i), n))
	}
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	stream = append(stream, buildPage(flagEOS, 22050, 77, 2, frames...)...)
	for _, total := range []int64{0, 220500} {
		raw := patchOggFLACTotal(t, stream, total)
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; tr.Samples != 22356 || !tr.SamplesExact {
			t.Errorf("total %d: %d samples (exact %v), want the frames' 22356", total, tr.Samples, tr.SamplesExact)
		}
		pkts := walkPackets(t, d)
		if last := pkts[len(pkts)-1]; last.PTS+last.Dur != 22356 {
			t.Errorf("total %d: the frames end at %d, want 22356", total, last.PTS+last.Dur)
		}
	}
}

// TestOggLoneFirstPageStands: a stream cut after its first audio page, with
// no end-of-stream flag and no later page, has nothing to check that page
// against, so it stands as a last page does: here it starts 48000 in, and the
// stream is the three packets it holds. Read from 0 instead, its length ran
// past its packets and a seek to 0 landed at 48000.
func TestOggLoneFirstPageStands(t *testing.T) {
	head := muxOpusHead(312)
	pkt := []byte{31<<3 | 0x4, 0, 0} // CELT FB 20 ms
	stream := buildPage(flagBOS, 0, 4321, 0, head)
	stream = append(stream, buildPage(0, 0, 4321, 1, append([]byte("OpusTags"), make([]byte, 8)...))...)
	stream = append(stream, buildPage(0, 48000+3*960, 4321, 2, pkt, pkt, pkt)...)
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != 3*960-312 || !tr.SamplesExact {
		t.Errorf("%d samples (exact %v), want the packets' %d", tr.Samples, tr.SamplesExact, 3*960-312)
	}
	if landed, err := d.SeekSample(0, 0); err != nil || landed != 0 {
		t.Errorf("seek to 0 landed at %d (%v)", landed, err)
	}
}

// TestOggSeekFromTheFirstPageLandsAtZero: a walk restarted at the first audio
// page begins with the first audio packet, which is raw 0 by definition, so
// granules far from the packets' own timeline cannot land a seek past it.
// Here page 1 claims to end at 50880 and page 2 at 56640 over three 960-sample
// packets each: they disagree, and the second still ran a seek to 0 out to
// 50880.
func TestOggSeekFromTheFirstPageLandsAtZero(t *testing.T) {
	head := muxOpusHead(312)
	pkt := []byte{31<<3 | 0x4, 0, 0} // CELT FB 20 ms
	stream := buildPage(flagBOS, 0, 4321, 0, head)
	stream = append(stream, buildPage(0, 0, 4321, 1, append([]byte("OpusTags"), make([]byte, 8)...))...)
	stream = append(stream, buildPage(0, 50880, 4321, 2, pkt, pkt, pkt)...)
	stream = append(stream, buildPage(0, 56640, 4321, 3, pkt, pkt, pkt)...)
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 1000} {
		if landed, err := d.SeekSample(0, target); err != nil || landed != 0 {
			t.Errorf("seek to %d landed at %d (%v), want the first packet at 0", target, landed, err)
		}
	}
}

// TestOggFLACFirstGranuleMovesNothing: an Ogg FLAC is its frames, as ffmpeg
// reads it, whatever its first page's granule says, a stream-copy cut's start
// before 0 included, and there is nothing to note about the granule.
func TestOggFLACFirstGranuleMovesNothing(t *testing.T) {
	const bs = 4096
	for _, pages := range []int{2, 40} {
		plain := synthOggFLAC(t, pages)
		// The shape `ffmpeg -ss X -c copy` leaves: the frames keep their
		// numbers and the pages count from the cut, 2286 samples into the
		// first frame.
		cut := testutil.ShiftOggGranules(t, synthOggFLACFrom(t, pages, 33), 2, -2286)
		first := testutil.OggFirstAudioPage(t, plain, 2)
		for _, c := range []struct {
			name    string
			raw     []byte
			granule int64 // -1 leaves the stream as it is
			notes   int
		}{
			{"as is", plain, -1, 0},
			{"raised", plain, bs + 1000, 0},
			{"zero", plain, 0, 0},
			// The first frame's note, which a cut always carries.
			{"cut as is", cut, -1, 1},
			{"cut raised", cut, bs - 2286 + 1000, 1},
			{"cut zero", cut, 0, 1},
		} {
			t.Run(fmt.Sprintf("%s, %d pages", c.name, pages), func(t *testing.T) {
				raw := c.raw
				if c.granule >= 0 {
					raw = testutil.SetOggGranule(t, raw, first, c.granule)
				}
				d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
				if err != nil {
					t.Fatal(err)
				}
				want := int64(pages) * bs
				if tr := d.Tracks()[0]; tr.Samples != want || !tr.SamplesExact {
					t.Errorf("%d samples (exact %v), want the frames' %d", tr.Samples, tr.SamplesExact, want)
				}
				if damage, notes := countKinds(d.Warnings()); damage != 0 || notes != c.notes {
					t.Errorf("warnings %v, want %d notes", d.Warnings(), c.notes)
				}
				pkts := walkPackets(t, d)
				if last := pkts[len(pkts)-1]; len(pkts) != pages || last.PTS+last.Dur != want {
					t.Errorf("%d frames ending at %d, want %d ending at %d", len(pkts), last.PTS+last.Dur, pages, want)
				}
			})
		}
	}
}

// TestOggLoneFirstPageShortOfItsSamples: only a page ending the stream trims
// it. A first page with no end-of-stream flag and nothing after it, whose
// granule falls short of its own samples, states no start: positions start at
// 0, with a note, and the stream is every packet the page holds, as ffmpeg
// reads it. Opus also calls such a granule invalid (RFC 7845 section 4.5). One
// whose granule runs past its samples starts that much later and holds them
// all. Read as an end trim, the short ones came out empty. An Ogg FLAC is its
// frames whatever the granule, with nothing to note.
func TestOggLoneFirstPageShortOfItsSamples(t *testing.T) {
	opus, _, _ := buildSyntheticOpus(312, 50, 2, 0)

	vorbis, _ := vorbisTwin(t)
	vorbis = relayFirstPage(t, vorbis, 3, 45)
	ref, err := NewDemuxer(container.BytesSource(vorbis), nil)
	if err != nil {
		t.Fatal(err)
	}
	var vorbisEnd int64
	for range 45 {
		var p container.Packet
		if err := ref.ReadPacket(&p); err != nil {
			t.Fatal(err)
		}
		vorbisEnd = p.PTS + p.Dur
	}

	const fbs = 4608
	flac := buildPage(flagBOS, 0, 77, 0, flacBOSPacketBlocks(fbs, fbs))
	flac = append(flac, buildPage(0, 0, 77, 1, commentPacket())...)
	var frames [][]byte
	for i := range 5 {
		frames = append(frames, buildFrameBytes(t, uint64(i), fbs))
	}
	flac = append(flac, buildPage(flagEOS, 5*fbs, 77, 2, frames...)...)

	for _, c := range []struct {
		name    string
		raw     []byte
		headers int
		packets int
		end     int64 // where the page's packets end
		delay   int64
		noted   bool // whether a short granule is noted
		// Opus calls a first granule below its own samples invalid.
		damage string
	}{
		{"opus", opus, 2, 50, 50 * 960, 312, true, "smaller than"},
		{"vorbis", vorbis, 3, 45, vorbisEnd, 0, true, ""},
		{"flac", flac, 2, 5, 5 * fbs, 0, false, ""},
	} {
		first := testutil.OggFirstAudioPage(t, c.raw, c.headers)
		lone := testutil.TruncateOggAfter(t, c.raw, first)
		for _, g := range []struct {
			name    string
			granule int64
		}{{"as is", c.end}, {"zero", 0}, {"short", c.end / 3}, {"past", c.end + 48000}} {
			t.Run(c.name+" "+g.name, func(t *testing.T) {
				raw := testutil.SetOggGranule(t, lone, first, g.granule)
				short := g.granule < c.end
				_, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
				if (err != nil) != (short && c.damage != "") {
					t.Errorf("a strict open returned %v", err)
				}
				d, err := NewDemuxer(container.BytesSource(raw), nil)
				if err != nil {
					t.Fatal(err)
				}
				if tr := d.Tracks()[0]; tr.Samples != c.end-c.delay || !tr.SamplesExact || tr.Delay != c.delay {
					t.Errorf("%d samples (exact %v), delay %d; want the page's %d, delay %d",
						tr.Samples, tr.SamplesExact, tr.Delay, c.end-c.delay, c.delay)
				}
				var notes, damage []string
				for _, w := range d.Warnings() {
					if w.Kind == container.Note {
						notes = append(notes, w.Msg)
					} else {
						damage = append(damage, w.Msg)
					}
				}
				wantNote, wantDamage := "", ""
				if short && c.noted {
					wantNote, wantDamage = "positions start at 0", c.damage
				}
				if !oneOrNone(notes, wantNote) || !oneOrNone(damage, wantDamage) {
					t.Errorf("warnings %v, want note %q and damage %q", d.Warnings(), wantNote, wantDamage)
				}
				if pkts := walkPackets(t, d); len(pkts) != c.packets || pkts[len(pkts)-1].PTS+pkts[len(pkts)-1].Dur != c.end {
					t.Errorf("%d packets, want the page's %d ending at %d", len(pkts), c.packets, c.end)
				}
				if landed, err := d.SeekSample(0, 0); err != nil || landed != 0 {
					t.Errorf("seek to 0 landed at %d (%v)", landed, err)
				}
			})
		}
	}
}

// failAfter is a source whose reads past limit fail.
type failAfter struct {
	data  []byte
	limit int64
}

func (s failAfter) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > s.limit {
		return 0, errors.New("the source failed")
	}
	return copy(p, s.data[off:]), nil
}

func (s failAfter) Size() int64 { return int64(len(s.data)) }

// TestOggLookAheadReadErrorFailsTheOpen: holding a first page's granule to the
// next page's is an open-time read, so a source failing under it fails the
// open, as every other open-time read does.
func TestOggLookAheadReadErrorFailsTheOpen(t *testing.T) {
	twin, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	raw := testutil.ShiftOggGranules(t, twin, 2, 480000)
	next := pageStarts(t, raw)[testutil.OggFirstAudioPage(t, raw, 2)+1]
	if _, err := NewDemuxer(failAfter{raw, int64(next) + 10}, nil); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("the open returned %v, want the source's failure", err)
	}
}

// TestOggTailReadErrorFailsTheOpen: the tail read that says where a stream
// ends is an open-time read too, so a source failing under it fails the open
// rather than leaving the last granule it read as an exact length.
func TestOggTailReadErrorFailsTheOpen(t *testing.T) {
	raw, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	if _, err := NewDemuxer(failAfter{raw, int64(len(raw)) - 50<<10}, nil); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("the open returned %v, want the source's failure", err)
	}
}

// TestOggLookAheadCapLeavesTheStartUnverified: a look ahead that reaches its
// packet cap before a later page states a granule has checked nothing, so the
// first page's start is not taken: positions start at 0, with a note. Here
// every page after the first says no packet completes on it.
func TestOggLookAheadCapLeavesTheStartUnverified(t *testing.T) {
	head := muxOpusHead(312)
	pkt := []byte{31<<3 | 0x4} // CELT FB 20 ms, one frame
	stream := buildPage(flagBOS, 0, 4321, 0, head)
	stream = append(stream, buildPage(0, 0, 4321, 1, append([]byte("OpusTags"), make([]byte, 8)...))...)
	stream = append(stream, buildPage(0, 48000+960, 4321, 2, pkt)...)
	pkts := make([][]byte, 255)
	for i := range pkts {
		pkts[i] = pkt
	}
	seq, n := uint32(3), 0
	for ; n <= maxSeekPackets; n += len(pkts) {
		stream = append(stream, buildPage(0, -1, 4321, seq, pkts...)...)
		seq++
	}
	end := 48000 + int64(n+2)*960
	stream = append(stream, buildPage(flagEOS, end, 4321, seq, pkt)...)

	d, err := NewDemuxer(container.BytesSource(stream), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != end-312 {
		t.Errorf("%d samples, want the last granule's %d from 0", tr.Samples, end-312)
	}
	if ws := d.Warnings(); len(ws) != 1 || ws[0].Kind != container.Note || !strings.Contains(ws[0].Msg, "positions start at 0") {
		t.Errorf("warnings %v, want one note that positions start at 0", ws)
	}
}

// TestOggBisectionSkipsADisownedFirstPage: a seek's bisection never reads the
// granule of a first page the stream disowns, even where the first packet
// begins pages before it and a probe lands in between. Here the first packet
// spans three pages, and a zero on the page it completes on put a seek into it
// on the packet after. An Ogg FLAC, whose seek reads its frames, lands on the
// frame holding its target either way.
func TestOggBisectionSkipsADisownedFirstPage(t *testing.T) {
	const packets = 6000
	full := bytes.Repeat([]byte{255}, 255)
	// A first packet spanning three pages, most of a bisection window past
	// where it begins. Its padding is not audio, which nothing here decodes.
	// granule is the granule of the page it completes on, and the pages after
	// it count on from end, step a packet.
	spanning := func(head []byte, granule, end, step int64, next func(i int) []byte, bos, comment []byte) []byte {
		big := append(append([]byte(nil), head...), make([]byte, 2*255*255)...)
		s := buildPage(flagBOS, 0, 77, 0, bos)
		s = append(s, buildPage(0, 0, 77, 1, comment)...)
		s = append(s, buildPageRaw(0, -1, 77, 2, full, big[:255*255])...)
		s = append(s, buildPageRaw(flagContinued, -1, 77, 3, full, big[255*255:2*255*255])...)
		tail, second := big[2*255*255:], next(1)
		s = append(s, buildPageRaw(flagContinued, granule, 77, 4, []byte{byte(len(tail)), byte(len(second))},
			append(append([]byte(nil), tail...), second...))...)
		for i := 2; i < packets; i++ {
			flags := byte(0)
			if i == packets-1 {
				flags = flagEOS
			}
			s = append(s, buildPage(flags, end+int64(i-1)*step, 77, uint32(i+3), next(i))...)
		}
		return s
	}
	head := muxOpusHead(312)
	opusTags := append([]byte("OpusTags"), make([]byte, 8)...)
	// 48 CELT frames of 2.5 ms, the longest packet, then 20 ms ones.
	first := []byte{16<<3 | 0x3, 48}
	opus := func(granule int64) []byte {
		return spanning(first, granule, 5760+960, 960, func(int) []byte { return []byte{31<<3 | 0x4} }, head, opusTags)
	}
	const bs = 4096
	flacFrame := func(i int) []byte { return buildFrameBytes(t, uint64(i), bs) }
	flacStream := func(granule int64) []byte {
		return patchOggFLACTotalB(t, spanning(flacFrame(0), granule, 2*bs, bs, flacFrame, flacBOSPacket(), commentPacket()), packets*bs)
	}
	for _, c := range []struct {
		name         string
		twin, zero   []byte
		targets      []int64
		frameLanding bool // FLAC lands on the frame holding its target
	}{
		{"opus", opus(5760 + 960), opus(0), []int64{0, 4000, 6000, 8000, 9000, 20000, packets * 480, packets*960 - 1}, false},
		{"flac", flacStream(2 * bs), flacStream(0), []int64{0, 100, bs - 1, bs, 2*bs + 5, 3 * bs, packets*bs/2 + 7, packets*bs - 1}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if len(c.twin) < 2*seekWindow {
				t.Fatalf("the stream is %d bytes, want more than %d to bisect", len(c.twin), 2*seekWindow)
			}
			ref, err := NewDemuxer(container.BytesSource(c.twin), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := NewDemuxer(container.BytesSource(c.zero), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range c.targets {
				la, err := ref.SeekSample(0, target)
				if err != nil {
					t.Fatalf("twin seek to %d: %v", target, err)
				}
				lb, err := got.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				var pa, pb container.Packet
				if err := ref.ReadPacket(&pa); err != nil {
					t.Fatal(err)
				}
				if err := got.ReadPacket(&pb); err != nil {
					t.Fatal(err)
				}
				// Without the first page to anchor on, an Opus seek may land a
				// packet later than its twin's, never past its target.
				if lb > target || pb.PTS != lb || la > target || pa.PTS != la {
					t.Errorf("seek to %d landed at %d, the next packet at %d; its twin's at %d and %d", target, lb, pb.PTS, la, pa.PTS)
				}
				if want := target / bs * bs; c.frameLanding && (lb != want || la != want) {
					t.Errorf("seek to %d landed at %d, its twin at %d, want the frame at %d", target, lb, la, want)
				}
			}
		})
	}
}

// TestOggFLACSourceLengthTotalIsANote: an Ogg FLAC stream-copy cut keeps its
// source's STREAMINFO too, whose total is the first frame's number plus the
// span: the source's length, so a note, and the track reads the span exactly,
// whether the pages count from the cut or from the frames. A total that is
// neither that nor the span is still a shortfall, which warns and which a
// strict open refuses.
func TestOggFLACSourceLengthTotalIsANote(t *testing.T) {
	const bs, frames, first = 4096, 40, 33
	span, origin := int64(frames*bs), int64(first*bs)
	renumbered := synthOggFLACFrom(t, frames, first)
	for _, layout := range []struct {
		name string
		raw  []byte
	}{
		{"pages from the cut", testutil.ShiftOggGranules(t, renumbered, 2, -2286)},
		{"pages from the frames", testutil.ShiftOggGranules(t, renumbered, 2, origin)},
	} {
		for _, c := range []struct {
			name   string
			total  int64
			damage bool
		}{
			{"the source's length", origin + span, false},
			{"past it", origin + span + bs, true},
			{"short of it", origin + span - bs, true},
		} {
			t.Run(layout.name+", "+c.name, func(t *testing.T) {
				raw := patchOggFLACTotal(t, layout.raw, c.total)
				if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); (err != nil) != c.damage {
					t.Errorf("a strict open returned %v; damage expected: %v", err, c.damage)
				}
				d, err := NewDemuxer(container.BytesSource(raw), nil)
				if err != nil {
					t.Fatal(err)
				}
				if tr := d.Tracks()[0]; tr.Samples != span || !tr.SamplesExact {
					t.Errorf("%d samples (exact %v), want the span's %d exact", tr.Samples, tr.SamplesExact, span)
				}
				ws := d.Warnings()
				if len(ws) != 2 || !strings.Contains(ws[0].Msg, "first frame") || !strings.Contains(ws[1].Msg, "STREAMINFO") ||
					(ws[1].Kind == container.Damage) != c.damage {
					t.Errorf("warnings %v, want the first frame's note and one about STREAMINFO, damage: %v", ws, c.damage)
				}
				if pkts := walkPackets(t, d); pkts[len(pkts)-1].PTS+pkts[len(pkts)-1].Dur != span {
					t.Errorf("the frames end at %d, want the span's %d", pkts[len(pkts)-1].PTS+pkts[len(pkts)-1].Dur, span)
				}
			})
		}
	}
}

// synthOggFLACPages builds an Ogg FLAC whose i'th page holds counts[i] frames
// of 4096 samples, numbered from frame first, whose STREAMINFO declares the
// frames' total.
func synthOggFLACPages(t *testing.T, counts []int, first uint64) []byte {
	t.Helper()
	const bs = 4096
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	n := 0
	for i, count := range counts {
		var frames [][]byte
		for range count {
			frames = append(frames, buildFrameBytes(t, first+uint64(n), bs))
			n++
		}
		flags := byte(0)
		if i == len(counts)-1 {
			flags = flagEOS
		}
		stream = append(stream, buildPage(flags, int64(n)*bs, 77, uint32(i+2), frames...)...)
	}
	return patchOggFLACTotalB(t, stream, int64(n)*bs)
}

// fiveToAPage is the frame counts of pages pages, five frames each.
func fiveToAPage(pages int) []int {
	counts := make([]int, pages)
	for i := range counts {
		counts[i] = 5
	}
	return counts
}

// copyCutShape is an Ogg FLAC as `ffmpeg -ss X -c copy` leaves one: frames
// numbered from 33, laid out by counts, and pages counted from the cut, 2286
// samples into the first frame.
func copyCutShape(t *testing.T, counts []int) []byte {
	t.Helper()
	return testutil.ShiftOggGranules(t, synthOggFLACPages(t, counts, 33), 2, -2286)
}

// TestOggFLACSeeksPastDamage: Ogg FLAC frames say where they are, so damage
// behind a first page starting before 0 moves no seek: every one lands on the
// frame holding its target. Timed off the durations a look ahead counted past
// the damage, the stream's origin came out too high by the lost frames, and
// seeks landed past their targets. The first damaged page holds more frames
// than the pages after it, so the error ran past a whole page.
func TestOggFLACSeeksPastDamage(t *testing.T) {
	const bs, pages = 4096, 3000
	counts := fiveToAPage(pages)
	counts[1] = 12
	raw := copyCutShape(t, counts)
	if len(raw) < 2*seekWindow {
		t.Fatalf("the stream is %d bytes, want more than %d to bisect", len(raw), 2*seekWindow)
	}
	first := testutil.OggFirstAudioPage(t, raw, 2)
	total := int64(0)
	for _, n := range counts {
		total += int64(n) * bs
	}
	for _, damaged := range [][]int{{1}, {1, 2}} {
		t.Run(fmt.Sprintf("%d damaged pages", len(damaged)), func(t *testing.T) {
			bad := raw
			lo, hi := int64(0), int64(0) // the frames the damage loses
			for i := range damaged[len(damaged)-1] + 1 {
				if i == damaged[0] {
					lo = hi
				}
				hi += int64(counts[i]) * bs
			}
			for _, k := range damaged {
				bad = testutil.DamageOggPage(t, bad, first+k)
			}
			lost := func(target int64) bool { return target >= lo && target < hi }
			d, err := NewDemuxer(container.BytesSource(bad), nil)
			if err != nil {
				t.Fatal(err)
			}
			for target := int64(0); target < total; target += 7*bs + 1013 {
				if lost(target) {
					continue
				}
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatalf("read after seek to %d: %v", target, err)
				}
				if want := target / bs * bs; landed != want || pkt.PTS != want {
					t.Fatalf("seek to %d landed at %d on the frame at %d, want the frame at %d", target, landed, pkt.PTS, want)
				}
			}
		})
	}
}

// TestOggFLACLengthIsTheFramesSpan: an Ogg FLAC's length is its frames' span,
// from the first frame's own position to the end of the last whole frame, read
// off their headers, so no single broken granule, on the first page, the next
// one or the last, can move it.
func TestOggFLACLengthIsTheFramesSpan(t *testing.T) {
	const bs, per, pages = 4096, 5, 40
	raw := copyCutShape(t, fiveToAPage(pages))
	first := testutil.OggFirstAudioPage(t, raw, 2)
	last := first + pages - 1
	span := int64(pages*per) * bs
	for _, c := range []struct {
		name    string
		page    int
		granule int64
	}{
		{"first zeroed", first, 0},
		{"second zeroed", first + 1, 0},
		{"second raised", first + 1, testutil.OggGranule(t, raw, first+1) + 1000},
		{"last zeroed", last, 0},
		{"last raised", last, testutil.OggGranule(t, raw, last) + 100000},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := testutil.SetOggGranule(t, raw, c.page, c.granule)
			d, err := NewDemuxer(container.BytesSource(bad), &DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.Samples != span || !tr.SamplesExact {
				t.Errorf("%d samples (exact %v), want the frames' span %d", tr.Samples, tr.SamplesExact, span)
			}
			if damage, _ := countKinds(d.Warnings()); damage != 0 {
				t.Errorf("warnings %v, want notes only", d.Warnings())
			}
			pkts := walkPackets(t, d)
			if end := pkts[len(pkts)-1].PTS + pkts[len(pkts)-1].Dur; end != span {
				t.Errorf("the frames end at %d, want %d", end, span)
			}
			for _, target := range []int64{0, 3*bs + 7, span / 2, span - 1} {
				landed, err := d.SeekSample(0, target)
				if err != nil || landed != target/bs*bs {
					t.Errorf("seek to %d landed at %d (%v), want %d", target, landed, err, target/bs*bs)
				}
			}
		})
	}
}

// TestOggLookAheadPastDamageLeavesTheStartUnverified: Opus and Vorbis packets
// cannot say where they are, so a look ahead that passed over damage cannot
// confirm a first page, whatever it found after: positions start at 0, with a
// note.
func TestOggLookAheadPastDamageLeavesTheStartUnverified(t *testing.T) {
	opus, _, _ := buildSyntheticOpus(312, 4, 400, 512)
	vorbis, _ := vorbisTwin(t)
	for _, c := range []struct {
		name    string
		raw     []byte
		headers int
		raise   int64 // added to the first page's granule
	}{
		{"opus", opus, 2, 0},
		{"opus raised by the lost page", opus, 2, 4 * 960},
		{"vorbis", vorbis, 3, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			shifted := testutil.ShiftOggGranules(t, c.raw, c.headers, 480000)
			first := testutil.OggFirstAudioPage(t, shifted, c.headers)
			ref := openTrack(t, shifted)
			raw := testutil.SetOggGranule(t, shifted, first, testutil.OggGranule(t, shifted, first)+c.raise)
			raw = testutil.DamageOggPage(t, raw, first+1)
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			// From 0, the length is the last granule's: the start read as audio.
			if tr := d.Tracks()[0]; tr.Samples != ref.Samples+480000 {
				t.Errorf("%d samples, want the last granule's %d from 0", tr.Samples, ref.Samples+480000)
			}
			if ws := d.Warnings(); len(ws) != 1 || ws[0].Kind != container.Note || !strings.Contains(ws[0].Msg, "damage after the first audio page") {
				t.Errorf("warnings %v, want one note that damage leaves the start unconfirmed", ws)
			}
		})
	}
}

// TestOggUnstatedPageAfterTheLastGranule: a page completing a packet states a
// granule, so one stating -1 after the last page that does means the granule
// read is not where the stream ends: the length is advisory, with a warning
// naming the page, and a strict open refuses it.
func TestOggUnstatedPageAfterTheLastGranule(t *testing.T) {
	head := muxOpusHead(312)
	pkt := []byte{31<<3 | 0x4} // CELT FB 20 ms, one frame
	start := buildPage(flagBOS, 0, 4321, 0, head)
	start = append(start, buildPage(0, 0, 4321, 1, append([]byte("OpusTags"), make([]byte, 8)...))...)

	short := append([]byte(nil), start...)
	short = append(short, buildPage(0, 3*960, 4321, 2, pkt, pkt, pkt)...)
	for i := range 3 {
		short = append(short, buildPage(0, -1, 4321, uint32(i+3), pkt, pkt, pkt)...)
	}
	// A first page starting 1 s in, then more packets than a look ahead
	// reads, on pages stating -1, cut short.
	capped := append([]byte(nil), start...)
	capped = append(capped, buildPage(0, 48000+960, 4321, 2, pkt)...)
	pkts := make([][]byte, 255)
	for i := range pkts {
		pkts[i] = pkt
	}
	seq := uint32(3)
	for n := 0; n <= maxSeekPackets; n += len(pkts) {
		capped = append(capped, buildPage(0, -1, 4321, seq, pkts...)...)
		seq++
	}
	for name, raw := range map[string][]byte{"short": short, "capped": capped} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
				t.Error("a strict open accepted it")
			}
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.SamplesExact || !tr.SamplesAdvisory {
				t.Errorf("%d samples, exact %v, advisory %v: want advisory", tr.Samples, tr.SamplesExact, tr.SamplesAdvisory)
			}
			var damage []string
			for _, w := range d.Warnings() {
				if w.Kind == container.Damage {
					damage = append(damage, w.Msg)
				}
			}
			if len(damage) != 1 || !strings.Contains(damage[0], "states no granule") {
				t.Errorf("warnings %v, want one naming the page that states no granule", d.Warnings())
			}
		})
	}
}

// bigFrameOggFLAC lays out frames larger than a page as ffmpeg does for
// hi-res FLAC: each frame, padded to size bytes, begins a page of 255 full
// segments stating no granule and ends on the next, which states where the
// frame ends. The padding is not FLAC, which nothing here decodes.
func bigFrameOggFLAC(t *testing.T, frames, size int) []byte {
	t.Helper()
	const bs = 4096
	full := bytes.Repeat([]byte{255}, 255)
	stream := buildPage(flagBOS, 0, 77, 0, flacBOSPacket())
	stream = append(stream, buildPage(0, 0, 77, 1, commentPacket())...)
	seq := uint32(2)
	for i := range frames {
		f := append(buildFrameBytes(t, uint64(i), bs), make([]byte, size)...)
		stream = append(stream, buildPageRaw(0, -1, 77, seq, full, f[:255*255])...)
		rest := f[255*255:]
		var lacing []byte
		for n := len(rest); ; n -= 255 {
			if n < 255 {
				lacing = append(lacing, byte(n))
				break
			}
			lacing = append(lacing, 255)
		}
		flags := byte(flagContinued)
		if i == frames-1 {
			flags |= flagEOS
		}
		stream = append(stream, buildPageRaw(flags, int64(i+1)*bs, 77, seq+1, lacing, rest)...)
		seq += 2
	}
	return patchOggFLACTotalB(t, stream, int64(frames)*bs)
}

// TestOggFLACSeeksOnFramesLargerThanAPage: frames larger than a page leave
// almost no page on which a frame both begins and ends, so a seek bisects on
// the frame that begins on each page, read off its header, and reads a few
// pages per bisection step. Holding granules to such frames left every seek
// walking the file from its start, several times over.
func TestOggFLACSeeksOnFramesLargerThanAPage(t *testing.T) {
	const bs, frames = 4096, 150
	raw := bigFrameOggFLAC(t, frames, 80000)
	cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := NewDemuxer(cs, &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != frames*bs || !tr.SamplesExact {
		t.Errorf("%d samples (exact %v), want the frames' %d", tr.Samples, tr.SamplesExact, frames*bs)
	}
	steps := 1
	for n := int64(len(raw)); n > seekWindow; n /= 2 {
		steps++
	}
	bound := int64(steps+4) * 3 * maxPageSize
	for _, target := range []int64{0, 7, frames*bs/4 + 5, frames * bs / 2, frames*bs*9/10 + 1, frames*bs - 1} {
		cs.Reset()
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if want := target / bs * bs; landed != want || pkt.PTS != want {
			t.Errorf("seek to %d landed at %d on the frame at %d, want %d", target, landed, pkt.PTS, want)
		}
		if cs.Bytes > bound {
			t.Errorf("seek to %d read %d bytes of a %d-byte stream, more than %d for %d bisection steps",
				target, cs.Bytes, len(raw), bound, steps)
		}
	}
}
