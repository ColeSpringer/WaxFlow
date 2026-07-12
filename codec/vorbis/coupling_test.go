package vorbis

import (
	"math"
	"testing"
)

// decoupleOne mirrors decode.go's inverse coupling for a single (M, A) pair, so
// the test checks coupleForward against the exact decoder arithmetic.
func decoupleOne(mv, av float32) (a, b float32) {
	var newM, newA float32
	if mv > 0 {
		if av > 0 {
			newM, newA = mv, mv-av
		} else {
			newA, newM = mv, mv+av
		}
	} else {
		if av > 0 {
			newM, newA = mv, mv+av
		} else {
			newA, newM = mv, mv-av
		}
	}
	return newM, newA
}

// TestCoupleForwardInverts proves coupleForward is the exact inverse of the
// decoder's decouple over a dense grid of channel-value pairs, including the sign
// boundaries where the four branches meet.
func TestCoupleForwardInverts(t *testing.T) {
	vals := []float32{-2, -1.5, -1, -0.5, -0.001, 0, 0.001, 0.5, 1, 1.5, 2}
	for _, a := range vals {
		for _, b := range vals {
			m, ang := coupleForward(a, b)
			ga, gb := decoupleOne(m, ang)
			if math.Abs(float64(ga-a)) > 1e-6 || math.Abs(float64(gb-b)) > 1e-6 {
				t.Fatalf("a=%g b=%g -> (M=%g A=%g) -> decoded (%g,%g)", a, b, m, ang, ga, gb)
			}
		}
	}
}

// TestCoupleDualMonoZerosAngle confirms that identical channels (dual mono)
// produce an exactly-zero angle residue, so coupling is free on such content.
func TestCoupleDualMonoZerosAngle(t *testing.T) {
	for _, v := range []float32{-1, -0.3, 0, 0.2, 0.9} {
		_, ang := coupleForward(v, v)
		if ang != 0 {
			t.Errorf("coupleForward(%g,%g) angle = %g, want 0", v, v, ang)
		}
	}
}
