package ape

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// fixture returns a committed .ape file, the raw material for the header
// tests: a real header is what a mutation has to start from to mean anything.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseHeader(t *testing.T) {
	raw := fixture(t, "sine-s16.ape")
	h, err := ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.FileVersion != 3990 || h.CompressionLevel != LevelNormal {
		t.Errorf("version %d level %d", h.FileVersion, h.CompressionLevel)
	}
	if h.Channels != 2 || h.BitsPerSample != 16 || h.Rate != 44100 {
		t.Errorf("shape: %dch %dbit %dHz", h.Channels, h.BitsPerSample, h.Rate)
	}
	if h.BlocksPerFrame != 73728 {
		t.Errorf("blocks per frame = %d, want 73728", h.BlocksPerFrame)
	}
	if h.Samples() != 22050 {
		t.Errorf("samples = %d, want 22050", h.Samples())
	}
	// The seek table's first entry and the header's own frame-data offset are
	// two statements of the same fact; a file where they disagree is refused
	// by the container, so the fixtures must not.
	if got := int64(binary.LittleEndian.Uint32(raw[h.SeekTableOffset:])); got != h.FrameDataOffset {
		t.Errorf("seek[0] = %d, header puts the frames at %d", got, h.FrameDataOffset)
	}
	if h.FrameDataOffset+h.FrameDataBytes > int64(len(raw)) {
		t.Errorf("frame data runs to %d, past the %d-byte file", h.FrameDataOffset+h.FrameDataBytes, len(raw))
	}
	if !Match(raw) {
		t.Error("a real file fails the sniff")
	}
}

// withHeaderField returns a copy of a 3980-and-later fixture with one
// little-endian field of the format header replaced.
func withHeaderField(t testing.TB, raw []byte, off, width int, v uint64) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	at := int(binary.LittleEndian.Uint32(raw[8:])) + off // past the descriptor
	switch width {
	case 2:
		binary.LittleEndian.PutUint16(out[at:], uint16(v))
	case 4:
		binary.LittleEndian.PutUint32(out[at:], uint32(v))
	default:
		t.Fatalf("width %d", width)
	}
	return out
}

// Offsets of the fields inside the 3980-and-later format header.
const (
	offLevel          = 0
	offFormatFlags    = 2
	offBlocksPerFrame = 4
	offFinalBlocks    = 8
	offTotalFrames    = 12
	offBits           = 16
	offChannels       = 18
	offRate           = 20
)

// TestHeaderRefusals pins the shapes this decoder turns away, each by a
// message that says what the file is. They are all legal Monkey's Audio except
// where noted, so the wording matters as much as the refusal.
func TestHeaderRefusals(t *testing.T) {
	raw := fixture(t, "sine-s16.ape")
	cases := map[string]struct {
		mutate func() []byte
		want   string
	}{
		"32-bit": {
			func() []byte { return withHeaderField(t, raw, offBits, 2, 32) },
			"only 8, 16, and 24-bit",
		},
		"float": {
			func() []byte { return withHeaderField(t, raw, offFormatFlags, 2, flagFloatingPoint) },
			"floating-point",
		},
		"multichannel": {
			func() []byte { return withHeaderField(t, raw, offChannels, 2, 6) },
			"only mono and stereo",
		},
		"zero channels": {
			func() []byte { return withHeaderField(t, raw, offChannels, 2, 0) },
			"only mono and stereo",
		},
		"old bitstream": {
			func() []byte {
				out := append([]byte(nil), raw...)
				binary.LittleEndian.PutUint16(out[4:], 3930)
				return out
			},
			"different codec",
		},
		"future bitstream": {
			func() []byte {
				out := append([]byte(nil), raw...)
				binary.LittleEndian.PutUint16(out[4:], 4100)
				return out
			},
			"past the supported",
		},
		"unknown level": {
			func() []byte { return withHeaderField(t, raw, offLevel, 2, 2500) },
			"compression level",
		},
		"no frames": {
			func() []byte { return withHeaderField(t, raw, offTotalFrames, 4, 0) },
			"never finalized",
		},
		"zero rate": {
			func() []byte { return withHeaderField(t, raw, offRate, 4, 0) },
			"sample rate",
		},
		"frame longer than the level allows": {
			func() []byte { return withHeaderField(t, raw, offBlocksPerFrame, 4, 2_000_000) },
			"exceeds",
		},
		"final frame longer than a frame": {
			func() []byte { return withHeaderField(t, raw, offFinalBlocks, 4, 100_000) },
			"final frame",
		},
		"not a Monkey's Audio file": {
			func() []byte {
				out := append([]byte(nil), raw...)
				copy(out, "MAD ")
				return out
			},
			"not a Monkey's Audio file",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHeader(tc.mutate())
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("code = %v, want %v", code, waxerr.CodeUnsupportedFormat)
			}
		})
	}
}

// oldHeader builds a pre-3980 file header by hand. No encoder in circulation
// writes one any more, so this is the only way the older layout gets read: the
// fields sit in a different order, the depth rides in the format flags, the
// frame length is implied rather than stored, and the stored source header
// comes before the seek table rather than after it.
func oldHeader(version, level, flags, channels, rate, headerBytes, terminating, frames, finalBlocks int, seekElements int) []byte {
	b := make([]byte, oldHeaderLen)
	copy(b, "MAC ")
	binary.LittleEndian.PutUint16(b[4:], uint16(version))
	binary.LittleEndian.PutUint16(b[6:], uint16(level))
	binary.LittleEndian.PutUint16(b[8:], uint16(flags))
	binary.LittleEndian.PutUint16(b[10:], uint16(channels))
	binary.LittleEndian.PutUint32(b[12:], uint32(rate))
	binary.LittleEndian.PutUint32(b[16:], uint32(headerBytes))
	binary.LittleEndian.PutUint32(b[20:], uint32(terminating))
	binary.LittleEndian.PutUint32(b[24:], uint32(frames))
	binary.LittleEndian.PutUint32(b[28:], uint32(finalBlocks))
	if flags&flagHasPeakLevel != 0 {
		b = append(b, 0, 0, 0, 0)
	}
	if flags&flagHasSeekElements != 0 {
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(seekElements))
		b = append(b, n[:]...)
	}
	return b
}

func TestParseOldHeader(t *testing.T) {
	const (
		frames      = 3
		finalBlocks = 1000
		wavHeader   = 44
	)
	b := oldHeader(3970, LevelHigh, flagHasSeekElements, 2, 48000, wavHeader, 0, frames, finalBlocks, frames)
	b = append(b, make([]byte, 4096)...) // room for the stored header and table
	h, err := ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	if h.FileVersion != 3970 || h.CompressionLevel != LevelHigh {
		t.Errorf("version %d level %d", h.FileVersion, h.CompressionLevel)
	}
	if h.BitsPerSample != 16 || h.Channels != 2 || h.Rate != 48000 {
		t.Errorf("shape: %dbit %dch %dHz", h.BitsPerSample, h.Channels, h.Rate)
	}
	// The old form does not store the frame length; every version in scope
	// used the same one.
	if h.BlocksPerFrame != 73728*4 {
		t.Errorf("blocks per frame = %d, want %d", h.BlocksPerFrame, 73728*4)
	}
	if h.Samples() != 2*73728*4+finalBlocks {
		t.Errorf("samples = %d", h.Samples())
	}
	// The seek-element count follows the header, the stored source header
	// follows that, and the table follows the header. Getting that order
	// wrong is what the offsets pin.
	wantTable := int64(oldHeaderLen + 4 + wavHeader)
	if h.SeekTableOffset != wantTable {
		t.Errorf("seek table at %d, want %d", h.SeekTableOffset, wantTable)
	}
	if h.FrameDataOffset != wantTable+frames*4 {
		t.Errorf("frames at %d, want %d", h.FrameDataOffset, wantTable+frames*4)
	}
	if h.FrameDataBytes != -1 {
		t.Errorf("frame-data byte count = %d; the old form does not carry one", h.FrameDataBytes)
	}
}

// TestParseOldHeaderDepthFlags pins the pre-3980 depth encoding: the sample
// width is two flag bits rather than a field, and no flag means 16.
func TestParseOldHeaderDepthFlags(t *testing.T) {
	for _, tc := range []struct {
		flags, depth int
	}{{0, 16}, {flag8Bit, 8}, {flag24Bit, 24}} {
		b := append(oldHeader(3950, LevelFast, tc.flags, 1, 44100, 0, 0, 1, 100, 1), make([]byte, 64)...)
		h, err := ParseHeader(b)
		if err != nil {
			t.Fatalf("flags %#x: %v", tc.flags, err)
		}
		if h.BitsPerSample != tc.depth {
			t.Errorf("flags %#x: %d-bit, want %d", tc.flags, h.BitsPerSample, tc.depth)
		}
	}
}

// TestOldHeaderPeakLevelShiftsTheTable pins the optional peak-level field: it
// sits between the header and the seek-element count, so missing it would put
// the table four bytes early and read the file as garbage.
func TestOldHeaderPeakLevelShiftsTheTable(t *testing.T) {
	plain := append(oldHeader(3950, LevelFast, flagHasSeekElements, 1, 44100, 0, 0, 1, 100, 1), make([]byte, 64)...)
	withPeak := append(oldHeader(3950, LevelFast, flagHasSeekElements|flagHasPeakLevel, 1, 44100, 0, 0, 1, 100, 1), make([]byte, 64)...)
	a, err := ParseHeader(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseHeader(withPeak)
	if err != nil {
		t.Fatal(err)
	}
	if b.SeekTableOffset != a.SeekTableOffset+4 {
		t.Errorf("peak level moved the table to %d, want %d", b.SeekTableOffset, a.SeekTableOffset+4)
	}
}

// TestOldHeaderWithoutSeekElementCount pins the fallback: a file that does not
// state its seek-table length has one entry per frame.
func TestOldHeaderWithoutSeekElementCount(t *testing.T) {
	b := append(oldHeader(3950, LevelFast, 0, 1, 44100, 0, 0, 7, 100, 0), make([]byte, 128)...)
	h, err := ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	if h.SeekTableEntries != 7 {
		t.Errorf("seek entries = %d, want one per frame (7)", h.SeekTableEntries)
	}
	if h.SeekTableOffset != oldHeaderLen {
		t.Errorf("seek table at %d, want %d", h.SeekTableOffset, oldHeaderLen)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	want := Config{FileVersion: 3990, CompressionLevel: LevelInsane, BlocksPerFrame: 73728 * 16,
		BitsPerSample: 24, Channels: 2, Rate: 96000}
	blob, err := want.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseConfig(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip: %+v, want %+v", got, want)
	}
	if f := got.Format(); f.Rate != 96000 || f.Channels != 2 || f.BitDepth != 24 || f.Type != audio.Int {
		t.Errorf("format = %v", f)
	}
	for _, bad := range [][]byte{nil, blob[:15], append(append([]byte(nil), blob...), 0), {9, 2, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		if _, err := ParseConfig(bad); err == nil {
			t.Errorf("ParseConfig(%v) accepted", bad)
		}
	}
}

func TestFrameHeaderRoundTrip(t *testing.T) {
	pkt := make([]byte, FrameHeaderLen+3)
	PutFrameHeader(pkt, 4321, 2)
	copy(pkt[FrameHeaderLen:], "abc")
	blocks, skip, data, err := ParseFrameHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if blocks != 4321 || skip != 2 || string(data) != "abc" {
		t.Errorf("blocks=%d skip=%d data=%q", blocks, skip, data)
	}
	for name, bad := range map[string][]byte{
		"short":            pkt[:4],
		"zero blocks":      headerBytes(0, 0),
		"huge blocks":      headerBytes(1<<30, 0),
		"skip past a word": headerBytes(1, 4),
	} {
		if _, _, _, err := ParseFrameHeader(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestMatchIsTheMagicAlone pins the sniff to the file magic. A leading ID3v2
// tag is the registry's business (format.resolve rebases the source past one
// before any driver is asked), so recognizing it here would be a second,
// weaker copy of that.
func TestMatchIsTheMagicAlone(t *testing.T) {
	raw := fixture(t, "sine-s16.ape")
	if !Match(raw) {
		t.Error("a real file fails the sniff")
	}
	float := append([]byte(nil), raw...)
	copy(float, "MACF")
	if !Match(float) {
		t.Error("a float stream fails the sniff; it is a file, refused later by name")
	}
	for _, bad := range []string{"MAD ", "MAC", "", "ID3v2 in front of a file"} {
		if Match([]byte(bad)) {
			t.Errorf("Match(%q) accepted", bad)
		}
	}
}

// TestFloatStreamRefusedByName pins the float refusal on the path that
// actually reaches it. It is a header flag, so only ParseHeader can see it:
// Config carries no flags and must not pretend to check.
func TestFloatStreamRefusedByName(t *testing.T) {
	raw := withHeaderField(t, fixture(t, "sine-s16.ape"), offFormatFlags, 2, flagFloatingPoint)
	_, err := ParseHeader(raw)
	if err == nil || !strings.Contains(err.Error(), "floating-point") {
		t.Fatalf("float refusal = %v", err)
	}
}

// TestRangeModelWidthsMatchTheReference pins the derivation. The reference
// lists each symbol's width as its own constant; they are the cumulative
// totals' first differences, so the widths are computed rather than
// transcribed. These spot checks are the reference's own numbers, and they are
// what keeps the 3950..3989 model honest, since no fixture exercises it.
func TestRangeModelWidthsMatchTheReference(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *rangeModel
		want  map[int]uint32
	}{
		{"3990", modelCurrent(), map[int]uint32{0: 19578, 1: 16582, 2: 12257, 3: 7906, 9: 119, 17: 2, 63: 1}},
		{"3950", modelOld(), map[int]uint32{0: 14824, 1: 13400, 2: 11124, 3: 8507, 9: 677, 17: 11, 63: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, want := range tc.want {
				if got := tc.model.width[i]; got != want {
					t.Errorf("width[%d] = %d, want %d", i, got, want)
				}
			}
			var sum uint32
			for _, w := range tc.model.width {
				sum += w
			}
			if sum != 1<<16 {
				t.Errorf("widths sum to %d, want the model's %d", sum, 1<<16)
			}
			// The inverse table has to agree with the totals it was built
			// from, at every rung's edges.
			for sym := range tc.model.width {
				lo, hi := tc.model.total[sym], tc.model.total[sym+1]-1
				if got := tc.model.tab[lo]; got != uint8(sym) {
					t.Errorf("tab[%d] = %d, want %d", lo, got, sym)
				}
				if got := tc.model.tab[hi]; got != uint8(sym) {
					t.Errorf("tab[%d] = %d, want %d", hi, got, sym)
				}
			}
		})
	}
}
