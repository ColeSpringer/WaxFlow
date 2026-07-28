package vorbis

import (
	"math"
	"math/rand"
	"testing"
)

// TestCoupledResidueRoundTripInvariant is the invariant that actually reaches a
// decoder. TestCoupleMagnitudeIsLarger pins coupleForward's choice before
// quantization; this drives the real emit path (encodeResidueType1) and the real
// decode path (residue.decode) and asserts on the reconstructed pair, because it
// is the quantized magnitude the decoder branches on, not the exact one. A
// cascade that rounds a magnitude into its zero cell while the angle survives
// re-creates the representation ffmpeg's vectorized inverse coupling
// mis-decodes, so the nudge has to hold the line here as well as coupleForward
// does above it.
func TestCoupledResidueRoundTripInvariant(t *testing.T) {
	cfg := newEncConfig(2, 44100)
	parsed, err := ParseConfig(cfg.codecConfig(encVendor, nil))
	if err != nil {
		t.Fatal(err)
	}
	n2 := longBlock / 2
	res := cfg.residues[slotLong]
	// The header writes floors/residues long-first, so parsed residue 0 is the
	// long one this builds against.
	dec := &parsed.residues[0]

	// Pair generators spanning the cases the representative choice turns on: a
	// masked channel against a live one (the F1 trigger), anti-phase content (what
	// the nudge exists for), and values scaled across the residue books' cells so
	// magnitudes land on both sides of every rounding boundary.
	gens := []struct {
		name string
		gen  func(rng *rand.Rand, i int) (float32, float32)
	}{
		{"left-masked", func(rng *rand.Rand, int_ int) (float32, float32) {
			return 0, float32(rng.NormFloat64() * 0.4)
		}},
		{"right-masked", func(rng *rand.Rand, int_ int) (float32, float32) {
			return float32(rng.NormFloat64() * 0.4), 0
		}},
		{"anti-phase", func(rng *rand.Rand, int_ int) (float32, float32) {
			v := float32(rng.NormFloat64() * 0.4)
			return v, -v
		}},
		{"decorrelated", func(rng *rand.Rand, int_ int) (float32, float32) {
			return float32(rng.NormFloat64() * 0.4), float32(rng.NormFloat64() * 0.4)
		}},
		// Straddle the lattice boundaries directly: tiny magnitudes at multiples of
		// half the coarse step are exactly where a cascade rounds one channel into
		// its zero cell and not the other.
		{"boundary-straddle", func(rng *rand.Rand, i int) (float32, float32) {
			step := float32(resCoarseDelta) * float32(i%9-4) / 2
			switch i % 3 {
			case 0:
				return step, 0
			case 1:
				return 0, step
			default:
				return step, -step
			}
		}},
	}

	for _, g := range gens {
		for _, cls := range []int{classNoise, classCoarse, classMed, classFine, classSuper} {
			rng := rand.New(rand.NewSource(int64(cls) * 7919))
			mag := make([]float32, n2)
			ang := make([]float32, n2)
			for i := 0; i < n2; i++ {
				a, b := g.gen(rng, i)
				mag[i], ang[i] = coupleForward(a, b)
			}
			// Force every partition onto the class under test, then let the real
			// derivation decide the angle's skips exactly as the encoder does.
			nParts := n2 / resPartSize
			magCls := make([]int, nParts)
			angCls := make([]int, nParts)
			for p := range magCls {
				magCls[p], angCls[p] = cls, cls
			}
			deriveCoupledClasses(magCls, angCls, ang, n2)

			var w bitWriter
			w.reset()
			encodeResidueType1(&w, res, cfg.books, [][]float32{mag, ang}, [][]int{magCls, angCls}, n2, 0)

			out := [][]float32{make([]float32, n2), make([]float32, n2)}
			vec := make([]float32, maxBookVecDim)
			if err := dec.decode(newBitReader(w.bytes()), parsed.codebooks, out, []bool{false, false}, n2, vec); err != nil {
				t.Fatalf("%s cls=%d: decode: %v", g.name, cls, err)
			}
			for i := 0; i < n2; i++ {
				if out[0][i] == 0 && out[1][i] != 0 {
					t.Fatalf("%s cls=%d line %d: reconstructed magnitude 0 with angle %g "+
						"(from M=%g A=%g): ffmpeg's vectorized inverse coupling negates the angle here",
						g.name, cls, i, out[1][i], mag[i], ang[i])
				}
			}
		}
	}
}

// TestCouplingIsEnabledForStereo asserts the rung was taken: the stereo mapping
// declares a coupled pair, so the gates that exercise coupling are exercising it
// rather than passing on an uncoupled stream. Without this a change that dropped
// couplingMag would make every per-channel correlation in tests/ go up while
// proving nothing.
func TestCouplingIsEnabledForStereo(t *testing.T) {
	for _, ch := range []int{1, 2} {
		cfg := newEncConfig(ch, 48000)
		for slot, m := range cfg.mappings {
			switch ch {
			case 1:
				if len(m.couplingMag) != 0 {
					t.Errorf("mono slot %d declares coupling %v", slot, m.couplingMag)
				}
			case 2:
				if len(m.couplingMag) != 1 || m.couplingMag[0] != 0 || len(m.couplingAng) != 1 || m.couplingAng[0] != 1 {
					t.Errorf("stereo slot %d couples mag=%v ang=%v, want mag=[0] ang=[1]", slot, m.couplingMag, m.couplingAng)
				}
			}
		}
	}
}

// TestNudgeHoldsAtTheAngleTie is the named regression for the boundary a
// strict |angle| > |magnitude| nudge test excludes. A residue pair whose
// angle-side channel is masked to zero gives |A| == |M| exactly, so a strict
// test never fires on it; at half a coarse step the bare quantizer then splits
// the pair, because round-half-away-from-zero sends the negative magnitude onto
// the zero point and the positive angle up to the next one. The cascade has to
// close that itself, so this drives the real emit and decode paths rather than
// the books underneath them.
func TestNudgeHoldsAtTheAngleTie(t *testing.T) {
	book := encBooks[bookResCoarse]
	// Only the negative side splits: a = -half gives M = -half and A = +half, and
	// rounding half away from zero sends those to opposite lattice points. At
	// a = +half both sides are +half and round together, so there is nothing to
	// defend there.
	for _, a := range []float64{-resCoarseDelta / 2} {
		m, ang := coupleForward(float32(a), 0)
		if math.Abs(float64(ang)) != math.Abs(float64(m)) {
			t.Fatalf("a=%g: expected the tie |A| == |M|, got M=%g A=%g", a, m, ang)
		}
		// The pair only splits because the bare lattice rounds the two sides
		// differently; if that ever stops being true the test is no longer
		// covering the boundary it names.
		if book.latValue(book.latIndex(float64(m))) != 0 || book.latValue(book.latIndex(float64(ang))) == 0 {
			t.Fatalf("a=%g: the coarse lattice no longer splits the tie, so this case is stale", a)
		}
		mHat, angHat := codeCoupledLine(t, m, ang, classCoarse)
		if mHat == 0 && angHat != 0 {
			t.Errorf("a=%g b=0: encoded pair decodes to M=0 with A=%g", a, angHat)
		}
	}
}

// codeCoupledLine runs one coupled (M, A) pair through the production emit and
// decode paths at the given class and returns the reconstruction the decoder
// holds, so a test can assert on what a player sees rather than on the books.
func codeCoupledLine(t *testing.T, m, ang float32, cls int) (float32, float32) {
	t.Helper()
	cfg := newEncConfig(2, 44100)
	parsed, err := ParseConfig(cfg.codecConfig(encVendor, nil))
	if err != nil {
		t.Fatal(err)
	}
	n2 := longBlock / 2
	mags := make([]float32, n2)
	angs := make([]float32, n2)
	for i := range mags {
		mags[i], angs[i] = m, ang
	}
	nParts := n2 / resPartSize
	magCls := make([]int, nParts)
	angCls := make([]int, nParts)
	for p := range magCls {
		magCls[p], angCls[p] = cls, cls
	}
	deriveCoupledClasses(magCls, angCls, angs, n2)

	var w bitWriter
	w.reset()
	encodeResidueType1(&w, cfg.residues[slotLong], cfg.books, [][]float32{mags, angs}, [][]int{magCls, angCls}, n2, 0)

	out := [][]float32{make([]float32, n2), make([]float32, n2)}
	if err := parsed.residues[0].decode(newBitReader(w.bytes()), parsed.codebooks, out,
		[]bool{false, false}, n2, make([]float32, maxBookVecDim)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out[0][0], out[1][0]
}
