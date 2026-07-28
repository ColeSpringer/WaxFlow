package vorbis

import "math"

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
// transcribed from libvorbis): given the two real channel values a (left) and b
// (right), it returns the stored (M, A) so the decoder's four-way sign branch
// reconstructs (a, b), exactly up to residue quantization (proven in
// coupling_test.go).
//
// The decoder's four branch conditions overlap wherever the channels straddle
// zero, so a line in those quadrants has two valid (M, A) representations and
// the encoder must choose. It codes the channel of larger absolute value as the
// magnitude, which is what makes M zero only on a line where both channels are
// zero. The alternative representation, M = 0 with a nonzero A, is legal Vorbis
// and both our decoder and libvorbis reconstruct it correctly, but ffmpeg's
// vectorized inverse coupling branches on M >= 0 where its own C fallback (and
// the spec) branch on M > 0, so it reconstructs those lines with the angle
// channel negated. Emitting them scrambled the right channel in every
// libavcodec-based player while libvorbis-based ones were clean. Every encoder
// in the wild codes the larger channel as the magnitude, so nothing else reaches
// that path, which makes keeping M away from zero the same convention the format
// is decoded under rather than a workaround bolted onto the mapping.
func coupleForward(a, b float32) (m, ang float32) {
	if abs32(a) > abs32(b) {
		if a > 0 {
			return a, a - b // decoder case M>0,A>0 (a > b, since a == |a| > |b| >= b)
		}
		return a, b - a // decoder case M<=0,A>0 (b > a, since a == -|a| < -|b| <= b)
	}
	if b > 0 {
		return b, a - b // decoder case M>0,A<=0 (a <= b, since a <= |a| <= |b| == b)
	}
	return b, b - a // decoder case M<=0,A<=0 (a >= b, since b == -|b| <= -|a| <= a)
}

// abs32 clears the sign bit rather than branching on x < 0, so a negative zero
// returns +0 instead of itself. The comparison in coupleForward cannot tell the
// two apart (IEEE 754 makes -0 == +0), but a helper named abs that can hand back
// a sign-bearing value is a trap for its next caller.
func abs32(x float32) float32 {
	return math.Float32frombits(math.Float32bits(x) &^ (1 << 31))
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
