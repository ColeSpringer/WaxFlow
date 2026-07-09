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
