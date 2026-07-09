package opus

// SILK resampler, ported from libopus silk/resampler.c,
// resampler_private_up2_HQ.c, and resampler_private_IIR_FIR.c.
// Opus decodes SILK at its internal 8/12/16 kHz rate and this
// resamples to the 48 kHz output; that path is always the IIR_FIR flavor (a 2x
// HQ IIR upsample followed by a 12-phase fractional FIR), so only the
// upsampling case is ported.

// delayMatrixDec[rateID(in)][rateID(out)] is the resampler input delay
// (silk/resampler.c). rateID(48000) == 4.
var delayMatrixDec = [3][6]int8{
	{4, 0, 2, 0, 0, 0},
	{0, 9, 4, 7, 4, 4},
	{0, 3, 12, 7, 7, 7},
}

// rateID maps a sample rate to its delay-matrix column (silk/resampler.c).
func rateID(r int) int {
	id := (((r >> 12) - b2iRate(r > 16000)) >> b2iRate(r > 24000)) - 1
	if id > 5 {
		id = 5
	}
	return id
}

func b2iRate(b bool) int {
	if b {
		return 1
	}
	return 0
}

// silkResampler holds one channel's resampler state (silk_resampler_state_struct).
type silkResampler struct {
	sIIR        [6]int32
	sFIR        [resamplerOrderFIR12]int16
	delayBuf    [96]int16
	fsInKHz     int
	fsOutKHz    int
	inputDelay  int
	invRatioQ16 int32
	batchSize   int
}

// init configures the resampler for the given input/output rates (Hz). Only
// upsampling to a higher rate (the decoder path) is supported.
func (S *silkResampler) init(fsInHz, fsOutHz int) {
	*S = silkResampler{}
	S.inputDelay = int(delayMatrixDec[rateID(fsInHz)][rateID(fsOutHz)])
	S.fsInKHz = fsInHz / 1000
	S.fsOutKHz = fsOutHz / 1000
	S.batchSize = S.fsInKHz * resamplerMaxBatchMS
	const up2x = 1
	S.invRatioQ16 = silkLSHIFT32(silkDIV32(silkLSHIFT32(int32(fsInHz), 14+up2x), int32(fsOutHz)), 2)
	for silkSMULWW(S.invRatioQ16, int32(fsOutHz)) < silkLSHIFT32(int32(fsInHz), up2x) {
		S.invRatioQ16++
	}
}

// resample resamples inLen input samples to out (silk_resampler; IIR_FIR path).
func (S *silkResampler) resample(out, in []int16, inLen int) {
	nSamples := S.fsInKHz - S.inputDelay
	copy(S.delayBuf[S.inputDelay:S.fsInKHz], in[:nSamples])
	S.iirFIR(out, S.delayBuf[:S.fsInKHz], S.fsInKHz)
	S.iirFIR(out[S.fsOutKHz:], in[nSamples:inLen], inLen-S.fsInKHz)
	copy(S.delayBuf[:S.inputDelay], in[inLen-S.inputDelay:inLen])
}

// iirFIR runs the 2x HQ upsampler then the fractional FIR interpolation
// (silk_resampler_private_IIR_FIR).
func (S *silkResampler) iirFIR(out, in []int16, inLen int) {
	buf := make([]int16, resamplerOrderFIR12+2*S.batchSize)
	copy(buf[:resamplerOrderFIR12], S.sFIR[:])
	indexInc := S.invRatioQ16
	outPos, inPos := 0, 0
	nSamplesIn := 0
	for {
		nSamplesIn = inLen
		if nSamplesIn > S.batchSize {
			nSamplesIn = S.batchSize
		}
		silkResamplerUp2HQ(S.sIIR[:], buf[resamplerOrderFIR12:], in[inPos:], nSamplesIn)
		maxIndex := int32(nSamplesIn) << (16 + 1)
		outPos += iirFIRInterpol(out[outPos:], buf, maxIndex, indexInc)
		inPos += nSamplesIn
		inLen -= nSamplesIn
		if inLen > 0 {
			copy(buf[:resamplerOrderFIR12], buf[nSamplesIn<<1:])
		} else {
			break
		}
	}
	copy(S.sFIR[:], buf[nSamplesIn<<1:])
}

// iirFIRInterpol applies the 12-phase fractional FIR and returns the number of
// output samples written (silk_resampler_private_IIR_FIR_INTERPOL).
func iirFIRInterpol(out []int16, buf []int16, maxIndexQ16, indexIncQ16 int32) int {
	n := 0
	for indexQ16 := int32(0); indexQ16 < maxIndexQ16; indexQ16 += indexIncQ16 {
		ti := silkSMULWB(indexQ16&0xFFFF, 12)
		b := buf[indexQ16>>16:]
		resQ15 := silkSMULBB(int32(b[0]), int32(silk_resampler_frac_FIR_12[ti][0]))
		resQ15 = silkSMLABB(resQ15, int32(b[1]), int32(silk_resampler_frac_FIR_12[ti][1]))
		resQ15 = silkSMLABB(resQ15, int32(b[2]), int32(silk_resampler_frac_FIR_12[ti][2]))
		resQ15 = silkSMLABB(resQ15, int32(b[3]), int32(silk_resampler_frac_FIR_12[ti][3]))
		resQ15 = silkSMLABB(resQ15, int32(b[4]), int32(silk_resampler_frac_FIR_12[11-ti][3]))
		resQ15 = silkSMLABB(resQ15, int32(b[5]), int32(silk_resampler_frac_FIR_12[11-ti][2]))
		resQ15 = silkSMLABB(resQ15, int32(b[6]), int32(silk_resampler_frac_FIR_12[11-ti][1]))
		resQ15 = silkSMLABB(resQ15, int32(b[7]), int32(silk_resampler_frac_FIR_12[11-ti][0]))
		out[n] = int16(silkSAT16(silkRSHIFTROUND(resQ15, 15)))
		n++
	}
	return n
}

// silkResamplerUp2HQ is the 2x high-quality IIR upsampler
// (silk_resampler_private_up2_HQ). S is the 6-element IIR state.
func silkResamplerUp2HQ(S []int32, out, in []int16, length int) {
	for k := 0; k < length; k++ {
		in32 := silkLSHIFT(int32(in[k]), 10)
		Y := in32 - S[0]
		X := silkSMULWB(Y, int32(silk_resampler_up2_hq_0[0]))
		out321 := S[0] + X
		S[0] = in32 + X
		Y = out321 - S[1]
		X = silkSMULWB(Y, int32(silk_resampler_up2_hq_0[1]))
		out322 := S[1] + X
		S[1] = out321 + X
		Y = out322 - S[2]
		X = silkSMLAWB(Y, Y, int32(silk_resampler_up2_hq_0[2]))
		out321 = S[2] + X
		S[2] = out322 + X
		out[2*k] = int16(silkSAT16(silkRSHIFTROUND(out321, 10)))

		Y = in32 - S[3]
		X = silkSMULWB(Y, int32(silk_resampler_up2_hq_1[0]))
		out321 = S[3] + X
		S[3] = in32 + X
		Y = out321 - S[4]
		X = silkSMULWB(Y, int32(silk_resampler_up2_hq_1[1]))
		out322 = S[4] + X
		S[4] = out321 + X
		Y = out322 - S[5]
		X = silkSMLAWB(Y, Y, int32(silk_resampler_up2_hq_1[2]))
		out321 = S[5] + X
		S[5] = out322 + X
		out[2*k+1] = int16(silkSAT16(silkRSHIFTROUND(out321, 10)))
	}
}
