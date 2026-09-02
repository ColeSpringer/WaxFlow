package musepack

// Requantisation, ported from libmpcdec's requant.c and the requantisation
// pass of mpc_decoder.c. A subband sample is q * Cc[res] * SCF[index]: Cc is
// 65536 over the quantiser's step count, the scalefactor table is geometric in
// steps of 0.83298066476582673961 (about 1.58 dB), and the table's 1/32768
// puts the output in +-1.0.

// cc is the requantisation coefficient per resolution, indexed res+1 so the
// noise resolution -1 sits at index 0.
var cc = [19]float32{
	111.285962475327, // noise: 32768/2/255*sqrt(3)
	65536.000000000000, 21845.333333333332, 13107.200000000001, 9362.285714285713,
	7281.777777777777, 4369.066666666666, 2114.064516129032, 1040.253968253968,
	516.031496062992, 257.003921568627, 128.250489236790, 64.062561094819,
	32.015632633121, 16.003907203907, 8.000976681723, 4.000244155527,
	2.000061037018, 1.000015259021,
}

// dc is the requantisation offset per resolution (half the step count less
// one), indexed res+1 like cc.
var dc = [19]int16{
	2,
	0, 1, 2, 3, 4, 7, 15, 31, 63,
	127, 255, 511, 1023, 2047, 4095, 8191, 16383, 32767,
}

// resBit is the raw sample width for the resolutions SV7 codes without a book.
var resBit = [18]uint8{0, 0, 0, 0, 0, 0, 0, 0, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// scfRatio is the scalefactor step.
const scfRatio = 0.83298066476582673961

// scfTable is the reference's SCF[256] at unit output scale: the index is a
// scalefactor index masked to a byte, so the 128 indexes above 1 shrink by
// the ratio per step and the ones below (wrapping through 255) grow. Built
// in float64 and rounded to float32 in the reference's own order, so the
// entries are its entries.
var scfTable = func() (t [256]float32) {
	factor := 1.0 / float64(1<<15)
	t[1] = float32(factor)
	f1, f2 := factor*scfRatio, factor*(1/scfRatio)
	for n := 1; n <= 128; n++ {
		t[uint8(1+n)] = float32(f1)
		t[uint8(1-n)] = float32(f2)
		f1 *= scfRatio
		f2 *= 1 / scfRatio
	}
	return t
}()

// requantize turns the frame's quantised samples into subband samples: Y[n][band]
// per channel, mid/side undone where the band is coded that way. Both channels
// are computed whatever the stream's channel count, as the reference does: a
// mono stream still codes two channels' worth of syntax, and only the synthesis
// leaves the second one unread. Bands above the stream's max band are never
// written and stay zero.
func (d *Decoder) requantize() {
	st := &d.st
	for band := 0; band <= d.cfg.MaxBand; band++ {
		resL, resR := st.res[0][band], st.res[1][band]
		qL, qR := &st.q[band][0], &st.q[band][1]
		yL, yR := &d.y[0], &d.y[1]
		switch {
		case st.ms[band] && resL != 0 && resR != 0:
			for part := range 3 {
				facL := cc[resL+1] * scfTable[uint8(st.scf[0][band][part])]
				facR := cc[resR+1] * scfTable[uint8(st.scf[1][band][part])]
				for n := part * 12; n < part*12+12; n++ {
					l := mul(facL, float32(qL[n]))
					r := mul(facR, float32(qR[n]))
					yL[n][band] = l + r
					yR[n][band] = l - r
				}
			}
		case st.ms[band] && resL != 0:
			for part := range 3 {
				facL := cc[resL+1] * scfTable[uint8(st.scf[0][band][part])]
				for n := part * 12; n < part*12+12; n++ {
					v := facL * float32(qL[n])
					yL[n][band], yR[n][band] = v, v
				}
			}
		case st.ms[band] && resR != 0:
			for part := range 3 {
				facR := cc[resR+1] * scfTable[uint8(st.scf[1][band][part])]
				for n := part * 12; n < part*12+12; n++ {
					v := facR * float32(qR[n])
					yL[n][band], yR[n][band] = v, -v
				}
			}
		case resL != 0 && resR != 0:
			for part := range 3 {
				facL := cc[resL+1] * scfTable[uint8(st.scf[0][band][part])]
				facR := cc[resR+1] * scfTable[uint8(st.scf[1][band][part])]
				for n := part * 12; n < part*12+12; n++ {
					yL[n][band] = facL * float32(qL[n])
					yR[n][band] = facR * float32(qR[n])
				}
			}
		case resL != 0:
			for part := range 3 {
				facL := cc[resL+1] * scfTable[uint8(st.scf[0][band][part])]
				for n := part * 12; n < part*12+12; n++ {
					yL[n][band] = facL * float32(qL[n])
					yR[n][band] = 0
				}
			}
		case resR != 0:
			for part := range 3 {
				facR := cc[resR+1] * scfTable[uint8(st.scf[1][band][part])]
				for n := part * 12; n < part*12+12; n++ {
					yL[n][band] = 0
					yR[n][band] = facR * float32(qR[n])
				}
			}
		default:
			for n := range 36 {
				yL[n][band], yR[n][band] = 0, 0
			}
		}
	}
}
