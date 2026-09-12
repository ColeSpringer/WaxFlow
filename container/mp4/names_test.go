package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// movieTimescale is the media timescale buildMovie writes, and the rate a
// caller's sample entry should declare so the two agree.
const movieTimescale = 8000

// buildMovie assembles a minimal QuickTime movie around one audio sample
// entry: ftyp, a moov holding one sound trak, and an mdat of unitBytes per
// unit. stts gives every unit one tick, so the media timescale is the frame
// rate for a byte-linear codec; chunkFrames units share a chunk, which is what
// lets a test build several chunks without depending on how ffmpeg happens to
// interleave. entry is a complete sample-entry box.
func buildMovie(entry []byte, unitBytes, frames, chunkFrames int) []byte {
	return movie{entry: entry, unitBytes: unitBytes, frames: frames, chunkFrames: chunkFrames}.build()
}

// movie is what buildMovie assembles, with the knobs a caller needs when the
// default table is the wrong shape for what it is testing. The zero value
// builds nothing; entry, unitBytes and frames are required.
type movie struct {
	entry       []byte // a complete sample-entry box
	unitBytes   int    // bytes per unit, and the constant stsz size
	frames      int    // units in the track
	chunkFrames int    // units per chunk; 0 puts the whole track in one
	timescale   int    // media timescale; 0 takes movieTimescale
	sttsDelta   int    // ticks per unit; 0 takes one
	// perSample writes a per-sample stsz rather than a constant one, the
	// shape a byte-linear track cannot be chunk-indexed from.
	perSample bool
}

func (m movie) build() []byte {
	entry, unitBytes, frames := m.entry, m.unitBytes, m.frames
	chunkFrames := m.chunkFrames
	if chunkFrames <= 0 {
		chunkFrames = frames
	}
	timescale := m.timescale
	if timescale <= 0 {
		timescale = movieTimescale
	}
	delta := m.sttsDelta
	if delta <= 0 {
		delta = 1
	}
	stsz := makeFullBox("stsz", 0, 0, u32(uint32(unitBytes)), u32(uint32(frames)))
	if m.perSample {
		sizes := make([][]byte, 0, frames+2)
		sizes = append(sizes, u32(0), u32(uint32(frames)))
		for range frames {
			sizes = append(sizes, u32(uint32(unitBytes)))
		}
		stsz = makeFullBox("stsz", 0, 0, sizes...)
	}
	chunks := 0
	if frames > 0 {
		chunks = (frames + chunkFrames - 1) / chunkFrames
	}

	// stco's offsets are only knowable once everything ahead of mdat is
	// sized, so it is built at its final size with zeros and patched below.
	offs := make([][]byte, chunks+1)
	offs[0] = u32(uint32(chunks))
	for c := range chunks {
		offs[c+1] = u32(0)
	}
	stbl := makeBox("stbl",
		makeFullBox("stsd", 0, 0, u32(1), entry),
		makeFullBox("stts", 0, 0, u32(1), u32(uint32(frames)), u32(uint32(delta))),
		// One stsc run: every chunk holds chunkFrames units but the last,
		// which a reader derives from the sample total, not a second run.
		makeFullBox("stsc", 0, 0, u32(1), u32(1), u32(uint32(chunkFrames)), u32(1)),
		stsz,
		makeFullBox("stco", 0, 0, offs...))
	minf := makeBox("minf",
		makeFullBox("smhd", 0, 0, u16(0), u16(0)),
		makeBox("dinf", makeFullBox("dref", 0, 0, u32(1), makeFullBox("url ", 0, 1))),
		stbl)
	mdia := makeBox("mdia",
		makeFullBox("mdhd", 0, 0, u32(0), u32(0),
			u32(uint32(timescale)), u32(uint32(frames*delta)), u16(0x55C4), u16(0)),
		makeFullBox("hdlr", 0, 0, u32(0), []byte("soun"), make([]byte, 12), []byte{0}),
		minf)
	trak := makeBox("trak",
		makeFullBox("tkhd", 0, 7, u32(0), u32(0), u32(1), u32(0),
			u32(uint32(frames*delta)), make([]byte, 8), u16(0), u16(0), u16(0x0100), u16(0),
			unityMatrix(), u32(0), u32(0)),
		mdia)
	moov := makeBox("moov",
		makeFullBox("mvhd", 0, 0, u32(0), u32(0), u32(uint32(timescale)),
			u32(uint32(frames*delta)), u32(0x00010000), u16(0x0100), u16(0),
			make([]byte, 8), unityMatrix(), make([]byte, 24), u32(2)),
		trak)
	ftyp := makeBox("ftyp", []byte("qt  "), u32(512), []byte("qt  "))

	payload := make([]byte, frames*unitBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	raw := append(append(ftyp, moov...), makeBox("mdat", payload)...)

	// mdat's payload starts 8 bytes past the box, and stco's first entry 12
	// bytes past its type (version+flags, then the entry count). Searched
	// from the end of the header region, since stco is the last box with any
	// content in the movie and a caller's sample entry could hold the word.
	base := len(ftyp) + len(moov) + 8
	at := bytes.LastIndex(raw[:len(ftyp)+len(moov)], []byte("stco")) + 12
	for c := range chunks {
		binary.BigEndian.PutUint32(raw[at+4*c:], uint32(base+c*chunkFrames*unitBytes))
	}
	return raw
}

// unityMatrix is the identity transform every movie and track header carries.
func unityMatrix() []byte {
	m := make([]byte, 0, 36)
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		m = append(m, u32(v)...)
	}
	return m
}

// soundEntry builds a version 0 AudioSampleEntry of the given format.
func soundEntry(format string, channels, bits, rate int) []byte {
	return soundEntryWith(format, channels, bits, rate)
}

// soundEntryWith is soundEntry with child boxes after the header, which is
// where every codec extension lives. A rate of 0 writes a zero 16.16 field,
// the shape an entry takes when the rate it needs to state will not fit.
func soundEntryWith(format string, channels, bits, rate int, children ...[]byte) []byte {
	head := [][]byte{
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), u32(0), // version 0, revision, vendor
		u16(uint16(channels)), u16(uint16(bits)),
		u16(0), u16(0), // compressionID, packetSize
		u32(uint32(rate) << 16),
	}
	return makeBox(format, append(head, children...)...)
}

// TestRefusalNamesTheSampleEntryFormat pins that an entry this build has no
// decoder for is refused by the codec's name rather than by its fourcc alone.
// The "ms" spelling matters most: its two trailing bytes are a WAVE format tag
// and are usually not printable, so a refusal echoing the raw fourcc would put
// a NUL in the middle of a sentence.
func TestRefusalNamesTheSampleEntryFormat(t *testing.T) {
	for _, tc := range []struct{ format, want string }{
		{"MAC3", "MAC3"},
		{"ms\x00\x02", "ADPCM (ms 0x0002)"},
		// No name either way: the fourcc as it stands.
		{"QDM2", "QDM2"},
	} {
		raw := buildMovie(soundEntry(tc.format, 1, 16, movieTimescale), 2, 64, 64)
		_, err := NewDemuxer(container.BytesSource(raw), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Errorf("%q: error = %v, want unsupported-format", tc.format, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v does not name %q", tc.format, err, tc.want)
		}
	}
}

// TestUnnamedEsdsCodecIsNotCountedAsAName pins the rule selectAudio's `named`
// counter exists for: the deferred stsd reason replaces the "found:" list only
// when the list says nothing a caller can act on. An esds object type nothing
// names reaches the list as the same placeholder a track with no sample
// description at all contributes, so counting it would hide the one specific
// reason the file was refused behind "found: unknown".
func TestUnnamedEsdsCodecIsNotCountedAsAName(t *testing.T) {
	reason := malformed("audio object type 1 is not AAC-LC")
	broken := &track{handler: "soun", stsdErr: reason}
	unnamed := &track{handler: "soun", codec: unnamedCodec}

	d := &Demuxer{}
	err := d.selectAudio([]*track{broken, unnamed})
	if !errors.Is(err, reason) {
		t.Fatalf("error = %v, want the deferred stsd reason", err)
	}
	if strings.Contains(err.Error(), "found:") {
		t.Errorf("error = %q; a list of nothing but placeholders must not replace the reason", err)
	}
	// A real codec name alongside it is actionable, so the list comes back.
	named := &track{handler: "soun", codec: "AC-3"}
	err = d.selectAudio([]*track{broken, named})
	if !strings.Contains(err.Error(), "found: unknown, AC-3") {
		t.Errorf("error = %q, want the codec list beside the reason", err)
	}
}
