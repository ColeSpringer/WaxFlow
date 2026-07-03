// Package firwin holds the windowed-sinc design primitives shared by the
// DSP kernels that build FIR filters at runtime (the resampler's
// polyphase banks, the limiter's true-peak interpolator). They live in
// one place so a precision tweak cannot silently diverge the filters
// that must stay in lockstep.
package firwin

import "math"

// Sinc is the normalized sinc, sin(pi x)/(pi x).
func Sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// BesselI0 is the zeroth-order modified Bessel function of the first
// kind, by its power series: the Kaiser window's kernel. Terms fall fast
// for the beta range Kaiser designs use; convergence to double precision
// takes tens of terms.
func BesselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	half := x / 2
	for k := 1; ; k++ {
		term *= (half / float64(k)) * (half / float64(k))
		sum += term
		if term < sum*1e-17 {
			return sum
		}
	}
}
