package opus

// CELT bit allocation (RFC 6716 section 4.3.3). Ported from libopus rate.c and
// rate.h. The allocator converts the target bit budget
// and the coarse allocation vectors into a per-band pulse count and fine-energy
// bit count, decoding the band-skip, intensity, and dual-stereo flags from the
// range coder along the way. It is pure integer arithmetic (identical in the
// float and fixed builds), so the port is exact.

const (
	allocSteps  = 6  // ALLOC_STEPS
	logMaxPseu  = 6  // LOG_MAX_PSEUDO
	fineOffset  = 21 // FINE_OFFSET
	celtAllocVs = celtAllocRows
)

// log2FracTable: log2 in 1/8-bit units of the number of bands, used to size the
// intensity-stereo parameter (libopus rate.c LOG2_FRAC_TABLE).
var log2FracTable = [24]byte{
	0, 8, 13, 16, 19, 21, 23, 24, 26, 27, 28, 29, 30, 31, 32,
	32, 33, 34, 34, 35, 36, 36, 37, 37,
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// getPulses maps a pseudo-pulse count index to the real pulse count (libopus
// rate.h get_pulses).
func getPulses(i int) int {
	if i < 8 {
		return i
	}
	return (8 + (i & 7)) << ((i >> 3) - 1)
}

// bits2pulses inverts the pulse cache: the largest pulse count whose coding cost
// fits in `bits` (libopus rate.h).
func bits2pulses(band, LM, bits int) int {
	LM++
	cache := celtCacheBits[celtCacheIndex[LM*celtNBands+band]:]
	lo := 0
	hi := int(cache[0])
	bits--
	for i := 0; i < logMaxPseu; i++ {
		mid := (lo + hi + 1) >> 1
		if int(cache[mid]) >= bits {
			hi = mid
		} else {
			lo = mid
		}
	}
	loVal := -1
	if lo != 0 {
		loVal = int(cache[lo])
	}
	if bits-loVal <= int(cache[hi])-bits {
		return lo
	}
	return hi
}

// pulses2bits returns the coding cost of `pulses` pulses in a band (libopus).
func pulses2bits(band, LM, pulses int) int {
	LM++
	cache := celtCacheBits[celtCacheIndex[LM*celtNBands+band]:]
	if pulses == 0 {
		return 0
	}
	return int(cache[pulses]) + 1
}

// initCaps fills cap[] with the per-band maximum useful allocation (libopus
// celt.c init_caps).
func initCaps(cap []int, LM, C int) {
	for i := 0; i < celtNBands; i++ {
		N := (int(celtEBands[i+1]) - int(celtEBands[i])) << LM
		cap[i] = (int(celtCacheCaps[celtNBands*(2*LM+C-1)+i]) + 64) * C * N >> 2
	}
}

// interpBits2Pulses is the decode-side allocator inner loop (libopus rate.c,
// encode==0). It bisects an interpolation between the two allocation vectors,
// decodes the manual band-skip flags plus intensity/dual-stereo, and splits each
// band's budget into fine-energy bits (ebits) and PVQ bits (bits).
func interpBits2Pulses(start, end, skipStart int, bits1, bits2, thresh, cap []int, total int, balance *int,
	skipRsv int, intensity *int, intensityRsv int, dualStereo *int, dualStereoRsv int,
	bits, ebits, finePriority []int, C, LM int, enc *rangeEncoder, d *rangeDecoder, encode bool, prev, signalBandwidth int) int {

	allocFloor := C << bitRes
	stereo := b2i(C > 1)
	logM := LM << bitRes

	lo, hi := 0, 1<<allocSteps
	for i := 0; i < allocSteps; i++ {
		mid := (lo + hi) >> 1
		psum := 0
		done := false
		for j := end - 1; j >= start; j-- {
			tmp := bits1[j] + (mid * bits2[j] >> allocSteps)
			if tmp >= thresh[j] || done {
				done = true
				psum += min(tmp, cap[j])
			} else if tmp >= allocFloor {
				psum += allocFloor
			}
		}
		if psum > total {
			hi = mid
		} else {
			lo = mid
		}
	}

	psum := 0
	done := false
	for j := end - 1; j >= start; j-- {
		tmp := bits1[j] + (lo * bits2[j] >> allocSteps)
		if tmp < thresh[j] && !done {
			if tmp >= allocFloor {
				tmp = allocFloor
			} else {
				tmp = 0
			}
		} else {
			done = true
		}
		tmp = min(tmp, cap[j])
		bits[j] = tmp
		psum += tmp
	}

	// Decide which bands to skip, working backward from the end.
	codedBands := end
	for {
		j := codedBands - 1
		if j <= skipStart {
			total += skipRsv
			break
		}
		left := total - psum
		span := int(celtEBands[codedBands]) - int(celtEBands[start])
		percoeff := left / span
		left -= span * percoeff
		rem := max(left-(int(celtEBands[j])-int(celtEBands[start])), 0)
		bandWidth := int(celtEBands[codedBands]) - int(celtEBands[j])
		bandBits := bits[j] + percoeff*bandWidth + rem
		if bandBits >= max(thresh[j], allocFloor+(1<<bitRes)) {
			if encode {
				// The only non-mandatory part of allocation: a band we skip
				// must be signaled. Hysteresis keeps bands from fluctuating.
				depthThreshold := 0
				if codedBands > 17 {
					if j < prev {
						depthThreshold = 7
					} else {
						depthThreshold = 9
					}
				}
				if codedBands <= start+2 || (bandBits > (depthThreshold*bandWidth<<LM<<bitRes)>>4 && j <= signalBandwidth) {
					enc.encodeBitLogp(1, 1)
					break
				}
				enc.encodeBitLogp(0, 1)
			} else if d.decodeBitLogp(1) != 0 {
				break
			}
			psum += 1 << bitRes
			bandBits -= 1 << bitRes
		}
		psum -= bits[j] + intensityRsv
		if intensityRsv > 0 {
			intensityRsv = int(log2FracTable[j-start])
		}
		psum += intensityRsv
		if bandBits >= allocFloor {
			psum += allocFloor
			bits[j] = allocFloor
		} else {
			bits[j] = 0
		}
		codedBands--
	}

	// Intensity and dual-stereo parameters.
	if intensityRsv > 0 {
		if encode {
			if *intensity > codedBands {
				*intensity = codedBands
			}
			enc.encodeUint(uint32(*intensity-start), uint32(codedBands+1-start))
		} else {
			*intensity = start + int(d.decodeUint(uint32(codedBands+1-start)))
		}
	} else {
		*intensity = 0
	}
	if *intensity <= start {
		total += dualStereoRsv
		dualStereoRsv = 0
	}
	if dualStereoRsv > 0 {
		if encode {
			enc.encodeBitLogp(*dualStereo, 1)
		} else {
			*dualStereo = d.decodeBitLogp(1)
		}
	} else {
		*dualStereo = 0
	}

	// Allocate the remaining bits.
	left := total - psum
	span := int(celtEBands[codedBands]) - int(celtEBands[start])
	percoeff := left / span
	left -= span * percoeff
	for j := start; j < codedBands; j++ {
		bits[j] += percoeff * (int(celtEBands[j+1]) - int(celtEBands[j]))
	}
	for j := start; j < codedBands; j++ {
		tmp := min(left, int(celtEBands[j+1])-int(celtEBands[j]))
		bits[j] += tmp
		left -= tmp
	}

	bal := 0
	var j int
	for j = start; j < codedBands; j++ {
		N0 := int(celtEBands[j+1]) - int(celtEBands[j])
		N := N0 << LM
		bit := bits[j] + bal
		var excess int
		if N > 1 {
			excess = max(bit-cap[j], 0)
			bits[j] = bit - excess
			den := C * N
			if C == 2 && N > 2 && *dualStereo == 0 && j < *intensity {
				den++
			}
			NClogN := den * (int(celtLogN[j]) + logM)
			offset := (NClogN >> 1) - den*fineOffset
			if N == 2 {
				offset += den << bitRes >> 2
			}
			if bits[j]+offset < den*2<<bitRes {
				offset += NClogN >> 2
			} else if bits[j]+offset < den*3<<bitRes {
				offset += NClogN >> 3
			}
			ebits[j] = max(0, bits[j]+offset+(den<<(bitRes-1)))
			ebits[j] = (ebits[j] / den) >> bitRes
			if C*ebits[j] > (bits[j] >> bitRes) {
				ebits[j] = bits[j] >> stereo >> bitRes
			}
			ebits[j] = min(ebits[j], maxFineBits)
			finePriority[j] = b2i(ebits[j]*(den<<bitRes) >= bits[j]+offset)
			bits[j] -= C * ebits[j] << bitRes
		} else {
			excess = max(0, bit-(C<<bitRes))
			bits[j] = bit - excess
			ebits[j] = 0
			finePriority[j] = 1
		}
		if excess > 0 {
			extraFine := min(excess>>(stereo+bitRes), maxFineBits-ebits[j])
			ebits[j] += extraFine
			extraBits := extraFine * C << bitRes
			finePriority[j] = b2i(extraBits >= excess-bal)
			excess -= extraBits
		}
		bal = excess
	}
	*balance = bal

	// Skipped bands spend all their bits on fine energy.
	for ; j < end; j++ {
		ebits[j] = bits[j] >> stereo >> bitRes
		bits[j] = 0
		finePriority[j] = b2i(ebits[j] < 1)
	}
	return codedBands
}

// cltComputeAllocation is the decode-side entry point (libopus rate.c
// clt_compute_allocation, encode==0): it reserves the skip/stereo bits, builds
// the two interpolation endpoints from the allocation vectors and trim, then
// runs interpBits2Pulses.
func cltComputeAllocation(start, end int, offsets, cap []int, allocTrim int, intensity, dualStereo *int,
	total int, balance *int, pulses, ebits, finePriority []int, C, LM int,
	enc *rangeEncoder, d *rangeDecoder, encode bool, prev, signalBandwidth int) int {

	total = max(total, 0)
	length := celtNBands
	skipStart := start
	skipRsv := 0
	if total >= 1<<bitRes {
		skipRsv = 1 << bitRes
	}
	total -= skipRsv
	intensityRsv, dualStereoRsv := 0, 0
	if C == 2 {
		intensityRsv = int(log2FracTable[end-start])
		if intensityRsv > total {
			intensityRsv = 0
		} else {
			total -= intensityRsv
			if total >= 1<<bitRes {
				dualStereoRsv = 1 << bitRes
			}
			total -= dualStereoRsv
		}
	}

	bits1 := make([]int, length)
	bits2 := make([]int, length)
	thresh := make([]int, length)
	trimOffset := make([]int, length)
	for j := start; j < end; j++ {
		width := int(celtEBands[j+1]) - int(celtEBands[j])
		thresh[j] = max(C<<bitRes, (3*width<<LM<<bitRes)>>4)
		trimOffset[j] = C * width * (allocTrim - 5 - LM) * (end - j - 1) * (1 << (LM + bitRes)) >> 6
		if width<<LM == 1 {
			trimOffset[j] -= C << bitRes
		}
	}

	lo, hi := 1, celtAllocVs-1
	for {
		done := false
		psum := 0
		mid := (lo + hi) >> 1
		for j := end - 1; j >= start; j-- {
			N := int(celtEBands[j+1]) - int(celtEBands[j])
			bitsj := C * N * int(celtBandAllocation[mid*length+j]) << LM >> 2
			if bitsj > 0 {
				bitsj = max(0, bitsj+trimOffset[j])
			}
			bitsj += offsets[j]
			if bitsj >= thresh[j] || done {
				done = true
				psum += min(bitsj, cap[j])
			} else if bitsj >= C<<bitRes {
				psum += C << bitRes
			}
		}
		if psum > total {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
		if lo > hi {
			break
		}
	}
	hi = lo
	lo--

	for j := start; j < end; j++ {
		N := int(celtEBands[j+1]) - int(celtEBands[j])
		bits1j := C * N * int(celtBandAllocation[lo*length+j]) << LM >> 2
		var bits2j int
		if hi >= celtAllocVs {
			bits2j = cap[j]
		} else {
			bits2j = C * N * int(celtBandAllocation[hi*length+j]) << LM >> 2
		}
		if bits1j > 0 {
			bits1j = max(0, bits1j+trimOffset[j])
		}
		if bits2j > 0 {
			bits2j = max(0, bits2j+trimOffset[j])
		}
		if lo > 0 {
			bits1j += offsets[j]
		}
		bits2j += offsets[j]
		if offsets[j] > 0 {
			skipStart = j
		}
		bits2j = max(0, bits2j-bits1j)
		bits1[j] = bits1j
		bits2[j] = bits2j
	}

	return interpBits2Pulses(start, end, skipStart, bits1, bits2, thresh, cap, total, balance,
		skipRsv, intensity, intensityRsv, dualStereo, dualStereoRsv, pulses, ebits, finePriority, C, LM,
		enc, d, encode, prev, signalBandwidth)
}
