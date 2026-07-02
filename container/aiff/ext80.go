package aiff

import "math"

// The COMM sample rate is an IEEE 754 80-bit extended float: 1 sign bit,
// 15 exponent bits (bias 16383), and a 64-bit mantissa with an explicit
// integer bit. Every integer sample rate below 2^53 converts exactly in
// both directions here, so rates survive round trips bit-for-bit.

// fromExt80 decodes a 10-byte extended float. Infinities, NaNs, and
// denormals decode to values the caller's range checks reject naturally.
func fromExt80(b []byte) float64 {
	sign := 1.0
	if b[0]&0x80 != 0 {
		sign = -1
	}
	exp := int(b[0]&0x7F)<<8 | int(b[1])
	mant := be.Uint64(b[2:10])
	if exp == 0 && mant == 0 {
		return 0
	}
	if exp == 0x7FFF {
		if mant<<1 == 0 { // ignore the integer bit for the inf/NaN split
			return sign * math.Inf(1)
		}
		return math.NaN()
	}
	return sign * math.Ldexp(float64(mant), exp-16383-63)
}

// toExt80 encodes a positive finite value.
func toExt80(v float64) [10]byte {
	var b [10]byte
	if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return b // zero; callers validate rates before encoding
	}
	frac, e := math.Frexp(v) // v = frac * 2^e, frac in [0.5, 1)
	exp := e + 16382
	if exp <= 0 || exp >= 0x7FFF {
		return b // out of range for a sample rate; encode zero
	}
	mant := uint64(frac * (1 << 63) * 2) // frac * 2^64, exact for rates
	b[0] = byte(exp >> 8)
	b[1] = byte(exp)
	be.PutUint64(b[2:], mant)
	return b
}
