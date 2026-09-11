//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/codec/wmavoice"
)

// The four transforms the denoise filter runs through, each against the closed
// form docs/notes/wma-voice-bitstream.md section 10.5 states for it. These are
// not "a DFT of the right size": each one's normalisation and sign is what
// decides whether the postfilter agrees with the reference at all, and the note
// asks for exactly this check before the postfilter is trusted.
//
// The bounds are float32 round-off on values of order one to ten, which is what
// the reference's own float32 transform library carries.
func TestTransformsMatchTheirClosedForms(t *testing.T) {
	tf := wmavoice.NewTransforms()

	var src [wmavoice.DFTLen]float32
	for i := range wmavoice.DFTLen {
		src[i] = float32(math.Sin(float64(i)*0.37) + 0.3*math.Cos(float64(i)*1.1))
	}

	t.Run("forward 128-point real DFT, scale 1", func(t *testing.T) {
		var spec [wmavoice.DFTCoefs]float32
		tf.ForwardDFT(&spec, &src)
		var worst float64
		for k := range wmavoice.DFTLen/2 + 1 {
			var re, im float64
			for n := range wmavoice.DFTLen {
				a := -2 * math.Pi * float64(k) * float64(n) / wmavoice.DFTLen
				re += float64(src[n]) * math.Cos(a)
				im += float64(src[n]) * math.Sin(a)
			}
			// The DC and Nyquist bins are real for a real input, and the
			// coefficient walk relies on the Nyquist imaginary part being
			// exactly zero.
			if k == 0 || k == wmavoice.DFTLen/2 {
				im = 0
			}
			worst = math.Max(worst, math.Abs(re-float64(spec[2*k])))
			worst = math.Max(worst, math.Abs(im-float64(spec[2*k+1])))
		}
		if worst > 1e-5 {
			t.Errorf("worst deviation %.3e", worst)
		}
	})

	t.Run("inverse 128-point real DFT, scale 1", func(t *testing.T) {
		var spec [wmavoice.DFTCoefs]float32
		tf.ForwardDFT(&spec, &src)
		var back [wmavoice.DFTLen]float32
		tf.InverseDFT(&back, &spec)
		var worst float64
		for n := range wmavoice.DFTLen {
			want := float64(spec[0]) + float64(spec[wmavoice.DFTLen])*math.Pow(-1, float64(n))
			for k := 1; k < wmavoice.DFTLen/2; k++ {
				a := 2 * math.Pi * float64(k) * float64(n) / wmavoice.DFTLen
				want += 2 * (float64(spec[2*k])*math.Cos(a) - float64(spec[2*k+1])*math.Sin(a))
			}
			worst = math.Max(worst, math.Abs(want-float64(back[n])))
		}
		if worst > 1e-4 {
			t.Errorf("worst deviation %.3e", worst)
		}
		// The pair is unnormalised, so a round trip multiplies by 128 and the
		// convolution of step 7 relies on that factor being exactly there.
		for n := range wmavoice.DFTLen {
			if r := float64(back[n]) / float64(src[n]); math.Abs(r-128) > 1e-3 {
				t.Fatalf("round trip at %d scales by %.5f, want 128", n, r)
			}
		}
	})

	var x [wmavoice.TrigLen]float32
	for i := range wmavoice.TrigLen {
		x[i] = float32(math.Cos(float64(i)*0.21) + 0.5)
	}

	t.Run("64-point DCT-I, scale 1/64", func(t *testing.T) {
		var y [wmavoice.TrigLen]float32
		tf.DCTI(&y, &x)
		var worst float64
		for k := range wmavoice.TrigLen {
			want := float64(x[0]) + math.Pow(-1, float64(k))*float64(x[wmavoice.TrigLen-1])
			for n := 1; n < wmavoice.TrigLen-1; n++ {
				want += 2 * float64(x[n]) * math.Cos(math.Pi*float64(n)*float64(k)/(wmavoice.TrigLen-1))
			}
			worst = math.Max(worst, math.Abs(want/wmavoice.TrigLen-float64(y[k])))
		}
		if worst > 1e-6 {
			t.Errorf("worst deviation %.3e", worst)
		}
	})

	t.Run("64-point DST-I, scale 1/64", func(t *testing.T) {
		var y [wmavoice.TrigLen]float32
		tf.DSTI(&y, &x)
		var worst float64
		for k := range wmavoice.TrigLen {
			var want float64
			for n := range wmavoice.TrigLen {
				want += float64(x[n]) * math.Sin(math.Pi*float64(n+1)*float64(k+1)/(wmavoice.TrigLen+1))
			}
			worst = math.Max(worst, math.Abs(2*want/wmavoice.TrigLen-float64(y[k])))
		}
		if worst > 1e-6 {
			t.Errorf("worst deviation %.3e", worst)
		}
	})

	// The phase reference h64 is the reference's transform library writing past
	// the DST-I's 64 outputs, not a value of the DST-I's own. It has a closed
	// form and this checks it against a direct evaluation of that form, which
	// is the only thing that can be checked: there is no meaning to appeal to.
	t.Run("the phase reference past the DST-I's outputs", func(t *testing.T) {
		var ext [2*wmavoice.TrigLen + 2]float64
		for i := 1; i <= wmavoice.TrigLen; i++ {
			ext[i] = -float64(x[i-1])
			ext[2*wmavoice.TrigLen+2-i] = float64(x[i-1])
		}
		var want float64
		for n := range wmavoice.TrigLen + 1 {
			a := 2 * math.Pi * float64(n) / (wmavoice.TrigLen + 1)
			want += ext[2*n]*math.Sin(a) + ext[2*n+1]*math.Cos(a)
		}
		got := float64(tf.PhaseRef65(&x))
		if math.Abs(got-want) > 1e-4 {
			t.Errorf("h64 = %.6f, the closed form gives %.6f", got, want)
		}
	})
}

// TestLSPToLPCExpandsAKnownSet checks the polynomial expansion of note 5.7
// before anything downstream is trusted, against the property that defines it:
// the LPC denominator's roots are on the unit circle at the LSP angles, so
// evaluating A(z) at z = exp(i*w) for an even-indexed LSP angle w makes the
// sum and difference polynomials cancel.
func TestLSPToLPCExpandsAKnownSet(t *testing.T) {
	for _, l := range []int{10, 16} {
		lsf := make([]float64, l)
		lsp := make([]float64, l)
		for n := range l {
			lsf[n] = math.Pi * float64(n+1) / float64(l+1)
			lsp[n] = math.Cos(lsf[n])
		}
		lpc := make([]float32, l)
		wmavoice.LSPToLPC(lpc, lsp)

		// A(z) = 1 + sum lpc[j] z^-(j+1). Its symmetric part P(z) vanishes at
		// every even-indexed LSP angle and its antisymmetric part Q(z) at every
		// odd-indexed one, which is the definition of a line spectral pair.
		for n := range l {
			w := lsf[n]
			var re, im float64
			for j := range l {
				a := -w * float64(j+1)
				re += float64(lpc[j]) * math.Cos(a)
				im += float64(lpc[j]) * math.Sin(a)
			}
			re += 1
			// P and Q are A(z) plus and minus its own reverse; the reverse at
			// z = exp(i w) is the conjugate times exp(-i (l+1) w).
			ra, ia := re, im
			c, s := math.Cos(-w*float64(l+1)), math.Sin(-w*float64(l+1))
			rb, ib := ra*c-(-ia)*s, ra*s+(-ia)*c
			var v float64
			if n%2 == 0 {
				v = math.Hypot(ra+rb, ia+ib)
			} else {
				v = math.Hypot(ra-rb, ia-ib)
			}
			if v > 1e-9 {
				t.Errorf("order %d, LSP %d: the polynomial does not vanish (%.3e)", l, n, v)
			}
		}
	}
}

// TestStabiliseClampsAndSorts covers note 5.6's three arms, none of which any
// file in this tree reaches: the floor, the ceiling, and the sort the ceiling
// can provoke by pushing the last value back below the one before it.
func TestStabiliseClampsAndSorts(t *testing.T) {
	pi := math.Pi
	t.Run("the floor", func(t *testing.T) {
		lsf := []float64{0, 0.5 * pi, 0.9 * pi}
		floored, capped, sorted := wmavoice.Stabilise(lsf)
		if !floored || capped || sorted {
			t.Errorf("flags %v %v %v, want only the floor", floored, capped, sorted)
		}
		if lsf[0] <= 0 {
			t.Errorf("the first value is still %v", lsf[0])
		}
	})
	t.Run("the spacing", func(t *testing.T) {
		lsf := []float64{0.5 * pi, 0.5 * pi, 0.5 * pi}
		wmavoice.Stabilise(lsf)
		for n := 1; n < len(lsf); n++ {
			if lsf[n] <= lsf[n-1] {
				t.Errorf("values %v are not strictly ascending", lsf)
				break
			}
		}
	})
	t.Run("the ceiling and the sort it provokes", func(t *testing.T) {
		// Every value at the top of the band: the spacing clamp drives them
		// past pi, then the ceiling pulls the last one back under its
		// neighbour, which is the only way the sort is reachable.
		lsf := []float64{0.99 * pi, 0.99 * pi, 0.99 * pi}
		_, capped, sorted := wmavoice.Stabilise(lsf)
		if !capped || !sorted {
			t.Errorf("flags capped=%v sorted=%v, want both", capped, sorted)
		}
		for n := 1; n < len(lsf); n++ {
			if lsf[n] < lsf[n-1] {
				t.Errorf("values %v are still out of order", lsf)
				break
			}
		}
	})
}

// TestEveryCodebookIsExactlyItsStagesLong is what makes the multi-stage
// dequantiser's indexing safe without a bounds check on the hot path: every
// index comes from a bitstream field exactly wide enough for its stage's size,
// so it cannot address past that stage, provided the table is the length its
// stages say.
func TestEveryCodebookIsExactlyItsStagesLong(t *testing.T) {
	want := map[string]int{
		"dqLSP10i":  (256 + 64 + 32 + 32) * 10,
		"dqLSP10r":  (128 + 64 + 64) * 20,
		"dqLSP16i1": (256 + 64) * 5,
		"dqLSP16i2": (128 + 64) * 5,
		"dqLSP16i3": 128 * 6,
		"dqLSP16r1": 128 * 10,
		"dqLSP16r2": 128 * 10,
		"dqLSP16r3": 128 * 12,
	}
	for name, n := range wmavoice.CodebookLens() {
		if n != want[name] {
			t.Errorf("%s is %d entries, its stages need %d", name, n, want[name])
		}
	}
}

// TestFrameTypeCodeIsComplete: the twenty-two codeword lengths are fixed for
// every stream and their Kraft sum is exactly 1, so the code is complete and
// every bit pattern decodes to some symbol. A canonical assignment then hands
// out contiguous runs per length, which is what the decoder's walk relies on.
func TestFrameTypeCodeIsComplete(t *testing.T) {
	lens, runs := wmavoice.TypeRuns()
	kraft := 0.0
	for _, n := range lens {
		kraft += math.Pow(2, -float64(n))
	}
	if kraft != 1 {
		t.Errorf("Kraft sum %v, want exactly 1", kraft)
	}
	total := 0
	for n, r := range runs {
		if r.Count == 0 {
			continue
		}
		total += r.Count
		// The run's codewords must fit inside n bits.
		if r.First+uint64(r.Count) > 1<<uint(n) {
			t.Errorf("length %d: codewords %d..%d do not fit in %d bits",
				n, r.First, r.First+uint64(r.Count)-1, n)
		}
	}
	if total != len(lens) {
		t.Errorf("the runs cover %d symbols, want %d", total, len(lens))
	}
}
