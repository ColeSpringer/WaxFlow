package aac

import "math"

func isIntensity(cb uint8) bool { return cb == intensityHCB || cb == intensityHCB2 }

// applyPNS fills perceptual-noise-substitution bands with scaled random
// values (ISO 14496-3 4.6.12). PNS noise is random by construction, so no
// two decoders agree bit-for-bit here; the differential fixtures disable
// PNS and this path is not exercised by them. Real files that use it decode
// with plausible noise at the coded energy rather than silence.
func (d *Decoder) applyPNS(cd *channelData) {
	info := &cd.info
	gs := groupStarts(info)
	for g := 0; g < info.numWindowGroups; g++ {
		for sfb := 0; sfb < info.maxSfb; sfb++ {
			if cd.sfbCb[g][sfb] != noiseHCB {
				continue
			}
			scale := math.Exp2(0.25 * float64(cd.sf[g][sfb]-sfOffset))
			start, end := int(info.swb[sfb]), int(info.swb[sfb+1])
			for w := 0; w < info.windowGroupLen[g]; w++ {
				base := (gs[g] + w) * 128
				for k := start; k < end; k++ {
					cd.spec[base+k] = d.pnsNext() * scale
				}
			}
		}
	}
}

// pnsNext returns the next PRNG sample in [-1, 1) (xorshift32).
func (d *Decoder) pnsNext() float64 {
	d.pnsState ^= d.pnsState << 13
	d.pnsState ^= d.pnsState >> 17
	d.pnsState ^= d.pnsState << 5
	return float64(int32(d.pnsState)) / 2147483648.0
}

// applyMS reverses M/S stereo per scalefactor band: left = mid+side,
// right = mid-side, skipping intensity and noise bands (ISO 14496-3 4.6.8.1).
func applyMS(left, right *channelData, msMask int, msUsed *[maxWindowGroups][maxSFBCount]bool) {
	info := &left.info
	gs := groupStarts(info)
	for g := 0; g < info.numWindowGroups; g++ {
		for sfb := 0; sfb < info.maxSfb; sfb++ {
			on := msMask == 2 || (msMask == 1 && msUsed[g][sfb])
			// M/S applies only where both channels carry a regular codebook;
			// skip a band that is noise or intensity in either (codebook
			// >= noiseHCB covers PNS and both intensity books).
			if !on || left.sfbCb[g][sfb] >= noiseHCB || right.sfbCb[g][sfb] >= noiseHCB {
				continue
			}
			start, end := int(info.swb[sfb]), int(info.swb[sfb+1])
			for w := 0; w < info.windowGroupLen[g]; w++ {
				base := (gs[g] + w) * 128
				for k := start; k < end; k++ {
					m, s := left.spec[base+k], right.spec[base+k]
					left.spec[base+k] = m + s
					right.spec[base+k] = m - s
				}
			}
		}
	}
}

// applyIntensity fills the right channel's intensity bands by scaling the
// left channel's coefficients (ISO 14496-3 4.6.8.2.3). INTENSITY_HCB2 flips
// the sign relative to INTENSITY_HCB, and ms_used on the band flips it again
// when ms_mask_present is 1.
func applyIntensity(left, right *channelData, msMask int, msUsed *[maxWindowGroups][maxSFBCount]bool) {
	info := &right.info
	gs := groupStarts(info)
	for g := 0; g < info.numWindowGroups; g++ {
		for sfb := 0; sfb < info.maxSfb; sfb++ {
			cb := right.sfbCb[g][sfb]
			if !isIntensity(cb) {
				continue
			}
			scale := math.Exp2(-0.25 * float64(right.sf[g][sfb]))
			if cb == intensityHCB2 {
				scale = -scale
			}
			if msMask == 1 && msUsed[g][sfb] {
				scale = -scale // ms_used inverts the intensity position
			}
			start, end := int(info.swb[sfb]), int(info.swb[sfb+1])
			for w := 0; w < info.windowGroupLen[g]; w++ {
				base := (gs[g] + w) * 128
				for k := start; k < end; k++ {
					right.spec[base+k] = left.spec[base+k] * scale
				}
			}
		}
	}
}
