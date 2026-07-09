package opus

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// FuzzDecode drives the full SILK/CELT/hybrid decode path with arbitrary packets
// on both mono and stereo decoders: no hostile packet may panic or read out of
// bounds, since a registered Opus decoder is reachable from untrusted input.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x00, 0x11, 0x22})       // SILK NB
	f.Add([]byte{0x64, 0x33, 0x44, 0x55}) // hybrid SWB
	f.Add([]byte{0xFC, 0x00, 0x01, 0x02}) // CELT FB
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, ch := range []int{1, 2} {
			cfg := Config{Channels: ch, Family: 1}
			d, err := NewDecoder(cfg, cfg.Format())
			if err != nil {
				continue
			}
			_ = d.Decode(data, func(b *audio.Buffer) error { return nil })
			d.Release()
		}
	})
}

// FuzzSplitPacket exercises TOC parsing and frame splitting on arbitrary bytes:
// the framing must never panic or index out of range on hostile packets.
func FuzzSplitPacket(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0x01, 0x02, 0x03, 0x04})
	f.Add([]byte{0xFF, 0x83, 0x10, 0x20, 0x30})       // code 3, VBR, 3 frames
	f.Add([]byte{0xFC, 0xC0, 0xFF, 0x05, 0x01, 0x02}) // code 3, padded
	f.Fuzz(func(t *testing.T, data []byte) {
		frames, err := splitPacket(data)
		if err != nil {
			return
		}
		// A successful split must not claim more bytes than the packet holds.
		total := 0
		for _, fr := range frames {
			total += len(fr.data)
		}
		if total > len(data) {
			t.Fatalf("frames total %d bytes exceed packet %d", total, len(data))
		}
	})
}

// FuzzRangeDecoder drives the entropy decoder with arbitrary buffers and a mix
// of symbol reads: it must never panic, spin, or read out of bounds.
func FuzzRangeDecoder(f *testing.F) {
	f.Add([]byte{0x80, 0x00, 0x00, 0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	icdf := []byte{224, 128, 64, 0}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		d := newRangeDecoder(data)
		for i := 0; i < 64; i++ {
			switch i % 5 {
			case 0:
				d.decodeICDF(icdf, 8)
			case 1:
				d.decodeBitLogp(3)
			case 2:
				if s := d.decode(16); s < 16 {
					d.update(s, s+1, 16)
				}
			case 3:
				d.decodeRawBits(5)
			case 4:
				d.decodeUint(1000)
			}
			_ = d.tell()
		}
	})
}
