package flac

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// TestSilentFrameDecodesToSilence: a silent frame parses to its own length and
// decodes to n zero samples per channel.
func TestSilentFrameDecodesToSilence(t *testing.T) {
	si := StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: 44100, Channels: 2, Bits: 16}
	tmpl := []byte{0xFF, 0xF8, 0xC9, 0x18, 0x00} // 4096 samples, 44.1 kHz, stereo, 16-bit, frame 0
	tmpl = append(tmpl, crc8(tmpl))
	for _, n := range []int{1, 1152, 4096, MaxBlockSize} {
		f, err := SilentFrame(tmpl, si, n)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := FrameLen(f, si, nil); err != nil || got != len(f) {
			t.Fatalf("n=%d: FrameLen = %d (%v), want %d", n, got, err, len(f))
		}
		dec, err := NewDecoder(si, si.PCMFormat())
		if err != nil {
			t.Fatal(err)
		}
		if err := dec.Decode(f, func(b *audio.Buffer) error {
			if b.N != n {
				t.Fatalf("decoded %d samples, want %d", b.N, n)
			}
			for c := range si.Channels {
				for _, v := range b.ChanI(c) {
					if v != 0 {
						t.Fatalf("channel %d holds %d, want silence", c, v)
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
