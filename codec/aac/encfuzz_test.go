package aac

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// FuzzEncode feeds arbitrary PCM (including non-finite and out-of-range
// values) and bit rates through the encoder and decodes every emitted
// access unit with our decoder. Invariants: no panic, every AU decodes to
// exactly one 1024-sample frame, and no AU exceeds the 6144-bit-per-channel
// spec ceiling. The opus encoder fuzz target is the precedent.
func FuzzEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint8(0), true)
	f.Add(make([]byte, 4096), uint8(255), false)
	f.Add([]byte{0xFF, 0x7F, 0x00, 0x80, 0x55}, uint8(64), true)

	f.Fuzz(func(t *testing.T, data []byte, rateSel uint8, stereo bool) {
		ch := 1
		if stereo {
			ch = 2
		}
		fm := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		bitrate := 8000 + int(rateSel)*1000
		e, err := NewEncoder(fm, &EncoderOptions{Bitrate: bitrate})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseASC(e.CodecConfig())
		if err != nil {
			t.Fatalf("our own ASC does not parse: %v", err)
		}
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}

		check := func(p codec.Packet) error {
			if len(p.Data)*8 > 6144*ch {
				t.Fatalf("AU of %d bytes exceeds the %d-bit buffer ceiling", len(p.Data), 6144*ch)
			}
			frames := 0
			if err := d.Decode(p.Data, func(b *audio.Buffer) error {
				frames++
				if b.N != frameLen {
					t.Fatalf("decoded %d samples, want %d", b.N, frameLen)
				}
				return nil
			}); err != nil {
				t.Fatalf("our decoder rejects our AU: %v", err)
			}
			if frames != 1 {
				t.Fatalf("AU decoded to %d frames, want 1", frames)
			}
			return nil
		}

		// Turn the fuzz bytes into a few blocks of PCM, seeding hostile
		// values (NaN, Inf, huge magnitudes) from the raw bytes.
		const blocks = 3
		buf := audio.Get(fm, frameLen)
		defer audio.Put(buf)
		for blk := 0; blk < blocks; blk++ {
			buf.N = frameLen
			for c := 0; c < ch; c++ {
				dst := buf.ChanF(c)
				for i := range dst {
					if len(data) == 0 {
						dst[i] = 0
						continue
					}
					b := data[(blk*frameLen+i*ch+c)%len(data)]
					v := (float32(b) - 127.5) / 127.5
					switch b {
					case 0xFF:
						v = float32(math.NaN())
					case 0xFE:
						v = float32(math.Inf(1))
					case 0xFD:
						v = -1e30
					}
					dst[i] = v
				}
			}
			if err := e.Encode(buf, check); err != nil {
				t.Fatalf("Encode: %v", err)
			}
		}
		if _, err := e.Finish(check); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	})
}
