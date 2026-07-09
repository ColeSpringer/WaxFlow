package opus

import (
	"math"
	"testing"
)

// TestCELTEncodeDecodeRoundTrip encodes a synthetic signal with the CELT encoder
// and decodes it with our CELT decoder, requiring the reconstruction to track the
// input at a high SNR. This is the first end-to-end exercise of the whole encode
// path (MDCT, energy, allocation, PVQ, range coder) against the proven decoder.
func TestCELTEncodeDecodeRoundTrip(t *testing.T) {
	const (
		LM  = 3   // 20 ms
		N   = 960 // 120<<3
		end = 21  // full band
		sr  = 48000
	)
	cases := []struct {
		name   string
		C      int
		nbytes int
		minSNR float64
	}{
		{"stereo_128k", 2, 320, 12},
		{"mono_96k", 1, 240, 12},
		{"stereo_160k", 2, 400, 14},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := 60
			total := frames * N
			// Spectrally rich, partly modulated signal per channel.
			src := make([][]float32, tc.C)
			for c := 0; c < tc.C; c++ {
				src[c] = make([]float32, total)
				f1 := 440.0 + 220*float64(c)
				for i := range src[c] {
					t := float64(i) / sr
					v := 0.3*math.Sin(2*math.Pi*f1*t) +
						0.15*math.Sin(2*math.Pi*2500*t) +
						0.08*math.Sin(2*math.Pi*9000*t)*(1+math.Sin(2*math.Pi*6*t))
					src[c][i] = float32(0.7 * v)
				}
			}

			enc := newCELTEncoder(tc.C)
			dec := newCELTDecoder(tc.C)
			got := make([][]float32, tc.C)

			pcm := make([][]float32, tc.C)
			out := make([][]float32, tc.C)
			for f := 0; f < frames; f++ {
				for c := 0; c < tc.C; c++ {
					pcm[c] = src[c][f*N : (f+1)*N]
					if out[c] == nil {
						out[c] = make([]float32, N)
					}
				}
				payload := enc.celtEncode(pcm, N, LM, tc.C, 0, end, tc.nbytes)
				if len(payload) != tc.nbytes {
					t.Fatalf("frame %d: payload %d bytes, want %d", f, len(payload), tc.nbytes)
				}
				if err := dec.celtDecode(payload, LM, tc.C, 0, end, out); err != nil {
					t.Fatalf("frame %d decode: %v", f, err)
				}
				for c := 0; c < tc.C; c++ {
					got[c] = append(got[c], out[c]...)
				}
			}

			// Align (encoder + decoder introduce a fixed delay) and score.
			bestSNR, bestD := math.Inf(-1), 0
			for d := 0; d <= 500; d++ {
				snr := roundTripSNR(src, got, d)
				if snr > bestSNR {
					bestSNR, bestD = snr, d
				}
			}
			t.Logf("%s: SNR %.1f dB at delay %d", tc.name, bestSNR, bestD)
			if bestSNR < tc.minSNR {
				t.Errorf("%s: round-trip SNR %.1f dB too low (want >=%.0f)", tc.name, bestSNR, tc.minSNR)
			}
		})
	}
}

// TestCELTEncodeComplexityLevels exercises every complexity setting (0..10) on a
// transient-rich signal so the gated analysis paths (tf_analysis >=2, spreading
// >=3, two-pass energy >=4, patch-transient >=5, second MDCT >=8) all run and
// still produce a decodable, reasonable-SNR stream. Higher complexity must not
// score worse than the lowest setting.
func TestCELTEncodeComplexityLevels(t *testing.T) {
	const (
		LM     = 3
		N      = 960
		end    = 21
		sr     = 48000
		C      = 2
		nbytes = 320
		frames = 60
	)
	total := frames * N
	// Tone bed plus a sharp amplitude burst every 5 ms to trigger transients.
	src := make([][]float32, C)
	for c := 0; c < C; c++ {
		src[c] = make([]float32, total)
		f1 := 500.0 + 130*float64(c)
		for i := range src[c] {
			ts := float64(i) / sr
			env := 1.0
			if i%240 < 12 {
				env = 6.0 // click
			}
			v := 0.25*math.Sin(2*math.Pi*f1*ts) + 0.12*math.Sin(2*math.Pi*7000*ts)
			src[c][i] = float32(0.6 * env * v)
			if src[c][i] > 1 {
				src[c][i] = 1
			} else if src[c][i] < -1 {
				src[c][i] = -1
			}
		}
	}

	var snrLow float64
	for cx := 0; cx <= 10; cx++ {
		enc := newCELTEncoder(C)
		enc.complexity = cx
		dec := newCELTDecoder(C)
		got := make([][]float32, C)
		pcm := make([][]float32, C)
		out := make([][]float32, C)
		for c := 0; c < C; c++ {
			out[c] = make([]float32, N)
		}
		for f := 0; f < frames; f++ {
			for c := 0; c < C; c++ {
				pcm[c] = src[c][f*N : (f+1)*N]
			}
			payload := enc.celtEncode(pcm, N, LM, C, 0, end, nbytes)
			if len(payload) != nbytes {
				t.Fatalf("cx=%d frame %d: payload %d bytes, want %d", cx, f, len(payload), nbytes)
			}
			if err := dec.celtDecode(payload, LM, C, 0, end, out); err != nil {
				t.Fatalf("cx=%d frame %d decode: %v", cx, f, err)
			}
			for c := 0; c < C; c++ {
				got[c] = append(got[c], out[c]...)
			}
		}
		bestSNR := math.Inf(-1)
		for d := 0; d <= 500; d++ {
			if snr := roundTripSNR(src, got, d); snr > bestSNR {
				bestSNR = snr
			}
		}
		t.Logf("complexity %2d: SNR %.1f dB", cx, bestSNR)
		if bestSNR < 8 {
			t.Errorf("complexity %d: round-trip SNR %.1f dB too low", cx, bestSNR)
		}
		if cx == 0 {
			snrLow = bestSNR
		} else if bestSNR < snrLow-2 {
			t.Errorf("complexity %d SNR %.1f dB regressed vs complexity 0 (%.1f dB)", cx, bestSNR, snrLow)
		}
	}
}

func roundTripSNR(src, got [][]float32, delay int) float64 {
	C := len(src)
	n := len(got[0]) - delay
	if n > len(src[0]) {
		n = len(src[0])
	}
	if n < 4000 {
		return math.Inf(-1)
	}
	// Skip the first 2000 samples (encoder warm-up) and the tail.
	var sig, errE float64
	for c := 0; c < C; c++ {
		for i := 2000; i < n-1000; i++ {
			s := float64(src[c][i])
			g := float64(got[c][i+delay])
			sig += s * s
			e := s - g
			errE += e * e
		}
	}
	if errE == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/errE)
}
