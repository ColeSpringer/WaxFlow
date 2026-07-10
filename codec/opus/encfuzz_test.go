package opus

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
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

// FuzzOpusEncode drives the full Opus encoder (SILK, hybrid, and CELT modes
// with analyser-driven switching) over arbitrary PCM at fuzz-chosen bitrates,
// hints, and rate-control modes, then decodes every packet with the full
// decoder: no panics, every packet decodable, exactly one frame out per
// packet, all samples finite. Low bitrates route through the SILK and hybrid
// paths the CELT-only fuzz above never reaches, and content shifts drive
// mode-switch redundancy packets.
func FuzzOpusEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint16(12), uint8(1), false, false)
	f.Add(make([]byte, 6000), uint16(32), uint8(0), true, false)
	f.Add([]byte{0xff, 0x00, 0x80, 0x7f}, uint16(96), uint8(2), false, true)
	f.Fuzz(func(t *testing.T, data []byte, kbpsSel uint16, sigSel uint8, stereo, vbr bool) {
		C := 1
		if stereo {
			C = 2
		}
		// Bitrate in [6, 510] kb/s: the low end pins SILK-only, the middle
		// hybrid, the top CELT.
		bitrate := (6 + int(kbpsSel)%505) * 1000
		f32 := audio.Format{Rate: SampleRate, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}
		enc, err := NewEncoder(f32, &EncoderOptions{
			Bitrate: bitrate,
			VBR:     vbr,
			Signal:  Signal(int(sigSel) % 3),
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{Channels: C}
		dec, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}

		const frames = 4
		decoded := 0
		emit := func(p codec.Packet) error {
			if len(p.Data) == 0 {
				t.Fatal("empty packet")
			}
			return dec.Decode(p.Data, func(b *audio.Buffer) error {
				decoded += b.N
				for c := 0; c < C; c++ {
					for _, v := range b.ChanF(c) {
						if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
							t.Fatalf("decoded non-finite sample %v", v)
						}
					}
				}
				return nil
			})
		}
		packets := 0
		for fi := 0; fi < frames; fi++ {
			b := audio.Get(f32, opusFrameSize)
			for c := 0; c < C; c++ {
				ch := b.ChanF(c)
				for i := range ch {
					idx := (fi*opusFrameSize + i*(c+1)) % max(1, len(data))
					var by byte
					if len(data) > 0 {
						by = data[idx]
					}
					ch[i] = float32(by)/127.5 - 1.0
				}
			}
			b.N = opusFrameSize
			err := enc.Encode(b, func(p codec.Packet) error {
				packets++
				return emit(p)
			})
			audio.Put(b)
			if err != nil {
				t.Fatal(err)
			}
		}
		if decoded != packets*opusFrameSize {
			t.Fatalf("decoded %d samples from %d packets, want %d", decoded, packets, packets*opusFrameSize)
		}
	})
}
