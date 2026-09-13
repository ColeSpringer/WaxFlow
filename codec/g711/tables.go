package g711

// The two expansion tables, built at init from the formulas in ITU-T G.711
// rather than copied from anywhere: the recommendation states the segment
// structure and the bit inversions, and 256 entries of arithmetic is a
// smaller thing to get right than 256 entries of transcription.
//
// Both produce a signed value in the 16-bit domain (A-law reaches +-32256,
// mu-law +-32124), which is the scale every reference decoder emits and what
// the pipeline's 16-bit integer format expects.
var (
	alawTable  [256]int16
	mulawTable [256]int16
)

// The field layout both laws share: the low four bits are the quantization
// step within a segment, the next three name the segment, and the top bit is
// the sign.
const (
	quantMask = 0x0F
	segMask   = 0x70
	segShift  = 4
	signBit   = 0x80
)

// mulawBias is the constant mu-law adds before the exponent shift and
// subtracts after, which is what makes its curve pass through zero.
const mulawBias = 0x84

func init() {
	for i := range alawTable {
		alawTable[i] = expandALaw(byte(i))
		mulawTable[i] = expandMuLaw(byte(i))
	}
}

// expandALaw inverts A-law's even bits (the transmitted form alternates them
// to keep the line busy on silence), then rebuilds the linear magnitude from
// its segment and step.
func expandALaw(b byte) int16 {
	v := int(b ^ 0x55)
	t := (v & quantMask) << 4
	switch seg := (v & segMask) >> segShift; seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t = (t + 0x108) << (seg - 1)
	}
	if v&signBit != 0 {
		return int16(t)
	}
	return int16(-t)
}

// expandMuLaw inverts the whole byte (mu-law transmits the complement), then
// rebuilds the magnitude around the bias.
func expandMuLaw(b byte) int16 {
	v := int(^b)
	t := ((v&quantMask)<<3 + mulawBias) << ((v & segMask) >> segShift)
	if v&signBit != 0 {
		return int16(mulawBias - t)
	}
	return int16(t - mulawBias)
}

// table returns the expansion table for a law.
func (l Law) table() *[256]int16 {
	if l == ALaw {
		return &alawTable
	}
	return &mulawTable
}
