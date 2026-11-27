package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestMatchStepsOverLeadingPadding: an ftyp behind a free, skip or wide box
// is still an MP4, while the padding fits the sniff window.
func TestMatchStepsOverLeadingPadding(t *testing.T) {
	// The window is a variable so the cases below are sized at run time.
	window := MatchNeed
	if window < 1024 {
		t.Fatalf("MatchNeed = %d, want room for the padding boxes ahead of ftyp", window)
	}
	ftyp := makeBox("ftyp", []byte("M4A "), u32(0), []byte("M4A "))
	for _, tc := range []struct {
		name string
		head []byte
		want bool
	}{
		{"ftyp first", ftyp, true},
		{"behind a free", joinBytes(makeBox("free", make([]byte, 24)), ftyp), true},
		{"behind a skip and a wide", joinBytes(makeBox("skip", make([]byte, 8)), makeBox("wide"), ftyp), true},
		{"padding to the end of the window", joinBytes(makeBox("free", make([]byte, window-16)), ftyp)[:window], true},
		{"padding past the window", joinBytes(makeBox("free", make([]byte, window)), ftyp)[:window], false},
		{"a free of the to-end form", joinBytes([]byte{0, 0, 0, 0, 'f', 'r', 'e', 'e'}, ftyp), false},
		{"another box first", joinBytes(makeBox("moov"), ftyp), false},
		{"an oversized ftyp", joinBytes(u32(4096), []byte("ftyp")), false},
		{"nothing", nil, false},
	} {
		if got := Match(tc.head); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
	// And such a file opens: the demuxer already walks past the padding.
	raw := fxMovie{lead: [][]byte{makeBox("free", make([]byte, 24))}}.build()
	if !Match(raw[:min(len(raw), window)]) {
		t.Fatal("a movie behind a free box does not match")
	}
	if tr := openFx(t, raw).Tracks()[0]; tr.Codec != codec.PCM || tr.Samples != 16000 {
		t.Errorf("a movie behind a free box opened as %s, %d samples", tr.Codec, tr.Samples)
	}
}

// mpegEsdsEntry is an mp4a sample entry whose esds names an object type
// with no DecoderSpecificInfo, the shape MPEG audio takes.
func mpegEsdsEntry(objectType byte, channels, rate int) []byte {
	decCfg := descriptor(tagDecoderConfig, concat([]byte{objectType, 0x05<<2 | 1, 0, 0, 0}, u32(0), u32(0)))
	es := descriptor(tagES, concat(u16(1), []byte{0}, decCfg, descriptor(0x06, []byte{0x02})))
	return soundEntryWith("mp4a", channels, 16, rate, makeFullBox("esds", 0, 0, es))
}

// mpegFrames is n frames of unit bytes each opening with an MPEG audio
// header: 0xFB is MPEG-1 Layer III, 0xFD Layer II, 0xFF Layer I.
func mpegFrames(second byte, unit, n int) []byte {
	f := make([]byte, unit)
	copy(f, []byte{0xFF, second, 0x90, 0x00})
	return bytes.Repeat(f, n)
}

// TestMPEGLayerIsReadFromTheFirstFrame: an mp4a object type says MPEG audio
// and names no layer. Layer III is the MP3 this build decodes; Layer I and
// II are named and refused at open rather than found out mid-decode.
func TestMPEGLayerIsReadFromTheFirstFrame(t *testing.T) {
	movie := func(second byte) fxMovie {
		return fxMovie{frames: 8, unit: 417, entry: mpegEsdsEntry(0x6B, 2, 44100), payload: mpegFrames(second, 417, 8)}
	}
	if tr := openFx(t, movie(0xFB).build()).Tracks()[0]; tr.Codec != codec.MP3 {
		t.Errorf("Layer III opened as %q, want mp3", tr.Codec)
	}
	for _, tc := range []struct {
		second byte
		name   string
	}{{0xFD, "MP2"}, {0xFF, "MP1"}} {
		_, err := NewDemuxer(container.BytesSource(movie(tc.second).build()), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Errorf("%s: error = %v, want unsupported-format", tc.name, err)
			continue
		}
		if !strings.Contains(err.Error(), "found: "+tc.name) {
			t.Errorf("%s: %v does not name the codec", tc.name, err)
		}
	}
	// An entry that names Layer III over Layer II frames is refused the same
	// way, where it used to open and fail at the first packet.
	raw := fxMovie{frames: 8, unit: 417, entry: v1SoundEntry(".mp3", 2, 16, 44100, 0), payload: mpegFrames(0xFD, 417, 8)}.build()
	if _, err := NewDemuxer(container.BytesSource(raw), nil); err == nil || !strings.Contains(err.Error(), "found: MP2") {
		t.Errorf("a .mp3 entry over Layer II frames: error = %v, want it named MP2", err)
	}
	// A track that is passed over does not take a later one down with it.
	m := movie(0xFD)
	pcm := func(off int64) [][]byte {
		return [][]byte{fxSoundTrak(3, soundEntry("sowt", 1, 16, 8000), 2, 64, off, 0, nil, nil)}
	}
	m.trak = pcm(0)
	m.trak = pcm(int64(len(m.build()) - len(m.payload)))
	if tr := openFx(t, m.build()).Tracks()[0]; tr.Codec != codec.PCM {
		t.Errorf("a PCM track behind a Layer II one opened as %q", tr.Codec)
	}
}

// TestRefusalNamesTheObjectType: an mp4a entry is the MPEG-4 Systems
// wrapper, so a refusal names what its object type says it holds.
func TestRefusalNamesTheObjectType(t *testing.T) {
	for ot, want := range map[byte]string{
		0xA5: "AC-3", 0xA6: "E-AC-3", 0xA9: "DTS", 0xAA: "DTS-HD High Resolution",
		0xAB: "DTS-HD Master Audio", 0xAC: "DTS Express", 0xAD: "Opus", 0xDD: "Vorbis", 0xE1: "QCELP",
	} {
		raw := fxMovie{frames: 8, unit: 64, entry: mpegEsdsEntry(ot, 2, 44100)}.build()
		_, err := NewDemuxer(container.BytesSource(raw), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Errorf("%#x: error = %v, want unsupported-format", ot, err)
			continue
		}
		if !strings.Contains(err.Error(), "found: "+want+")") {
			t.Errorf("%#x: %v does not name %q", ot, err, want)
		}
	}
}

// TestFirstHeaderDecides: of two mdhd or mvhd boxes the first is the one
// read, so a second cannot restate the timeline the samples are on.
func TestFirstHeaderDecides(t *testing.T) {
	plain := openFx(t, fxMovie{}.build()).Tracks()[0]
	mdhd2 := makeFullBox("mdhd", 0, 0, u32(0), u32(0), u32(16000), u32(99), u16(0x55C4), u16(0))
	mvhd2 := makeFullBox("mvhd", 0, 0, u32(0), u32(0), u32(600), u32(99),
		u32(0x00010000), u16(0x0100), u16(0), make([]byte, 8), unityMatrix(), make([]byte, 24), u32(9))
	d := openFx(t, fxMovie{mdia: [][]byte{mdhd2}}.build())
	if tr := d.Tracks()[0]; tr.Samples != plain.Samples || tr.SamplesAdvisory {
		t.Errorf("a second mdhd moved the track: %d samples (advisory %v), want %d", tr.Samples, tr.SamplesAdvisory, plain.Samples)
	}
	if d.sel.timescale != 8000 {
		t.Errorf("media timescale = %d, want the first mdhd's 8000", d.sel.timescale)
	}
	d = openFx(t, fxMovie{moov: [][]byte{mvhd2}}.build())
	if d.movieTimescale != 8000 {
		t.Errorf("movie timescale = %d, want the first mvhd's 8000", d.movieTimescale)
	}
}

// TestMPEGLayerIsReadInAFragmentedMovie: a fragmented movie is held to the
// same check. Where its moov's table holds samples the first of those names
// the layer, and where it is empty the first fragment's first sample does,
// at a cost that does not grow with the fragments.
func TestMPEGLayerIsReadInAFragmentedMovie(t *testing.T) {
	const unit = 417
	entry := mpegEsdsEntry(0x6B, 2, 44100)
	mvex := makeBox("mvex", makeFullBox("trex", 0, 0, u32(trackID), u32(1), u32(0), u32(0), u32(0)))
	// fragments is an empty-table movie followed by n one-frame fragments.
	fragments := func(second byte, n int) []byte {
		raw := hybridMovie(movie{entry: entry, unitBytes: unit, timescale: 44100, sttsDelta: 1152}, 0, 0, true)
		for i := range n {
			raw = append(raw, fragmentBoxes(uint32(i+1), int64(i)*1152, []uint32{1152}, []uint32{unit}, mpegFrames(second, unit, 1))...)
		}
		return raw
	}
	refused := func(t *testing.T, raw []byte) {
		t.Helper()
		_, err := NewDemuxer(container.BytesSource(raw), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) || !strings.Contains(err.Error(), "found: MP2") {
			t.Errorf("error = %v, want unsupported-format naming MP2", err)
		}
	}
	t.Run("layer II in the fragments", func(t *testing.T) { refused(t, fragments(0xFD, 4)) })
	t.Run("layer II in the moov's table", func(t *testing.T) {
		refused(t, fxMovie{frames: 8, unit: unit, entry: entry, payload: mpegFrames(0xFD, unit, 8), moov: [][]byte{mvex}}.build())
	})
	t.Run("layer III", func(t *testing.T) {
		d := openFx(t, fragments(0xFB, 4))
		if tr := d.Tracks()[0]; !d.fragmented || tr.Codec != codec.MP3 {
			t.Errorf("fragmented %v, codec %q; want a fragmented mp3 track", d.fragmented, tr.Codec)
		}
		// The peek is taken beside the packet reads, which start where they did.
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil || len(pkt.Data) != unit || pkt.Data[1] != 0xFB {
			t.Errorf("first packet: %d bytes, %v", len(pkt.Data), err)
		}
	})
	t.Run("the open reads one fragment", func(t *testing.T) {
		var reads [2]int
		var bytes [2]int64
		for i, n := range []int{20, 40} {
			src := &testutil.CountingSource{Src: container.BytesSource(fragments(0xFB, n))}
			if _, err := NewDemuxer(src, nil); err != nil {
				t.Fatal(err)
			}
			reads[i], bytes[i] = src.Reads, src.Bytes
		}
		if reads[0] != reads[1] || bytes[0] != bytes[1] {
			t.Errorf("the open made %d reads of %d bytes for 20 fragments and %d of %d for 40", reads[0], bytes[0], reads[1], bytes[1])
		}
	})
}

// TestLayerCheckLooksAtEachCandidateOnce: every sound track is looked at as
// it comes up for selection, however many there are, at one small read
// each, and none once a track is chosen.
func TestLayerCheckLooksAtEachCandidateOnce(t *testing.T) {
	const unit = 417
	entry := mpegEsdsEntry(0x6B, 2, 44100)
	// movie is n sound tracks, each with its own Layer II frame, and a PCM
	// track behind them when pcm is set.
	movie := func(n int, pcm bool) []byte {
		m := fxMovie{frames: 1, unit: unit, entry: entry, payload: mpegFrames(0xFD, unit, n)}
		build := func(base int64) {
			m.trak = nil
			for i := 1; i < n; i++ {
				m.trak = append(m.trak, fxSoundTrak(uint32(10+i), entry, unit, 1, base+int64(i*unit), 0, nil, nil))
			}
			if pcm {
				m.trak = append(m.trak, fxSoundTrak(9, soundEntry("sowt", 1, 16, 8000), 2, 64, base, 0, nil, nil))
			}
		}
		build(0)
		build(int64(len(m.build()) - len(m.payload)))
		return m.build()
	}
	reads := func(raw []byte) (int, error) {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		_, err := NewDemuxer(src, nil)
		return src.Reads, err
	}
	one, err := reads(movie(1, false))
	if err == nil || strings.Count(err.Error(), "MP2") != 1 {
		t.Fatalf("one Layer II track: error = %v, want it named once", err)
	}
	many, err := reads(movie(40, false))
	if err == nil || strings.Count(err.Error(), "MP2") != 40 {
		t.Errorf("forty Layer II tracks: error = %v, want each named", err)
	}
	if many-one != 39 {
		t.Errorf("forty tracks cost %d reads where one costs %d, want one more read a track", many, one)
	}
	// The track behind them all is still found.
	if tr := openFx(t, movie(40, true)).Tracks()[0]; tr.Codec != codec.PCM {
		t.Errorf("a PCM track behind forty Layer II ones opened as %q", tr.Codec)
	}
	// And a movie whose first track decodes looks at that one alone.
	first := fxMovie{frames: 1, unit: unit, entry: entry, payload: mpegFrames(0xFB, unit, 40)}
	for i := 1; i < 40; i++ {
		first.trak = append(first.trak, fxSoundTrak(uint32(10+i), entry, unit, 1, 0, 0, nil, nil))
	}
	if got, err := reads(first.build()); err != nil || got > one {
		t.Errorf("a first track that decodes: %d reads, %v; want no more than the %d of a one-track movie", got, err, one)
	}
}

// fragmentedMPEGHead is the head of a fragmented movie of n MPEG sound
// tracks, ids 1 to n, each with an empty sample table.
func fragmentedMPEGHead(n int) []byte {
	entry := mpegEsdsEntry(0x6B, 2, 44100)
	moov := [][]byte{makeFullBox("mvhd", 0, 0, u32(0), u32(0), u32(8000), u32(0),
		u32(0x00010000), u16(0x0100), u16(0), make([]byte, 8), unityMatrix(), make([]byte, 24), u32(uint32(n+1)))}
	var trexes [][]byte
	for id := uint32(1); id <= uint32(n); id++ {
		moov = append(moov, fxSoundTrak(id, entry, 417, 0, 0, 0, nil, nil))
		trexes = append(trexes, makeFullBox("trex", 0, 0, u32(id), u32(1), u32(0), u32(0), u32(0)))
	}
	moov = append(moov, makeBox("mvex", trexes...))
	return joinBytes(makeBox("ftyp", []byte("isom"), u32(0), []byte("isom")), makeBox("moov", moov...))
}

// TestFragmentedLayerCheckReadsOneMoofHead pins what naming the layer costs
// a fragmented open: the head of the first moof that holds the candidate
// track, read once whatever the number of tracks, and one frame header for
// each track considered. At worst that is the heads of the moofs among the
// first eight boxes behind the moov.
func TestFragmentedLayerCheckReadsOneMoofHead(t *testing.T) {
	const unit = 417
	frag := func(second byte, i int) []byte {
		return fragmentBoxes(uint32(i+1), int64(i)*1152, []uint32{1152}, []uint32{unit}, mpegFrames(second, unit, 1))
	}
	// fragOf is frag for another track's id.
	fragOf := func(id uint32, second byte, i int) []byte {
		b := frag(second, i)
		binary.BigEndian.PutUint32(b[bytes.Index(b, []byte("tfhd"))+8:], id)
		return b
	}
	// A movie that gives each track its own moofs opens with another
	// track's: the candidate's first frame is in a later moof of the window.
	t.Run("the track's first moof is not the movie's", func(t *testing.T) {
		raw := fragmentedMPEGHead(2)
		head := int64(len(raw))
		for i := range 2 {
			raw = append(raw, fragOf(2, 0xFB, i)...)
		}
		raw = append(raw, fragOf(1, 0xFD, 0)...)
		for i := range 20 {
			raw = append(raw, fragOf(2, 0xFB, 2+i)...)
		}
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		d, err := NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d.sel.id != 2 {
			t.Errorf("selected track %d, want the second: the first is Layer II, three moofs in", d.sel.id)
		}
		if past := src.Bytes - head; past > 1024 {
			t.Errorf("the open read %d bytes behind the head, want three small moofs and two frame headers", past)
		}
	})
	// Past the window, which four fragments of a moof and an mdat each
	// fill, the track is not looked for and stays as named.
	t.Run("the track's first moof is past the window", func(t *testing.T) {
		raw := fragmentedMPEGHead(2)
		for i := range 4 {
			raw = append(raw, fragOf(2, 0xFB, i)...)
		}
		raw = append(raw, fragOf(1, 0xFD, 0)...)
		if d := openFx(t, raw); d.sel.id != 1 {
			t.Errorf("selected track %d, want the first, whose layer nothing in the window states", d.sel.id)
		}
	})
	// Four tracks, the first of them Layer II: it is passed over and the
	// second, with no run in the first moof, stays the mp3 it was named.
	t.Run("several tracks", func(t *testing.T) {
		var bytes [2]int64
		for i, n := range []int{20, 40} {
			raw := fragmentedMPEGHead(4)
			head := int64(len(raw))
			for f := range n {
				raw = append(raw, frag(0xFD, f)...)
			}
			src := &testutil.CountingSource{Src: container.BytesSource(raw)}
			d, err := NewDemuxer(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d.sel.id != 2 || d.Tracks()[0].Codec != codec.MP3 {
				t.Errorf("selected track %d as %q, want the second track as mp3", d.sel.id, d.Tracks()[0].Codec)
			}
			if bytes[i] = src.Bytes - head; bytes[i] > 1024 {
				t.Errorf("the open read %d bytes behind a %d-byte head, want one small moof and a frame header", bytes[i], head)
			}
		}
		if bytes[0] != bytes[1] {
			t.Errorf("the open read %d bytes behind the head for 20 fragments and %d for 40", bytes[0], bytes[1])
		}
	})
	// The shape that makes a careless peek expensive: many tracks, and large
	// moofs that hold a run for none of them.
	t.Run("many tracks behind large moofs", func(t *testing.T) {
		raw := fragmentedMPEGHead(256)
		head := int64(len(raw))
		for range 8 {
			raw = append(raw, makeBox("moof", make([]byte, 1<<20))...)
		}
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		if past := src.Bytes - head; past > maxHeadPeekBoxes*maxHeadPeekBytes+1024 || src.Reads > 32 {
			t.Errorf("the open read %d bytes behind a %d-byte head in %d reads, want at most the heads of eight moofs", past, head, src.Reads)
		}
	})
}
