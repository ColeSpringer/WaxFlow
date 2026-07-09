package opus

import "math"

// CELT band shape decoding and synthesis prep (RFC 6716 sections 4.3.4-4.3.6).
// Ported from libopus bands.c. This is the decode path
// only: the reference's encoder branches (theta_rdo distortion search, encode
// side of compute_theta) and the experimental QEXT paths are not part of the
// bitstream and are omitted. Every band's unit-norm shape is decoded by PVQ,
// with recursive mid/side (theta) splitting, stereo, transient time-frequency
// resolution changes, and spectral folding of empty bands.

const (
	qthetaOffset         = 4  // QTHETA_OFFSET
	qthetaOffsetTwophase = 16 // QTHETA_OFFSET_TWOPHASE
)

// --- bit-exact integer helpers (libopus mathops.c / bands.c) ---

// fracMul16 is the Q15 fractional multiply used in the theta allocation, kept
// bit-exact because it affects mid/side bit splitting.
func fracMul16(a, b int) int {
	return (16384 + int(int16(a))*int(int16(b))) >> 15
}

// bitexactCos is a platform-independent cos() approximation (libopus). Bit
// exactness matters because the result drives bit allocation.
func bitexactCos(x int16) int {
	tmp := (4096 + int32(x)*int32(x)) >> 13
	x2 := int(tmp)
	x2 = (32767 - x2) + fracMul16(x2, -7651+fracMul16(x2, 8277+fracMul16(-626, x2)))
	return 1 + x2
}

// bitexactLog2Tan approximates 2^11·log2(tan) for the mid/side split (libopus).
func bitexactLog2Tan(isin, icos int) int {
	lc := ilog(uint32(icos))
	ls := ilog(uint32(isin))
	icos <<= uint(15 - lc)
	isin <<= uint(15 - ls)
	return (ls-lc)*(1<<11) +
		fracMul16(isin, fracMul16(isin, -2597)+7932) -
		fracMul16(icos, fracMul16(icos, -2597)+7932)
}

// isqrt32 is the integer square root used in the triangular-PDF theta decode.
func isqrt32(val uint32) uint32 {
	var g uint32
	bshift := (ilog(val) - 1) >> 1
	b := uint32(1) << uint(bshift)
	for {
		t := ((g << 1) + b) << uint(bshift)
		if t <= val {
			g += b
			val -= t
		}
		b >>= 1
		bshift--
		if bshift < 0 {
			break
		}
	}
	return g
}

// celtLCGRand is the linear congruential generator used to fold/fill empty bands.
func celtLCGRand(seed uint32) uint32 { return 1664525*seed + 1013904223 }

// orderyTable is the Hadamard reordering used when interleaving short blocks.
var orderyTable = []int{
	1, 0,
	3, 0, 2, 1,
	7, 0, 4, 3, 6, 1, 5, 2,
	15, 0, 8, 7, 12, 3, 11, 4, 14, 1, 9, 6, 13, 2, 10, 5,
}

// --- band-domain transforms ---

// haar1 applies an in-place Haar (sum/difference) step used for transient
// time-frequency resolution changes (libopus bands.c).
func haar1(X []float32, N0, stride int) {
	N0 >>= 1
	const s = float32(0.70710678)
	for i := 0; i < stride; i++ {
		for j := 0; j < N0; j++ {
			t1 := s * X[stride*2*j+i]
			t2 := s * X[stride*(2*j+1)+i]
			X[stride*2*j+i] = t1 + t2
			X[stride*(2*j+1)+i] = t1 - t2
		}
	}
}

func deinterleaveHadamard(X []float32, N0, stride, hadamard int) {
	N := N0 * stride
	tmp := make([]float32, N)
	if hadamard != 0 {
		ordery := orderyTable[stride-2:]
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[ordery[i]*N0+j] = X[j*stride+i]
			}
		}
	} else {
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[i*N0+j] = X[j*stride+i]
			}
		}
	}
	copy(X[:N], tmp)
}

func interleaveHadamard(X []float32, N0, stride, hadamard int) {
	N := N0 * stride
	tmp := make([]float32, N)
	if hadamard != 0 {
		ordery := orderyTable[stride-2:]
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[j*stride+i] = X[ordery[i]*N0+j]
			}
		}
	} else {
		for i := 0; i < stride; i++ {
			for j := 0; j < N0; j++ {
				tmp[j*stride+i] = X[i*N0+j]
			}
		}
	}
	copy(X[:N], tmp)
}

// --- denormalization and anti-collapse ---

// denormaliseBands scales each band's unit-norm shape X by its linear energy
// (2^logE) into the MDCT-domain freq array (libopus bands.c denormalise_bands).
func denormaliseBands(X, freq, bandLogE []float32, start, end, M, downsample int, silence bool) {
	N := M * celtShortMDCTSize
	bound := M * int(celtEBands[end])
	if downsample != 1 {
		bound = min(bound, N/downsample)
	}
	if silence {
		bound = 0
		start, end = 0, 0
	}
	fstart := M * int(celtEBands[start])
	for i := 0; i < fstart; i++ {
		freq[i] = 0
	}
	for i := start; i < end; i++ {
		bandStart := M * int(celtEBands[i])
		bandEnd := M * int(celtEBands[i+1])
		lg := bandLogE[i] + celtEMeans[i]
		g := float32(math.Exp2(math.Min(32, float64(lg))))
		for j := bandStart; j < bandEnd; j++ {
			freq[j] = X[j] * g
		}
	}
	for i := bound; i < N; i++ {
		freq[i] = 0
	}
}

// antiCollapse refills bands whose short blocks collapsed to zero with shaped
// noise, preventing energy holes on transients (libopus bands.c anti_collapse;
// decode path, C-channel merge for mono applied).
func antiCollapse(X []float32, collapseMasks []byte, LM, C, size, start, end int,
	logE, prev1logE, prev2logE []float32, pulses []int, seed uint32) {
	for i := start; i < end; i++ {
		N0 := int(celtEBands[i+1]) - int(celtEBands[i])
		depth := (1 + pulses[i]) / (int(celtEBands[i+1]) - int(celtEBands[i])) >> LM
		thresh := 0.5 * float32(math.Exp2(-0.125*float64(depth)))
		sqrt1 := float32(1.0 / math.Sqrt(float64(N0<<LM)))
		for c := 0; c < C; c++ {
			prev1 := prev1logE[c*celtNBands+i]
			prev2 := prev2logE[c*celtNBands+i]
			if C == 1 {
				prev1 = max(prev1, prev1logE[celtNBands+i])
				prev2 = max(prev2, prev2logE[celtNBands+i])
			}
			Ediff := logE[c*celtNBands+i] - min(prev1, prev2)
			Ediff = max(0, Ediff)
			r := 2.0 * float32(math.Exp2(float64(-Ediff)))
			if LM == 3 {
				r *= 1.41421356
			}
			r = min(thresh, r)
			r *= sqrt1
			Xp := X[c*size+(int(celtEBands[i])<<LM):]
			renorm := false
			for k := 0; k < 1<<LM; k++ {
				if collapseMasks[i*C+c]&(1<<uint(k)) == 0 {
					for j := 0; j < N0; j++ {
						seed = celtLCGRand(seed)
						if seed&0x8000 != 0 {
							Xp[(j<<LM)+k] = r
						} else {
							Xp[(j<<LM)+k] = -r
						}
					}
					renorm = true
				}
			}
			if renorm {
				renormaliseVector(Xp, N0<<LM, 1.0)
			}
		}
	}
}

// --- stereo helpers (decode path) ---
//
// intensity_stereo and stereo_split are encoder-only (compute_theta's analysis
// branch) and have no decode-side counterpart, so they are not ported.

// stereoMerge reconstructs left/right from the decoded mid (X) and side (Y)
// shapes (libopus bands.c stereo_merge; float build).
func stereoMerge(X, Y []float32, mid float32, N int) {
	var xp, side float32
	for j := 0; j < N; j++ {
		xp += X[j] * Y[j]
		side += Y[j] * Y[j]
	}
	xp *= mid
	El := mid*mid + side - 2*xp
	Er := mid*mid + side + 2*xp
	if Er < 6e-4 || El < 6e-4 {
		copy(Y[:N], X[:N])
		return
	}
	lgain := 1.0 / float32(math.Sqrt(float64(El)))
	rgain := 1.0 / float32(math.Sqrt(float64(Er)))
	for j := 0; j < N; j++ {
		l := mid * X[j]
		r := Y[j]
		X[j] = lgain * (l - r)
		Y[j] = rgain * (l + r)
	}
}

// --- band context and theta split ---

type bandCtx struct {
	i             int
	intensity     int
	spread        int
	tfChange      int
	ec            *rangeDecoder
	remainingBits int
	bandE         []float32
	seed          uint32
	disableInv    int
	avoidSplit    int
	iy            []int    // alg_unquant pulse scratch
	u             []uint32 // CWRS U-row scratch
}

type splitCtx struct {
	inv, imid, iside, delta, itheta, qalloc int
}

func computeQn(N, b, offset, pulseCap, stereo int) int {
	N2 := 2*N - 1
	if stereo != 0 && N == 2 {
		N2--
	}
	qb := (b + N2*offset) / N2
	qb = min(b-pulseCap-(4<<bitRes), qb)
	qb = min(8<<bitRes, qb)
	if qb < (1 << bitRes >> 1) {
		return 1
	}
	qn := int(celtExp2Table8[qb&0x7]) >> uint(14-(qb>>bitRes))
	return (qn + 1) >> 1 << 1
}

// computeTheta decodes the mid/side split angle for a band (libopus bands.c
// compute_theta, decode path).
func (ctx *bandCtx) computeTheta(sctx *splitCtx, X, Y []float32, N int, b *int, B, B0, LM, stereo int, fill *int) {
	pulseCap := int(celtLogN[ctx.i]) + LM*(1<<bitRes)
	off := qthetaOffset
	if stereo != 0 && N == 2 {
		off = qthetaOffsetTwophase
	}
	offset := (pulseCap >> 1) - off
	qn := computeQn(N, *b, offset, pulseCap, stereo)
	if stereo != 0 && ctx.i >= ctx.intensity {
		qn = 1
	}
	tell := ctx.ec.tellFrac()
	itheta := 0
	inv := 0
	if qn != 1 {
		if stereo != 0 && N > 2 {
			p0 := 3
			x0 := qn / 2
			ft := p0*(x0+1) + x0
			fs := int(ctx.ec.decode(uint32(ft)))
			var x int
			if fs < (x0+1)*p0 {
				x = fs / p0
			} else {
				x = x0 + 1 + (fs - (x0+1)*p0)
			}
			var fl, fh int
			if x <= x0 {
				fl = p0 * x
				fh = p0 * (x + 1)
			} else {
				fl = (x - 1 - x0) + (x0+1)*p0
				fh = (x - x0) + (x0+1)*p0
			}
			ctx.ec.update(uint32(fl), uint32(fh), uint32(ft))
			itheta = x
		} else if B0 > 1 || stereo != 0 {
			itheta = int(ctx.ec.decodeUint(uint32(qn + 1)))
		} else {
			ft := ((qn >> 1) + 1) * ((qn >> 1) + 1)
			fm := int(ctx.ec.decode(uint32(ft)))
			var fl, fs int
			if fm < ((qn>>1)*((qn>>1)+1))>>1 {
				itheta = (int(isqrt32(uint32(8*fm+1))) - 1) >> 1
				fs = itheta + 1
				fl = itheta * (itheta + 1) >> 1
			} else {
				itheta = (2*(qn+1) - int(isqrt32(uint32(8*(ft-fm-1)+1)))) >> 1
				fs = qn + 1 - itheta
				fl = ft - ((qn+1-itheta)*(qn+2-itheta))>>1
			}
			ctx.ec.update(uint32(fl), uint32(fl+fs), uint32(ft))
		}
		itheta = itheta * 16384 / qn
	} else if stereo != 0 {
		if *b > 2<<bitRes && ctx.remainingBits > 2<<bitRes {
			inv = ctx.ec.decodeBitLogp(2)
		}
		if ctx.disableInv != 0 {
			inv = 0
		}
		itheta = 0
	}
	qalloc := ctx.ec.tellFrac() - tell
	*b -= qalloc
	var imid, iside, delta int
	switch {
	case itheta == 0:
		imid, iside = 32767, 0
		*fill &= (1 << B) - 1
		delta = -16384
	case itheta == 16384:
		imid, iside = 0, 32767
		*fill &= ((1 << B) - 1) << B
		delta = 16384
	default:
		imid = bitexactCos(int16(itheta))
		iside = bitexactCos(int16(16384 - itheta))
		delta = fracMul16((N-1)<<7, bitexactLog2Tan(iside, imid))
	}
	sctx.inv, sctx.imid, sctx.iside = inv, imid, iside
	sctx.delta, sctx.itheta, sctx.qalloc = delta, itheta, qalloc
}

// --- band quantizers (decode path, resynth always on) ---

func (ctx *bandCtx) quantBandN1(X, Y, lowbandOut []float32) uint32 {
	stereo := 0
	if Y != nil {
		stereo = 1
	}
	x := X
	for c := 0; c <= stereo; c++ {
		sign := 0
		if ctx.remainingBits >= 1<<bitRes {
			sign = int(ctx.ec.decodeRawBits(1))
			ctx.remainingBits -= 1 << bitRes
		}
		if sign != 0 {
			x[0] = -1
		} else {
			x[0] = 1
		}
		x = Y
	}
	if lowbandOut != nil {
		lowbandOut[0] = X[0]
	}
	return 1
}

// quantPartition decodes a mono partition, recursively splitting it into
// mid/side halves when the budget is high enough (libopus bands.c).
func (ctx *bandCtx) quantPartition(X []float32, N, b, B int, lowband []float32, LM int, gain float32, fill int) uint32 {
	i := ctx.i
	spread := ctx.spread
	cacheOff := celtCacheIndex[(LM+1)*celtNBands+i]
	cache := celtCacheBits[cacheOff:]
	B0 := B
	var cm uint32

	if LM != -1 && b > int(cache[cache[0]])+12 && N > 2 {
		var sctx splitCtx
		N >>= 1
		Y := X[N:]
		LM--
		if B == 1 {
			fill = (fill & 1) | (fill << 1)
		}
		B = (B + 1) >> 1
		ctx.computeTheta(&sctx, X, Y, N, &b, B, B0, LM, 0, &fill)
		imid, iside := sctx.imid, sctx.iside
		delta, itheta, qalloc := sctx.delta, sctx.itheta, sctx.qalloc
		mid := float32(imid) * (1.0 / 32768)
		side := float32(iside) * (1.0 / 32768)

		if B0 > 1 && (itheta&0x3fff) != 0 {
			if itheta > 8192 {
				delta -= delta >> uint(4-LM)
			} else {
				delta = min(0, delta+(N<<bitRes>>uint(5-LM)))
			}
		}
		mbits := max(0, min(b, (b-delta)/2))
		sbits := b - mbits
		ctx.remainingBits -= qalloc

		var nextLowband2 []float32
		if lowband != nil {
			nextLowband2 = lowband[N:]
		}
		rebalance := ctx.remainingBits
		if mbits >= sbits {
			cm = ctx.quantPartition(X, N, mbits, B, lowband, LM, gain*mid, fill)
			rebalance = mbits - (rebalance - ctx.remainingBits)
			if rebalance > 3<<bitRes && itheta != 0 {
				sbits += rebalance - (3 << bitRes)
			}
			cm |= ctx.quantPartition(Y, N, sbits, B, nextLowband2, LM, gain*side, fill>>uint(B)) << uint(B0>>1)
		} else {
			cm = ctx.quantPartition(Y, N, sbits, B, nextLowband2, LM, gain*side, fill>>uint(B)) << uint(B0>>1)
			rebalance = sbits - (rebalance - ctx.remainingBits)
			if rebalance > 3<<bitRes && itheta != 16384 {
				mbits += rebalance - (3 << bitRes)
			}
			cm |= ctx.quantPartition(X, N, mbits, B, lowband, LM, gain*mid, fill)
		}
		return cm
	}

	// No-split base case.
	q := bits2pulses(i, LM, b)
	currBits := pulses2bits(i, LM, q)
	ctx.remainingBits -= currBits
	for ctx.remainingBits < 0 && q > 0 {
		ctx.remainingBits += currBits
		q--
		currBits = pulses2bits(i, LM, q)
		ctx.remainingBits -= currBits
	}
	if q != 0 {
		K := getPulses(q)
		cm = algUnquant(X, N, K, spread, B, ctx.ec, gain, ctx.iy, ctx.u)
	} else {
		// Fill an unpulsed band with folded spectrum or noise.
		cmMask := uint32(1<<uint(B)) - 1
		fill &= int(cmMask)
		if fill == 0 {
			for j := 0; j < N; j++ {
				X[j] = 0
			}
		} else {
			if lowband == nil {
				for j := 0; j < N; j++ {
					ctx.seed = celtLCGRand(ctx.seed)
					X[j] = float32(int32(ctx.seed) >> 20)
				}
				cm = cmMask
			} else {
				const tmp = float32(1.0 / 256)
				for j := 0; j < N; j++ {
					ctx.seed = celtLCGRand(ctx.seed)
					if ctx.seed&0x8000 != 0 {
						X[j] = lowband[j] + tmp
					} else {
						X[j] = lowband[j] - tmp
					}
				}
				cm = uint32(fill)
			}
			renormaliseVector(X, N, gain)
		}
	}
	return cm
}

// bitInterleaveTable / bitDeinterleaveTable drive the collapse-mask bookkeeping
// through recombine steps (libopus bands.c).
var bitInterleaveTable = [16]byte{0, 1, 1, 1, 2, 3, 3, 3, 2, 3, 3, 3, 2, 3, 3, 3}
var bitDeinterleaveTable = [16]byte{
	0x00, 0x03, 0x0C, 0x0F, 0x30, 0x33, 0x3C, 0x3F,
	0xC0, 0xC3, 0xCC, 0xCF, 0xF0, 0xF3, 0xFC, 0xFF,
}

// quantBand decodes a mono band, handling transient time/frequency resolution
// changes around the PVQ partition decode (libopus bands.c quant_band).
func (ctx *bandCtx) quantBand(X []float32, N, b, B int, lowband []float32, LM int,
	lowbandOut []float32, gain float32, lowbandScratch []float32, fill int) uint32 {
	N0 := N
	NB := N
	B0 := B
	timeDivide := 0
	recombine := 0
	longBlocks := b2i(B0 == 1)
	tfChange := ctx.tfChange
	var cm uint32

	NB /= B
	if N == 1 {
		return ctx.quantBandN1(X, nil, lowbandOut)
	}
	if tfChange > 0 {
		recombine = tfChange
	}
	if lowbandScratch != nil && lowband != nil && (recombine != 0 || ((NB&1) == 0 && tfChange < 0) || B0 > 1) {
		copy(lowbandScratch[:N], lowband[:N])
		lowband = lowbandScratch
	}
	for k := 0; k < recombine; k++ {
		if lowband != nil {
			haar1(lowband, N>>uint(k), 1<<uint(k))
		}
		fill = int(bitInterleaveTable[fill&0xF]) | int(bitInterleaveTable[fill>>4])<<2
	}
	B >>= recombine
	NB <<= recombine
	for (NB&1) == 0 && tfChange < 0 {
		if lowband != nil {
			haar1(lowband, NB, B)
		}
		fill |= fill << uint(B)
		B <<= 1
		NB >>= 1
		timeDivide++
		tfChange++
	}
	B0 = B
	NB0 := NB
	if B0 > 1 {
		if lowband != nil {
			deinterleaveHadamard(lowband, NB>>uint(recombine), B0<<uint(recombine), longBlocks)
		}
	}

	cm = ctx.quantPartition(X, N, b, B, lowband, LM, gain, fill)

	// Resynthesis (always on for decode): undo the time/frequency reorderings.
	if B0 > 1 {
		interleaveHadamard(X, NB>>uint(recombine), B0<<uint(recombine), longBlocks)
	}
	NB = NB0
	B = B0
	for k := 0; k < timeDivide; k++ {
		B >>= 1
		NB <<= 1
		cm |= cm >> uint(B)
		haar1(X, NB, B)
	}
	for k := 0; k < recombine; k++ {
		cm = uint32(bitDeinterleaveTable[cm&0xFF])
		haar1(X, N0>>uint(k), 1<<uint(k))
	}
	B <<= recombine
	if lowbandOut != nil {
		n := float32(math.Sqrt(float64(N0)))
		for j := 0; j < N0; j++ {
			lowbandOut[j] = n * X[j]
		}
	}
	cm &= (1 << uint(B)) - 1
	return cm
}

// quantBandStereo decodes a stereo band, decoding the mid/side angle and, for
// N=2, the one-bit side sign, then merging back to left/right (libopus bands.c).
func (ctx *bandCtx) quantBandStereo(X, Y []float32, N, b, B int, lowband []float32, LM int,
	lowbandOut, lowbandScratch []float32, fill int) uint32 {
	var sctx splitCtx
	var cm uint32

	if N == 1 {
		return ctx.quantBandN1(X, Y, lowbandOut)
	}
	origFill := fill
	ctx.computeTheta(&sctx, X, Y, N, &b, B, B, LM, 1, &fill)
	inv := sctx.inv
	imid, iside := sctx.imid, sctx.iside
	delta, itheta, qalloc := sctx.delta, sctx.itheta, sctx.qalloc
	mid := float32(imid) * (1.0 / 32768)
	side := float32(iside) * (1.0 / 32768)

	if N == 2 {
		mbits := b
		sbits := 0
		if itheta != 0 && itheta != 16384 {
			sbits = 1 << bitRes
		}
		mbits -= sbits
		c := b2i(itheta > 8192)
		ctx.remainingBits -= qalloc + sbits
		x2, y2 := X, Y
		if c != 0 {
			x2, y2 = Y, X
		}
		sign := 0
		if sbits != 0 {
			sign = int(ctx.ec.decodeRawBits(1))
		}
		signf := 1 - 2*sign
		cm = ctx.quantBand(x2, N, mbits, B, lowband, LM, lowbandOut, 1.0, lowbandScratch, origFill)
		y2[0] = float32(-signf) * x2[1]
		y2[1] = float32(signf) * x2[0]
		// resynth
		X[0] = mid * X[0]
		X[1] = mid * X[1]
		Y[0] = side * Y[0]
		Y[1] = side * Y[1]
		t := X[0]
		X[0] = t - Y[0]
		Y[0] = t + Y[0]
		t = X[1]
		X[1] = t - Y[1]
		Y[1] = t + Y[1]
	} else {
		mbits := max(0, min(b, (b-delta)/2))
		sbits := b - mbits
		ctx.remainingBits -= qalloc
		rebalance := ctx.remainingBits
		if mbits >= sbits {
			cm = ctx.quantBand(X, N, mbits, B, lowband, LM, lowbandOut, 1.0, lowbandScratch, fill)
			rebalance = mbits - (rebalance - ctx.remainingBits)
			if rebalance > 3<<bitRes && itheta != 0 {
				sbits += rebalance - (3 << bitRes)
			}
			cm |= ctx.quantBand(Y, N, sbits, B, nil, LM, nil, side, nil, fill>>uint(B))
		} else {
			cm = ctx.quantBand(Y, N, sbits, B, nil, LM, nil, side, nil, fill>>uint(B))
			rebalance = sbits - (rebalance - ctx.remainingBits)
			if rebalance > 3<<bitRes && itheta != 16384 {
				mbits += rebalance - (3 << bitRes)
			}
			cm |= ctx.quantBand(X, N, mbits, B, lowband, LM, lowbandOut, 1.0, lowbandScratch, fill)
		}
		// resynth
		stereoMerge(X, Y, mid, N)
	}
	if inv != 0 {
		for j := 0; j < N; j++ {
			Y[j] = -Y[j]
		}
	}
	return cm
}

// specialHybridFolding duplicates first-band folding data so the second band can
// fold (libopus bands.c; a no-op for CELT-only start==0).
func specialHybridFolding(norm, norm2 []float32, start, M, dualStereo int) {
	n1 := M * (int(celtEBands[start+1]) - int(celtEBands[start]))
	n2 := M * (int(celtEBands[start+2]) - int(celtEBands[start+1]))
	copy(norm[n1:n1+(n2-n1)], norm[2*n1-n2:2*n1-n2+(n2-n1)])
	if dualStereo != 0 {
		copy(norm2[n1:n1+(n2-n1)], norm2[2*n1-n2:2*n1-n2+(n2-n1)])
	}
}

// quantAllBands drives the per-band shape decode across the frame, managing the
// folding "lowband" history and stereo mode (libopus bands.c quant_all_bands;
// decode path).
func quantAllBands(start, end int, X, Y []float32, collapseMasks []byte, bandE []float32,
	pulses []int, shortBlocks, spread, dualStereo, intensity int, tfRes []int,
	totalBits, balance int, ec *rangeDecoder, LM, codedBands int, seed *uint32, disableInv int, iy []int, u []uint32) {

	M := 1 << LM
	B := 1
	if shortBlocks != 0 {
		B = M
	}
	normOffset := M * int(celtEBands[start])
	normLen := M*int(celtEBands[celtNBands-1]) - normOffset
	C := 1
	if Y != nil {
		C = 2
	}
	norm := make([]float32, C*normLen)
	norm2 := norm[normLen:]

	lowbandOffset := 0
	updateLowband := true
	ctx := bandCtx{
		bandE: bandE, ec: ec, intensity: intensity, spread: spread,
		seed: *seed, disableInv: disableInv, iy: iy, u: u,
	}
	// A scratch band at the end of X used for lowband copies during decode.
	lowbandScratch := X[M*int(celtEBands[celtNBands-1]):]
	ctx.avoidSplit = b2i(B > 1)

	for i := start; i < end; i++ {
		ctx.i = i
		last := i == end-1
		Xb := X[M*int(celtEBands[i]):]
		var Yb []float32
		if Y != nil {
			Yb = Y[M*int(celtEBands[i]):]
		}
		N := M*int(celtEBands[i+1]) - M*int(celtEBands[i])
		tell := ec.tellFrac()
		if i != start {
			balance -= tell
		}
		remainingBits := totalBits - tell - 1
		ctx.remainingBits = remainingBits

		var b int
		if i <= codedBands-1 {
			currBalance := balance / min(3, codedBands-i)
			b = max(0, min(16383, min(remainingBits+1, pulses[i]+currBalance)))
		} else {
			b = 0
		}

		if (M*int(celtEBands[i])-N >= M*int(celtEBands[start]) || i == start+1) && (updateLowband || lowbandOffset == 0) {
			lowbandOffset = i
		}
		if i == start+1 {
			specialHybridFolding(norm, norm2, start, M, dualStereo)
		}

		ctx.tfChange = tfRes[i]

		var xCM, yCM uint32
		effectiveLowband := -1
		if lowbandOffset != 0 && (spread != spreadAggr || B > 1 || ctx.tfChange < 0) {
			effectiveLowband = max(0, M*int(celtEBands[lowbandOffset])-normOffset-N)
			foldStart := lowbandOffset
			for {
				foldStart--
				if !(M*int(celtEBands[foldStart]) > effectiveLowband+normOffset) {
					break
				}
			}
			foldEnd := lowbandOffset - 1
			for {
				foldEnd++
				if !(foldEnd < i && M*int(celtEBands[foldEnd]) < effectiveLowband+normOffset+N) {
					break
				}
			}
			for fi := foldStart; fi < foldEnd; fi++ {
				xCM |= uint32(collapseMasks[fi*C+0])
				yCM |= uint32(collapseMasks[fi*C+C-1])
			}
		} else {
			xCM = (1 << uint(B)) - 1
			yCM = (1 << uint(B)) - 1
		}

		if dualStereo != 0 && i == intensity {
			dualStereo = 0
			for j := 0; j < M*int(celtEBands[i])-normOffset; j++ {
				norm[j] = 0.5 * (norm[j] + norm2[j])
			}
		}

		var lowOut []float32
		if !last {
			lowOut = norm[M*int(celtEBands[i])-normOffset:]
		}
		var scratch []float32
		if !(i >= celtNBands || last) { // effEBands == nbEBands for the standard mode
			scratch = lowbandScratch
		}

		if dualStereo != 0 {
			var lbX, lbY []float32
			if effectiveLowband != -1 {
				lbX = norm[effectiveLowband:]
				lbY = norm2[effectiveLowband:]
			}
			var lowOut2 []float32
			if !last {
				lowOut2 = norm2[M*int(celtEBands[i])-normOffset:]
			}
			xCM = ctx.quantBand(Xb, N, b/2, B, lbX, LM, lowOut, 1.0, scratch, int(xCM))
			yCM = ctx.quantBand(Yb, N, b/2, B, lbY, LM, lowOut2, 1.0, scratch, int(yCM))
		} else if Yb != nil {
			var lb []float32
			if effectiveLowband != -1 {
				lb = norm[effectiveLowband:]
			}
			xCM = ctx.quantBandStereo(Xb, Yb, N, b, B, lb, LM, lowOut, scratch, int(xCM|yCM))
			yCM = xCM
		} else {
			var lb []float32
			if effectiveLowband != -1 {
				lb = norm[effectiveLowband:]
			}
			xCM = ctx.quantBand(Xb, N, b, B, lb, LM, lowOut, 1.0, scratch, int(xCM|yCM))
			yCM = xCM
		}
		collapseMasks[i*C+0] = byte(xCM)
		collapseMasks[i*C+C-1] = byte(yCM)
		balance += pulses[i] + tell
		updateLowband = b > (N << bitRes)
		ctx.avoidSplit = 0
	}
	*seed = ctx.seed
}
