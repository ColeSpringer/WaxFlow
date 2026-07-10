package opus

// SILK NLSF quantization, ported from libopus silk/process_NLSFs.c,
// NLSF_encode.c, NLSF_VQ.c, NLSF_del_dec_quant.c, NLSF_VQ_weights_laroia.c,
// interpolate.c, and sort.c. The decode direction lives in silk_nlsf.go;
// silk_NLSF_encode ends by running that decoder so the encoder continues
// from the exact quantized NLSFs the decoder will see.

const (
	nlsfQuantMaxAmpExt        = 10 // NLSF_QUANT_MAX_AMPLITUDE_EXT
	nlsfQuantDelDecStatesLog2 = 2
	nlsfQuantDelDecStates     = 1 << nlsfQuantDelDecStatesLog2
	nlsfWQ                    = 2 // NLSF_W_Q
)

// silkInsertionSortIncreasing sorts a increasing, tracking source indices;
// only the first K positions are guaranteed sorted (silk_insertion_sort_increasing).
func silkInsertionSortIncreasing(a []int32, idx []int, L, K int) {
	for i := 0; i < K; i++ {
		idx[i] = i
	}
	for i := 1; i < K; i++ {
		value := a[i]
		j := i - 1
		for ; j >= 0 && value < a[j]; j-- {
			a[j+1] = a[j]
			idx[j+1] = idx[j]
		}
		a[j+1] = value
		idx[j+1] = i
	}
	for i := K; i < L; i++ {
		value := a[i]
		if value < a[K-1] {
			j := K - 2
			for ; j >= 0 && value < a[j]; j-- {
				a[j+1] = a[j]
				idx[j+1] = idx[j]
			}
			a[j+1] = value
			idx[j+1] = i
		}
	}
}

// silkNLSFVQ computes the weighted quantization error of the input against
// every first-stage codebook vector (silk_NLSF_VQ).
func silkNLSFVQ(errQ24 []int32, inQ15 []int16, cbQ8 []uint8, wghtQ9 []int16, K, order int) {
	for i := 0; i < K; i++ {
		cb := cbQ8[i*order:]
		w := wghtQ9[i*order:]
		var sumErrorQ24, predQ24 int32
		for m := order - 2; m >= 0; m -= 2 {
			diffQ15 := int32(inQ15[m+1]) - silkLSHIFT(int32(cb[m+1]), 7)
			diffwQ24 := silkSMULBB(diffQ15, int32(w[m+1]))
			sumErrorQ24 += silkAbs32(diffwQ24 - predQ24>>1)
			predQ24 = diffwQ24

			diffQ15 = int32(inQ15[m]) - silkLSHIFT(int32(cb[m]), 7)
			diffwQ24 = silkSMULBB(diffQ15, int32(w[m]))
			sumErrorQ24 += silkAbs32(diffwQ24 - predQ24>>1)
			predQ24 = diffwQ24
		}
		errQ24[i] = sumErrorQ24
	}
}

// silkNLSFVQWeightsLaroia computes the Laroia low-complexity NLSF weights
// (silk_NLSF_VQ_weights_laroia), output in Q(nlsfWQ).
func silkNLSFVQWeightsLaroia(w []int16, nlsfQ15 []int16, D int) {
	tmp1 := silkMaxInt(int32(nlsfQ15[0]), 1)
	tmp1 = silkDIV32_16(1<<(15+nlsfWQ), tmp1)
	tmp2 := silkMaxInt(int32(nlsfQ15[1])-int32(nlsfQ15[0]), 1)
	tmp2 = silkDIV32_16(1<<(15+nlsfWQ), tmp2)
	w[0] = int16(silkMinInt(tmp1+tmp2, silkInt16Max))
	for k := 1; k < D-1; k += 2 {
		tmp1 = silkMaxInt(int32(nlsfQ15[k+1])-int32(nlsfQ15[k]), 1)
		tmp1 = silkDIV32_16(1<<(15+nlsfWQ), tmp1)
		w[k] = int16(silkMinInt(tmp1+tmp2, silkInt16Max))
		tmp2 = silkMaxInt(int32(nlsfQ15[k+2])-int32(nlsfQ15[k+1]), 1)
		tmp2 = silkDIV32_16(1<<(15+nlsfWQ), tmp2)
		w[k+1] = int16(silkMinInt(tmp1+tmp2, silkInt16Max))
	}
	tmp1 = silkMaxInt(1<<15-int32(nlsfQ15[D-1]), 1)
	tmp1 = silkDIV32_16(1<<(15+nlsfWQ), tmp1)
	w[D-1] = int16(silkMinInt(tmp1+tmp2, silkInt16Max))
}

// silkInterpolate interpolates two vectors with weight ifactQ2/4 on the
// second (silk_interpolate).
func silkInterpolate(xi, x0, x1 []int16, ifactQ2 int, d int) {
	for i := 0; i < d; i++ {
		xi[i] = int16(int32(x0[i]) + silkSMULBB(int32(x1[i])-int32(x0[i]), int32(ifactQ2))>>2)
	}
}

// silkNLSFDelDecQuant is the delayed-decision trellis quantizer for the NLSF
// residuals (silk_NLSF_del_dec_quant). Returns the winning RD value in Q25.
func silkNLSFDelDecQuant(indices []int8, xQ10, wQ5 []int16, predCoefQ8 []uint8, ecIx []int16,
	ecRatesQ5 []uint8, quantStepSizeQ16 int32, invQuantStepSizeQ6 int32, muQ20 int32, order int) int32 {

	var out0Q10Table, out1Q10Table [2 * nlsfQuantMaxAmpExt]int32
	for i := -nlsfQuantMaxAmpExt; i <= nlsfQuantMaxAmpExt-1; i++ {
		out0Q10 := int16(silkLSHIFT(int32(i), 10))
		out1Q10 := out0Q10 + 1024
		if i > 0 {
			out0Q10 -= int16(silkFixConst(nlsfQuantLevelAdj, 10))
			out1Q10 -= int16(silkFixConst(nlsfQuantLevelAdj, 10))
		} else if i == 0 {
			out1Q10 -= int16(silkFixConst(nlsfQuantLevelAdj, 10))
		} else if i == -1 {
			out0Q10 += int16(silkFixConst(nlsfQuantLevelAdj, 10))
		} else {
			out0Q10 += int16(silkFixConst(nlsfQuantLevelAdj, 10))
			out1Q10 += int16(silkFixConst(nlsfQuantLevelAdj, 10))
		}
		out0Q10Table[i+nlsfQuantMaxAmpExt] = silkRSHIFT(silkSMULBB(int32(out0Q10), quantStepSizeQ16), 16)
		out1Q10Table[i+nlsfQuantMaxAmpExt] = silkRSHIFT(silkSMULBB(int32(out1Q10), quantStepSizeQ16), 16)
	}

	var indSort [nlsfQuantDelDecStates]int
	var ind [nlsfQuantDelDecStates][silkMaxLPCOrder]int8
	var prevOutQ10 [2 * nlsfQuantDelDecStates]int16
	var rdQ25 [2 * nlsfQuantDelDecStates]int32
	var rdMinQ25, rdMaxQ25 [nlsfQuantDelDecStates]int32

	nStates := 1
	rdQ25[0] = 0
	prevOutQ10[0] = 0
	for i := order - 1; i >= 0; i-- {
		rates := ecRatesQ5[ecIx[i]:]
		inQ10 := xQ10[i]
		for j := 0; j < nStates; j++ {
			predQ10 := int16(silkRSHIFT(silkSMULBB(int32(int16(predCoefQ8[i])), int32(prevOutQ10[j])), 8))
			resQ10 := inQ10 - predQ10
			indTmp := silkRSHIFT(silkSMULBB(invQuantStepSizeQ6, int32(resQ10)), 16)
			indTmp = silkLIMIT(indTmp, -nlsfQuantMaxAmpExt, nlsfQuantMaxAmpExt-1)
			ind[j][i] = int8(indTmp)

			out0Q10 := int16(out0Q10Table[indTmp+nlsfQuantMaxAmpExt]) + predQ10
			out1Q10 := int16(out1Q10Table[indTmp+nlsfQuantMaxAmpExt]) + predQ10
			prevOutQ10[j] = out0Q10
			prevOutQ10[j+nStates] = out1Q10

			var rate0Q5, rate1Q5 int32
			switch {
			case indTmp+1 >= nlsfQuantMaxAmp:
				if indTmp+1 == nlsfQuantMaxAmp {
					rate0Q5 = int32(rates[indTmp+nlsfQuantMaxAmp])
					rate1Q5 = 280
				} else {
					rate0Q5 = silkSMLABB(280-43*nlsfQuantMaxAmp, 43, indTmp)
					rate1Q5 = rate0Q5 + 43
				}
			case indTmp <= -nlsfQuantMaxAmp:
				if indTmp == -nlsfQuantMaxAmp {
					rate0Q5 = 280
					rate1Q5 = int32(rates[indTmp+1+nlsfQuantMaxAmp])
				} else {
					rate0Q5 = silkSMLABB(280-43*nlsfQuantMaxAmp, -43, indTmp)
					rate1Q5 = rate0Q5 - 43
				}
			default:
				rate0Q5 = int32(rates[indTmp+nlsfQuantMaxAmp])
				rate1Q5 = int32(rates[indTmp+1+nlsfQuantMaxAmp])
			}

			rdTmpQ25 := rdQ25[j]
			diffQ10 := inQ10 - out0Q10
			rdQ25[j] = silkSMLABB(silkMLA(rdTmpQ25, silkSMULBB(int32(diffQ10), int32(diffQ10)), int32(wQ5[i])), muQ20, rate0Q5)
			diffQ10 = inQ10 - out1Q10
			rdQ25[j+nStates] = silkSMLABB(silkMLA(rdTmpQ25, silkSMULBB(int32(diffQ10), int32(diffQ10)), int32(wQ5[i])), muQ20, rate1Q5)
		}

		if nStates <= nlsfQuantDelDecStates/2 {
			for j := 0; j < nStates; j++ {
				ind[j+nStates][i] = ind[j][i] + 1
			}
			nStates <<= 1
			for j := nStates; j < nlsfQuantDelDecStates; j++ {
				ind[j][i] = ind[j-nStates][i]
			}
		} else {
			for j := 0; j < nlsfQuantDelDecStates; j++ {
				if rdQ25[j] > rdQ25[j+nlsfQuantDelDecStates] {
					rdMaxQ25[j] = rdQ25[j]
					rdMinQ25[j] = rdQ25[j+nlsfQuantDelDecStates]
					rdQ25[j] = rdMinQ25[j]
					rdQ25[j+nlsfQuantDelDecStates] = rdMaxQ25[j]
					prevOutQ10[j], prevOutQ10[j+nlsfQuantDelDecStates] = prevOutQ10[j+nlsfQuantDelDecStates], prevOutQ10[j]
					indSort[j] = j + nlsfQuantDelDecStates
				} else {
					rdMinQ25[j] = rdQ25[j]
					rdMaxQ25[j] = rdQ25[j+nlsfQuantDelDecStates]
					indSort[j] = j
				}
			}
			for {
				minMaxQ25 := int32(silkInt32Max)
				maxMinQ25 := int32(0)
				indMinMax := 0
				indMaxMin := 0
				for j := 0; j < nlsfQuantDelDecStates; j++ {
					if minMaxQ25 > rdMaxQ25[j] {
						minMaxQ25 = rdMaxQ25[j]
						indMinMax = j
					}
					if maxMinQ25 < rdMinQ25[j] {
						maxMinQ25 = rdMinQ25[j]
						indMaxMin = j
					}
				}
				if minMaxQ25 >= maxMinQ25 {
					break
				}
				indSort[indMaxMin] = indSort[indMinMax] ^ nlsfQuantDelDecStates
				rdQ25[indMaxMin] = rdQ25[indMinMax+nlsfQuantDelDecStates]
				prevOutQ10[indMaxMin] = prevOutQ10[indMinMax+nlsfQuantDelDecStates]
				rdMinQ25[indMaxMin] = 0
				rdMaxQ25[indMinMax] = silkInt32Max
				ind[indMaxMin] = ind[indMinMax]
			}
			for j := 0; j < nlsfQuantDelDecStates; j++ {
				ind[j][i] += int8(silkRSHIFT(int32(indSort[j]), nlsfQuantDelDecStatesLog2))
			}
		}
	}

	indTmp := 0
	minQ25 := int32(silkInt32Max)
	for j := 0; j < 2*nlsfQuantDelDecStates; j++ {
		if minQ25 > rdQ25[j] {
			minQ25 = rdQ25[j]
			indTmp = j
		}
	}
	for j := 0; j < order; j++ {
		indices[j] = ind[indTmp&(nlsfQuantDelDecStates-1)][j]
	}
	indices[0] += int8(silkRSHIFT(int32(indTmp), nlsfQuantDelDecStatesLog2))
	return minQ25
}

// silkNLSFEncode quantizes an NLSF vector: first-stage VQ with nSurvivors,
// trellis second stage, RD winner (silk_NLSF_encode). pNLSFQ15 is replaced by
// the quantized vector.
func silkNLSFEncode(nlsfIndices []int8, pNLSFQ15 []int16, cb *nlsfCB, pWQ2 []int16,
	nlsfMuQ20 int32, nSurvivors int, signalType int8) int32 {

	silkNLSFStabilize(pNLSFQ15, cb.deltaMinQ15, cb.order)

	errQ24 := make([]int32, cb.nVectors)
	silkNLSFVQ(errQ24, pNLSFQ15, cb.cb1NLSFQ8, cb.cb1WghtQ9, cb.nVectors, cb.order)

	tempIndices1 := make([]int, nSurvivors)
	silkInsertionSortIncreasing(errQ24, tempIndices1, cb.nVectors, nSurvivors)

	rdQ25 := make([]int32, nSurvivors)
	tempIndices2 := make([]int8, nSurvivors*silkMaxLPCOrder)

	var resQ10, nlsfTmpQ15, wAdjQ5 [silkMaxLPCOrder]int16
	var predQ8 [silkMaxLPCOrder]uint8
	var ecIx [silkMaxLPCOrder]int16
	for s := 0; s < nSurvivors; s++ {
		ind1 := tempIndices1[s]
		cbElem := cb.cb1NLSFQ8[ind1*cb.order:]
		cbWght := cb.cb1WghtQ9[ind1*cb.order:]
		for i := 0; i < cb.order; i++ {
			nlsfTmpQ15[i] = int16(silkLSHIFT(int32(cbElem[i]), 7))
			wTmpQ9 := int32(cbWght[i])
			resQ10[i] = int16(silkRSHIFT(silkSMULBB(int32(pNLSFQ15[i])-int32(nlsfTmpQ15[i]), wTmpQ9), 14))
			wAdjQ5[i] = int16(silkDIV32varQ(int32(pWQ2[i]), silkSMULBB(wTmpQ9, wTmpQ9), 21))
		}
		silkNLSFUnpack(ecIx[:], predQ8[:], cb, ind1)
		rdQ25[s] = silkNLSFDelDecQuant(tempIndices2[s*silkMaxLPCOrder:], resQ10[:], wAdjQ5[:], predQ8[:], ecIx[:],
			cb.ecRatesQ5, cb.quantStepSizeQ16, cb.invQuantStepSizeQ6, nlsfMuQ20, cb.order)

		iCDF := cb.cb1ICDF[(int(signalType)>>1)*cb.nVectors:]
		var probQ8 int32
		if ind1 == 0 {
			probQ8 = 256 - int32(iCDF[ind1])
		} else {
			probQ8 = int32(iCDF[ind1-1]) - int32(iCDF[ind1])
		}
		bitsQ7 := 8<<7 - silkLin2Log(probQ8)
		rdQ25[s] = silkSMLABB(rdQ25[s], bitsQ7, silkRSHIFT(nlsfMuQ20, 2))
	}

	var bestIndex [1]int
	silkInsertionSortIncreasing(rdQ25, bestIndex[:], nSurvivors, 1)
	nlsfIndices[0] = int8(tempIndices1[bestIndex[0]])
	copy(nlsfIndices[1:1+cb.order], tempIndices2[bestIndex[0]*silkMaxLPCOrder:])

	silkNLSFDecode(pNLSFQ15, nlsfIndices, cb)
	return rdQ25[0]
}

// processNLSFs limits, stabilizes, and quantizes the frame's NLSFs, producing
// the quantized prediction coefficients (silk_process_NLSFs).
func (ch *silkEncoderChannel) processNLSFs(predCoefQ12 *[2][silkMaxLPCOrder]int16, pNLSFQ15 []int16, prevNLSFqQ15 []int16) {
	var pNLSF0TempQ15 [silkMaxLPCOrder]int16
	var pNLSFWQW, pNLSFW0TempQW [silkMaxLPCOrder]int16

	// NLSF_mu = 0.003 - 0.0015 * speech_activity.
	nlsfMuQ20 := silkSMLAWB(silkFixConst(0.003, 20), silkFixConst(-0.001, 28), ch.speechActivityQ8)
	if ch.nbSubfr == 2 {
		nlsfMuQ20 += nlsfMuQ20 >> 1
	}

	silkNLSFVQWeightsLaroia(pNLSFWQW[:], pNLSFQ15, ch.predictLPCOrder)

	doInterpolate := ch.useInterpolatedNLSFs && ch.indices.NLSFInterpCoefQ2 < 4
	if doInterpolate {
		silkInterpolate(pNLSF0TempQ15[:], prevNLSFqQ15, pNLSFQ15, int(ch.indices.NLSFInterpCoefQ2), ch.predictLPCOrder)
		silkNLSFVQWeightsLaroia(pNLSFW0TempQW[:], pNLSF0TempQ15[:], ch.predictLPCOrder)
		iSqrQ15 := silkLSHIFT(silkSMULBB(int32(ch.indices.NLSFInterpCoefQ2), int32(ch.indices.NLSFInterpCoefQ2)), 11)
		for i := 0; i < ch.predictLPCOrder; i++ {
			pNLSFWQW[i] = int16(silkRSHIFT(int32(pNLSFWQW[i]), 1) + silkRSHIFT(silkSMULBB(int32(pNLSFW0TempQW[i]), iSqrQ15), 16))
		}
	}

	silkNLSFEncode(ch.indices.NLSFIndices[:], pNLSFQ15, ch.psNLSFCB, pNLSFWQW[:],
		nlsfMuQ20, ch.nlsfMSVQSurvivors, ch.indices.signalType)

	silkNLSF2A(predCoefQ12[1][:], pNLSFQ15, ch.predictLPCOrder)

	if doInterpolate {
		silkInterpolate(pNLSF0TempQ15[:], prevNLSFqQ15, pNLSFQ15, int(ch.indices.NLSFInterpCoefQ2), ch.predictLPCOrder)
		silkNLSF2A(predCoefQ12[0][:], pNLSF0TempQ15[:], ch.predictLPCOrder)
	} else {
		copy(predCoefQ12[0][:ch.predictLPCOrder], predCoefQ12[1][:ch.predictLPCOrder])
	}
}
