package opus

// SILK float-analysis primitives, ported from libopus silk/float/:
// SigProc_FLP.h, energy_FLP.c, inner_product_FLP.c, scale_vector_FLP.c,
// scale_copy_vector_FLP.c, sort_FLP.c, bwexpander_FLP.c,
// apply_sine_window_FLP.c, autocorrelation_FLP.c, schur_FLP.c, k2a_FLP.c,
// LPC_inv_pred_gain_FLP.c, LPC_analysis_filter_FLP.c,
// warped_autocorrelation_FLP.c, and burg_modified_FLP.c.
// silk_float is float32; accumulators the reference declares double stay
// float64.

import "math"

// silkFloat2Int rounds to nearest like the reference's lrintf under the
// default rounding mode (silk_float2int).
func silkFloat2Int(x float32) int32 {
	return int32(math.RoundToEven(float64(x)))
}

// silkFloat2ShortArray converts and saturates to int16 (silk_float2short_array).
func silkFloat2ShortArray(out []int16, in []float32, length int) {
	for k := 0; k < length; k++ {
		out[k] = int16(silkSAT16(silkFloat2Int(in[k])))
	}
}

// silkShort2FloatArray widens int16 samples to float32 (silk_short2float_array).
func silkShort2FloatArray(out []float32, in []int16, length int) {
	for k := 0; k < length; k++ {
		out[k] = float32(in[k])
	}
}

// silkSigmoid is 1/(1+exp(-x)) (silk_sigmoid).
func silkSigmoid(x float32) float32 {
	return float32(1.0 / (1.0 + math.Exp(-float64(x))))
}

// silkLog2F is log2 through log10, matching the reference's constant
// (silk_log2).
func silkLog2F(x float64) float32 {
	return float32(3.32192809488736 * math.Log10(x))
}

// silkEnergyFLP is the sum of squares with a float64 accumulator
// (silk_energy_FLP).
func silkEnergyFLP(data []float32, dataSize int) float64 {
	var result float64
	for i := 0; i < dataSize; i++ {
		result += float64(data[i]) * float64(data[i])
	}
	return result
}

// silkInnerProductFLP is the inner product with a float64 accumulator
// (silk_inner_product_FLP_c).
func silkInnerProductFLP(data1, data2 []float32, dataSize int) float64 {
	var result float64
	for i := 0; i < dataSize; i++ {
		result += float64(data1[i]) * float64(data2[i])
	}
	return result
}

// silkScaleVectorFLP multiplies a vector by a constant (silk_scale_vector_FLP).
func silkScaleVectorFLP(data []float32, gain float32, dataSize int) {
	for i := 0; i < dataSize; i++ {
		data[i] *= gain
	}
}

// silkScaleCopyVectorFLP copies with a gain (silk_scale_copy_vector_FLP).
func silkScaleCopyVectorFLP(dataOut, dataIn []float32, gain float32, dataSize int) {
	for i := 0; i < dataSize; i++ {
		dataOut[i] = gain * dataIn[i]
	}
}

// silkInsertionSortDecreasingFLP sorts a decreasing, tracking indices, with
// only the first K positions guaranteed (silk_insertion_sort_decreasing_FLP).
func silkInsertionSortDecreasingFLP(a []float32, idx []int, L, K int) {
	for i := 0; i < K; i++ {
		idx[i] = i
	}
	for i := 1; i < K; i++ {
		value := a[i]
		j := i - 1
		for ; j >= 0 && value > a[j]; j-- {
			a[j+1] = a[j]
			idx[j+1] = idx[j]
		}
		a[j+1] = value
		idx[j+1] = i
	}
	for i := K; i < L; i++ {
		value := a[i]
		if value > a[K-1] {
			j := K - 2
			for ; j >= 0 && value > a[j]; j-- {
				a[j+1] = a[j]
				idx[j+1] = idx[j]
			}
			a[j+1] = value
			idx[j+1] = i
		}
	}
}

// silkBWExpanderFLP chirps an AR filter (silk_bwexpander_FLP).
func silkBWExpanderFLP(ar []float32, d int, chirp float32) {
	cfac := chirp
	for i := 0; i < d-1; i++ {
		ar[i] *= cfac
		cfac *= chirp
	}
	ar[d-1] *= cfac
}

// silkApplySineWindowFLP applies a sine window: type 1 rises 0..pi/2, type 2
// falls pi/2..pi (silk_apply_sine_window_FLP). Length must be a multiple of 4.
func silkApplySineWindowFLP(pxWin, px []float32, winType, length int) {
	freq := float32(math.Pi) / float32(length+1)
	c := 2.0 - freq*freq
	var S0, S1 float32
	if winType < 2 {
		S0 = 0.0
		S1 = freq
	} else {
		S0 = 1.0
		S1 = 0.5 * c
	}
	for k := 0; k < length; k += 4 {
		pxWin[k+0] = px[k+0] * 0.5 * (S0 + S1)
		pxWin[k+1] = px[k+1] * S1
		S0 = c*S1 - S0
		pxWin[k+2] = px[k+2] * 0.5 * (S1 + S0)
		pxWin[k+3] = px[k+3] * S0
		S1 = c*S0 - S1
	}
}

// silkAutocorrelationFLP computes autocorrelation taps (silk_autocorrelation_FLP).
func silkAutocorrelationFLP(results []float32, inputData []float32, inputDataSize, correlationCount int) {
	if correlationCount > inputDataSize {
		correlationCount = inputDataSize
	}
	for i := 0; i < correlationCount; i++ {
		results[i] = float32(silkInnerProductFLP(inputData, inputData[i:], inputDataSize-i))
	}
}

// silkSchurFLP computes reflection coefficients from autocorrelations and
// returns the residual energy (silk_schur_FLP).
func silkSchurFLP(reflCoef []float32, autoCorr []float32, order int) float32 {
	var C [silkMaxOrderLPC + 1][2]float64
	for k := 0; k <= order; k++ {
		C[k][0] = float64(autoCorr[k])
		C[k][1] = float64(autoCorr[k])
	}
	for k := 0; k < order; k++ {
		den := C[0][1]
		if den < 1e-9 {
			den = 1e-9
		}
		rcTmp := -C[k+1][0] / den
		reflCoef[k] = float32(rcTmp)
		for n := 0; n < order-k; n++ {
			ctmp1 := C[n+k+1][0]
			ctmp2 := C[n][1]
			C[n+k+1][0] = ctmp1 + ctmp2*rcTmp
			C[n][1] = ctmp2 + ctmp1*rcTmp
		}
	}
	return float32(C[0][1])
}

// silkK2AFLP converts reflection coefficients to prediction coefficients
// (silk_k2a_FLP).
func silkK2AFLP(A []float32, rc []float32, order int) {
	for k := 0; k < order; k++ {
		rck := rc[k]
		for n := 0; n < (k+1)>>1; n++ {
			tmp1 := A[n]
			tmp2 := A[k-n-1]
			A[n] = tmp1 + tmp2*rck
			A[k-n-1] = tmp2 + tmp1*rck
		}
		A[k] = -rck
	}
}

// silkLPCInversePredGainFLP computes the inverse prediction gain and returns
// 0 for unstable filters (silk_LPC_inverse_pred_gain_FLP).
func silkLPCInversePredGainFLP(A []float32, order int) float32 {
	var atmp [silkMaxOrderLPC]float32
	copy(atmp[:order], A[:order])
	invGain := 1.0
	for k := order - 1; k > 0; k-- {
		rc := -float64(atmp[k])
		rcMult1 := 1.0 - rc*rc
		invGain *= rcMult1
		if invGain*maxPredictionPowerGain < 1.0 {
			return 0.0
		}
		rcMult2 := 1.0 / rcMult1
		for n := 0; n < (k+1)>>1; n++ {
			tmp1 := float64(atmp[n])
			tmp2 := float64(atmp[k-n-1])
			atmp[n] = float32((tmp1 - tmp2*rc) * rcMult2)
			atmp[k-n-1] = float32((tmp2 - tmp1*rc) * rcMult2)
		}
	}
	rc := -float64(atmp[0])
	rcMult1 := 1.0 - rc*rc
	invGain *= rcMult1
	if invGain*maxPredictionPowerGain < 1.0 {
		return 0.0
	}
	return float32(invGain)
}

// silkLPCAnalysisFilterFLP runs the zero-state whitening filter; the first
// order output samples are zeroed (silk_LPC_analysis_filter_FLP).
func silkLPCAnalysisFilterFLP(rLPC []float32, predCoef []float32, s []float32, length, order int) {
	for ix := order; ix < length; ix++ {
		var lpcPred float32
		for j := 0; j < order; j++ {
			lpcPred += s[ix-1-j] * predCoef[j]
		}
		rLPC[ix] = s[ix] - lpcPred
	}
	for i := 0; i < order; i++ {
		rLPC[i] = 0
	}
}

// silkWarpedAutocorrelationFLP computes autocorrelations on a warped
// frequency axis (silk_warped_autocorrelation_FLP).
func silkWarpedAutocorrelationFLP(corr []float32, input []float32, warping float32, length, order int) {
	var state [maxShapeLPCOrder + 1]float64
	var C [maxShapeLPCOrder + 1]float64
	w := float64(warping)
	for n := 0; n < length; n++ {
		tmp1 := float64(input[n])
		for i := 0; i < order; i += 2 {
			tmp2 := state[i] + w*state[i+1] - w*tmp1
			state[i] = tmp1
			C[i] += state[0] * tmp1
			tmp1 = state[i+1] + w*state[i+2] - w*tmp2
			state[i+1] = tmp2
			C[i+1] += state[0] * tmp2
		}
		state[order] = tmp1
		C[order] += state[0] * tmp1
	}
	for i := 0; i <= order; i++ {
		corr[i] = float32(C[i])
	}
}

// silkBurgModifiedFLP computes prediction coefficients with Burg's method,
// bounded by a minimum inverse prediction gain, and returns the residual
// energy (silk_burg_modified_FLP).
func silkBurgModifiedFLP(A []float32, x []float32, minInvGain float32, subfrLength, nbSubfr, D int) float32 {
	var cFirstRow, cLastRow [silkMaxOrderLPC]float64
	var cAf, cAb [silkMaxOrderLPC + 1]float64
	var af [silkMaxOrderLPC]float64

	c0 := silkEnergyFLP(x, nbSubfr*subfrLength)
	for s := 0; s < nbSubfr; s++ {
		xPtr := x[s*subfrLength:]
		for n := 1; n < D+1; n++ {
			cFirstRow[n-1] += silkInnerProductFLP(xPtr, xPtr[n:], subfrLength-n)
		}
	}
	cLastRow = cFirstRow

	cAb[0] = c0 + findLPCCondFac*c0 + 1e-9
	cAf[0] = cAb[0]
	invGain := 1.0
	reachedMaxGain := false
	for n := 0; n < D; n++ {
		for s := 0; s < nbSubfr; s++ {
			xPtr := x[s*subfrLength:]
			tmp1 := float64(xPtr[n])
			tmp2 := float64(xPtr[subfrLength-n-1])
			for k := 0; k < n; k++ {
				cFirstRow[k] -= float64(xPtr[n]) * float64(xPtr[n-k-1])
				cLastRow[k] -= float64(xPtr[subfrLength-n-1]) * float64(xPtr[subfrLength-n+k])
				atmp := af[k]
				tmp1 += float64(xPtr[n-k-1]) * atmp
				tmp2 += float64(xPtr[subfrLength-n+k]) * atmp
			}
			for k := 0; k <= n; k++ {
				cAf[k] -= tmp1 * float64(xPtr[n-k])
				cAb[k] -= tmp2 * float64(xPtr[subfrLength-n+k-1])
			}
		}
		tmp1 := cFirstRow[n]
		tmp2 := cLastRow[n]
		for k := 0; k < n; k++ {
			atmp := af[k]
			tmp1 += cLastRow[n-k-1] * atmp
			tmp2 += cFirstRow[n-k-1] * atmp
		}
		cAf[n+1] = tmp1
		cAb[n+1] = tmp2

		num := cAb[n+1]
		nrgB := cAb[0]
		nrgF := cAf[0]
		for k := 0; k < n; k++ {
			atmp := af[k]
			num += cAb[n-k] * atmp
			nrgB += cAb[k+1] * atmp
			nrgF += cAf[k+1] * atmp
		}

		rc := -2.0 * num / (nrgF + nrgB)

		tmp1 = invGain * (1.0 - rc*rc)
		if tmp1 <= float64(minInvGain) {
			// Set the reflection coefficient to hit the max prediction gain
			// exactly.
			rc = math.Sqrt(1.0 - float64(minInvGain)/invGain)
			if num > 0 {
				rc = -rc
			}
			invGain = float64(minInvGain)
			reachedMaxGain = true
		} else {
			invGain = tmp1
		}

		for k := 0; k < (n+1)>>1; k++ {
			tmp1 := af[k]
			tmp2 := af[n-k-1]
			af[k] = tmp1 + rc*tmp2
			af[n-k-1] = tmp2 + rc*tmp1
		}
		af[n] = rc

		if reachedMaxGain {
			for k := n + 1; k < D; k++ {
				af[k] = 0.0
			}
			break
		}

		for k := 0; k <= n+1; k++ {
			tmp1 := cAf[k]
			cAf[k] += rc * cAb[n-k+1]
			cAb[n-k+1] += rc * tmp1
		}
	}

	var nrgF float64
	if reachedMaxGain {
		for k := 0; k < D; k++ {
			A[k] = float32(-af[k])
		}
		for s := 0; s < nbSubfr; s++ {
			c0 -= silkEnergyFLP(x[s*subfrLength:], D)
		}
		nrgF = c0 * invGain
	} else {
		nrgF = cAf[0]
		tmp1 := 1.0
		for k := 0; k < D; k++ {
			atmp := af[k]
			nrgF += cAf[k+1] * atmp
			tmp1 += atmp * atmp
			A[k] = float32(-atmp)
		}
		nrgF -= findLPCCondFac * c0 * tmp1
	}
	return float32(nrgF)
}
