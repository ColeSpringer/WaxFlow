package g711_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/g711"
	"github.com/colespringer/waxflow/waxerr"
)

// wantALaw and wantMuLaw are the expansions written as ITU-T G.711 states
// them in prose: a magnitude in the law's own precision (13 bits for A-law,
// 14 for mu-law) scaled up to the 16-bit domain. The package builds its
// tables from the segment-and-step form instead, so the two agree only if
// both are right; transcribing the implementation would have tested nothing.
func wantALaw(b byte) int {
	v := int(b ^ 0x55)
	q, seg := v&0x0F, (v&0x70)>>4
	mag := 2*q + 1
	if seg > 0 {
		mag = (2*q + 33) << (seg - 1)
	}
	mag *= 8 // 13-bit magnitude into the 16-bit domain
	if v&0x80 == 0 {
		return -mag
	}
	return mag
}

func wantMuLaw(b byte) int {
	v := int(^b & 0xFF)
	q, seg := v&0x0F, (v&0x70)>>4
	mag := ((2*q+33)<<seg - 33) * 4 // 14-bit magnitude into the 16-bit domain
	if v&0x80 != 0 {
		return -mag
	}
	return mag
}

// TestEveryCode walks all 256 inputs of both laws against the closed forms
// above, which is the whole of each codec: G.711 has no state, no block
// structure and no configuration, so a table that matches at every code
// matches on every stream.
func TestEveryCode(t *testing.T) {
	for _, tt := range []struct {
		law  g711.Law
		want func(byte) int
	}{{g711.ALaw, wantALaw}, {g711.MuLaw, wantMuLaw}} {
		t.Run(tt.law.String(), func(t *testing.T) {
			got := decodeBytes(t, tt.law, 1, allCodes())
			for i, v := range got {
				if want := tt.want(byte(i)); int(v) != want {
					t.Fatalf("code %#02x -> %d, want %d", i, v, want)
				}
			}
		})
	}
}

// TestKnownPairs pins the four values every G.711 implementation is checked
// against by hand: each law's two codes nearest silence, one per sign.
func TestKnownPairs(t *testing.T) {
	cases := []struct {
		law  g711.Law
		code byte
		want int32
	}{
		{g711.ALaw, 0xD5, 8},
		{g711.ALaw, 0x55, -8},
		// mu-law's two zero codes: the law is symmetric about a value it can
		// spell twice, so both sides of silence decode to exactly 0.
		{g711.MuLaw, 0xFF, 0},
		{g711.MuLaw, 0x7F, 0},
	}
	for _, tt := range cases {
		got := decodeBytes(t, tt.law, 1, []byte{tt.code})
		if got[0] != tt.want {
			t.Errorf("%v code %#02x -> %d, want %d", tt.law, tt.code, got[0], tt.want)
		}
	}
}

// TestPeaks pins each law's full-scale magnitude, which is what says the
// tables land in the 16-bit domain rather than the law's own precision.
func TestPeaks(t *testing.T) {
	for _, tt := range []struct {
		law  g711.Law
		peak int32
	}{{g711.ALaw, 32256}, {g711.MuLaw, 32124}} {
		var maxAbs int32
		for _, v := range decodeBytes(t, tt.law, 1, allCodes()) {
			if v > maxAbs {
				maxAbs = v
			}
			if -v > maxAbs {
				maxAbs = -v
			}
		}
		if maxAbs != tt.peak {
			t.Errorf("%v peak magnitude %d, want %d", tt.law, maxAbs, tt.peak)
		}
	}
}

// TestStereoDeinterleave checks the channel split, the only thing in the
// decoder that is not the table.
func TestStereoDeinterleave(t *testing.T) {
	pkt := []byte{0xD5, 0x55, 0xD5, 0x55}
	dec, err := g711.NewDecoder(g711.ALaw, g711.Format(8000, 2, audio.DefaultLayout(2)))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	err = dec.Decode(pkt, func(b *audio.Buffer) error {
		if b.N != 2 {
			t.Fatalf("N = %d, want 2", b.N)
		}
		for i, want := range []int32{8, 8} {
			if got := b.ChanI(0)[i]; got != want {
				t.Errorf("left[%d] = %d, want %d", i, got, want)
			}
		}
		for i, want := range []int32{-8, -8} {
			if got := b.ChanI(1)[i]; got != want {
				t.Errorf("right[%d] = %d, want %d", i, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestChunking pins the emit contract: no buffer longer than the pipeline's
// working size, and the frames all arrive in order.
func TestChunking(t *testing.T) {
	const frames = audio.StandardChunk*2 + 7
	got := decodeBytes(t, g711.MuLaw, 1, make([]byte, frames))
	if len(got) != frames {
		t.Fatalf("decoded %d frames, want %d", len(got), frames)
	}
}

// TestRaggedPacket pins the one malformed shape a packet can have.
func TestRaggedPacket(t *testing.T) {
	dec, err := g711.NewDecoder(g711.ALaw, g711.Format(8000, 2, audio.DefaultLayout(2)))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	err = dec.Decode([]byte{0, 0, 0}, func(*audio.Buffer) error { return nil })
	if waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Errorf("3 bytes into a stereo decoder: %v (code %v), want malformed", err, waxerr.CodeOf(err))
	}
}

// TestConstructorRejects pins that a decoder cannot be built around a format
// it does not produce, which is the wiring check demuxers rely on.
func TestConstructorRejects(t *testing.T) {
	cases := []struct {
		name string
		law  g711.Law
		f    audio.Format
	}{
		{"unknown law", g711.Law(7), g711.Format(8000, 1, audio.DefaultLayout(1))},
		{"wrong depth", g711.ALaw, audio.Format{Rate: 8000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 8}},
		{"float output", g711.ALaw, audio.Format{Rate: 8000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := g711.NewDecoder(tt.law, tt.f); err == nil {
				t.Error("built a decoder for a format it does not produce")
			}
		})
	}
}

func allCodes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// decodeBytes runs one packet through a fresh decoder and returns the
// interleaved result.
func decodeBytes(t *testing.T, law g711.Law, channels int, pkt []byte) []int32 {
	t.Helper()
	dec, err := g711.NewDecoder(law, g711.Format(8000, channels, audio.DefaultLayout(channels)))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	var out []int32
	err = dec.Decode(pkt, func(b *audio.Buffer) error {
		if b.N > audio.StandardChunk {
			t.Errorf("emitted %d frames, more than the %d standard chunk", b.N, audio.StandardChunk)
		}
		for i := range b.N {
			for c := range channels {
				out = append(out, b.ChanI(c)[i])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
