package opus

import (
	"math/rand"
	"testing"
)

// TestSilkPulsesRoundTrip drives silkEncodePulses over random excitation
// frames (including large magnitudes that force the LSB-shift path) and
// asserts the existing bit-exact decoder reproduces every pulse.
func TestSilkPulsesRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 400; trial++ {
		frameLength := []int{160, 240, 320}[trial%3]
		signalType := int8(trial % 3)
		quantOffsetType := int8((trial / 3) % 2)
		var pulses [silkMaxFrameLen]int8
		mag := []int{1, 2, 5, 30, 127}[trial%5]
		for i := 0; i < frameLength; i++ {
			if rng.Intn(3) == 0 {
				pulses[i] = int8(rng.Intn(2*mag+1) - mag)
			}
		}
		want := pulses

		buf := make([]byte, 1275)
		enc := newRangeEncoder(buf)
		silkEncodePulses(enc, signalType, quantOffsetType, pulses[:], frameLength)
		enc.done()

		dec := newRangeDecoder(enc.payload())
		got := make([]int16, frameLength+shellFrameLen)
		silkDecodePulses(dec, got, signalType, quantOffsetType, frameLength)
		for i := 0; i < frameLength; i++ {
			if int16(want[i]) != got[i] {
				t.Fatalf("trial %d (len %d type %d/%d): pulse %d = %d, want %d",
					trial, frameLength, signalType, quantOffsetType, i, got[i], want[i])
			}
		}
	}
}

// TestSilkIndicesRoundTrip encodes random valid side-info indices and asserts
// the decoder reproduces them, across bandwidths and both conditional and
// independent coding.
func TestSilkIndicesRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 400; trial++ {
		fsKHz := []int{8, 12, 16}[trial%3]
		condCoding := codeIndependently
		if trial%2 == 1 {
			condCoding = codeConditionally
		}

		var ch silkEncoderChannel
		ch.nbSubfr = silkMaxNBSubfr
		ch.fsKHz = fsKHz
		if fsKHz == 16 {
			ch.psNLSFCB = silkNLSFCBWB
		} else {
			ch.psNLSFCB = silkNLSFCBNBMB
		}
		switch fsKHz {
		case 16:
			ch.pitchLagLowBitsICDF = silk_uniform8_iCDF
			ch.pitchContourICDF = silk_pitch_contour_iCDF
		case 12:
			ch.pitchLagLowBitsICDF = silk_uniform6_iCDF
			ch.pitchContourICDF = silk_pitch_contour_iCDF
		case 8:
			ch.pitchLagLowBitsICDF = silk_uniform4_iCDF
			ch.pitchContourICDF = silk_pitch_contour_NB_iCDF
		}

		ix := &ch.indices
		ix.signalType = int8(rng.Intn(3))
		ix.quantOffsetType = int8(rng.Intn(2))
		ix.GainsIndices[0] = int8(rng.Intn(nLevelsQGain))
		if condCoding == codeConditionally {
			ix.GainsIndices[0] = int8(rng.Intn(maxDeltaGain - minDeltaGain + 1))
		}
		for i := 1; i < ch.nbSubfr; i++ {
			ix.GainsIndices[i] = int8(rng.Intn(maxDeltaGain - minDeltaGain + 1))
		}
		ix.NLSFIndices[0] = int8(rng.Intn(ch.psNLSFCB.nVectors))
		for i := 1; i <= ch.psNLSFCB.order; i++ {
			ix.NLSFIndices[i] = int8(rng.Intn(21) - 10)
		}
		ix.NLSFInterpCoefQ2 = int8(rng.Intn(5))
		if ix.signalType == typeVoiced {
			ix.lagIndex = rng.Intn(32 * (fsKHz >> 1))
			nContour := len(ch.pitchContourICDF) - 1
			ix.contourIndex = int8(rng.Intn(nContour))
			ix.PERIndex = int8(rng.Intn(3))
			for k := 0; k < ch.nbSubfr; k++ {
				ix.LTPIndex[k] = int8(rng.Intn(8 << ix.PERIndex))
			}
			if condCoding == codeIndependently {
				ix.LTPScaleIndex = int8(rng.Intn(3))
			} else {
				ix.LTPScaleIndex = 0
			}
		}
		ix.Seed = int8(rng.Intn(4))

		// Conditional lag deltas must reference a plausible previous lag.
		ch.ecPrevSignalType = typeVoiced
		ch.ecPrevLagIndex = 100
		want := *ix

		buf := make([]byte, 1275)
		enc := newRangeEncoder(buf)
		ch.encodeIndices(enc, 0, false, condCoding)
		enc.done()

		var cs silkChannelState
		cs.nbSubfr = ch.nbSubfr
		cs.setFS(fsKHz, 48000)
		cs.VADFlags[0] = 0
		if int(want.signalType)*2+int(want.quantOffsetType) >= 2 {
			cs.VADFlags[0] = 1
		}
		cs.ecPrevSignalType = typeVoiced
		cs.ecPrevLagIndex = 100
		dec := newRangeDecoder(enc.payload())
		cs.decodeIndices(dec, 0, false, condCoding)

		got := cs.indices
		if got.NLSFInterpCoefQ2 != want.NLSFInterpCoefQ2 && ch.nbSubfr != silkMaxNBSubfr {
			got.NLSFInterpCoefQ2 = want.NLSFInterpCoefQ2
		}
		if want.signalType != typeVoiced {
			// The decoder leaves pitch fields untouched for unvoiced frames.
			got.lagIndex = want.lagIndex
			got.contourIndex = want.contourIndex
			got.PERIndex = want.PERIndex
			got.LTPIndex = want.LTPIndex
			got.LTPScaleIndex = want.LTPScaleIndex
		}
		if got != want {
			t.Fatalf("trial %d (fs %d cond %d):\n got %+v\nwant %+v", trial, fsKHz, condCoding, got, want)
		}
	}
}
