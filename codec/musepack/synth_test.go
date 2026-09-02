package musepack_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
)

// directV is the ISO 11172-3 matrixing written out: V[i] = sum over k of
// N[i][k] S[k] with N[i][k] = cos((16+i)(2k+1)pi/64), in float64.
func directV(s *[32]float32) [64]float64 {
	var v [64]float64
	for i := range 64 {
		for k := range 32 {
			v[i] += math.Cos(float64((16+i)*(2*k+1))*math.Pi/64) * float64(s[k])
		}
	}
	return v
}

// TestMatrixingIsTheISOSum scores the fast DCT against the definition it
// implements, on random subband input, so the transform is checked on its own
// terms rather than only through the decode differential.
func TestMatrixingIsTheISOSum(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 9))
	for trial := 0; trial < 200; trial++ {
		var s [32]float32
		for k := range s {
			s[k] = float32(rng.Float64()*2 - 1)
		}
		got := musepack.ComputeNewV(&s)
		want := directV(&s)
		for i := range 64 {
			if math.Abs(float64(got[i])-want[i]) > 1e-5 {
				t.Fatalf("trial %d: V[%d] = %v, the direct sum gives %v", trial, i, got[i], want[i])
			}
		}
	}
}

// TestSynthesisIsTheISOWindowSum scores the whole filterbank against the
// direct evaluation: the matrixing above, the V history shifted 64 per
// subband sample, and the 16-tap window sum at the reference's tap positions.
func TestSynthesisIsTheISOWindowSum(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 7))
	const frames = 3
	input := make([][36][32]float32, frames)
	for f := range input {
		for n := range 36 {
			for k := range 32 {
				input[f][n][k] = float32(rng.Float64()*2-1) * 0.5
			}
		}
	}
	got := musepack.Synthesize(input)

	// The direct model: V is a history of 64-value blocks, newest first.
	var v []float64
	want := make([]float64, 0, frames*musepack.FrameLength)
	for f := range input {
		for n := range 36 {
			block := directV(&input[f][n])
			v = append(block[:], v...)
			if len(v) > 64*17 {
				v = v[:64*17]
			}
			for k := range 32 {
				var sum float64
				for j, tap := range musepack.VTaps {
					if idx := k + tap; idx < len(v) {
						sum += v[idx] * float64(musepack.DiOpt[k][j])
					}
				}
				want = append(want, sum)
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("synthesized %d samples, want %d", len(got), len(want))
	}
	var peak float64
	for i := range got {
		peak = math.Max(peak, math.Abs(want[i]))
		if math.Abs(float64(got[i])-want[i]) > 2e-5 {
			t.Fatalf("sample %d = %v, the direct sum gives %v", i, got[i], want[i])
		}
	}
	if peak < 0.1 {
		t.Fatal("a near-silent test signal proves nothing")
	}
}
