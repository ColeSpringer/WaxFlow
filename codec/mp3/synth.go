package mp3

// Polyphase synthesis filterbank (ISO 11172-3 section 2.4.3.4.10.4, the
// matrixing form carried by PDMP3): each of the granule's 18 slots turns
// 32 subband samples into 64 matrixed values pushed onto the V ring,
// then a windowed sum over V yields 32 PCM samples.
//
// The matrixing kernel cos((16+i)(2j+1)pi/64) is computed for half its
// rows: row 16 is identically zero, rows 17..31 are the negated mirror
// of 15..1, row 48 is the plain negated sum, and rows 49..63 mirror
// 47..33. The reflection halves the dominant cost of the whole decoder.

// synth runs one channel's granule through the filterbank, appending 576
// PCM samples to dst.
func (d *Decoder) synth(g *granule, ch int, dst []float32) {
	spec := &g.spec[ch]
	v := &d.v[ch]
	var s [32]float32
	var u [512]float32
	dst = dst[:576]
	for ss := 0; ss < 18; ss++ {
		copy(v[64:], v[:1024-64])
		for j := 0; j < 32; j++ {
			s[j] = spec[j*18+ss]
		}
		var total float32
		for _, x := range s {
			total += x
		}
		for i := 0; i < 16; i++ {
			nw := &synthNWin[i]
			var sum float32
			for j := 0; j < 32; j++ {
				sum += nw[j] * s[j]
			}
			v[i] = sum
			if i > 0 {
				v[32-i] = -sum
			}
		}
		v[16] = 0
		v[48] = -total
		for i := 32; i < 48; i++ {
			nw := &synthNWin[i]
			var sum float32
			for j := 0; j < 32; j++ {
				sum += nw[j] * s[j]
			}
			v[i] = sum
			if i > 32 {
				v[96-i] = sum
			}
		}
		for i := 0; i < 512; i += 64 {
			copy(u[i:i+32], v[i*2:i*2+32])
			copy(u[i+32:i+64], v[i*2+96:i*2+128])
		}
		out := dst[32*ss : 32*ss+32]
		for i := range out {
			var sum float32
			for j := 0; j < 512; j += 32 {
				sum += u[j+i] * synthD[j+i]
			}
			out[i] = sum
		}
	}
}
