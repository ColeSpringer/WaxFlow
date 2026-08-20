package wavpack

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// firstBlock returns the first block of the shared .wv fixture, header
// included: a real block to mutate for the header and refusal tests.
func firstBlock(t testing.TB) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "sine-s16.wv"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseBlockHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw[:h.Size]
}

// withFlags returns a copy of block with its header flags replaced.
func withFlags(block []byte, flags uint32) []byte {
	out := append([]byte(nil), block...)
	binary.LittleEndian.PutUint32(out[24:], flags)
	return out
}

func TestParseBlockHeader(t *testing.T) {
	block := firstBlock(t)
	h, err := ParseBlockHeader(block)
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != int64(len(block)) {
		t.Errorf("size = %d, want %d", h.Size, len(block))
	}
	if h.TotalSamples != 15435 || h.BlockIndex != 0 || h.BlockSamples <= 0 {
		t.Errorf("header = %+v", h)
	}
	if !h.Audio() || h.Channels() != 2 || h.BytesPerSample() != 2 || h.shift() != 0 {
		t.Errorf("shape: audio=%v chans=%d bytes=%d shift=%d",
			h.Audio(), h.Channels(), h.BytesPerSample(), h.shift())
	}
	if !SyncOK(block) || !Match(block) {
		t.Error("a real header fails the sync predicate")
	}
	if h.StreamVersion < MinStreamVersion || h.StreamVersion > MaxStreamVersion {
		t.Errorf("stream version %#x", h.StreamVersion)
	}
}

// TestUnknownTotalSentinel pins the all-ones escape, which is what tells a
// stream whose length the encoder never learned from one that is empty.
func TestUnknownTotalSentinel(t *testing.T) {
	block := append([]byte(nil), firstBlock(t)...)
	binary.LittleEndian.PutUint32(block[12:], 0xffffffff)
	h, err := ParseBlockHeader(block)
	if err != nil {
		t.Fatal(err)
	}
	if h.TotalSamples != -1 {
		t.Errorf("total = %d, want -1 for the unknown escape", h.TotalSamples)
	}
}

func TestSyncOKRejects(t *testing.T) {
	block := firstBlock(t)
	cases := map[string]func([]byte){
		"odd block size":    func(b []byte) { b[4] |= 1 },
		"huge block size":   func(b []byte) { b[6] = 0x10 },
		"stream version 3":  func(b []byte) { binary.LittleEndian.PutUint16(b[8:], 0x0397) },
		"stream version 5":  func(b []byte) { binary.LittleEndian.PutUint16(b[8:], 0x0500) },
		"absurd block size": func(b []byte) { b[22] = 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), block...)
			mutate(b)
			if SyncOK(b) {
				t.Error("the sync predicate accepted it")
			}
		})
	}
	if SyncOK(block[:BlockHeaderLen-1]) {
		t.Error("the sync predicate accepted a short header")
	}
	if Match([]byte("wvp")) || Match(nil) {
		t.Error("Match accepted a short buffer")
	}
}

// TestRefusalsNameTheSituation pins that every shape outside this decoder's
// scope is refused by name. These are all legal WavPack, so the message has to
// say what the file is, not that it is broken.
func TestRefusalsNameTheSituation(t *testing.T) {
	block := firstBlock(t)
	base := binary.LittleEndian.Uint32(block[24:])
	cases := []struct {
		name  string
		block []byte
		want  string
	}{
		{"dsd", withFlags(block, base|flagDSD), "DSD"},
		{"hybrid", withFlags(block, base|flagHybrid), "hybrid"},
		{"float", withFlags(block, base|flagFloatData), "float"},
		{"multichannel", withFlags(block, base&^flagFinalBlock), "channels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ProbeBlock(tc.block)
			if err == nil {
				t.Fatal("accepted an out-of-scope block")
			}
			if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
				t.Errorf("code = %v", waxerr.CodeOf(err))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
	t.Run("stream version", func(t *testing.T) {
		b := append([]byte(nil), block...)
		binary.LittleEndian.PutUint16(b[8:], MinStreamVersion-1)
		_, err := ParseBlockHeader(b)
		if err == nil || !strings.Contains(err.Error(), "stream version") {
			t.Fatalf("pre-4.0 stream version: %v", err)
		}
	})
	t.Run("channel info", func(t *testing.T) {
		// A stereo block that declares six channels in its metadata is the
		// first stream of a multichannel file whose siblings we never reach.
		b := append([]byte(nil), block...)
		b = append(b, byte(idChannelInfo), 1, 6, 0)
		binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
		if _, err := ProbeBlock(b); err == nil || !strings.Contains(err.Error(), "channels") {
			t.Fatalf("six-channel declaration: %v", err)
		}
	})
}

func TestConfigRoundTrip(t *testing.T) {
	for _, cfg := range []Config{
		{Rate: 44100, Channels: 2, BitDepth: 16, ValidBits: 16},
		{Rate: 8000, Channels: 1, BitDepth: 8, ValidBits: 8},
		{Rate: 192000, Channels: 2, BitDepth: 32, ValidBits: 32},
		{Rate: 36000, Channels: 2, BitDepth: 24, ValidBits: 20},
	} {
		blob, err := cfg.MarshalBinary()
		if err != nil {
			t.Fatalf("%+v: %v", cfg, err)
		}
		got, err := ParseConfig(blob)
		if err != nil {
			t.Fatalf("%+v: %v", cfg, err)
		}
		if got != cfg {
			t.Errorf("round trip = %+v, want %+v", got, cfg)
		}
		if f := cfg.Format(); f.Rate != cfg.Rate || f.Channels != cfg.Channels || f.BitDepth != cfg.BitDepth {
			t.Errorf("format = %v for %+v", f, cfg)
		}
	}
	for _, bad := range [][]byte{nil, {1}, {2, 2, 16, 16, 0x44, 0xac, 0, 0}} {
		if _, err := ParseConfig(bad); err == nil {
			t.Errorf("accepted a malformed config blob %v", bad)
		}
	}
	for _, cfg := range []Config{
		{Rate: 0, Channels: 2, BitDepth: 16, ValidBits: 16},
		{Rate: 44100, Channels: 6, BitDepth: 16, ValidBits: 16},
		{Rate: 44100, Channels: 2, BitDepth: 12, ValidBits: 12},
		{Rate: 44100, Channels: 2, BitDepth: 16, ValidBits: 17},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted an invalid config %+v", cfg)
		}
	}
}

// TestDecodeRejectsFormatMismatch pins the wiring check: a decoder built for
// one format must not accept a track claiming another.
func TestDecodeRejectsFormatMismatch(t *testing.T) {
	cfg := Config{Rate: 44100, Channels: 2, BitDepth: 16, ValidBits: 16}
	f := cfg.Format()
	f.Rate = 48000
	if _, err := NewDecoder(cfg, f); err == nil {
		t.Fatal("accepted a track format that disagrees with the stream")
	}
}

// TestDecodeSkipsSampleFreeBlock checks a metadata-only block emits nothing
// rather than an empty buffer.
func TestDecodeSkipsSampleFreeBlock(t *testing.T) {
	block := append([]byte(nil), firstBlock(t)...)
	binary.LittleEndian.PutUint32(block[20:], 0)
	cfg, err := ProbeBlock(firstBlock(t))
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	if err := d.Decode(block, func(*audio.Buffer) error {
		t.Fatal("a block with no samples emitted a buffer")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDecodeCatchesCorruptedBlock checks the header CRC is load-bearing: it
// covers the decoded samples, so damage anywhere between the coded bits and
// the reconstruction is an error rather than silent noise.
func TestDecodeCatchesCorruptedBlock(t *testing.T) {
	block := firstBlock(t)
	cfg, err := ProbeBlock(block)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(t *testing.T, b []byte) error {
		t.Helper()
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		defer d.Release()
		return d.Decode(b, func(*audio.Buffer) error { return nil })
	}
	t.Run("declared CRC", func(t *testing.T) {
		damaged := append([]byte(nil), block...)
		damaged[28] ^= 0x01
		err := decode(t, damaged)
		if err == nil {
			t.Fatal("a block whose CRC disagrees with its samples decoded without error")
		}
		if !strings.Contains(err.Error(), "CRC") {
			t.Errorf("error %q does not name the CRC", err)
		}
	})
	t.Run("coded samples", func(t *testing.T) {
		damaged := append([]byte(nil), block...)
		damaged[len(damaged)/2] ^= 0x40
		if err := decode(t, damaged); err == nil {
			t.Fatal("a block with damaged coded samples decoded without error")
		}
	})
}

// TestExp2sIsTheFunctionItClaims checks the fixed-point exponential against
// the closed form it approximates, 2^(log/256 - 1), rather than against
// numbers copied out of it. Every median and every decorrelation history value
// passes through this table, and a single transcription slip in it decodes as
// plausible-looking noise, so the check has to be independent of the table.
func TestExp2sIsTheFunctionItClaims(t *testing.T) {
	// Stop below 2^31: past there the reference's own shift overflows int32
	// and wraps, which only a crafted stream reaches and which we reproduce.
	for log := 0; log <= 8000; log++ {
		want := math.Exp2(float64(log)/256 - 1)
		got := float64(exp2s(log))
		// The mantissa is a truncated 9-bit value, so the relative error is
		// bounded by one part in 256, plus a whole LSB from the final shift.
		if diff := math.Abs(got - want); diff > want/256+1 {
			t.Fatalf("exp2s(%d) = %v, want about %v", log, got, want)
		}
		if neg := exp2s(-log); neg != -exp2s(log) {
			t.Fatalf("exp2s(%d) = %d, want the negation of exp2s(%d) = %d", -log, neg, log, exp2s(log))
		}
	}
	// The powers of two are exact by construction: their mantissa entry is
	// the table's first, so nothing but the shift applies.
	for log, want := range map[int]int32{2304: 256, 2560: 512, 2816: 1024, 7936: 1 << 30} {
		if got := exp2s(log); got != want {
			t.Errorf("exp2s(%d) = %d, want %d", log, got, want)
		}
	}
}

// TestRestoreWeightRoundsLikeReference pins the asymmetric rounding the stored
// weight form uses; getting it wrong drifts the predictor slowly enough to
// pass a short fixture and fail a real file.
func TestRestoreWeightRoundsLikeReference(t *testing.T) {
	for _, tc := range []struct {
		stored int8
		want   int32
	}{{0, 0}, {1, 8}, {127, 1024}, {-1, -8}, {-128, -1024}, {64, 516}} {
		if got := restoreWeight(tc.stored); got != tc.want {
			t.Errorf("restoreWeight(%d) = %d, want %d", tc.stored, got, tc.want)
		}
	}
}

// TestBlockChecksumMatchesTheReference recomputes a reference encoder's own
// checksum over its own block. Our encoder writing what our own fold computes
// proves nothing about whether libwavpack agrees on the fold or on which bytes
// it covers; this is the only check that does.
func TestBlockChecksumMatchesTheReference(t *testing.T) {
	block := firstBlock(t)
	h, err := ParseBlockHeader(block)
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&flagHasChecksum == 0 {
		t.Fatal("the fixture carries no block checksum, so it can no longer check the fold")
	}
	// Clobbered first, so a function that wrote nothing at all fails here
	// rather than passing on the bytes that were already right.
	ours := append([]byte(nil), block...)
	off, width := checksumOffset(ours)
	if off < 0 {
		t.Fatal("no checksum sub-block in a block whose flags say it has one")
	}
	for i := range width {
		ours[off+i] ^= 0xff
	}
	if !UpdateBlockChecksum(ours) {
		t.Fatal("UpdateBlockChecksum found nothing to rewrite")
	}
	if !bytes.Equal(ours[off:off+width], block[off:off+width]) {
		t.Errorf("recomputed % x, the reference stored % x", ours[off:off+width], block[off:off+width])
	}
}

// TestBlockChecksumCoversTheHeader pins what the muxer depends on: the fold
// runs over the header too, so patching the total-samples field there leaves
// the stored checksum stale until it is recomputed.
func TestBlockChecksumCoversTheHeader(t *testing.T) {
	block := append([]byte(nil), firstBlock(t)...)
	off, width := checksumOffset(block)
	if off < 0 {
		t.Fatal("the fixture carries no block checksum")
	}
	before := append([]byte(nil), block[off:off+width]...)
	block[11] ^= 0xff // the total-samples field, which the muxer rewrites
	UpdateBlockChecksum(block)
	if bytes.Equal(block[off:off+width], before) {
		t.Error("editing the header left the checksum alone; a patched block would go out stale")
	}
}

// TestSetTotalSamplesRoundTrip pins the writer of the total-samples field
// against the header parser that reads it back, at the boundaries where the
// two could disagree, and pins that the checksum covering the field is
// restated with it.
//
// The field is not simply the count's high and low words: the writer skips
// every value whose low word would collide with the unknown-length escape and
// the reader subtracts that skip back off, which is what keeps a length of
// exactly 2^32-1 distinguishable from "unknown". It is also what puts the
// largest writable count 257 short of the forty-bit field's range rather than
// one, since past that the skip carries into a ninth bit of the high byte and
// the byte wraps to zero. MaxSamples is that bound, and a longer stream states
// the escape rather than a wrapped number.
func TestSetTotalSamplesRoundTrip(t *testing.T) {
	block := append([]byte(nil), firstBlock(t)...)
	const escape = int64(0xffffffff)
	for _, n := range []int64{-1, 0, 1, escape - 1, escape, escape + 1, escape + 2,
		2 * escape, 2*escape + 1, MaxSamples - 1, MaxSamples, MaxSamples + 1} {
		if err := SetTotalSamples(block, n); err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		h, err := ParseBlockHeader(block)
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		want := n
		if n > MaxSamples {
			want = -1
		}
		if h.TotalSamples != want {
			t.Errorf("wrote %d, header reads back %d (want %d)", n, h.TotalSamples, want)
		}
		restated := append([]byte(nil), block...)
		if !UpdateBlockChecksum(restated) {
			t.Fatal("the fixture carries no block checksum")
		}
		if !bytes.Equal(restated, block) {
			t.Errorf("%d: the stored checksum does not match the block it was written into", n)
		}
	}
}
