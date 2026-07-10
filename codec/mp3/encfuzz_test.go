package mp3

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// FuzzEncode feeds arbitrary PCM (including non-finite and out-of-range
// values), bit rates, and the VBR flag through the encoder and decodes
// every emitted frame with our decoder. Invariants: no panic, every frame
// parses with the exact declared size, and the whole stream decodes. The
// opus and aac encoder fuzz targets are the precedent.
func FuzzEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint8(128), true, false)
	f.Add(make([]byte, 4096), uint8(8), false, true)
	f.Add([]byte{0xFF, 0x7F, 0x00, 0x80, 0x55}, uint8(64), true, true)

	f.Fuzz(func(t *testing.T, data []byte, rateSel uint8, stereo, vbr bool) {
		ch := 1
		if stereo {
			ch = 2
		}
		fm := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		bitrate := (8 + int(rateSel)) * 1000
		e, err := NewEncoder(fm, &EncoderOptions{Bitrate: bitrate, VBR: vbr})
		if err != nil {
			t.Fatal(err)
		}
		d, err := NewDecoder(fm)
		if err != nil {
			t.Fatal(err)
		}

		check := func(p codec.Packet) error {
			h, err := ParseHeader(p.Data)
			if err != nil {
				t.Fatalf("emitted frame does not parse: %v", err)
			}
			if h.Size() != len(p.Data) {
				t.Fatalf("frame is %d bytes, header says %d", len(p.Data), h.Size())
			}
			if err := d.Decode(p.Data, func(*audio.Buffer) error { return nil }); err != nil {
				t.Fatalf("our decoder rejects our frame: %v", err)
			}
			return nil
		}

		const blocks = 3
		buf := audio.Get(fm, 1152)
		defer audio.Put(buf)
		for blk := 0; blk < blocks; blk++ {
			buf.N = 1152
			for c := 0; c < ch; c++ {
				dst := buf.ChanF(c)
				for i := range dst {
					if len(data) == 0 {
						dst[i] = 0
						continue
					}
					b := data[(blk*1152+i*ch+c)%len(data)]
					v := (float32(b) - 127.5) / 127.5
					switch b {
					case 0xFF:
						v = float32(math.NaN())
					case 0xFE:
						v = float32(math.Inf(-1))
					case 0xFD:
						v = 1e30
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
