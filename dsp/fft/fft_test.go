package fft

import (
	"math"
	"math/rand"
	"testing"
)

// naiveDFT is the float64 reference: X[k] = sum_j x[j] e^(-2*pi*i*j*k/n).
func naiveDFT(re, im []float32) (outRe, outIm []float64) {
	n := len(re)
	outRe = make([]float64, n)
	outIm = make([]float64, n)
	for k := 0; k < n; k++ {
		var sr, si float64
		for j := 0; j < n; j++ {
			a := -2 * math.Pi * float64(j) * float64(k) / float64(n)
			c, s := math.Cos(a), math.Sin(a)
			sr += float64(re[j])*c - float64(im[j])*s
			si += float64(re[j])*s + float64(im[j])*c
		}
		outRe[k] = sr
		outIm[k] = si
	}
	return outRe, outIm
}

var testSizes = []int{1, 2, 3, 4, 5, 6, 8, 10, 12, 15, 16, 20, 24, 30, 32,
	45, 48, 60, 64, 100, 120, 240, 480, 512, 600, 960}

func TestTransformMatchesNaiveDFT(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, n := range testSizes {
		p := NewPlan(n)
		if p.N() != n {
			t.Fatalf("N() = %d, want %d", p.N(), n)
		}
		srcRe := make([]float32, n)
		srcIm := make([]float32, n)
		for i := range srcRe {
			srcRe[i] = rng.Float32()*2 - 1
			srcIm[i] = rng.Float32()*2 - 1
		}
		wantRe, wantIm := naiveDFT(srcRe, srcIm)
		dstRe := make([]float32, n)
		dstIm := make([]float32, n)
		p.Transform(dstRe, dstIm, srcRe, srcIm)

		// Error budget: float32 round-off grows with the pass count, a few
		// ulp of the transform's norm. The bound is generous against the
		// observed error yet far below any codec tolerance.
		norm := 0.0
		for k := 0; k < n; k++ {
			norm += wantRe[k]*wantRe[k] + wantIm[k]*wantIm[k]
		}
		tol := 3e-6 * math.Sqrt(norm/float64(n))
		tol = math.Max(tol, 1e-7)
		for k := 0; k < n; k++ {
			if d := math.Abs(float64(dstRe[k]) - wantRe[k]); d > tol {
				t.Fatalf("n=%d re[%d]: got %g want %g (|d|=%g tol=%g)", n, k, dstRe[k], wantRe[k], d, tol)
			}
			if d := math.Abs(float64(dstIm[k]) - wantIm[k]); d > tol {
				t.Fatalf("n=%d im[%d]: got %g want %g (|d|=%g tol=%g)", n, k, dstIm[k], wantIm[k], d, tol)
			}
		}
	}
}

// An impulse at zero transforms to exactly all-ones: the permute pass moves
// the single 1, and every butterfly then adds and multiplies exact zeros and
// ones, so any inexactness here is a kernel indexing bug, not round-off.
func TestTransformImpulseExact(t *testing.T) {
	for _, n := range testSizes {
		p := NewPlan(n)
		srcRe := make([]float32, n)
		srcIm := make([]float32, n)
		srcRe[0] = 1
		dstRe := make([]float32, n)
		dstIm := make([]float32, n)
		p.Transform(dstRe, dstIm, srcRe, srcIm)
		for k := 0; k < n; k++ {
			if dstRe[k] != 1 || dstIm[k] != 0 {
				t.Fatalf("n=%d k=%d: impulse response (%g, %g), want (1, 0)", n, k, dstRe[k], dstIm[k])
			}
		}
	}
}

func TestTransformDeterministic(t *testing.T) {
	const n = 480
	p := NewPlan(n)
	rng := rand.New(rand.NewSource(7))
	srcRe := make([]float32, n)
	srcIm := make([]float32, n)
	for i := range srcRe {
		srcRe[i] = rng.Float32()*2 - 1
		srcIm[i] = rng.Float32()*2 - 1
	}
	aRe, aIm := make([]float32, n), make([]float32, n)
	bRe, bIm := make([]float32, n), make([]float32, n)
	p.Transform(aRe, aIm, srcRe, srcIm)
	NewPlan(n).Transform(bRe, bIm, srcRe, srcIm)
	for i := 0; i < n; i++ {
		if aRe[i] != bRe[i] || aIm[i] != bIm[i] {
			t.Fatalf("output differs at %d across identical runs", i)
		}
	}
}

func TestTransformLeavesSrcUntouched(t *testing.T) {
	const n = 60
	p := NewPlan(n)
	srcRe := make([]float32, n)
	srcIm := make([]float32, n)
	for i := range srcRe {
		srcRe[i] = float32(i) * 0.25
		srcIm[i] = float32(n-i) * 0.5
	}
	wantRe := append([]float32(nil), srcRe...)
	wantIm := append([]float32(nil), srcIm...)
	p.Transform(make([]float32, n), make([]float32, n), srcRe, srcIm)
	for i := 0; i < n; i++ {
		if srcRe[i] != wantRe[i] || srcIm[i] != wantIm[i] {
			t.Fatalf("src modified at %d", i)
		}
	}
}

// The in-place call shape must fail loudly: the permutation pass reads src
// while writing dst, so aliased buffers would corrupt silently.
func TestTransformRejectsAliasedBuffers(t *testing.T) {
	p := NewPlan(60)
	a := make([]float32, 60)
	b := make([]float32, 60)
	c := make([]float32, 60)
	for _, args := range [][4][]float32{
		{a, b, a, c}, // dstRe == srcRe
		{a, b, c, a}, // dstRe == srcIm
		{a, b, b, c}, // dstIm == srcRe
		{a, b, c, b}, // dstIm == srcIm
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("aliased Transform did not panic")
				}
			}()
			p.Transform(args[0], args[1], args[2], args[3])
		}()
	}
}

func TestNewPlanRejectsUnsupportedLengths(t *testing.T) {
	for _, n := range []int{0, -1, 7, 11, 13, 14, 22, 49, 481} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewPlan(%d) did not panic", n)
				}
			}()
			NewPlan(n)
		}()
	}
}

func BenchmarkTransform480(b *testing.B) {
	benchTransform(b, 480)
}

func BenchmarkTransform60(b *testing.B) {
	benchTransform(b, 60)
}

func benchTransform(b *testing.B, n int) {
	p := NewPlan(n)
	rng := rand.New(rand.NewSource(3))
	srcRe := make([]float32, n)
	srcIm := make([]float32, n)
	for i := range srcRe {
		srcRe[i] = rng.Float32()*2 - 1
		srcIm[i] = rng.Float32()*2 - 1
	}
	dstRe := make([]float32, n)
	dstIm := make([]float32, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Transform(dstRe, dstIm, srcRe, srcIm)
	}
}
