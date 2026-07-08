package alac

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// cookie builds a valid ALACSpecificConfig for the fuzz target.
func cookie(frameLen uint32, bitDepth, channels, rate int) []byte {
	b := make([]byte, CookieLen)
	binary.BigEndian.PutUint32(b[0:], frameLen)
	b[5] = byte(bitDepth)
	b[6], b[7], b[8] = 40, 10, 14 // pb, mb, kb defaults
	b[9] = byte(channels)
	binary.BigEndian.PutUint16(b[10:], 255) // maxRun
	binary.BigEndian.PutUint32(b[12:], 1024)
	binary.BigEndian.PutUint32(b[16:], 0)
	binary.BigEndian.PutUint32(b[20:], uint32(rate))
	return b
}

// FuzzDecode feeds arbitrary packet bytes to the decoder against a config
// whose frame length, bit depth, and channel count are fuzzed too, so small
// frame lengths (which exercise the predictor warm-up clamp) and the 20/24/
// 32-bit paths are reachable, not just 16-bit mono/stereo at 4096. The
// selectors map onto valid ranges so every iteration builds a real config.
// Invariant: no panic, and an emitted buffer never reports more frames than
// the configured frame length.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x20, 0x00}, uint16(4096), uint8(0), uint8(0))                 // 16-bit mono
	f.Add([]byte{0x21, 0x00, 0x00, 0x00}, uint16(4096), uint8(0), uint8(1))     // 16-bit stereo
	f.Add([]byte{0xe0}, uint16(7), uint8(2), uint8(0))                          // tiny frame, 24-bit
	f.Add([]byte{0x00, 0x00, 0x08, 0x00, 0x00}, uint16(16), uint8(3), uint8(1)) // 32-bit stereo

	depths := []int{16, 20, 24, 32}
	f.Fuzz(func(t *testing.T, data []byte, frameSel uint16, depthSel, chanSel uint8) {
		bitDepth := depths[int(depthSel)%len(depths)]
		channels := int(chanSel)%2 + 1
		frameLen := uint32(frameSel)%maxFrameLength + 1 // 1..maxFrameLength
		cfg, err := ParseMagicCookie(cookie(frameLen, bitDepth, channels, 44100))
		if err != nil {
			t.Fatalf("valid cookie rejected: %v", err)
		}
		d, err := NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		_ = d.Decode(data, func(b *audio.Buffer) error {
			if b.N > int(cfg.FrameLength) {
				t.Fatalf("emitted %d frames, cap is %d", b.N, cfg.FrameLength)
			}
			return nil
		})
	})
}

// FuzzEncode drives arbitrary PCM through the encoder and back through the
// decoder. The bytes select the depth, channel count, and frame length, and
// seed a sample generator, so the predictor, mixRes search, adaptive-Golomb
// coder, zero-run escape, and verbatim fallback all see hostile input.
// Invariant: no panic, and decode(encode(x)) == x (ALAC is lossless).
func FuzzEncode(f *testing.F) {
	f.Add(uint8(0), uint8(1), uint16(4096), uint64(1))
	f.Add(uint8(2), uint8(0), uint16(4096), uint64(7))
	f.Add(uint8(3), uint8(1), uint16(17), uint64(99))
	f.Add(uint8(1), uint8(1), uint16(9111), uint64(3))

	depths := []int{16, 20, 24, 32}
	f.Fuzz(func(t *testing.T, depthSel, chanSel uint8, lenSel uint16, seed uint64) {
		depth := depths[int(depthSel)%len(depths)]
		ch := int(chanSel)%2 + 1
		n := int(lenSel)%(FrameSize*2) + 1
		fm := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: depth}

		src := audio.Get(fm, n)
		defer audio.Put(src)
		src.N = n
		// A cheap xorshift fill spanning the depth's range.
		st := seed | 1
		lo := int32(-1) << (depth - 1)
		span := uint64(-(lo + 1)) - uint64(lo) + 1
		for c := 0; c < ch; c++ {
			s := src.ChanI(c)
			for i := range s {
				st ^= st << 13
				st ^= st >> 7
				st ^= st << 17
				s[i] = lo + int32(st%span)
			}
		}

		enc, err := NewEncoder(fm, nil)
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		cfg, err := ParseMagicCookie(enc.CodecConfig())
		if err != nil {
			t.Fatalf("cookie: %v", err)
		}
		dec, err := NewDecoder(cfg, fm)
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		defer dec.Release()

		got := audio.Get(fm, n)
		defer audio.Put(got)
		pos := 0
		emit := func(pkt codec.Packet) error {
			return dec.Decode(pkt.Data, func(b *audio.Buffer) error {
				// A correct round-trip decodes exactly the encoded samples; a
				// bug that over-produces is reported here rather than as an
				// opaque slice-bounds panic in the copy below.
				if pos+b.N > n {
					t.Fatalf("decoded %d+%d samples exceeds source length %d", pos, b.N, n)
				}
				for c := 0; c < ch; c++ {
					copy(got.I[c*got.Stride+pos:c*got.Stride+pos+b.N], b.ChanI(c))
				}
				pos += b.N
				return nil
			})
		}
		frame := audio.Get(fm, FrameSize)
		defer audio.Put(frame)
		for off := 0; off < n; off += FrameSize {
			m := min(FrameSize, n-off)
			frame.N = m
			for c := 0; c < ch; c++ {
				copy(frame.ChanI(c), src.I[c*src.Stride+off:c*src.Stride+off+m])
			}
			if err := enc.Encode(frame, emit); err != nil {
				t.Fatalf("encode: %v", err)
			}
		}
		if pos != n {
			t.Fatalf("decoded %d samples, want %d", pos, n)
		}
		for c := 0; c < ch; c++ {
			w := src.I[c*src.Stride : c*src.Stride+n]
			g := got.I[c*got.Stride : c*got.Stride+n]
			for i := range w {
				if w[i] != g[i] {
					t.Fatalf("ch%d[%d] = %d, want %d", c, i, g[i], w[i])
				}
			}
		}
	})
}
