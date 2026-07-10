package opus

// SILK gain and LTP quantization, ported from libopus silk/gain_quant.c
// (encode half; the dequantizer lives in silk.go), silk/quant_LTP_gains.c,
// and silk/VQ_WMat_EC.c.

const (
	gainQuantOffset      = (minQGainDB*128)/6 + 16*128
	gainQuantScaleQ16    = (65536 * (nLevelsQGain - 1)) / (((maxQGainDB - minQGainDB) * 128) / 6)
	gainQuantInvScaleQ16 = (65536 * (((maxQGainDB - minQGainDB) * 128) / 6)) / (nLevelsQGain - 1)
)

// silkGainsQuant quantizes the subframe gains uniformly on a log scale with
// hysteresis (silk_gains_quant).
func silkGainsQuant(ind []int8, gainQ16 []int32, prevInd *int8, conditional bool, nbSubfr int) {
	for k := 0; k < nbSubfr; k++ {
		v := silkSMULWB(gainQuantScaleQ16, silkLin2Log(gainQ16[k])-gainQuantOffset)
		if v < int32(*prevInd) {
			v++
		}
		v = silkLIMIT(v, 0, nLevelsQGain-1)
		if k == 0 && !conditional {
			v = silkLIMIT(v, int32(*prevInd)+minDeltaGain, nLevelsQGain-1)
			*prevInd = int8(v)
		} else {
			v -= int32(*prevInd)
			doubleStepSizeThreshold := int32(2*maxDeltaGain - nLevelsQGain + int(*prevInd))
			if v > doubleStepSizeThreshold {
				v = doubleStepSizeThreshold + silkRSHIFT(v-doubleStepSizeThreshold+1, 1)
			}
			v = silkLIMIT(v, minDeltaGain, maxDeltaGain)
			if v > doubleStepSizeThreshold {
				*prevInd = int8(int32(*prevInd) + silkLSHIFT(v, 1) - doubleStepSizeThreshold)
				*prevInd = int8(silkMinInt(int32(*prevInd), nLevelsQGain-1))
			} else {
				*prevInd = int8(int32(*prevInd) + v)
			}
			v -= minDeltaGain
		}
		ind[k] = int8(v)
		gainQ16[k] = silkLog2Lin(silkMinInt(silkSMULWB(gainQuantInvScaleQ16, int32(*prevInd))+gainQuantOffset, 3967))
	}
}

// silkGainsID computes a unique identifier of a gain index vector
// (silk_gains_ID).
func silkGainsID(ind []int8, nbSubfr int) int32 {
	var gainsID int32
	for k := 0; k < nbSubfr; k++ {
		gainsID = int32(ind[k]) + silkLSHIFT32(gainsID, 8)
	}
	return gainsID
}

// LTP codebook wiring (silk/tables_LTP.c).
var silkLTPGainBITSQ5Ptrs = [][]uint8{silk_LTP_gain_BITS_Q5_0, silk_LTP_gain_BITS_Q5_1, silk_LTP_gain_BITS_Q5_2}
var silkLTPVQGainPtrsQ7 = [][]uint8{silk_LTP_gain_vq_0_gain, silk_LTP_gain_vq_1_gain, silk_LTP_gain_vq_2_gain}
var silkLTPVQSizes = []int{8, 16, 32}

// silkVQWMatEC is the entropy-constrained matrix-weighted VQ over 5-element
// LTP vectors (silk_VQ_WMat_EC_c).
func silkVQWMatEC(ind *int8, resNrgQ15, rateDistQ8 *int32, gainQ7 *int32,
	xxQ17, xXQ17 []int32, cbQ7 [][]int8, cbGainQ7 []uint8, clQ5 []uint8,
	subfrLen int, maxGainQ7 int32, L int) {

	var negxXQ24 [5]int32
	for i := 0; i < 5; i++ {
		negxXQ24[i] = -silkLSHIFT32(xXQ17[i], 7)
	}

	*rateDistQ8 = silkInt32Max
	*resNrgQ15 = silkInt32Max
	*ind = 0
	for k := 0; k < L; k++ {
		cbRow := cbQ7[k]
		gainTmpQ7 := int32(cbGainQ7[k])
		sum1Q15 := silkFixConst(1.001, 15)
		penalty := silkLSHIFT32(silkMaxInt(gainTmpQ7-maxGainQ7, 0), 11)

		// Quantization error: 1 - 2*xX*cb + cb'*XX*cb, exploiting symmetry.
		sum2Q24 := silkMLA(negxXQ24[0], xxQ17[1], int32(cbRow[1]))
		sum2Q24 = silkMLA(sum2Q24, xxQ17[2], int32(cbRow[2]))
		sum2Q24 = silkMLA(sum2Q24, xxQ17[3], int32(cbRow[3]))
		sum2Q24 = silkMLA(sum2Q24, xxQ17[4], int32(cbRow[4]))
		sum2Q24 = silkLSHIFT32(sum2Q24, 1)
		sum2Q24 = silkMLA(sum2Q24, xxQ17[0], int32(cbRow[0]))
		sum1Q15 = silkSMLAWB(sum1Q15, sum2Q24, int32(cbRow[0]))

		sum2Q24 = silkMLA(negxXQ24[1], xxQ17[7], int32(cbRow[2]))
		sum2Q24 = silkMLA(sum2Q24, xxQ17[8], int32(cbRow[3]))
		sum2Q24 = silkMLA(sum2Q24, xxQ17[9], int32(cbRow[4]))
		sum2Q24 = silkLSHIFT32(sum2Q24, 1)
		sum2Q24 = silkMLA(sum2Q24, xxQ17[6], int32(cbRow[1]))
		sum1Q15 = silkSMLAWB(sum1Q15, sum2Q24, int32(cbRow[1]))

		sum2Q24 = silkMLA(negxXQ24[2], xxQ17[13], int32(cbRow[3]))
		sum2Q24 = silkMLA(sum2Q24, xxQ17[14], int32(cbRow[4]))
		sum2Q24 = silkLSHIFT32(sum2Q24, 1)
		sum2Q24 = silkMLA(sum2Q24, xxQ17[12], int32(cbRow[2]))
		sum1Q15 = silkSMLAWB(sum1Q15, sum2Q24, int32(cbRow[2]))

		sum2Q24 = silkMLA(negxXQ24[3], xxQ17[19], int32(cbRow[4]))
		sum2Q24 = silkLSHIFT32(sum2Q24, 1)
		sum2Q24 = silkMLA(sum2Q24, xxQ17[18], int32(cbRow[3]))
		sum1Q15 = silkSMLAWB(sum1Q15, sum2Q24, int32(cbRow[3]))

		sum2Q24 = silkLSHIFT32(negxXQ24[4], 1)
		sum2Q24 = silkMLA(sum2Q24, xxQ17[24], int32(cbRow[4]))
		sum1Q15 = silkSMLAWB(sum1Q15, sum2Q24, int32(cbRow[4]))

		if sum1Q15 >= 0 {
			// 6 dB == 1 bit/sample under the high-rate assumption.
			bitsResQ8 := silkSMULBB(int32(subfrLen), silkLin2Log(sum1Q15+penalty)-15<<7)
			bitsTotQ8 := bitsResQ8 + silkLSHIFT32(int32(clQ5[k]), 3-1)
			if bitsTotQ8 <= *rateDistQ8 {
				*rateDistQ8 = bitsTotQ8
				*resNrgQ15 = sum1Q15 + penalty
				*ind = int8(k)
				*gainQ7 = gainTmpQ7
			}
		}
	}
}

// silkQuantLTPGains picks the best LTP codebook and quantizes the per-subframe
// LTP gain vectors (silk_quant_LTP_gains).
func silkQuantLTPGains(bQ14 []int16, cbkIndex []int8, periodicityIndex *int8,
	sumLogGainQ7 *int32, predGainDBQ7 *int32, xxQ17, xXQ17 []int32, subfrLen, nbSubfr int) {

	var tempIdx [silkMaxNBSubfr]int8
	minRateDistQ7 := int32(silkInt32Max)
	bestSumLogGainQ7 := int32(0)
	// Deliberately reference-faithful: the residual energy driving
	// predGainDBQ7 below is the LAST codebook's accumulation, not the
	// winner's (quant_LTP_gains.c leaves res_nrg_Q15 at its k==2 value).
	resNrgQ15 := int32(0)
	for k := 0; k < 3; k++ {
		gainSafety := silkFixConst(0.4, 7)
		clPtrQ5 := silkLTPGainBITSQ5Ptrs[k]
		cbkPtrQ7 := silkLTPVQPtrsQ7[k]
		cbkGainPtrQ7 := silkLTPVQGainPtrsQ7[k]
		cbkSize := silkLTPVQSizes[k]

		resNrgQ15 = 0
		rateDistQ7 := int32(0)
		sumLogGainTmpQ7 := *sumLogGainQ7
		for j := 0; j < nbSubfr; j++ {
			maxGainQ7 := silkLog2Lin(silkFixConst(maxSumLogGainDB/6.0, 7)-sumLogGainTmpQ7+silkFixConst(7, 7)) - gainSafety
			var resNrgQ15Subfr, rateDistQ7Subfr, gainQ7 int32
			silkVQWMatEC(&tempIdx[j], &resNrgQ15Subfr, &rateDistQ7Subfr, &gainQ7,
				xxQ17[j*silkLTPOrder*silkLTPOrder:], xXQ17[j*silkLTPOrder:],
				cbkPtrQ7, cbkGainPtrQ7, clPtrQ5, subfrLen, maxGainQ7, cbkSize)
			resNrgQ15 = silkADDPOSSAT32(resNrgQ15, resNrgQ15Subfr)
			rateDistQ7 = silkADDPOSSAT32(rateDistQ7, rateDistQ7Subfr)
			sumLogGainTmpQ7 = silkMaxInt(0, sumLogGainTmpQ7+silkLin2Log(gainSafety+gainQ7)-silkFixConst(7, 7))
		}
		if rateDistQ7 <= minRateDistQ7 {
			minRateDistQ7 = rateDistQ7
			*periodicityIndex = int8(k)
			copy(cbkIndex[:nbSubfr], tempIdx[:nbSubfr])
			bestSumLogGainQ7 = sumLogGainTmpQ7
		}
	}

	cbkPtrQ7 := silkLTPVQPtrsQ7[*periodicityIndex]
	for j := 0; j < nbSubfr; j++ {
		for k := 0; k < silkLTPOrder; k++ {
			bQ14[j*silkLTPOrder+k] = int16(silkLSHIFT(int32(cbkPtrQ7[cbkIndex[j]][k]), 7))
		}
	}

	if nbSubfr == 2 {
		resNrgQ15 = silkRSHIFT(resNrgQ15, 1)
	} else {
		resNrgQ15 = silkRSHIFT(resNrgQ15, 2)
	}
	*sumLogGainQ7 = bestSumLogGainQ7
	*predGainDBQ7 = silkSMULBB(-3, silkLin2Log(resNrgQ15)-15<<7)
}
