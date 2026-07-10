package opus

import (
	"math/rand"
	"testing"
)

// TestSilkA2NLSFInversion checks that silkA2NLSF recovers the NLSF vector
// that generated a filter through silkNLSF2A. Random vectors can trip
// NLSF2A's internal bandwidth limiting (which legitimately moves the NLSFs),
// so the inputs are the first-stage codebook vectors: real speech spectra
// that convert cleanly. The pair shares one piecewise-linear cos
// approximation, so the round trip is tight.
func TestSilkA2NLSFInversion(t *testing.T) {
	for _, cb := range []*nlsfCB{silkNLSFCBNBMB, silkNLSFCBWB} {
		order := cb.order
		for v := 0; v < cb.nVectors; v++ {
			nlsf := make([]int16, order)
			for i := 0; i < order; i++ {
				nlsf[i] = int16(silkLSHIFT(int32(cb.cb1NLSFQ8[v*order+i]), 7))
			}

			var aQ12 [silkMaxLPCOrder]int16
			silkNLSF2A(aQ12[:], nlsf, order)

			aQ16 := make([]int32, order)
			for i := 0; i < order; i++ {
				aQ16[i] = silkLSHIFT(int32(aQ12[i]), 4)
			}
			got := make([]int16, order)
			silkA2NLSF(got, aQ16, order)

			for i := 0; i < order; i++ {
				diff := int32(got[i]) - int32(nlsf[i])
				if diff < -250 || diff > 250 {
					t.Fatalf("order %d vector %d: NLSF[%d] = %d, want ~%d (diff %d)",
						order, v, i, got[i], nlsf[i], diff)
				}
			}
		}
	}
}

// TestSilkNLSFEncodeDecodeAgree checks that the indices silkNLSFEncode picks
// are wire-legal and that its in-place quantized output equals what the
// decoder reconstructs from those indices (the encoder runs the decoder
// internally, so this pins that they stay in lockstep).
func TestSilkNLSFEncodeDecodeAgree(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for _, cb := range []*nlsfCB{silkNLSFCBNBMB, silkNLSFCBWB} {
		for trial := 0; trial < 200; trial++ {
			order := cb.order
			nlsf := make([]int16, order)
			acc := int32(0)
			for i := 0; i < order; i++ {
				acc += int32(500 + rng.Intn(2*32768/(order+1)))
				if acc > 32000 {
					acc = 32000
				}
				nlsf[i] = int16(acc)
			}
			w := make([]int16, order)
			silkNLSFVQWeightsLaroia(w, nlsf, order)

			indices := make([]int8, order+1)
			signalType := int8(trial % 3)
			silkNLSFEncode(indices, nlsf, cb, w, silkFixConst(0.003, 20), 4, signalType)

			if int(indices[0]) < 0 || int(indices[0]) >= cb.nVectors {
				t.Fatalf("stage-1 index %d out of range", indices[0])
			}
			for i := 1; i <= order; i++ {
				if indices[i] < -nlsfQuantMaxAmpExt || indices[i] > nlsfQuantMaxAmpExt {
					t.Fatalf("residual index %d = %d outside +-%d", i, indices[i], nlsfQuantMaxAmpExt)
				}
			}

			dec := make([]int16, order)
			silkNLSFDecode(dec, indices, cb)
			for i := 0; i < order; i++ {
				if dec[i] != nlsf[i] {
					t.Fatalf("decode disagrees at %d: %d != %d", i, dec[i], nlsf[i])
				}
			}
		}
	}
}
