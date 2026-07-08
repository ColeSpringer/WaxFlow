package alac

import (
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// roundTrip encodes src frame by frame and decodes the packets back,
// asserting bit-exact reconstruction (ALAC is lossless).
func roundTrip(t *testing.T, f audio.Format, src *audio.Buffer) {
	t.Helper()
	enc, err := NewEncoder(f, nil)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	cfg, err := ParseMagicCookie(enc.CodecConfig())
	if err != nil {
		t.Fatalf("cookie: %v", err)
	}
	dec, err := NewDecoder(cfg, f)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Release()

	got := audio.Get(f, src.N)
	defer audio.Put(got)
	frame := audio.Get(f, FrameSize)
	defer audio.Put(frame)

	pos := 0
	emit := func(pkt codec.Packet) error {
		return dec.Decode(pkt.Data, func(b *audio.Buffer) error {
			if int64(b.N) != pkt.Dur {
				t.Fatalf("decoded %d frames, packet says %d", b.N, pkt.Dur)
			}
			for c := 0; c < f.Channels; c++ {
				copy(got.I[c*got.Stride+pos:c*got.Stride+pos+b.N], b.ChanI(c))
			}
			pos += b.N
			return nil
		})
	}

	for off := 0; off < src.N; off += FrameSize {
		n := min(FrameSize, src.N-off)
		frame.N = n
		for c := 0; c < f.Channels; c++ {
			copy(frame.ChanI(c), src.I[c*src.Stride+off:c*src.Stride+off+n])
		}
		if err := enc.Encode(frame, emit); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if _, err := enc.Finish(func(codec.Packet) error { return nil }); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if pos != src.N {
		t.Fatalf("decoded %d samples, want %d", pos, src.N)
	}
	for c := 0; c < f.Channels; c++ {
		w := src.I[c*src.Stride : c*src.Stride+src.N]
		g := got.I[c*got.Stride : c*got.Stride+src.N]
		for i := range w {
			if w[i] != g[i] {
				t.Fatalf("ch%d[%d] = %d, want %d", c, i, g[i], w[i])
			}
		}
	}
}

// fillNoise writes deterministic full-range noise, which stresses the
// verbatim fallback and the Golomb escape (incompressible input).
func fillNoise(b *audio.Buffer, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed))
	lo := int32(-1) << (b.Fmt.BitDepth - 1)
	hi := -(lo + 1)
	for c := 0; c < b.Fmt.Channels; c++ {
		s := b.ChanI(c)
		for i := range s {
			s[i] = lo + int32(rng.Uint64N(uint64(hi)-uint64(lo)+1))
		}
		if len(s) >= 2 {
			s[0], s[1] = lo, hi // exercise the range extremes
		}
	}
}

// fillSine writes a compressible tone (a smooth signal the predictor
// tracks), so the compressed path and the mixRes search are exercised.
func fillSine(b *audio.Buffer, seed uint64) {
	amp := float64(int32(1)<<(b.Fmt.BitDepth-1)) - 1
	for c := 0; c < b.Fmt.Channels; c++ {
		s := b.ChanI(c)
		freq := 0.01 + 0.003*float64(c) + 0.0001*float64(seed%7)
		for i := range s {
			s[i] = int32(amp * sinApprox(freq*float64(i)))
		}
	}
}

// sinApprox is a small deterministic sine (no math import needed to keep
// the test independent of platform libm rounding).
func sinApprox(x float64) float64 {
	// wrap into [-pi, pi]
	const pi = 3.141592653589793
	x -= 2 * pi * float64(int(x/(2*pi)))
	if x > pi {
		x -= 2 * pi
	} else if x < -pi {
		x += 2 * pi
	}
	x2 := x * x
	return x * (1 - x2/6*(1-x2/20*(1-x2/42)))
}

func TestEncodeRoundTrip(t *testing.T) {
	depths := []int{16, 20, 24, 32}
	channels := []int{1, 2}
	lengths := []int{FrameSize, FrameSize*2 + 137, 1, 33, 9111} // full, multi, tiny, short, partial-tail
	for _, depth := range depths {
		for _, ch := range channels {
			for _, n := range lengths {
				f := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: depth}
				for _, gen := range []struct {
					name string
					fill func(*audio.Buffer, uint64)
				}{{"sine", fillSine}, {"noise", fillNoise}} {
					t.Run(f.String()+"/"+gen.name+"/"+itoa(n), func(t *testing.T) {
						src := audio.Get(f, n)
						defer audio.Put(src)
						src.N = n
						gen.fill(src, uint64(depth*10+ch))
						roundTrip(t, f, src)
					})
				}
			}
		}
	}
}

// frameEscape reads the escape bit of the first element in an ALAC frame
// (element tag 3, elementInstanceTag 4, reserved 12, then the 4-bit header
// whose low bit is escape).
func frameEscape(pkt []byte) bool {
	r := &bitReader{data: pkt, validBits: len(pkt) * 8}
	r.read(3)
	r.read(4)
	r.read(12)
	return r.read(4)&1 != 0
}

// TestCompressedVsVerbatimChoice pins the size-driven escape decision, which
// losslessness alone cannot exercise (both branches decode identically):
// incompressible full-range noise takes the uncompressed escape element,
// while a compressible tone stays in the Golomb-coded form.
func TestCompressedVsVerbatimChoice(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	cases := []struct {
		name       string
		fill       func(*audio.Buffer, uint64)
		wantEscape bool
	}{
		{"noise takes verbatim", fillNoise, true},
		{"tone takes compressed", fillSine, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := audio.Get(f, FrameSize)
			defer audio.Put(src)
			src.N = FrameSize
			tc.fill(src, 5)
			enc, err := NewEncoder(f, nil)
			if err != nil {
				t.Fatal(err)
			}
			var pkt []byte
			if err := enc.Encode(src, func(p codec.Packet) error {
				pkt = append([]byte(nil), p.Data...)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := frameEscape(pkt); got != tc.wantEscape {
				t.Errorf("escape bit = %v, want %v", got, tc.wantEscape)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
