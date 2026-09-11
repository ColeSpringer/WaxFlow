package wmalossless_test

// The frame layouts no encoder writes, built by hand.
//
// Windows' encoder writes one decode-flags word and one tiling shape, so the
// committed corpus reaches a single path through the tile header. Everything
// else in it is structural, and structural code that nothing executes is code
// that is wrong until someone tries it. These cells are the try.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
)

// rawPCMFrame writes one frame whose single subframe is a raw PCM tile
// carrying vals per channel, plus the trailer that ends the packet's frames.
// Raw PCM needs no filter state, so this is the shortest frame that decodes
// from a cold decoder and the cleanest way to test a layout rather than a
// filter.
func rawPCMFrame(cfg wmalossless.Config, w *bitWriter, vals [][]int32) {
	w.put(0, 1) // tile aligned: read whatever the subframe depth
	if cfg.MaxSubframes() > 1 {
		for i := 0; i < cfg.Channels; i++ {
			w.put(1, 1) // every channel takes the subframe
		}
		// The ratio field's width is floorLog2(maxSubframes-1)+1, so it is one
		// bit at depth 1 and five at depth 5. Writing four everywhere is what
		// a corpus of depth-4 streams would let a reader get away with.
		w.put(uint32(cfg.MaxSubframes()-1), ratioBits(cfg.MaxSubframes()))
	}
	w.put(0, 8) // dynamic range gain, discarded
	w.put(0, 1) // no skip fields
	w.put(0, 1) // not a seekable tile
	w.put(1, 1) // raw PCM
	w.put(0, 1) // no padding
	for c := 0; c < cfg.Channels; c++ {
		for _, v := range vals[c] {
			w.put(uint32(v)&(1<<uint(cfg.BitsPerSample)-1), cfg.BitsPerSample)
		}
	}
	w.put(0, 1) // trailer: no more frames begin here
}

// ratioBits is the width of the subframe-length ratio field.
func ratioBits(maxSubframes int) int {
	n := 0
	for v := maxSubframes - 1; v > 0; v >>= 1 {
		n++
	}
	if n == 0 {
		return 1
	}
	return n
}

// TestTileHeaderAtEverySubframeDepth is the cell that catches a tile-header
// field read conditionally.
//
// The tile-aligned bit is in the header at every subframe depth; only its
// meaning is subsumed when the depth allows one subframe. A decoder that folds
// the read into the condition skips it at depth 0 and is then one bit out of
// phase for the whole frame, which shows up as a refusal from somewhere far
// away rather than as a layout error. Every corpus cell carries depth 4, so
// only depth 0 reaches it.
func TestTileHeaderAtEverySubframeDepth(t *testing.T) {
	for _, depth := range []int{0, 1, 2, 3, 4, 5} {
		flags := uint16(0x01a1&^0x38) | uint16(depth)<<3
		raw := config(16000, 2, 16, 0x03, 4096, flags)
		cfg, err := wmalossless.ParseConfig(raw)
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		if cfg.MaxSubframes() != 1<<depth {
			t.Fatalf("depth %d: MaxSubframes %d", depth, cfg.MaxSubframes())
		}
		// A recognizable ramp per channel, so a bit-phase error cannot come
		// back looking plausible.
		vals := make([][]int32, cfg.Channels)
		for c := range vals {
			vals[c] = make([]int32, cfg.SamplesPerFrame())
			for i := range vals[c] {
				vals[c][i] = int32(i%251) - 125 + int32(c)*1000
			}
		}
		var w bitWriter
		rawPCMFrame(cfg, &w, vals)

		dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		var got []int32
		emit := func(b *audio.Buffer) error {
			got = append(got, interleave(b)...)
			return nil
		}
		if err := dec.Decode(packet(t, cfg, 0, &w), emit); err != nil {
			dec.Release()
			t.Fatalf("depth %d: %v", depth, err)
		}
		if err := dec.Drain(emit); err != nil {
			dec.Release()
			t.Fatalf("depth %d drain: %v", depth, err)
		}
		dec.Release()
		if len(got) != cfg.SamplesPerFrame()*cfg.Channels {
			t.Fatalf("depth %d: decoded %d samples, want %d",
				depth, len(got), cfg.SamplesPerFrame()*cfg.Channels)
		}
		for i := range got {
			frame, ch := i/cfg.Channels, i%cfg.Channels
			if got[i] != vals[ch][frame] {
				t.Fatalf("depth %d: frame %d channel %d = %d, want %d",
					depth, frame, ch, got[i], vals[ch][frame])
			}
		}
	}
}

// TestSeekableTileClearsTheGolombState pins the one piece of filter state a
// seekable tile clears that no coded channel would notice.
//
// The moving-average accumulator is re-seeded per CODED channel, so a channel
// that is silent across a seekable tile is the only one whose accumulator the
// clear is load-bearing for. Coded again later in a non-seekable subframe, it
// would set its Golomb parameter from an accumulator belonging to samples
// before the tile and read the wrong number of remainder bits per sample.
//
// Nothing in the corpus reaches this: every cell codes both channels at every
// seekable tile, or leaves one uncoded for its whole length. The assertion is
// on the state rather than on decoded samples because reaching it from the
// bitstream needs an encoder, and the invariant is what the notes state.
func TestSeekableTileClearsTheGolombState(t *testing.T) {
	cfg, err := wmalossless.ParseConfig(stereoConfig())
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()

	// Poison every channel's accumulator, then run one seekable tile whose
	// coded flags are both clear so nothing re-seeds them.
	wmalossless.PoisonGolomb(dec, 0xDEADBEEF, 3)
	var w bitWriter
	w.put(0, 1) // tile aligned
	w.put(3, 2) // both channels take
	w.put(15, 4)
	w.put(0, 8) // gain
	w.put(0, 1) // no skips
	w.put(1, 1) // seekable tile
	w.put(0, 4) // no arithmetic coding, no AC filter, no decorrelation, no MCLMS
	cdlmsDef(&w, cfg.Channels)
	w.put(5, 3) // moving-average scaling
	w.put(0, 8) // quantisation step 1
	w.put(0, 1) // not raw PCM
	w.put(0, 2) // NEITHER channel coded, so nothing re-seeds
	w.put(0, 1) // no LPC
	w.put(0, 1) // no padding
	w.put(0, 1) // trailer

	if err := dec.Decode(packet(t, cfg, 0, &w), func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := dec.Drain(func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for c := 0; c < cfg.Channels; c++ {
		sum, scaling := wmalossless.GolombState(dec, c)
		if sum != 0 {
			t.Errorf("channel %d accumulator = %d after a seekable tile, want 0", c, sum)
		}
		if scaling != 5 {
			t.Errorf("channel %d scaling = %d, want the tile's 5", c, scaling)
		}
	}
}

// TestMultipleSubframesInAFrame walks the tiling rather than stepping over it.
//
// Every corpus cell carries one subframe spanning the whole frame, so the walk
// that splits a frame runs exactly one iteration and both of its shortcuts are
// dead: the length ratio is read once, and the branch that makes the LAST
// subframe take its length from the minimum without reading a ratio at all is
// never taken. A decoder that read a ratio there would desynchronise on any
// stream that tiles, and nothing in the corpus could say so.
//
// Four raw PCM subframes of the minimum length, each carrying its own
// recognizable values, so the offsets are checked and not just the count.
func TestMultipleSubframesInAFrame(t *testing.T) {
	flags := uint16(0x01a1&^0x38) | 2<<3 // subframe depth 2, so four at 128
	cfg, err := wmalossless.ParseConfig(config(16000, 2, 16, 0x03, 4096, flags))
	if err != nil {
		t.Fatal(err)
	}
	sub := cfg.MinSubframeLen()
	n := cfg.SamplesPerFrame() / sub
	if n != 4 {
		t.Fatalf("%d subframes of %d in a %d-sample frame", n, sub, cfg.SamplesPerFrame())
	}
	want := make([][]int32, cfg.Channels)
	for c := range want {
		want[c] = make([]int32, cfg.SamplesPerFrame())
	}

	var w bitWriter
	w.put(0, 1) // tile aligned
	last := cfg.SamplesPerFrame() - sub
	for at := 0; at < cfg.SamplesPerFrame(); at += sub {
		if at != last {
			// Every channel takes it, then the length as a ratio of the
			// minimum. At the last subframe both fields are absent.
			for i := 0; i < cfg.Channels; i++ {
				w.put(1, 1)
			}
			w.put(0, ratioBits(cfg.MaxSubframes())) // ratio 0, so one minimum
		}
	}
	w.put(0, 8) // gain
	w.put(0, 1) // no skips
	for s := 0; s < n; s++ {
		w.put(0, 1) // not seekable
		w.put(1, 1) // raw PCM, so no filter state is needed
		w.put(0, 1) // no padding
		for c := 0; c < cfg.Channels; c++ {
			for i := 0; i < sub; i++ {
				v := int32(s*1000 + i - 500 + c*7)
				want[c][s*sub+i] = v
				w.put(uint32(v)&0xFFFF, 16)
			}
		}
	}
	w.put(0, 1) // trailer

	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	var got []int32
	emit := func(b *audio.Buffer) error {
		got = append(got, interleave(b)...)
		return nil
	}
	if err := dec.Decode(packet(t, cfg, 0, &w), emit); err != nil {
		t.Fatal(err)
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != cfg.SamplesPerFrame()*cfg.Channels {
		t.Fatalf("decoded %d samples, want %d", len(got), cfg.SamplesPerFrame()*cfg.Channels)
	}
	for i := range got {
		frame, ch := i/cfg.Channels, i%cfg.Channels
		if got[i] != want[ch][frame] {
			t.Fatalf("frame %d (subframe %d) channel %d = %d, want %d",
				frame, frame/sub, ch, got[i], want[ch][frame])
		}
	}
}

// filterSpec is one CDLMS filter of a hand-built cascade.
type filterSpec struct{ order, scaling int }

// cascadeStream writes one seekable frame whose channels carry the same small
// residuals through the given cascade. The residuals are pure unary runs: a
// transmitted mean of zero and a moving-average scaling of seven keep the
// Golomb parameter at zero or one for values this small, so no remainder bits
// are needed and the bitstream is the same whatever the filters are.
func cascadeStream(t *testing.T, cfg wmalossless.Config, filters []filterSpec) []int32 {
	t.Helper()
	var w bitWriter
	w.put(0, 1) // tile aligned
	for i := 0; i < cfg.Channels; i++ {
		w.put(1, 1)
	}
	w.put(uint32(cfg.MaxSubframes()-1), ratioBits(cfg.MaxSubframes()))
	w.put(0, 8) // gain
	w.put(0, 1) // no skips
	w.put(1, 1) // seekable tile
	w.put(0, 4) // no arithmetic coding, no AC filter, no decorrelation, no MCLMS
	w.put(0, 1) // no transmitted coefficients
	for c := 0; c < cfg.Channels; c++ {
		w.put(uint32(len(filters)-1), 3)
		for _, f := range filters {
			w.put(uint32(f.order/8-1), 7)
		}
		for _, f := range filters {
			w.put(uint32(f.scaling), 4)
		}
	}
	w.put(7, 3) // moving-average scaling
	w.put(0, 8) // quantisation step 1
	w.put(0, 1) // not raw PCM
	w.put(1<<uint(cfg.Channels)-1, cfg.Channels)
	w.put(0, 1) // no LPC
	w.put(0, 1) // no padding
	for c := 0; c < cfg.Channels; c++ {
		w.put(0, 1)                 // no transient
		w.put(0, cfg.BitsPerSample) // transmitted mean: zero
		w.put(0, cfg.BitsPerSample) // first sample, a literal
		for i := 1; i < cfg.SamplesPerFrame(); i++ {
			for q := 0; q < (i+c)%3; q++ {
				w.put(1, 1)
			}
			w.put(0, 1)
		}
	}
	w.put(0, 1) // trailer

	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	var got []int32
	emit := func(b *audio.Buffer) error {
		got = append(got, interleave(b)...)
		return nil
	}
	if err := dec.Decode(packet(t, cfg, 0, &w), emit); err != nil {
		t.Fatal(err)
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCDLMSCascadeIsApplied: every seekable tile any encoder writes declares
// exactly one filter per channel, so the cascade is a loop that runs once and
// a decoder that ignored every filter past the first, or ran them in the wrong
// order, would pass the whole corpus.
//
// Three things are asserted: a declared second filter changes the output, the
// order it is declared in changes it, and the two-filter decode matches a
// digest. Only the digest catches the cascade being run the other way round,
// because reversing the loop and swapping the declarations are the same
// change seen from either end.
//
// The digest pins THIS decoder's arithmetic, not the format's: no stream
// exists that declares more than one filter, so nothing says which order is
// right and docs/quality-gates.md carries that as a named deficit. What it
// does buy is that the order cannot change by accident.
func TestCDLMSCascadeIsApplied(t *testing.T) {
	cfg, err := wmalossless.ParseConfig(config(16000, 2, 16, 0x03, 4096, testFlags))
	if err != nil {
		t.Fatal(err)
	}
	one := cascadeStream(t, cfg, []filterSpec{{16, 12}})
	two := cascadeStream(t, cfg, []filterSpec{{16, 12}, {8, 10}})
	swapped := cascadeStream(t, cfg, []filterSpec{{8, 10}, {16, 12}})

	if len(one) != cfg.SamplesPerFrame()*cfg.Channels {
		t.Fatalf("decoded %d samples, want %d", len(one), cfg.SamplesPerFrame()*cfg.Channels)
	}
	if same(one, two) {
		t.Error("a second CDLMS filter changed nothing; the cascade past the first is dead")
	}
	if same(two, swapped) {
		t.Error("swapping the cascade's filters changed nothing; the order is not load-bearing")
	}
	if got := digest(two); got != cascadeDigest {
		t.Errorf("the two-filter decode digests %s, want %s: the cascade's arithmetic moved",
			got, cascadeDigest)
	}
}

// cascadeDigest is the two-filter decode above. It is our own arithmetic and
// says nothing about whether it is the format's; see the comment on the test.
const cascadeDigest = "ba0d142801c86534"

func digest(v []int32) string {
	h := sha256.New()
	var b [4]byte
	for _, x := range v {
		binary.LittleEndian.PutUint32(b[:], uint32(x))
		h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func same(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
