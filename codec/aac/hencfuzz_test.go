package aac

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// FuzzHEEncode is FuzzEncode's HE-AAC twin: arbitrary PCM (non-finite
// and out-of-range values included) and bit rates through the HEEncoder,
// every emitted access unit decoded by our own decoder. Invariants: no
// panic, every AU decodes to exactly one 2048-sample frame at the
// doubled rate, and no AU exceeds the 6144-bit-per-channel ceiling
// (which now includes the SBR fill element). v2 runs the same
// invariants over the PS front end and the mono core (whose AU ceiling
// is single-channel; the decode still emits the widened pair).
func FuzzHEEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint8(0), true, false)
	f.Add(make([]byte, 4096), uint8(255), false, false)
	f.Add([]byte{0xFF, 0x7F, 0x00, 0x80, 0x55}, uint8(64), true, true)

	f.Fuzz(func(t *testing.T, data []byte, rateSel uint8, stereo, v2 bool) {
		ch := 1
		if stereo {
			ch = 2
		}
		v2 = v2 && stereo // v2 needs the stereo pair; mono+v2 is the refusal path
		rate := heOutputRates[int(rateSel)%len(heOutputRates)]
		fm := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		bitrate := 8000 + int(rateSel)*1000
		e, err := NewHEEncoder(fm, &EncoderOptions{Bitrate: bitrate, ParametricStereo: v2})
		if err != nil {
			t.Fatal(err)
		}
		defer e.Release()
		cfg, err := ParseASC(e.CodecConfig())
		if err != nil {
			t.Fatalf("our own ASC does not parse: %v", err)
		}
		df, err := cfg.Format()
		if err != nil {
			t.Fatal(err)
		}
		if df != fm {
			t.Fatalf("ASC round-trips to %v, want %v", df, fm)
		}
		d, err := NewDecoder(cfg, df)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Release()

		coreCh := ch
		if v2 {
			coreCh = 1
		}
		check := func(p codec.Packet) error {
			if len(p.Data)*8 > 6144*coreCh {
				t.Fatalf("AU of %d bytes exceeds the %d-bit buffer ceiling", len(p.Data), 6144*coreCh)
			}
			frames := 0
			if err := d.Decode(p.Data, func(b *audio.Buffer) error {
				frames++
				if b.N != 2*frameLen {
					t.Fatalf("decoded %d samples, want %d", b.N, 2*frameLen)
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

		const blocks = 3
		buf := audio.Get(fm, 2*frameLen)
		defer audio.Put(buf)
		for blk := 0; blk < blocks; blk++ {
			buf.N = 2 * frameLen
			for c := 0; c < ch; c++ {
				dst := buf.ChanF(c)
				for i := range dst {
					if len(data) == 0 {
						dst[i] = 0
						continue
					}
					b := data[(blk*2*frameLen+i*ch+c)%len(data)]
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
