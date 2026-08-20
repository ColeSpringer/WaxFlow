package ape

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// FuzzEncode feeds arbitrary integer PCM through the encoder at every level it
// writes and decodes each frame back with our own decoder. The invariant is
// the one the format exists for: what comes out is what went in, bit for bit,
// whatever the samples were. It is a sharper target than a decode fuzz,
// because a lossless codec has no tolerance to hide a mistake in -- one wrong
// bit anywhere in the range coder desynchronizes everything after it, and the
// frame's own CRC catches what the comparison would not.
//
// codec/wavpack's FuzzEncode is the precedent, including its lesson: the
// samples are narrowed to the depth with an arithmetic shift rather than
// clamped to it, since a clamp lands in range about once in 65536 tries at
// 16-bit and leaves every sample on a rail.
func FuzzEncode(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint8(0), uint8(1), true)
	f.Add(make([]byte, 4096), uint8(1), uint8(1), false)
	f.Add([]byte{0xFF, 0x7F, 0x00, 0x80, 0x55, 0xAA}, uint8(2), uint8(2), true)
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF}, uint8(1), uint8(0), true)

	f.Fuzz(func(t *testing.T, data []byte, levelSel, depthSel uint8, stereo bool) {
		if len(data) == 0 {
			return
		}
		ch := 1
		if stereo {
			ch = 2
		}
		depths := [3]int{8, 16, 24}
		depth := depths[int(depthSel)%3]
		level := LevelFast + 1000*(int(levelSel)%(1+(MaxEncodeLevel-LevelFast)/1000))
		fm := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch),
			Type: audio.Int, BitDepth: depth}

		enc, err := NewEncoder(fm, &EncoderOptions{Level: level})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseConfig(enc.CodecConfig())
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewDecoder(cfg, fm)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()

		// Short chunks on purpose: a whole frame is 73728 blocks, which no
		// fuzz iteration should spend, and the encoder's own accumulation is
		// then what is under test alongside the coding. Everything lands in
		// the one frame Finish flushes.
		const chunks = 3
		chunk := 700
		buf := audio.Get(fm, chunk)
		defer audio.Put(buf)
		want := make([][]int32, ch)
		p := 0
		shift := uint(32 - depth)
		next := func() int32 {
			v := int32(0)
			for range 4 {
				v = v<<8 | int32(data[p%len(data)])
				p++
			}
			return v >> shift
		}

		got := make([][]int32, ch)
		emit := func(pkt codec.Packet) error {
			if err := dec.Decode(pkt.Data, func(b *audio.Buffer) error {
				for c := range ch {
					got[c] = append(got[c], b.ChanI(c)[:b.N]...)
				}
				return nil
			}); err != nil {
				t.Fatalf("our decoder rejects our own frame: %v", err)
			}
			return nil
		}
		for range chunks {
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
