package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// runSpec is one trun of a crafted traf: its data offset where it states one
// and its samples' sizes, written per sample unless bare, which leaves them
// to the defaults. A dur is written with every sample.
type runSpec struct {
	off   *int32
	sizes []uint32
	bare  bool
	dur   uint32
}

func at(v int32) *int32 { return &v }

// trafOf builds a traf: a tfhd with flags (and the base offset where they
// say one follows) and the runs. It states no decode time.
func trafOf(id, flags uint32, base uint64, runs ...runSpec) []byte {
	parts := [][]byte{u32(id)}
	if flags&0x000001 != 0 {
		parts = append(parts, u64(base))
	}
	boxes := [][]byte{makeFullBox("tfhd", 0, flags, parts...)}
	for _, r := range runs {
		var tf uint32
		body := [][]byte{u32(uint32(len(r.sizes)))}
		if r.off != nil {
			tf |= 0x000001
			body = append(body, u32(uint32(*r.off)))
		}
		if r.dur != 0 {
			tf |= 0x000100
		}
		if !r.bare {
			tf |= 0x000200
		}
		for _, s := range r.sizes {
			if r.dur != 0 {
				body = append(body, u32(r.dur))
			}
			if !r.bare {
				body = append(body, u32(s))
			}
		}
		boxes = append(boxes, makeFullBox("trun", 0, tf, body...))
	}
	return makeBox("traf", boxes...)
}

// TestTrafBaseDataOffset: a traf's runs are placed from the base its tfhd
// states, from the moof where the tfhd says so or the traf is the moof's
// first, and otherwise from the end of the data the traf before it defined
// (ISO/IEC 14496-12, 8.8.7). A run with no data offset follows the run
// before it (8.8.8).
func TestTrafBaseDataOffset(t *testing.T) {
	const moofOff = 1000
	mfhd := makeFullBox("mfhd", 0, 0, u32(1))
	other := trafOf(99, 0, 0, runSpec{off: at(200), sizes: []uint32{4, 4, 4}}) // ends at 1212
	trexes := []trexDefaults{{trackID: 1, defaultDur: 1024}, {trackID: 99, defaultSize: 10}}
	for _, tc := range []struct {
		name  string
		trafs [][]byte
		want  []int64 // where track 1's samples lie
	}{
		{"the moof's first traf", [][]byte{trafOf(1, 0, 0, runSpec{off: at(300), sizes: []uint32{8, 8}})}, []int64{1300, 1308}},
		{"behind another traf, no offset", [][]byte{other, trafOf(1, 0, 0, runSpec{sizes: []uint32{8, 8}})}, []int64{1212, 1220}},
		{"behind another traf, an offset", [][]byte{other, trafOf(1, 0, 0, runSpec{off: at(5), sizes: []uint32{8}})}, []int64{1217}},
		{"default-base-is-moof", [][]byte{other, trafOf(1, 0x020000, 0, runSpec{off: at(300), sizes: []uint32{8}})}, []int64{1300}},
		{"a stated base", [][]byte{other, trafOf(1, 0x000001, 5000, runSpec{off: at(7), sizes: []uint32{8}})}, []int64{5007}},
		{"a stated base outranks default-base-is-moof",
			[][]byte{trafOf(1, 0x020001, 5000, runSpec{off: at(7), sizes: []uint32{8}})}, []int64{5007}},
		{"several runs", [][]byte{trafOf(1, 0, 0,
			runSpec{off: at(200), sizes: []uint32{8, 8}}, runSpec{sizes: []uint32{3}}, runSpec{off: at(400), sizes: []uint32{2, 2}})},
			[]int64{1200, 1208, 1216, 1400, 1402}},
		{"behind a traf of several runs", [][]byte{
			trafOf(99, 0, 0, runSpec{off: at(200), sizes: []uint32{4}}, runSpec{sizes: []uint32{6}}),
			trafOf(1, 0, 0, runSpec{sizes: []uint32{8}})}, []int64{1210}},
		{"behind a traf sized by its trex", [][]byte{
			trafOf(99, 0, 0, runSpec{off: at(200), sizes: make([]uint32, 3), bare: true}),
			trafOf(1, 0, 0, runSpec{sizes: []uint32{8}})}, []int64{1230}},
		{"behind two trafs", [][]byte{other, trafOf(98, 0, 0, runSpec{sizes: []uint32{5}}),
			trafOf(1, 0, 0, runSpec{sizes: []uint32{8}})}, []int64{1217}},
		{"behind a traf with no run", [][]byte{other, trafOf(98, 0, 0), trafOf(1, 0, 0, runSpec{sizes: []uint32{8}})}, []int64{1212}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fi fragInfo
			if err := parseFragment(&fi, joinBytes(append([][]byte{mfhd}, tc.trafs...)...), trexes, 1, moofOff); err != nil {
				t.Fatal(err)
			}
			var got []int64
			for _, s := range fi.samples {
				got = append(got, s.off)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("samples at %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("samples at %v, want %v", got, tc.want)
				}
			}
			// The head peek places the first sample where the read does.
			h := readMoofHead(joinBytes(append([][]byte{mfhd}, tc.trafs...)...), trexes, moofOff)
			if off, _, ok := h.firstSample(1, trexes); !ok || off != tc.want[0] {
				t.Errorf("the head places the first sample at %d (%v), want %d", off, ok, tc.want[0])
			}
		})
	}

	// A traf that cannot be measured leaves nothing to base the next on.
	// One that states its own base is read all the same.
	broken := trafOf(99, 0, 0, runSpec{off: at(200), sizes: []uint32{4, 4, 4}})
	copy(broken[bytes.Index(broken, []byte("trun"))+8:], u32(9)) // more samples than it holds
	var fi fragInfo
	err := parseFragment(&fi, joinBytes(mfhd, broken, trafOf(1, 0, 0, runSpec{sizes: []uint32{8}})), trexes, 1, moofOff)
	if !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a traf based on one that cannot be measured: %v, want malformed", err)
	}
	err = parseFragment(&fi, joinBytes(mfhd, broken, trafOf(1, 0x020000, 0, runSpec{off: at(300), sizes: []uint32{8}})), trexes, 1, moofOff)
	if err != nil || len(fi.samples) != 1 || fi.samples[0].off != 1300 {
		t.Errorf("a traf with its own base behind a broken one: %v, samples %+v", err, fi.samples)
	}
}

// TestTrafSampleCap: the runs of one traf together stay under the cap one
// run is held to.
func TestTrafSampleCap(t *testing.T) {
	half := runSpec{sizes: make([]uint32, maxSamplesPerFragment/2+1), bare: true}
	var fi fragInfo
	err := parseFragment(&fi, trafOf(1, 0, 0, half, half), nil, 1, 0)
	if !errors.Is(err, waxerr.ErrMalformedInput) || !strings.Contains(err.Error(), "samples") {
		t.Errorf("two runs past the cap together: %v, want malformed", err)
	}
}

// interleaved is a fragmented movie of two MPEG sound tracks whose moofs
// each hold a traf for track 2 and then one for track 1, neither stating a
// base, as ffmpeg's omit_tfhd_offset writes them: the first traf's run
// counts from the moof and the second begins where the first's data ends.
// Track 1's frames open with first and track 2's with second, and each
// lasts a second of the head's 8000 ticks.
func interleaved(first, second byte, n int) (raw []byte, frames [][]byte) {
	const unit = 417
	raw = fragmentedMPEGHead(2)
	mfhd := makeFullBox("mfhd", 0, 0, u32(1))
	for i := range n {
		two := mpegFrames(second, 300, 1)
		one := mpegFrames(first, unit, 2)
		one[4], one[unit+4] = byte(i), byte(i)+0x80
		frames = append(frames, one[:unit], one[unit:])
		build := func(dataOff int32) []byte {
			return makeBox("moof", mfhd,
				trafOf(2, 0, 0, runSpec{off: at(dataOff), sizes: []uint32{300}, dur: 8000}),
				trafOf(1, 0, 0, runSpec{off: at(0), sizes: []uint32{unit, unit}, dur: 8000}))
		}
		raw = append(raw, build(int32(len(build(0))+8))...)
		raw = append(raw, makeBox("mdat", two, one)...)
	}
	return raw, frames
}

// TestSecondTrafIsReadFromBehindTheFirst: both the read and the walk place
// the second traf's samples behind the first traf's data, and the open-time
// layer check reads the frame that is there.
func TestSecondTrafIsReadFromBehindTheFirst(t *testing.T) {
	raw, frames := interleaved(0xFB, 0xFB, 3)
	d, err := NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.sel.id != 1 {
		t.Fatalf("selected track %d, want the first", d.sel.id)
	}
	var pkt container.Packet
	for i, want := range frames {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if !bytes.Equal(pkt.Data, want) {
			t.Fatalf("packet %d is not track 1's frame %d", i, i)
		}
	}
	if err := d.ReadPacket(&pkt); err != io.EOF {
		t.Fatalf("behind the last frame: %v, want EOF", err)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != int64(len(frames))*44100 || !tr.SamplesExact {
		t.Errorf("the walk settled %d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, len(frames)*44100)
	}
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			t.Errorf("damage on a clean file: %s", w.Msg)
		}
	}
	// A seek lands through the index the same placement built.
	landed, err := d.SeekSample(context.Background(), 0, 4*44100)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReadPacket(&pkt); err != nil || !bytes.Equal(pkt.Data, frames[landed/44100]) {
		t.Errorf("the packet behind a seek that landed at %d is not that frame (%v)", landed, err)
	}

	// Track 1 holds Layer II frames: it is named and passed over, which takes
	// reading its first frame from behind track 2's data.
	raw, _ = interleaved(0xFD, 0xFB, 3)
	d, err = NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.sel.id != 2 || d.Tracks()[0].Codec != codec.MP3 {
		t.Errorf("selected track %d as %q, want the second as mp3", d.sel.id, d.Tracks()[0].Codec)
	}
}

// TestLayerIsReadInABareSegment: fragments handed over with an init of their
// own begin at offset 0, and the first of them still names the layer.
func TestLayerIsReadInABareSegment(t *testing.T) {
	init := fragmentedMPEGHead(1)
	media := func(second byte) container.Source {
		var raw []byte
		for i := range 3 {
			raw = append(raw, fragmentBoxes(uint32(i+1), int64(i)*1152, []uint32{1152}, []uint32{417}, mpegFrames(second, 417, 1))...)
		}
		return container.BytesSource(raw)
	}
	_, err := NewFragmentedDemuxer(init, media(0xFD))
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) || !strings.Contains(err.Error(), "found: MP2") {
		t.Errorf("Layer II segments: %v, want unsupported-format naming MP2", err)
	}
	d, err := NewFragmentedDemuxer(init, media(0xFB))
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || len(pkt.Data) != 417 || pkt.Data[1] != 0xFB {
		t.Errorf("first packet: %d bytes, %v", len(pkt.Data), err)
	}
}

// TestMoofHeadWalk: the heads are read in file order from where the
// fragments begin, moofs only, within the box budget, and a box that runs to
// the end of the file ends the walk.
func TestMoofHeadWalk(t *testing.T) {
	frag := func(i int) []byte {
		return fragmentBoxes(uint32(i+1), int64(i)*1152, []uint32{1152}, []uint32{417}, mpegFrames(0xFB, 417, 1))
	}
	free := makeBox("free", make([]byte, 16))
	offs := func(d *Demuxer) (out []int64) {
		for i := 0; ; i++ {
			h, ok := d.moofHead(i)
			if !ok {
				return out
			}
			out = append(out, h.off)
		}
	}
	same := func(t *testing.T, got, want []int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("moof heads at %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("moof heads at %v, want %v", got, want)
			}
		}
	}
	head := fragmentedMPEGHead(1)
	t.Run("moofs among other boxes", func(t *testing.T) {
		raw := joinBytes(head, free, frag(0), free, frag(1))
		first := int64(len(head) + len(free))
		same(t, offs(openFx(t, raw)), []int64{first, first + int64(len(frag(0))+len(free))})
	})
	t.Run("the box budget", func(t *testing.T) {
		raw := append([]byte(nil), head...)
		var want []int64
		for i := range 6 {
			if i < maxHeadPeekBoxes/2 {
				want = append(want, int64(len(raw)))
			}
			raw = append(raw, frag(i)...)
		}
		d := openFx(t, raw)
		same(t, offs(d), want)
		if d.moofHeads.boxes != maxHeadPeekBoxes {
			t.Errorf("stepped over %d boxes, want the budget of %d", d.moofHeads.boxes, maxHeadPeekBoxes)
		}
	})
	t.Run("a box that runs to the end", func(t *testing.T) {
		toEnd := makeBox("mdat", make([]byte, 32))
		copy(toEnd, u32(0))
		raw := joinBytes(head, frag(0), toEnd, frag(1))
		same(t, offs(openFx(t, raw)), []int64{int64(len(head))})
	})
	t.Run("a bare segment starts at 0", func(t *testing.T) {
		d, err := NewFragmentedDemuxer(head, container.BytesSource(joinBytes(frag(0), frag(1))))
		if err != nil {
			t.Fatal(err)
		}
		same(t, offs(d), []int64{0, int64(len(frag(0)))})
	})
}

// TestFirstSampleOfAMoofHead covers the head-of-moof parse the fragment peek
// rests on, whole and cut off.
func TestFirstSampleOfAMoofHead(t *testing.T) {
	moof := fragmentBoxes(1, 0, []uint32{1152, 1152}, []uint32{417, 300}, make([]byte, 717))
	payload := findBoxForTest(t, moof, "moof")
	want := int64(1000 + 8 + len(payload) + 8) // behind the moof and the mdat header
	for _, tc := range []struct {
		name string
		buf  []byte
		id   int
		ok   bool
	}{
		{"the whole moof", payload, 1, true},
		{"cut behind the first trun entry", payload[:len(payload)-8], 1, true},
		{"cut inside the first trun entry", payload[:len(payload)-12], 1, false},
		{"cut inside the tfhd", payload[:30], 1, false},
		{"another track's traf", payload, 2, false},
		{"an unknown track takes the first traf", payload, 0, true},
		{"nothing", nil, 1, false},
	} {
		off, size, ok := readMoofHead(tc.buf, nil, 1000).firstSample(tc.id, nil)
		if ok != tc.ok || ok && (off != want || size != 417) {
			t.Errorf("%s: sample at %d of %d bytes, ok %v; want %d of 417, %v", tc.name, off, size, ok, want, tc.ok)
		}
	}

	// Cut anywhere, a head of two trafs places a first sample where the
	// whole moof does, or places none: the second traf's needs the first
	// whole, and a tfhd cut inside its base offset states no base.
	whole := joinBytes(makeFullBox("mfhd", 0, 0, u32(1)),
		trafOf(2, 0x000001, 5000, runSpec{off: at(64), sizes: []uint32{4, 4}}),
		trafOf(1, 0, 0, runSpec{sizes: []uint32{8, 8}}))
	for id, want := range map[int]struct {
		off  int64
		size uint32
	}{2: {5064, 4}, 1: {5072, 8}} {
		placed := 0
		for cut := 0; cut <= len(whole); cut++ {
			off, size, ok := readMoofHead(whole[:cut], nil, 1000).firstSample(id, nil)
			if !ok {
				continue
			}
			placed++
			if off != want.off || size != want.size {
				t.Fatalf("track %d, cut at %d: sample at %d of %d bytes, want %d of %d", id, cut, off, size, want.off, want.size)
			}
		}
		if placed == 0 || placed == len(whole)+1 {
			t.Errorf("track %d: %d of %d cuts placed a sample, want the whole head to and a short one not to", id, placed, len(whole)+1)
		}
	}
}
