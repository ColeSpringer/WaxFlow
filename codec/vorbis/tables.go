package vorbis

import "math"

// floor1InverseDB maps a floor1 curve value (0..255) to a linear amplitude
// multiplier. libvorbis ships this as a 256-entry literal table; it is
// exactly geometric between its endpoints (1.0649863e-07 at 0, 1.0 at 255),
// so computing it reproduces the literal values to float32 precision without
// transcribing 256 constants. The floor curve indexes this table and the
// result multiplies the residue spectrum (spec 7.2.4).
var floor1InverseDB = func() [256]float32 {
	const first = 1.0649863e-07
	var t [256]float32
	ratio := 1.0 / first
	for i := range t {
		t[i] = float32(first * math.Pow(ratio, float64(i)/255))
	}
	t[255] = 1.0
	return t
}()

// floor1Ranges is the amplitude range for each floor1 multiplier value
// (spec 7.2.2): multiplier 1..4 selects range 256/128/86/64.
var floor1Ranges = [4]int{256, 128, 86, 64}
