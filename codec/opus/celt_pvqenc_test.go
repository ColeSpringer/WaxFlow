package opus

import (
	"math"
	"testing"
)

// pvqVFloat computes V(n,k) in float64, mirroring ncwrsURow's recurrence, so the
// test can skip (n,k) whose codeword count overflows uint32. CELT's partition
// split guarantees every real alg_quant call stays under that bound, so those
// pairs are out of scope, not bugs.
func pvqVFloat(n, k int) float64 {
	u := make([]float64, k+2)
	u[1] = 1
	for i := 2; i < k+2; i++ {
		u[i] = float64(2*i - 1)
	}
	for i := 2; i < n; i++ {
		// unext(u[1:], k+1, 1) in float64.
		ui0 := 1.0
		for j := 2; j <= k+1; j++ {
			ui1 := u[j] + u[j-1] + ui0
			u[j-1] = ui0
			ui0 = ui1
		}
		u[k+1] = ui0
	}
	return u[k] + u[k+1]
}

// TestPVQEncodeRoundTrip drives random band shapes through algQuant (search +
// encode + resynth) and algUnquant (decode + resynth) and requires the
// reconstructed vectors and collapse masks to match bit-for-bit. Both sides run
// the same normalise/rotation on the same pulse vector, so equality confirms the
// PVQ search, CWRS index (icwrs/cwrsi), and range coder all pair correctly. Only
// (N,K) whose codeword count fits uint32 are exercised, the range CELT produces.
func TestPVQEncodeRoundTrip(t *testing.T) {
	widths := []int{2, 3, 4, 6, 8, 12, 16, 24, 32, 44, 88, 176}
	spreads := []int{spreadNone, spreadLight, spreadNormal, spreadAggr}
	blocks := []int{1, 2, 4}
	var seed uint32 = 0xabcdef
	rnd := func() float32 {
		seed = seed*1664525 + 1013904223
		return float32(int32(seed)) / float32(1<<31)
	}
	tested := 0
	for _, N := range widths {
		for _, K := range []int{1, 2, 3, 5, 8, 13, 21, 34, 55} {
			if pvqVFloat(N, K) >= float64(uint32(1)<<31) {
				continue // out of CELT's range; the partition split avoids it
			}
			spread := spreads[(N+K)%len(spreads)]
			B := blocks[K%len(blocks)]
			if B > N {
				B = 1
			}
			for trial := 0; trial < 8; trial++ {
				X := make([]float32, N)
				for i := range X {
					X[i] = rnd()
				}
				buf := make([]byte, 4096)
				enc := newRangeEncoder(buf)
				iy := make([]int, N+3)
				u := make([]uint32, K+2)
				xEnc := append([]float32(nil), X...)
				cmEnc := algQuant(xEnc, N, K, spread, B, enc, 1.0, iy, u, true)
				enc.done()
				if enc.err {
					t.Fatalf("N=%d K=%d: encoder overflow", N, K)
				}
				dec := newRangeDecoder(enc.payload())
				iy2 := make([]int, N+3)
				u2 := make([]uint32, K+2)
				xDec := make([]float32, N)
				cmDec := algUnquant(xDec, N, K, spread, B, dec, 1.0, iy2, u2)
				if cmEnc != cmDec {
					t.Fatalf("N=%d K=%d trial=%d: collapse mask enc=%#x dec=%#x", N, K, trial, cmEnc, cmDec)
				}
				for i := 0; i < N; i++ {
					if d := math.Abs(float64(xEnc[i] - xDec[i])); d > 1e-6 {
						t.Fatalf("N=%d K=%d trial=%d idx=%d: enc=%g dec=%g diff=%g",
							N, K, trial, i, xEnc[i], xDec[i], d)
					}
				}
				tested++
			}
		}
	}
	t.Logf("PVQ round-trip exercised %d (N,K,trial) combinations", tested)
}
