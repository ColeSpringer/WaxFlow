package vorbis

// Square-polar channel coupling (encode side). For a stereo pair the mapping
// codes a magnitude channel and an angle channel instead of left and right; the
// decoder's inverse step (decode.go, "Inverse channel coupling") reconstructs the
// two channels from them. Coupling runs on the floor-normalized residues, before
// quantization, exactly where the decoder decouples: the encoder floor-fits each
// real channel, normalizes, couples, and quantizes; the decoder decodes,
// decouples, then multiplies each channel by its own floor. A stereo pair whose
// channels agree leaves the angle residue near zero, so its partitions skip and
// cost almost nothing.
//
// coupleForward is the exact inverse of that decode step, derived from it (not
// transcribed from libvorbis): given the two real channel values a (magnitude
// channel) and b (angle channel), it returns the stored (M, A) so the decoder's
// four-way sign branch reconstructs (a, b). The branch conditions partition the
// plane with no overlap, so the mapping is single-valued and a round-trip is
// exact up to residue quantization (proven in coupling_test.go).
func coupleForward(a, b float32) (m, ang float32) {
	switch {
	case a > 0 && a > b:
		return a, a - b // decoder case M>0,A>0
	case a <= 0 && b > a:
		return a, b - a // decoder case M<=0,A>0
	case b > 0 && a <= b:
		return b, a - b // decoder case M>0,A<=0
	default:
		return b, b - a // decoder case M<=0,A<=0
	}
}

// coupleResidues couples channel 0 (magnitude) and channel 1 (angle) in place
// over [0,n2). Applied after per-channel masking so a dual-mono input (identical
// channels) produces an exactly-zero angle residue.
func coupleResidues(resid [][]float32, n2 int) {
	m, a := resid[0], resid[1]
	for i := 0; i < n2; i++ {
		m[i], a[i] = coupleForward(m[i], a[i])
	}
}

// deriveCoupledClasses reclassifies a coupled stereo pair after coupling. On
// entry magCls and angCls hold the two source channels' per-band classes; on exit
// magCls describes the magnitude and angCls the angle. Both take the band's
// masking allocation (the more demanding of the two source classes, so a band
// audible in either channel is never under-coded), which is the right precision
// for the angle too: the angle's quantization error becomes a stereo-image error
// scaled by the band's floor, so it is audible exactly where the band is loud
// (fine) and masked where the band is quiet (coarse). The one asymmetry is that
// the angle additionally skips wherever it is zero: a dual-mono or channel-
// agreeing band carries no image, and skipping the whole angle channel there is
// the coupled-stereo size win a shared (type-2) class stream could not express.
func deriveCoupledClasses(magCls, angCls []int, ang []float32, n2 int) {
	for p := range magCls {
		band := magCls[p]
		if angCls[p] > band {
			band = angCls[p]
		}
		magCls[p] = band
		angCls[p] = classSkip
		if band == classSkip {
			continue
		}
		for l := p * resPartSize; l < (p+1)*resPartSize && l < n2; l++ {
			if ang[l] != 0 {
				angCls[p] = band
				break
			}
		}
	}
}
