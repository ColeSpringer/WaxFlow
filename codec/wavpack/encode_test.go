package wavpack

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// The encoder is the decoder's mirror, so the gate is the round trip: every
// signal shape, depth, channel count, and level must come back bit-for-bit.
// A single wrong bit anywhere in the chain (a median that moves at the wrong
// moment, a weight quantized on one side only, a history ring left rotated)
// desynchronizes the entropy coder and the samples come back as noise, so
// these are sharp tests rather than approximate ones.

// encodeAll runs samples (planar, per channel) through the encoder and returns
// the blocks it emits.
func encodeAll(t testing.TB, f audio.Format, level int, chans [][]int32, chunk int) [][]byte {
	t.Helper()
	enc, err := NewEncoder(f, &EncoderOptions{Level: level})
	if err != nil {
		t.Fatal(err)
	}
	var blocks [][]byte
	emit := func(p codec.Packet) error {
		blocks = append(blocks, append([]byte(nil), p.Data...))
		return nil
	}
	buf := audio.Get(f, chunk)
	defer audio.Put(buf)
	n := len(chans[0])
	for off := 0; off < n; off += chunk {
		buf.N = min(chunk, n-off)
		for c := range f.Channels {
			copy(buf.ChanI(c), chans[c][off:off+buf.N])
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if trailer.Samples != int64(n) {
		t.Fatalf("trailer says %d samples, encoded %d", trailer.Samples, n)
	}
	return blocks
}

// decodeAll decodes blocks back to planar samples.
func decodeAll(t testing.TB, f audio.Format, blocks [][]byte) [][]int32 {
	t.Helper()
	dec, err := NewDecoder(Config{Rate: f.Rate, Channels: f.Channels, BitDepth: f.BitDepth, ValidBits: f.BitDepth}, f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	out := make([][]int32, f.Channels)
	emit := func(b *audio.Buffer) error {
		for c := range f.Channels {
			out[c] = append(out[c], b.ChanI(c)[:b.N]...)
		}
		return nil
	}
	for _, blk := range blocks {
		if err := dec.Decode(blk, emit); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out
}

// roundTrip encodes and decodes, failing on the first sample that differs.
func roundTrip(t testing.TB, f audio.Format, level int, chans [][]int32, chunk int) {
	t.Helper()
	got := decodeAll(t, f, encodeAll(t, f, level, chans, chunk))
	for c := range f.Channels {
		if len(got[c]) != len(chans[c]) {
			t.Fatalf("channel %d: decoded %d samples, encoded %d", c, len(got[c]), len(chans[c]))
		}
		for i := range chans[c] {
			if got[c][i] != chans[c][i] {
				t.Fatalf("channel %d sample %d: got %d, want %d", c, i, got[c][i], chans[c][i])
			}
		}
	}
}

func fmtOf(rate, channels, depth int) audio.Format {
	return audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels),
		Type: audio.Int, BitDepth: depth}
}

// signals builds one channel of test material of the named shape, scaled to
// the depth. Each shape stresses a different part of the coder: silence is the
// zero-run path, noise the escape codes, the sine the predictor, and the ramp
// the two-tap terms.
//
// Every shape but silence varies with the seed, which is what makes a
// two-channel fixture a stereo one. The deterministic shapes used to ignore
// it, so both channels came out identical and every block of them took the
// false-stereo path: the stereo cascades, the joint-stereo matrix, and the
// cross-channel terms went unmeasured in the tests that exist to measure them.
// Silence is the exception on purpose, since it is the false-stereo case.
func signal(kind string, n, depth int, seed uint64) []int32 {
	full := int32(1)<<(depth-1) - 1
	out := make([]int32, n)
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	// A per-seed offset for the shapes that are otherwise a fixed sequence.
	// Small enough to leave each shape recognizable as itself.
	skew := int(seed) * 7
	switch kind {
	case "silence":
	case "noise":
		for i := range out {
			out[i] = int32(r.Int64N(int64(full)*2+1)) - full
		}
	case "quiet-noise":
		for i := range out {
			out[i] = int32(r.Int64N(9)) - 4
		}
	case "sine":
		for i := range out {
			ph := 2 * math.Pi * 440 * float64(i+skew) / 44100
			out[i] = int32(float64(full) * 0.7 * math.Sin(ph))
		}
	case "ramp":
		for i := range out {
			out[i] = int32(int64(i+skew)%(int64(full)+1)) - full/2
		}
	case "rails":
		// Alternating full-scale values: the widest magnitudes the depth can
		// hold, which is where the coder's escape paths and the extension
		// stream live. The seed picks the polarity, so a stereo pair is the
		// hardest stereo case rather than the same channel twice.
		for i := range out {
			if (i+int(seed))%2 == 0 {
				out[i] = -full - 1
			} else {
				out[i] = full
			}
		}
	case "bursts":
		// Silence broken by loud runs: the zero-run coder has to start and
		// stop, and the medians collapse and climb repeatedly.
		for i := range out {
			if i/97%3 == 0 {
				out[i] = int32(r.Int64N(int64(full))) - full/2
			}
		}
	case "shifted":
		// A narrow source in a wide container: every sample shares low zero
		// bits, which the block's shift field strips.
		lo := depth / 2
		for i := range out {
			out[i] = (int32(r.Int64N(1<<uint(lo))) - 1<<uint(lo-1)) << uint(depth-lo)
		}
	}
	return out
}

func TestEncodeRoundTrip(t *testing.T) {
	kinds := []string{"silence", "noise", "quiet-noise", "sine", "ramp", "rails", "bursts", "shifted"}
	for _, depth := range []int{8, 16, 24, 32} {
		for _, channels := range []int{1, 2} {
			for _, level := range []int{LevelFast, LevelNormal, LevelHigh, LevelVeryHigh} {
				for _, kind := range kinds {
					name := fmt.Sprintf("%s/%dch/%dbit/l%d", kind, channels, depth, level)
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						f := fmtOf(44100, channels, depth)
						chans := make([][]int32, channels)
						for c := range channels {
							chans[c] = signal(kind, 5000, depth, uint64(c)+1)
						}
						roundTrip(t, f, level, chans, 2048)
					})
				}
			}
		}
	}
}

// TestEncodeFalseStereo pins the dual-mono path: identical channels are coded
// once and duplicated on the way back, and the block says so.
func TestEncodeFalseStereo(t *testing.T) {
	f := fmtOf(44100, 2, 16)
	mono := signal("sine", 4000, 16, 1)
	chans := [][]int32{mono, append([]int32(nil), mono...)}
	blocks := encodeAll(t, f, LevelNormal, chans, 2048)
	h, err := ParseBlockHeader(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&flagFalseStereo == 0 {
		t.Error("identical channels did not produce a false-stereo block")
	}
	if h.Channels() != 2 || !h.Mono() {
		t.Errorf("false-stereo block: channels=%d mono=%v", h.Channels(), h.Mono())
	}
	got := decodeAll(t, f, blocks)
	for c := range 2 {
		for i := range mono {
			if got[c][i] != mono[i] {
				t.Fatalf("channel %d sample %d: got %d, want %d", c, i, got[c][i], mono[i])
			}
		}
	}
}

// TestEncodeJointStereo pins the fourth per-block optimization, the one the
// other three had a flag assertion for and this did not: the mid/side matrix.
// It shipped disabled on every block a stream ever contained, because the
// decision compared summed magnitudes and a magnitude sum is blind to the
// channel the matrix collapses -- for R = -L the two sums are equal to within
// a rounding error and the strict test never fired. Nothing noticed, because
// round-trip, ffmpeg and `wvunpack -v` all pass on a stream that simply never
// takes the optimization. Coded length is a sum of logs, so the decision is
// made in logs.
func TestEncodeJointStereo(t *testing.T) {
	f := fmtOf(44100, 2, 16)
	const n = 8192
	left := signal("sine", n, 16, 1)
	right := make([]int32, n)
	for i, v := range left {
		right[i] = -v // the case a magnitude sum cannot see
	}
	chans := [][]int32{left, right}
	blocks := encodeAll(t, f, LevelNormal, chans, n)
	h, err := ParseBlockHeader(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&flagJointStereo == 0 {
		t.Error("out-of-phase channels did not engage the joint-stereo matrix")
	}
	roundTrip(t, f, LevelNormal, chans, n)

	// And it has to pay: the side channel is identically zero here, so
	// refusing the matrix costs about a bit a sample.
	joint := 0
	for _, b := range blocks {
		joint += len(b)
	}
	if plain := len(packWithoutJoint(t, f, chans, n)); joint >= plain {
		t.Errorf("joint stereo took %d bytes, plain took %d; the matrix bought nothing", joint, plain)
	}
}

// packWithoutJoint encodes chans with the matrix suppressed, for the size
// comparison above. It reaches past the public API on purpose: the decision is
// internal and a test that cannot see both sides of it is the test that missed
// this bug in the first place.
func packWithoutJoint(t *testing.T, f audio.Format, chans [][]int32, chunk int) []byte {
	t.Helper()
	forceNoJoint = true
	defer func() { forceNoJoint = false }()
	out := []byte(nil)
	for _, b := range encodeAll(t, f, LevelNormal, chans, chunk) {
		out = append(out, b...)
	}
	return out
}

// TestEncodeLevelsOrdered pins the compression ladder across shapes and
// depths, not one signal at one depth: the levels nest, so a deeper level runs
// the shallower one's candidates and must not come out larger. It used to,
// by 4% on a sustained tone, because candidates were ranked by a proxy for
// coded length rather than by coded length. Exact ties are fine (a deeper
// level often picks the same chain); growth is not.
func TestEncodeLevelsOrdered(t *testing.T) {
	for _, kind := range []string{"sine", "ramp", "bursts", "noise", "quiet-noise"} {
		for _, depth := range []int{8, 16, 24, 32} {
			t.Run(fmt.Sprintf("%s/%dbit", kind, depth), func(t *testing.T) {
				t.Parallel()
				f := fmtOf(44100, 2, depth)
				chans := [][]int32{signal(kind, 30000, depth, 1), signal(kind, 30000, depth, 2)}
				prev, prevLevel := 0, 0
				for _, level := range []int{LevelFast, LevelNormal, LevelHigh, LevelVeryHigh} {
					total := 0
					for _, blk := range encodeAll(t, f, level, chans, BlockSamples) {
						total += len(blk)
					}
					if prev != 0 && total > prev {
						t.Errorf("level %d is %d bytes, level %d was %d (+%.2f%%)",
							level, total, prevLevel, prev, 100*float64(total-prev)/float64(prev))
					}
					prev, prevLevel = total, level
				}
			})
		}
	}
}

// TestEncodeSilentBlockKeepsState pins what a digitally silent block costs the
// blocks after it: nothing. Silence is dual-mono, so it takes the false-stereo
// path, and the two coding modes carry incompatible cascades. Rebuilding one
// set on every crossing made a single silent block cost the four blocks after
// it 4% -- and leading silence, trailing silence and inter-track gaps are
// ordinary things for a stream to contain. Holding one set per mode makes the
// crossing free.
func TestEncodeSilentBlockKeepsState(t *testing.T) {
	f := fmtOf(44100, 2, 16)
	const n = BlockSamples
	chans := [][]int32{signal("bursts", n*5, 16, 1), signal("bursts", n*5, 16, 2)}
	gapped := make([][]int32, 2)
	for c := range chans {
		gapped[c] = append(append(append([]int32{}, chans[c][:n*2]...), make([]int32, n)...), chans[c][n*2:]...)
	}
	plain := encodeAll(t, f, LevelNormal, chans, n)
	withGap := encodeAll(t, f, LevelNormal, gapped, n)
	if len(withGap) != len(plain)+1 {
		t.Fatalf("gapped stream has %d blocks, want %d", len(withGap), len(plain)+1)
	}
	// Block index 2 is the silent one; every block after it has to cost
	// exactly what it cost without the gap. Not byte for byte: the header
	// carries the block's sample position, which the gap moved by a block.
	for i := 3; i < len(withGap); i++ {
		if len(withGap[i]) != len(plain[i-1]) {
			t.Errorf("block %d after the silent one is %d bytes, want %d (+%d)",
				i, len(withGap[i]), len(plain[i-1]), len(withGap[i])-len(plain[i-1]))
		}
	}
	roundTrip(t, f, LevelNormal, gapped, n)
}

// TestEncodeModeSwitch mixes dual-mono and true-stereo blocks in one stream:
// the two coding modes carry different cascade shapes, so the encoder has to
// hand the decoder a consistent state at every boundary.
func TestEncodeModeSwitch(t *testing.T) {
	f := fmtOf(44100, 2, 16)
	const n = 8192
	left := signal("sine", n, 16, 1)
	right := append([]int32(nil), left...)
	// The middle third differs, so the stream runs false-stereo, then true
	// stereo, then false-stereo again.
	noise := signal("noise", n/3, 16, 7)
	copy(right[n/3:], noise)
	roundTrip(t, f, LevelNormal, [][]int32{left, right}, 1024)
}

// TestEncodeShiftAndExtension pins the two block fields that reshape samples
// before the coder sees them: the shift for a source narrower than its
// container, and the extension stream for one wider than the coder's range.
func TestEncodeShiftAndExtension(t *testing.T) {
	t.Run("shift", func(t *testing.T) {
		f := fmtOf(44100, 2, 24)
		chans := [][]int32{signal("shifted", 3000, 24, 1), signal("shifted", 3000, 24, 2)}
		blocks := encodeAll(t, f, LevelNormal, chans, 2048)
		h, err := ParseBlockHeader(blocks[0])
		if err != nil {
			t.Fatal(err)
		}
		if h.shift() == 0 {
			t.Error("a source with redundant low bits produced no shift")
		}
		roundTrip(t, f, LevelNormal, chans, 2048)
	})
	t.Run("extension", func(t *testing.T) {
		f := fmtOf(48000, 2, 32)
		chans := [][]int32{signal("noise", 3000, 32, 1), signal("noise", 3000, 32, 2)}
		blocks := encodeAll(t, f, LevelNormal, chans, 2048)
		h, err := ParseBlockHeader(blocks[0])
		if err != nil {
			t.Fatal(err)
		}
		if h.Flags&flagInt32Data == 0 {
			t.Error("full-scale 32-bit samples did not engage the extension stream")
		}
		roundTrip(t, f, LevelNormal, chans, 2048)
	})
}

// TestEncodeBlockShape pins what a block says about itself: the demuxer walks
// by declared length, seeks by declared index, and probes the first block for
// the whole stream's shape, so all three have to be right in the encoder's
// own output.
func TestEncodeBlockShape(t *testing.T) {
	f := fmtOf(37000, 2, 16) // a rate outside the header's table
	chans := [][]int32{signal("noise", 5000, 16, 1), signal("sine", 5000, 16, 2)}
	blocks := encodeAll(t, f, LevelNormal, chans, 2048)
	if len(blocks) != 3 {
		t.Fatalf("%d blocks for 5000 samples in 2048-sample chunks, want 3", len(blocks))
	}
	pos := int64(0)
	for i, blk := range blocks {
		h, err := ParseBlockHeader(blk)
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		switch {
		case !SyncOK(blk):
			t.Errorf("block %d fails the sync predicate a boundary scan applies", i)
		case h.Size != int64(len(blk)):
			t.Errorf("block %d declares %d bytes, wrote %d", i, h.Size, len(blk))
		case h.BlockIndex != pos:
			t.Errorf("block %d index = %d, want %d", i, h.BlockIndex, pos)
		case h.TotalSamples != -1:
			t.Errorf("block %d states a total (%d); the muxer owns that field", i, h.TotalSamples)
		}
		pos += int64(h.BlockSamples)
	}
	if pos != 5000 {
		t.Errorf("blocks cover %d samples, want 5000", pos)
	}
	cfg, err := ProbeBlock(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := (Config{Rate: 37000, Channels: 2, BitDepth: 16, ValidBits: 16}); cfg != want {
		t.Errorf("probe = %+v, want %+v", cfg, want)
	}
}

// TestEncodeCompresses is the sanity floor: a predictable signal must come out
// smaller than raw. A cascade wired backward still round-trips (the decoder
// undoes whatever the encoder did) but stops predicting, so size is the check
// that catches it.
//
// The two channels are a real stereo pair, out of phase with each other, so
// the stereo cascades, the joint-stereo matrix and the cross-channel terms are
// all in the measurement. They used to be the same channel twice, which every
// block coded as false stereo, and the mono cascade was the only thing this
// ever sized.
//
// A third of raw is a floor with room in it rather than a fit to the current
// number: the working encoder lands at 27% of raw, a chain of one useless term
// at 87%, and no decorrelation at all at 94%.
func TestEncodeCompresses(t *testing.T) {
	f := fmtOf(44100, 2, 16)
	chans := [][]int32{signal("sine", 44100, 16, 1), signal("sine", 44100, 16, 2)}
	if slices.Equal(chans[0], chans[1]) {
		t.Fatal("the two channels are identical, so this measures false stereo, not stereo")
	}
	for _, level := range []int{LevelFast, LevelNormal, LevelHigh, LevelVeryHigh} {
		total := 0
		for _, blk := range encodeAll(t, f, level, chans, BlockSamples) {
			total += len(blk)
		}
		raw := 44100 * 2 * 2
		if total > raw/3 {
			t.Errorf("level %d: %d bytes for %d raw, want under a third", level, total, raw)
		}
	}
}

// TestEncodeWideSampleRate pins the sample-rate sub-block's width. Rates
// outside the header's table ride in a sub-block that the encoder wrote three
// bytes wide unconditionally, so any rate at or above 2^24 was silently
// truncated on the way in: nothing bounded the rate either, so an ordinary WAV
// claiming 16821316 Hz encoded, reported success, and read back as 44100 --
// the truncation of exactly that number. A rate of exactly 2^24 truncated to
// zero, which our own demuxer then refused. The reference writes the wider
// form for the same range, and the parser already read it.
func TestEncodeWideSampleRate(t *testing.T) {
	for _, rate := range []int{37000, 1<<24 - 1, 1 << 24, 16821316, MaxRate} {
		f := fmtOf(rate, 2, 16)
		chans := [][]int32{signal("sine", 3000, 16, 1), signal("sine", 3000, 16, 2)}
		blocks := encodeAll(t, f, LevelNormal, chans, 2048)
		cfg, err := ProbeBlock(blocks[0])
		if err != nil {
			t.Fatalf("%d Hz: %v", rate, err)
		}
		if cfg.Rate != rate {
			t.Errorf("%d Hz encodes to a block that reads back as %d Hz", rate, cfg.Rate)
		}
		roundTrip(t, f, LevelNormal, chans, 2048)
	}
	// And the bound is stated rather than left to overflow the field.
	if _, err := NewEncoder(fmtOf(MaxRate+1, 2, 16), nil); err == nil {
		t.Errorf("a rate above MaxRate (%d) was accepted", MaxRate)
	}
}

func TestEncoderRejects(t *testing.T) {
	good := fmtOf(44100, 2, 16)
	cases := map[string]struct {
		f     audio.Format
		level int
	}{
		"level below the range": {good, LevelFast - 1},
		"level above the range": {good, LevelVeryHigh + 1},
		"float input":           {audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}, LevelNormal},
		"odd bit depth":         {fmtOf(44100, 2, 20), LevelNormal},
		"too many channels":     {fmtOf(44100, 3, 16), LevelNormal},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewEncoder(tc.f, &EncoderOptions{Level: tc.level}); err == nil {
				t.Fatal("NewEncoder accepted it")
			}
		})
	}
}

func TestEncoderMisuse(t *testing.T) {
	f := fmtOf(44100, 2, 16)
	enc, err := NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	drop := func(codec.Packet) error { return nil }
	buf := audio.Get(f, BlockSamples)
	defer audio.Put(buf)
	buf.N = 128
	if err := enc.Encode(buf, drop); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Finish(drop); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(buf, drop); err == nil {
		t.Error("Encode after Finish succeeded")
	}
	if _, err := enc.Finish(drop); err == nil {
		t.Error("second Finish succeeded")
	}
}

// TestStoreWeightRoundTrip pins the weight quantizer as idempotent: the
// encoder adopts the value the block will decode to, so a second pass through
// the pair must not move it again.
func TestStoreWeightRoundTrip(t *testing.T) {
	for w := int32(-2000); w <= 2000; w++ {
		got := restoreWeight(storeWeight(w))
		if again := restoreWeight(storeWeight(got)); again != got {
			t.Fatalf("weight %d quantizes to %d then %d", w, got, again)
		}
		if got > 1024 || got < -1024 {
			t.Fatalf("weight %d quantizes outside the range: %d", w, got)
		}
	}
}

// TestLog2RoundTrip pins the log quantizer the same way, over the range the
// decorrelation history and the entropy medians actually reach.
func TestLog2RoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 2, 3, 255, 256, 257, 1 << 15, 1<<23 - 1, 1 << 23, 1<<31 - 1,
		-1, -2, -255, -1 << 15, -(1 << 23), -(1<<31 - 1), -1 << 31} {
		q, log := quantizeSample(v)
		if log < math.MinInt16 || log > math.MaxInt16 {
			t.Fatalf("value %d stores log %d, outside the 16-bit field", v, log)
		}
		if again, _ := quantizeSample(q); again != q {
			t.Fatalf("value %d quantizes to %d then %d", v, q, again)
		}
		if (v < 0) != (q < 0) && q != 0 {
			t.Fatalf("value %d quantizes to %d, flipping sign", v, q)
		}
		// exp2s is what the decoder applies to the stored field; the two
		// have to agree on the number.
		if exp2s(log) != q {
			t.Fatalf("value %d: stored log %d decodes to %d, encoder holds %d", v, log, exp2s(log), q)
		}
	}
	for m := range uint32(1 << 20) {
		log := log2u(m)
		if got := uint32(exp2s(log)); got > m || (m > 0 && got == 0) {
			t.Fatalf("median %d stores log %d, decoding to %d", m, log, got)
		}
	}
}

// TestLog2InvIsTheTable pins log2u's lookup table against the search it
// replaced. The table is derived from exp2Table at init rather than
// hand-written, so this is not a second copy to keep in step; it is the proof
// that the derivation is the search, over every input the search could take.
// The table exists because the search was the encoder's hottest loop.
func TestLog2InvIsTheTable(t *testing.T) {
	for m := range 256 {
		lo, hi := 0, 255
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if int(exp2Table[mid]) <= m {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if got := int(log2Inv[m]); got != lo {
			t.Errorf("mantissa %d: table says %d, search says %d", m, got, lo)
		}
	}
}

// TestBitWriterRoundTrip pins the writer against the reader field by field,
// including the two shapes that are easy to get backward: readCode's
// short/long split and readElias's implicit leading one.
func TestBitWriterRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	var bw bitWriter
	bw.reset(nil)
	type field struct{ kind, a, b uint32 }
	var fields []field
	for range 20000 {
		switch r.IntN(4) {
		case 0:
			v := uint32(r.Uint64() >> uint(r.IntN(32)))
			fields = append(fields, field{0, v, 0})
			bw.putElias(v)
		case 1:
			maxcode := uint32(r.Uint64() >> uint(32+r.IntN(24)))
			code := uint32(r.Uint64N(uint64(maxcode) + 1))
			fields = append(fields, field{1, code, maxcode})
			bw.putCode(code, maxcode)
		case 2:
			n := r.IntN(33)
			v := uint32(r.Uint64())
			if n < 32 {
				v &= 1<<uint(n) - 1
			}
			fields = append(fields, field{2, v, uint32(n)})
			bw.putBits(v, n)
		default:
			v := uint32(r.IntN(2))
			fields = append(fields, field{3, v, 0})
			bw.putBit(v)
		}
	}
	data := bw.flush()
	if len(data)&1 != 0 {
		t.Fatalf("payload of %d bytes is not a whole number of words", len(data))
	}
	var br bitReader
	br.reset(data)
	for i, f := range fields {
		var got uint32
		switch f.kind {
		case 0:
			v, ok := br.readElias()
			if !ok {
				t.Fatalf("field %d: readElias gave up", i)
			}
			got = v
		case 1:
			got = br.readCode(f.b)
		case 2:
			got = br.getBits(int(f.b))
		default:
			got = br.getBit()
		}
		if got != f.a {
			t.Fatalf("field %d (kind %d, arg %d): read %d, wrote %d", i, f.kind, f.b, got, f.a)
		}
	}
	if br.over {
		t.Error("the reader ran past the end of what the writer produced")
	}
}

// TestEncodeMetadataFraming pins the sub-block framing the decoder's walk
// reads: word counts, the odd-size flag, and the large form.
func TestEncodeMetadataFraming(t *testing.T) {
	payloads := [][]byte{{}, {1}, {1, 2}, {1, 2, 3}, make([]byte, 510), make([]byte, 511), make([]byte, 4096)}
	var block []byte
	block = append(block, make([]byte, BlockHeaderLen)...)
	for i, p := range payloads {
		for j := range p {
			p[j] = byte(i + j)
		}
		block = appendMeta(block, byte(0x10+i), p)
	}
	seen := 0
	err := walkMeta(block, func(id byte, data []byte) error {
		if int(id) != 0x10+seen {
			t.Fatalf("sub-block %d has id %#x", seen, id)
		}
		want := payloads[seen]
		if len(data) != len(want) {
			t.Fatalf("sub-block %d: %d bytes, wrote %d", seen, len(data), len(want))
		}
		for j := range want {
			if data[j] != want[j] {
				t.Fatalf("sub-block %d byte %d: %d, wrote %d", seen, j, data[j], want[j])
			}
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(payloads) {
		t.Fatalf("walked %d sub-blocks, wrote %d", seen, len(payloads))
	}
}

// TestEncodeTotalSamplesPatch pins the field the muxer back-patches: the
// encoder leaves the escape, and the escape's own encoding round-trips.
func TestEncodeTotalSamplesPatch(t *testing.T) {
	f := fmtOf(44100, 1, 16)
	blocks := encodeAll(t, f, LevelFast, [][]int32{signal("sine", 1000, 16, 1)}, 1000)
	blk := append([]byte(nil), blocks[0]...)
	binary.LittleEndian.PutUint32(blk[12:], 1000)
	h, err := ParseBlockHeader(blk)
	if err != nil {
		t.Fatal(err)
	}
	if h.TotalSamples != 1000 {
		t.Errorf("patched total = %d, want 1000", h.TotalSamples)
	}
}
