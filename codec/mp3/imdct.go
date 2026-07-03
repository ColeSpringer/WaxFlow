package mp3

// Hybrid filterbank: per-subband inverse MDCT with window overlap-add,
// then frequency inversion (ISO 11172-3 section 2.4.3.4.10.2-3).
//
// The 36-point IMDCT computes only 18 of its outputs: with
// angle(j) = pi/72 (2j+1+18)(2m+1), angles at j and 17-j sum to an odd
// multiple of pi (so x[17-j] = -x[j]) and angles at j and 53-j differ by
// a full turn (so x[53-j] = x[j]); the other half is reflection. The
// synthesis matrixing in synth.go plays the same trick.

// imdctSubband transforms one subband's 18 spectral lines in from the
// granule into 36 windowed time samples in out, for window shape bt.
// Short blocks are three 12-point transforms offset by 6.
func imdctSubband(in []float32, bt int, out *[36]float32) {
	in = in[:18]
	if bt == blockShort {
		*out = [36]float32{}
		win := &imdctWinF[blockShort]
		for w := 0; w < 3; w++ {
			var x [12]float32
			for p := 0; p < 3; p++ {
				var sum float32
				for m := 0; m < 6; m++ {
					sum += in[w+3*m] * cosN12f[m][p]
				}
				x[p] = sum
				x[5-p] = -sum
			}
			for p := 6; p < 9; p++ {
				var sum float32
				for m := 0; m < 6; m++ {
					sum += in[w+3*m] * cosN12f[m][p]
				}
				x[p] = sum
				x[17-p] = sum
			}
			for p := 0; p < 12; p++ {
				out[6*w+p+6] += x[p] * win[p]
			}
		}
		return
	}
	win := &imdctWinF[bt]
	var x [36]float32
	for p := 0; p < 9; p++ {
		var sum float32
		for m := 0; m < 18; m++ {
			sum += in[m] * cosN36f[m][p]
		}
		x[p] = sum
		x[17-p] = -sum
	}
	for p := 18; p < 27; p++ {
		var sum float32
		for m := 0; m < 18; m++ {
			sum += in[m] * cosN36f[m][p]
		}
		x[p] = sum
		x[53-p] = sum
	}
	for p := 0; p < 36; p++ {
		out[p] = x[p] * win[p]
	}
}

// hybrid runs the IMDCT over all 32 subbands of one channel's granule
// with overlap-add against the decoder's persistent store, then applies
// frequency inversion. The first nLongBands subbands of a mixed block
// use the normal window regardless of the granule's block type.
func (d *Decoder) hybrid(gi *grInfo, g *granule, ch int, nLongBands int) {
	spec := &g.spec[ch]
	store := &d.store[ch]
	var out [36]float32
	for sb := 0; sb < 32; sb++ {
		bt := gi.blockType
		if sb < nLongBands {
			bt = blockNormal
		}
		imdctSubband(spec[sb*18:sb*18+18], bt, &out)
		for i := 0; i < 18; i++ {
			spec[sb*18+i] = out[i] + store[sb][i]
			store[sb][i] = out[i+18]
		}
	}
	// Frequency inversion: odd time samples of odd subbands change sign
	// so the polyphase filterbank sees the right spectral orientation.
	for sb := 1; sb < 32; sb += 2 {
		for i := 1; i < 18; i += 2 {
			spec[sb*18+i] = -spec[sb*18+i]
		}
	}
}
