package wavpack

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// FuzzEncode feeds arbitrary integer PCM through the encoder at every level
// and decodes each block back with our own decoder. The invariant is the one
// the format exists for: what comes out is what went in, bit for bit, whatever
// the samples were. It is a sharper target than a decode fuzz, because a
// lossless codec has no tolerance to hide a mistake in -- a single wrong bit
// anywhere in the entropy coder desynchronizes everything after it.
//
// The aac and opus encoder fuzz targets are the precedent; unlike theirs, the
// samples here are hostile only in magnitude (the format has no float input to
// poison with NaN), so the interesting inputs are the rails, the runs of
// zeros, and the alternations that drive the coder's escape paths.
func FuzzEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint8(0), uint8(16), true)
	f.Add(make([]byte, 8192), uint8(3), uint8(32), false)
	f.Add([]byte{0xFF, 0x7F, 0x00, 0x80, 0x55, 0xAA}, uint8(2), uint8(24), true)
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF}, uint8(1), uint8(8), true)

	f.Fuzz(func(t *testing.T, data []byte, levelSel, depthSel uint8, stereo bool) {
		if len(data) == 0 {
			return
		}
		ch := 1
		if stereo {
			ch = 2
		}
		depths := [4]int{8, 16, 24, 32}
		depth := depths[int(depthSel)%4]
		level := LevelFast + int(levelSel)%(LevelVeryHigh-LevelFast+1)
		fm := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch),
			Type: audio.Int, BitDepth: depth}

		enc, err := NewEncoder(fm, &EncoderOptions{Level: level})
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(Config{Rate: fm.Rate, Channels: ch, BitDepth: depth, ValidBits: depth}, fm)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()

		// The fuzz bytes become samples in range for the depth, three blocks
		// of them, so the carried cascade and entropy state are exercised
		// across boundaries rather than only from a cold start.
		const blocks = 3
		chunk := 700
		buf := audio.Get(fm, chunk)
		defer audio.Put(buf)
		want := make([][]int32, ch)
		p := 0
		// Narrowed to the depth, not clamped to it: a clamp puts a uniform
		// 32-bit value inside a 16-bit range about once in 65536 tries, so
		// every sample came out on a rail and three of the four depths never
		// saw a quiet signal at all. The arithmetic shift keeps the sign and
		// spreads the low bits, which is what reaches the zero runs, the
		// median adaptation, and the escape paths this target claims to
		// exercise.
		shift := uint(32 - depth)
		next := func() int32 {
			v := int32(0)
			for range 4 {
				v = v<<8 | int32(data[p%len(data)])
				p++
			}
			return v >> shift
		}

		var got [][]int32
		emit := func(pkt codec.Packet) error {
			if err := dec.Decode(pkt.Data, func(b *audio.Buffer) error {
				if got == nil {
					got = make([][]int32, ch)
				}
				for c := range ch {
					got[c] = append(got[c], b.ChanI(c)[:b.N]...)
				}
				return nil
			}); err != nil {
				t.Fatalf("our decoder rejects our own block: %v", err)
			}
			return nil
		}
		for range blocks {
			buf.N = chunk
			for c := range ch {
				s := buf.ChanI(c)
				for i := range s {
					s[i] = next()
				}
				want[c] = append(want[c], s...)
			}
			if err := enc.Encode(buf, emit); err != nil {
				t.Fatalf("encode: %v", err)
			}
		}
		if _, err := enc.Finish(emit); err != nil {
			t.Fatalf("finish: %v", err)
		}
		for c := range ch {
			if len(got[c]) != len(want[c]) {
				t.Fatalf("channel %d: decoded %d samples, encoded %d", c, len(got[c]), len(want[c]))
			}
			for i := range want[c] {
				if got[c][i] != want[c][i] {
					t.Fatalf("channel %d sample %d: got %d, want %d", c, i, got[c][i], want[c][i])
				}
			}
		}
	})
}
