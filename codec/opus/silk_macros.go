package opus

// SILK fixed-point arithmetic primitives, ported from libopus silk/macros.h,
// silk/SigProc_FIX.h, silk/Inlines.h, silk/lin2log.c, and silk/log2lin.c.
// SILK's decoder is integer-only in the reference, so
// porting the fixed-point arithmetic faithfully makes our decode bit-exact with
// the reference decoder, clearing the RFC 6716 section 6 conformance bar with
// large margin. These are the OPUS_FAST_INT64 variants (the amd64/arm64 build).
//
// Go's >> on signed integers is an arithmetic shift, matching the behavior every
// real platform gives libopus's RSHIFT macros; int32*int32 wraps in two's
// complement, matching the reference's assumed overflow behavior.

import "math/bits"

const (
	silkInt32Max = 0x7FFFFFFF
	silkInt32Min = -0x80000000
	silkInt16Max = 0x7FFF
	silkInt16Min = -0x8000
	silkInt8Max  = 0x7F
	silkInt8Min  = -0x80

	randMultiplier = 196314165
	randIncrement  = 907633515
)

// (a32 * (int16)b32) >> 16
func silkSMULWB(a, b int32) int32 { return int32((int64(a) * int64(int16(b))) >> 16) }

// a32 + ((b32 * (int16)c32) >> 16)
func silkSMLAWB(a, b, c int32) int32 { return int32(int64(a) + ((int64(b) * int64(int16(c))) >> 16)) }

// (a32 * (b32 >> 16)) >> 16
func silkSMULWT(a, b int32) int32 { return int32((int64(a) * int64(b>>16)) >> 16) }

// a32 + ((b32 * (c32 >> 16)) >> 16)
func silkSMLAWT(a, b, c int32) int32 { return int32(int64(a) + ((int64(b) * int64(c>>16)) >> 16)) }

// (int16)a32 * (int16)b32
func silkSMULBB(a, b int32) int32 { return int32(int16(a)) * int32(int16(b)) }

// a32 + (int16)b32 * (int16)c32
func silkSMLABB(a, b, c int32) int32 { return a + int32(int16(b))*int32(int16(c)) }

// (int16)a32 * (b32 >> 16)
func silkSMULBT(a, b int32) int32 { return int32(int16(a)) * (b >> 16) }

// a32 + (int16)b32 * (c32 >> 16)
func silkSMLABT(a, b, c int32) int32 { return a + int32(int16(b))*(c>>16) }

// (a32 * b32) >> 16
func silkSMULWW(a, b int32) int32 { return int32((int64(a) * int64(b)) >> 16) }

// a32 + ((b32 * c32) >> 16)
func silkSMLAWW(a, b, c int32) int32 { return int32(int64(a) + ((int64(b) * int64(c)) >> 16)) }

// (a32 >> 16) * (b32 >> 16)
func silkSMULTT(a, b int32) int32 { return (a >> 16) * (b >> 16) }

// a32 + (b32 >> 16) * (c32 >> 16)
func silkSMLATT(a, b, c int32) int32 { return a + (b>>16)*(c>>16) }

// (int64)a32 * b32
func silkSMULL(a, b int32) int64 { return int64(a) * int64(b) }

// (a32 * b32) >> 32
func silkSMMUL(a, b int32) int32 { return int32((int64(a) * int64(b)) >> 32) }

// a32 + b32*c32
func silkMLA(a, b, c int32) int32 { return a + b*c }

// wrapping add/sub (two's-complement overflow allowed)
func silkADD32ovflw(a, b int32) int32 { return int32(uint32(a) + uint32(b)) }
func silkSUB32ovflw(a, b int32) int32 { return int32(uint32(a) - uint32(b)) }

// a32 + wrap(b32*c32)
func silkMLAovflw(a, b, c int32) int32 { return silkADD32ovflw(a, int32(uint32(b)*uint32(c))) }

// a32 + wrap((int16)b32 * (int16)c32)
func silkSMLABBovflw(a, b, c int32) int32 {
	return silkADD32ovflw(a, int32(int16(b))*int32(int16(c)))
}

// SILK's linear-congruential noise generator (silk_RAND).
func silkRAND(seed int32) int32 { return silkMLAovflw(randIncrement, seed, randMultiplier) }

func silkDIV32_16(a, b int32) int32 { return a / b }
func silkDIV32(a, b int32) int32    { return a / b }

func silkSAT8(a int32) int32 {
	if a > silkInt8Max {
		return silkInt8Max
	}
	if a < silkInt8Min {
		return silkInt8Min
	}
	return a
}

func silkSAT16(a int32) int32 {
	if a > silkInt16Max {
		return silkInt16Max
	}
	if a < silkInt16Min {
		return silkInt16Min
	}
	return a
}

func silkSAT32(a int64) int32 {
	if a > silkInt32Max {
		return silkInt32Max
	}
	if a < silkInt32Min {
		return silkInt32Min
	}
	return int32(a)
}

// saturating add/sub, computed in int64 then clamped (equivalent to the
// reference's bit-trick forms).
func silkADDSAT32(a, b int32) int32 { return silkSAT32(int64(a) + int64(b)) }
func silkSUBSAT32(a, b int32) int32 { return silkSAT32(int64(a) - int64(b)) }

func silkLSHIFT32(a int32, shift int) int32    { return int32(uint32(a) << uint(shift)) }
func silkLSHIFTovflw(a int32, shift int) int32 { return int32(uint32(a) << uint(shift)) }
func silkRSHIFT(a int32, shift int) int32      { return a >> uint(shift) }
func silkLSHIFT(a int32, shift int) int32      { return silkLSHIFT32(a, shift) }

// silk_LIMIT(a, l1, l2): clamp a into the interval bounded by l1 and l2,
// whichever ordering they are in.
func silkLIMIT(a, l1, l2 int32) int32 {
	if l1 > l2 {
		if a > l1 {
			return l1
		}
		if a < l2 {
			return l2
		}
		return a
	}
	if a > l2 {
		return l2
	}
	if a < l1 {
		return l1
	}
	return a
}

func silkLSHIFTSAT32(a int32, shift int) int32 {
	return silkLSHIFT32(silkLIMIT(a, silkInt32Min>>uint(shift), silkInt32Max>>uint(shift)), shift)
}

// silk_RSHIFT_ROUND: right shift with rounding.
func silkRSHIFTROUND(a int32, shift int) int32 {
	if shift == 1 {
		return (a >> 1) + (a & 1)
	}
	return ((a >> uint(shift-1)) + 1) >> 1
}

func silkRSHIFTROUND64(a int64, shift int) int64 {
	if shift == 1 {
		return (a >> 1) + (a & 1)
	}
	return ((a >> uint(shift-1)) + 1) >> 1
}

func silkMinInt(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func silkMaxInt(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// silk_abs on int32: undefined at MIN, exactly like the reference.
func silkAbs32(a int32) int32 {
	if a > 0 {
		return a
	}
	return -a
}

func silkSign(a int32) int32 {
	if a > 0 {
		return 1
	}
	if a < 0 {
		return -1
	}
	return 0
}

// silk_CLZ32: count leading zeros of a 32-bit value (32 for zero).
func silkCLZ32(x int32) int32 {
	if x == 0 {
		return 32
	}
	return int32(32 - bits.Len32(uint32(x)))
}

// silk_ROR32: rotate a32 right by rot bits (rot may be negative = rotate left).
func silkROR32(a int32, rot int) int32 { return int32(bits.RotateLeft32(uint32(a), -rot)) }

// silk_CLZ_FRAC: leading zeros plus the 7 bits just after the leading one.
func silkCLZFrac(in int32) (lz, fracQ7 int32) {
	lz = silkCLZ32(in)
	fracQ7 = silkROR32(in, int(24-lz)) & 0x7F
	return
}

// silk_SQRT_APPROX: Q0 approximate square root of a Q0 value.
func silkSQRTAPPROX(x int32) int32 {
	if x <= 0 {
		return 0
	}
	lz, fracQ7 := silkCLZFrac(x)
	var y int32
	if lz&1 != 0 {
		y = 32768
	} else {
		y = 46214 // sqrt(2) * 32768
	}
	y >>= uint(lz >> 1)
	y = silkSMLAWB(y, y, silkSMULBB(213, fracQ7))
	return y
}

// silk_DIV32_varQ: (a32 << Qres) / b32, approximate, Qres >= 0.
func silkDIV32varQ(a32, b32 int32, Qres int) int32 {
	aHeadrm := int(silkCLZ32(silkAbs32(a32))) - 1
	a32nrm := silkLSHIFT(a32, aHeadrm)
	bHeadrm := int(silkCLZ32(silkAbs32(b32))) - 1
	b32nrm := silkLSHIFT(b32, bHeadrm)
	b32inv := silkDIV32_16(silkInt32Max>>2, silkRSHIFT(b32nrm, 16))
	result := silkSMULWB(a32nrm, b32inv)
	a32nrm = silkSUB32ovflw(a32nrm, silkLSHIFTovflw(silkSMMUL(b32nrm, result), 3))
	result = silkSMLAWB(result, a32nrm, b32inv)
	lshift := 29 + aHeadrm - bHeadrm - Qres
	if lshift < 0 {
		return silkLSHIFTSAT32(result, -lshift)
	}
	if lshift < 32 {
		return silkRSHIFT(result, lshift)
	}
	return 0
}

// silk_INVERSE32_varQ: (1 << Qres) / b32, approximate, Qres > 0.
func silkINVERSE32varQ(b32 int32, Qres int) int32 {
	bHeadrm := int(silkCLZ32(silkAbs32(b32))) - 1
	b32nrm := silkLSHIFT(b32, bHeadrm)
	b32inv := silkDIV32_16(silkInt32Max>>2, silkRSHIFT(b32nrm, 16))
	result := silkLSHIFT(b32inv, 16)
	errQ32 := silkLSHIFT(int32(1)<<29-silkSMULWB(b32nrm, b32inv), 3)
	result = silkSMLAWW(result, errQ32, b32inv)
	lshift := 61 - bHeadrm - Qres
	if lshift <= 0 {
		return silkLSHIFTSAT32(result, -lshift)
	}
	if lshift < 32 {
		return silkRSHIFT(result, lshift)
	}
	return 0
}

// silk_lin2log: log-domain value (Q7) of a linear input.
func silkLin2Log(inLin int32) int32 {
	lz, fracQ7 := silkCLZFrac(inLin)
	return silkSMLAWB(fracQ7, silkMLA(0, fracQ7, 128-fracQ7), 179) + silkLSHIFT(31-lz, 7)
}

// silk_log2lin: linear value of a log-domain (Q7) input.
func silkLog2Lin(inLogQ7 int32) int32 {
	if inLogQ7 < 0 {
		return 0
	}
	if inLogQ7 >= 3967 {
		return silkInt32Max
	}
	out := silkLSHIFT(1, int(silkRSHIFT(inLogQ7, 7)))
	fracQ7 := inLogQ7 & 0x7F
	if inLogQ7 < 2048 {
		out = out + silkRSHIFT(silkMLA(0, out, silkSMLAWB(fracQ7, silkSMULBB(fracQ7, 128-fracQ7), -174)), 7)
	} else {
		out = silkMLA(out, silkRSHIFT(out, 7), silkSMLAWB(fracQ7, silkSMULBB(fracQ7, 128-fracQ7), -174))
	}
	return out
}
