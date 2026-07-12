package mka

import (
	"encoding/binary"
	"math"
)

// ebmlwrite.go is the write-side inverse of ebml.go: EBML element-ID and
// variable-length-integer serialization for the muxer. parseVint/readElement
// decode these forms; the helpers here produce them.

// appendID appends an element ID in its on-wire form. An ID carries its
// length-descriptor marker bits as part of the value (the mka.go constants are
// already in that form), so the byte width follows the magnitude: 0xAE is one
// byte, 0x4282 two, 0x1A45DFA3 four.
func appendID(dst []byte, id uint32) []byte {
	switch {
	case id <= 0xFF:
		return append(dst, byte(id))
	case id <= 0xFFFF:
		return append(dst, byte(id>>8), byte(id))
	case id <= 0xFFFFFF:
		return append(dst, byte(id>>16), byte(id>>8), byte(id))
	default:
		return append(dst, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	}
}

// vintLen is the byte width of the minimal definite-size vint encoding of v:
// the smallest w in 1..8 whose 7*w value bits hold v without spending the
// reserved all-ones pattern (which parseVint reads as an unknown size).
func vintLen(v uint64) int {
	for w := 1; w <= 8; w++ {
		if v < (uint64(1)<<(7*w))-1 {
			return w
		}
	}
	return 8
}

// appendVint appends v as a definite-size EBML vint of minimal width (a size
// field or an unsigned value that is itself vint-coded, like a block's track
// number). The marker bit for width w sits at 0x80>>(w-1) in the first byte;
// v < 2^(7w) guarantees that bit is free.
func appendVint(dst []byte, v uint64) []byte {
	w := vintLen(v)
	var b [8]byte
	for i := w - 1; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	b[0] |= 0x80 >> (w - 1)
	return append(dst, b[:w]...)
}

// appendElement appends a complete definite-size element: ID, a minimal size
// vint, then the body.
func appendElement(dst []byte, id uint32, body []byte) []byte {
	dst = appendID(dst, id)
	dst = appendVint(dst, uint64(len(body)))
	return append(dst, body...)
}

// appendUint appends an unsigned-integer element whose body is the minimal
// big-endian encoding of v (a Matroska unsigned integer, 1..8 bytes).
func appendUint(dst []byte, id uint32, v uint64) []byte {
	return appendElement(dst, id, beUintBytes(v))
}

// appendString appends a UTF-8 / ASCII string element.
func appendString(dst []byte, id uint32, s string) []byte {
	return appendElement(dst, id, []byte(s))
}

// appendFloat appends an 8-byte IEEE-754 float element (SamplingFrequency).
func appendFloat(dst []byte, id uint32, v float64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
	return appendElement(dst, id, b[:])
}

// beUintBytes renders v as the minimal-length big-endian unsigned integer, at
// least one byte (so a zero value is a single 0x00).
func beUintBytes(v uint64) []byte {
	var full [8]byte
	binary.BigEndian.PutUint64(full[:], v)
	i := 0
	for i < 7 && full[i] == 0 {
		i++
	}
	return append([]byte(nil), full[i:]...)
}

// beIntBytes renders v as the minimal-length big-endian two's-complement signed
// integer: the shortest encoding whose sign bit still recovers v under the
// sign-extension beInt performs. DiscardPadding is the only signed element the
// muxer writes, and only ever with v > 0.
func beIntBytes(v int64) []byte {
	var full [8]byte
	binary.BigEndian.PutUint64(full[:], uint64(v))
	i := 0
	if v >= 0 {
		for i < 7 && full[i] == 0x00 && full[i+1]&0x80 == 0 {
			i++
		}
	} else {
		for i < 7 && full[i] == 0xFF && full[i+1]&0x80 != 0 {
			i++
		}
	}
	return append([]byte(nil), full[i:]...)
}

// samplesToNs converts a sample count at rate to a nanosecond duration, rounded
// to nearest: the exact inverse of nsToSamples, so a mux->demux round-trip of
// the gapless trims (CodecDelay, DiscardPadding) recovers the sample count that
// was written (312 samples at 48 kHz is 6_500_000 ns, and nsToSamples rounds it
// back to 312). It splits into whole seconds plus a sub-second remainder so the
// samples*1e9 product cannot overflow for any real duration.
func samplesToNs(samples int64, rate int) int64 {
	if samples <= 0 || rate <= 0 {
		return 0
	}
	r := int64(rate)
	sec := samples / r
	rem := samples % r
	return sec*1_000_000_000 + (rem*1_000_000_000+r/2)/r
}
