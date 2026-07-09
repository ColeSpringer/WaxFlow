package opus

// Laplace-model decoding for CELT coarse band energy (RFC 6716 section 4.3.2.1).
// Ported from libopus laplace.c (ec_laplace_decode): the
// energy deltas are entropy-coded with a Laplace-like PDF whose zero-probability
// and decay come from e_prob_model.

const (
	laplaceLogMinP = 0
	laplaceMinP    = 1 << laplaceLogMinP // minimum symbol probability out of 32768
	laplaceNMin    = 16                  // guaranteed representable deltas per side
)

// laplaceGetFreq1 is the frequency of the first decaying symbol given the
// zero-probability fs0 and the decay rate (both already scaled).
func laplaceGetFreq1(fs0 uint32, decay int) uint32 {
	ft := uint32(32768-laplaceMinP*(2*laplaceNMin)) - fs0
	return uint32((uint64(ft) * uint64(16384-decay)) >> 15)
}

// laplaceDecode decodes one signed energy delta. fs is the zero-probability
// (out of 32768) and decay the geometric decay rate.
func (d *rangeDecoder) laplaceDecode(fs uint32, decay int) int {
	val := 0
	fm := d.decodeBin(15)
	fl := uint32(0)
	if fm >= fs {
		val++
		fl = fs
		fs = laplaceGetFreq1(fs, decay) + laplaceMinP
		// Walk the decaying part of the PDF.
		for fs > laplaceMinP && fm >= fl+2*fs {
			fs *= 2
			fl += fs
			fs = uint32((uint64(fs-2*laplaceMinP) * uint64(decay)) >> 15)
			fs += laplaceMinP
			val++
		}
		// Everything beyond decays to the floor probability.
		if fs <= laplaceMinP {
			di := int((fm - fl) >> (laplaceLogMinP + 1))
			val += di
			fl += uint32(2*di) * laplaceMinP
		}
		if fm < fl+fs {
			val = -val
		} else {
			fl += fs
		}
	}
	hi := fl + fs
	if hi > 32768 {
		hi = 32768
	}
	d.update(fl, hi, 32768)
	return val
}

// laplaceEncode codes one signed energy delta (the inverse of laplaceDecode,
// libopus ec_laplace_encode). It returns the value actually coded: a delta that
// lands past the decaying part of the PDF is clamped toward zero, and the caller
// must feed the returned value back into its prediction so encoder and decoder
// reconstruct the same energy.
func (e *rangeEncoder) laplaceEncode(value int, fs uint32, decay int) int {
	fl := uint32(0)
	val := value
	if val != 0 {
		s := 0
		if val < 0 {
			s = -1
		}
		val = (val + s) ^ s // |value|
		fl = fs
		fs = laplaceGetFreq1(fs, decay)
		// Walk the decaying part of the PDF.
		i := 1
		for ; fs > 0 && i < val; i++ {
			fs *= 2
			fl += fs + 2*laplaceMinP
			fs = uint32((int64(fs) * int64(decay)) >> 15)
		}
		// Everything beyond decays to the floor probability.
		if fs == 0 {
			ndiMax := int(32768-fl+laplaceMinP-1) >> laplaceLogMinP
			ndiMax = (ndiMax - s) >> 1
			di := min(val-i, ndiMax-1)
			fl += uint32(2*di+1+s) * laplaceMinP
			fs = uint32(min(int(laplaceMinP), int(32768-fl)))
			value = (i + di + s) ^ s
		} else {
			fs += laplaceMinP
			fl += fs &^ uint32(s)
		}
	}
	e.encodeBin(fl, fl+fs, 15)
	return value
}
