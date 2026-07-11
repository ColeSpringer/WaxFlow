package loudness

import "math"

// The K-weighting pre-filter of BS.1770-4: a high-frequency shelf
// modelling the head's acoustic effect, then a high pass (the revised
// low-frequency B curve). The standard publishes coefficients only at
// 48 kHz; the meter derives them for any rate by bilinear transform
// from the analog parameters behind that table (the de-quantized center
// frequency, gain, and Q recovered from the published coefficients).
// vbExp maps the shelf gain onto its band coefficient; it is not
// exactly 0.5 because the table was rounded before the parameters were
// recovered.
const (
	shelfHz   = 1681.9744509742096
	shelfGain = 3.999843853973347 // dB
	shelfQ    = 0.7071752369554193
	vbExp     = 0.4996667741545416

	highpassHz = 38.13547087613982
	highpassQ  = 0.5003270373238773
)

// biquad is one second-order section in normalized direct form (a0 = 1).
type biquad struct {
	b0, b1, b2, a1, a2 float64
}

// kState is one channel's filter memory, direct form II transposed per
// stage.
type kState struct {
	s1a, s1b, s2a, s2b float64
}

// kWeighting derives the two K-weighting stages for a sample rate. At
// 48000 Hz the result reproduces the published BS.1770-4 table to about
// 1e-6 (pinned in tests). The high pass keeps the table's fixed
// [1, -2, 1] numerator; only its poles adapt to the rate.
func kWeighting(rate int) (shelf, highpass biquad) {
	fs := float64(rate)

	k := math.Tan(math.Pi * shelfHz / fs)
	vh := math.Pow(10, shelfGain/20)
	vb := math.Pow(vh, vbExp)
	a0 := 1 + k/shelfQ + k*k
	shelf = biquad{
		b0: (vh + vb*k/shelfQ + k*k) / a0,
		b1: 2 * (k*k - vh) / a0,
		b2: (vh - vb*k/shelfQ + k*k) / a0,
		a1: 2 * (k*k - 1) / a0,
		a2: (1 - k/shelfQ + k*k) / a0,
	}

	k = math.Tan(math.Pi * highpassHz / fs)
	a0 = 1 + k/highpassQ + k*k
	highpass = biquad{
		b0: 1, b1: -2, b2: 1,
		a1: 2 * (k*k - 1) / a0,
		a2: (1 - k/highpassQ + k*k) / a0,
	}
	return shelf, highpass
}
