package g711_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/g711"
)

// FuzzDecode asserts the hostile-input invariants on arbitrary packets under
// arbitrary channel counts: no panic, no production past what the packet
// holds, and every emitted buffer in the decoder's own format.
//
// There is no bitstream to corrupt here (every byte is a legal code), so what
// this actually exercises is the packet geometry: the channel split, the
// chunking loop and the ragged-length refusal.
func FuzzDecode(f *testing.F) {
	f.Add(uint8(0), uint8(1), []byte{0xD5, 0x55})
	f.Add(uint8(1), uint8(2), []byte{0xFF, 0x7F, 0x00, 0x80})
	f.Add(uint8(1), uint8(2), []byte{0x01})
	f.Add(uint8(0), uint8(6), make([]byte, 6*audio.StandardChunk+3))
	f.Add(uint8(9), uint8(1), []byte{0})
	f.Add(uint8(0), uint8(0), []byte{})

	f.Fuzz(func(t *testing.T, law, channels uint8, pkt []byte) {
		if channels < 1 || int(channels) > audio.MaxChannels {
			return
		}
		ch := int(channels)
		fmtOut := g711.Format(8000, ch, audio.DefaultLayout(ch))
		dec, err := g711.NewDecoder(g711.Law(law), fmtOut)
		if err != nil {
			return
		}
		defer dec.Release()
		var frames int
		emit := func(b *audio.Buffer) error {
			if b.Fmt != fmtOut {
				t.Fatalf("emitted %v, want %v", b.Fmt, fmtOut)
			}
			if b.N < 0 || b.N > audio.StandardChunk {
				t.Fatalf("emitted %d frames", b.N)
			}
			frames += b.N
			return nil
		}
		if err := dec.Decode(pkt, emit); err != nil {
			return
		}
		if want := len(pkt) / ch; frames != want {
			t.Fatalf("emitted %d frames from %d bytes over %d channels, want %d", frames, len(pkt), ch, want)
		}
		if err := dec.Drain(emit); err != nil {
			t.Fatalf("drain: %v", err)
		}
	})
}
