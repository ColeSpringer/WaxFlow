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
// The amplitude math is int64: 1<<31 overflows int32 and would flip the
// waveform's sign at depth 32, with the out-of-range peaks then landing
// wherever the platform's float-to-int conversion puts them.
func fillSine(b *audio.Buffer, seed uint64) {
	amp := float64(int64(1)<<(b.Fmt.BitDepth-1) - 1)
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

// frameHeader reads the first element's 4-bit header in an ALAC frame
// (element tag 3, elementInstanceTag 4, reserved 12, then partialFrame,
// bytesShifted, and the escape bit).
func frameHeader(pkt []byte) (bytesShifted uint32, escape bool) {
	r := &bitReader{data: pkt, validBits: len(pkt) * 8}
	r.read(3)
	r.read(4)
	r.read(12)
	hdr := r.read(4)
	return hdr >> 1 & 3, hdr&1 != 0
}

// encodeOneFrame encodes one full frame at the given depth and returns the
// packet bytes.
func encodeOneFrame(t *testing.T, depth, ch int, fill func(*audio.Buffer, uint64)) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: depth}
	src := audio.Get(f, FrameSize)
	defer audio.Put(src)
	src.N = FrameSize
	fill(src, 5)
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
	return pkt
}

// TestCompressedVsVerbatimChoice pins the size-driven escape decision, which
// losslessness alone cannot exercise (both branches decode identically):
// incompressible full-range noise takes the uncompressed escape element,
// while a compressible tone stays in the Golomb-coded form. At 24 bits the
// compressed form carries the shift region on top of the coded high part,
// so full-range noise exceeds its own raw width there too; the escape must
// stay reachable, and its header must carry no shift (the reference writes
// escapes at the plain depth, shift bits zero, and decoders read them that
// way). The 32-bit stereo pair is the exception and never escapes, ffmpeg
// lacking the uncompressed pair at that width; 32-bit mono escapes as the
// reference does. See encodeFrame.
func TestCompressedVsVerbatimChoice(t *testing.T) {
	cases := []struct {
		name       string
		depth, ch  int
		fill       func(*audio.Buffer, uint64)
		wantEscape bool
	}{
		{"16-bit noise takes verbatim", 16, 2, fillNoise, true},
		{"16-bit tone takes compressed", 16, 2, fillSine, false},
		{"24-bit noise takes verbatim", 24, 2, fillNoise, true},
		{"24-bit tone takes compressed", 24, 2, fillSine, false},
		{"32-bit stereo noise stays compressed", 32, 2, fillNoise, false},
		{"32-bit mono noise takes verbatim", 32, 1, fillNoise, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkt := encodeOneFrame(t, tc.depth, tc.ch, tc.fill)
			shift, escape := frameHeader(pkt)
			if escape != tc.wantEscape {
				t.Errorf("escape bit = %v, want %v", escape, tc.wantEscape)
			}
			if escape && shift != 0 {
				t.Errorf("escape header carries shift %d, the reference writes 0", shift)
			}
		})
	}
}

// TestShiftPolicyMatchesTheReference pins the per-depth shift-off choice to
// the reference encoder's: nothing at 16 and 20 bits, one byte at 24, two at
// 32 ("24-bit mode really improves with one byte shifted off"; 32-bit cannot
// be matrixed at full width). Losslessness cannot see the choice, since any
// shift round-trips; what it decides is size. Unshifted, a 24-bit stream's
// residuals outgrow the Golomb parameter's kb cap and pay the 9-ones escape
// on loud material, which is what made a 24-bit encode of widened 16-bit
// audio more than twice the 16-bit encode.
func TestShiftPolicyMatchesTheReference(t *testing.T) {
	want := map[int]uint32{16: 0, 20: 0, 24: 1, 32: 2}
	for depth, wantShift := range want {
		for _, ch := range []int{1, 2} {
			t.Run(itoa(depth)+"-bit/"+itoa(ch)+"ch", func(t *testing.T) {
				pkt := encodeOneFrame(t, depth, ch, fillSine)
				shift, escape := frameHeader(pkt)
				if escape {
					t.Fatal("tone frame took the escape; the shift choice is unobservable")
				}
				if shift != wantShift {
					t.Errorf("bytesShifted = %d, want %d", shift, wantShift)
				}
			})
		}
	}
}

// TestWidenedDepthCostsOnlyTheRawBytes is the size property behind the shift:
// a 16-bit signal widened to a deeper depth (low bytes all zero) must cost
// exactly the raw low bytes on top of its own 16-bit encode. With the shift
// in place this is an identity, not an approximation: the coded high part IS
// the 16-bit stream (same channel width, same residuals, same headers), and
// the shift region adds its bytes verbatim. Without the shift the widened
// residuals drag the Golomb coder into its escape and the cost has no bound
// worth stating, which is how this was found (a 24-bit APE/ALAC encode of
// widened 16-bit audio several times its 16-bit size).
func TestWidenedDepthCostsOnlyTheRawBytes(t *testing.T) {
	for _, ch := range []int{1, 2} {
		for _, tc := range []struct {
			depth int
			shift uint // widen by this many bits; shifted bytes = shift/8
		}{{24, 8}, {32, 16}} {
			t.Run(itoa(tc.depth)+"-bit/"+itoa(ch)+"ch", func(t *testing.T) {
				n := FrameSize + 1517 // a full frame plus a partial one
				f16 := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Int, BitDepth: 16}
				fw := f16
				fw.BitDepth = tc.depth

				src16 := audio.Get(f16, n)
				defer audio.Put(src16)
				src16.N = n
				fillSine(src16, 9)
				srcW := audio.Get(fw, n)
				defer audio.Put(srcW)
				srcW.N = n
				for c := 0; c < ch; c++ {
					w, s := srcW.ChanI(c), src16.ChanI(c)
					for i := 0; i < n; i++ {
						w[i] = s[i] << tc.shift
					}
				}

				sizes := func(f audio.Format, src *audio.Buffer) []int {
					enc, err := NewEncoder(f, nil)
					if err != nil {
						t.Fatal(err)
					}
					frame := audio.Get(f, FrameSize)
					defer audio.Put(frame)
					var out []int
					for off := 0; off < src.N; off += FrameSize {
						m := min(FrameSize, src.N-off)
						frame.N = m
						for c := 0; c < f.Channels; c++ {
							copy(frame.ChanI(c), src.I[c*src.Stride+off:c*src.Stride+off+m])
						}
						if err := enc.Encode(frame, func(p codec.Packet) error {
							out = append(out, len(p.Data))
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					return out
				}
				got16 := sizes(f16, src16)
				gotW := sizes(fw, srcW)
				raw := int(tc.shift) / 8 * ch
				for i := range got16 {
					m := min(FrameSize, n-i*FrameSize)
					if want := got16[i] + m*raw; gotW[i] != want {
						t.Errorf("frame %d: %d-bit packet is %d bytes, want the 16-bit packet's %d plus %d raw low bytes = %d",
							i, tc.depth, gotW[i], got16[i], m*raw, want)
					}
				}
			})
		}
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
