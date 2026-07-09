package opus

import (
	"math"
	"testing"
)

// FuzzCELTEncode feeds arbitrary PCM and packet sizes through the CELT encoder
// and then the decoder, asserting the pipeline never panics, always emits
// exactly the requested payload size, and produces a stream our decoder accepts.
// The encoder must be robust to any finite input and to the smallest legal
// packet sizes (where the range coder runs out of room).
func FuzzCELTEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint8(40), true)
	f.Add(make([]byte, 4000), uint8(3), false)
	f.Fuzz(func(t *testing.T, data []byte, sizeSel uint8, stereo bool) {
		C := 1
		if stereo {
			C = 2
		}
		// Payload size in [2, 1275] (the CELT range).
		nbBytes := 2 + int(sizeSel)%1274
		enc := newCELTEncoder(C)
		dec := newCELTDecoder(C)

		pcm := make([][]float32, C)
		for c := range pcm {
			pcm[c] = make([]float32, opusFrameSize)
		}
		out := make([][]float32, C)
		for c := range out {
			out[c] = make([]float32, opusFrameSize)
		}

		// Turn the fuzz bytes into a few frames of PCM in [-1, 1].
		frames := 3
		for fi := 0; fi < frames; fi++ {
			for c := 0; c < C; c++ {
				for i := 0; i < opusFrameSize; i++ {
					idx := (fi*opusFrameSize + i) % max(1, len(data))
					var b byte
					if len(data) > 0 {
						b = data[idx]
					}
					pcm[c][i] = (float32(b)/127.5 - 1.0)
				}
			}
			payload := enc.celtEncode(pcm, opusFrameSize, opusFrameLM, C, 0, opusCELTBands, nbBytes)
			if len(payload) != nbBytes {
				t.Fatalf("payload %d bytes, want %d", len(payload), nbBytes)
			}
			if err := dec.celtDecode(payload, opusFrameLM, C, 0, opusCELTBands, out); err != nil {
				t.Fatalf("decode of self-produced payload failed: %v", err)
			}
			for c := 0; c < C; c++ {
				for i := 0; i < opusFrameSize; i++ {
					if v := out[c][i]; math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
						t.Fatalf("decoded non-finite sample %v", v)
					}
				}
			}
		}
	})
}
