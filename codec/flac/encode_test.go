package flac_test

import (
	"crypto/md5"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/internal/testutil"
)

func fmtFor(rate, channels, bits int) audio.Format {
	return audio.Format{
		Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels),
		Type: audio.Int, BitDepth: bits,
	}
}

// encodeAll runs src through a fresh encoder at the given level,
// chunked at the encoder's frame size, and returns the frame packets,
// the STREAMINFO config, the trailer, and the finished encoder.
func encodeAll(t *testing.T, src *audio.Buffer, level int) ([][]byte, flac.StreamInfo, codec.Trailer, *flac.Encoder) {
	t.Helper()
	enc, err := flac.NewEncoder(src.Fmt, &flac.EncoderOptions{Level: level})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	si, err := flac.ParseStreamInfo(enc.CodecConfig())
	if err != nil {
		t.Fatalf("CodecConfig does not parse: %v", err)
	}

	var packets [][]byte
	var pts int64
	emit := func(p codec.Packet) error {
		if p.PTS != pts {
			t.Fatalf("packet PTS %d, want %d", p.PTS, pts)
		}
		if !p.Sync {
			t.Fatal("FLAC frames are all sync points")
		}
		pts += p.Dur
		packets = append(packets, append([]byte(nil), p.Data...))
		return nil
	}

	block := enc.FrameSize()
	chunk := audio.Get(src.Fmt, block)
	defer audio.Put(chunk)
	for off := 0; off < src.N; off += block {
		n := min(block, src.N-off)
		audio.CopyFrames(chunk, 0, src, off, n)
		chunk.N = n
		if err := enc.Encode(chunk, emit); err != nil {
			t.Fatalf("Encode at %d: %v", off, err)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if trailer.Samples != int64(src.N) || trailer.Delay != 0 || trailer.Padding != 0 {
		t.Fatalf("trailer %+v, want %d samples and no trims", trailer, src.N)
	}
	return packets, si, trailer, enc
}

// decodeAll decodes packets and asserts the result equals src exactly.
func decodeAll(t *testing.T, packets [][]byte, si flac.StreamInfo, src *audio.Buffer) {
	t.Helper()
	dec, err := flac.NewDecoder(si, src.Fmt)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pos := 0
	for i, pkt := range packets {
		err := dec.Decode(pkt, func(out *audio.Buffer) error {
			for c := 0; c < src.Fmt.Channels; c++ {
				want := src.ChanI(c)[pos : pos+out.N]
				if d := testutil.DiffI32(out.ChanI(c), want); d >= 0 {
					t.Fatalf("frame %d channel %d differs at sample %d", i, c, pos+d)
				}
			}
			pos += out.N
			return nil
		})
		if err != nil {
			t.Fatalf("decoding frame %d: %v", i, err)
		}
	}
	if pos != src.N {
		t.Fatalf("decoded %d samples, want %d", pos, src.N)
	}
}

func TestEncodeRoundTripSignals(t *testing.T) {
	cases := []struct {
		name   string
		fmt    audio.Format
		frames int
		fill   func(f audio.Format, frames int) *audio.Buffer
	}{
		{"sine-16-stereo", fmtFor(44100, 2, 16), 20000, sineFill},
		{"noise-16-stereo", fmtFor(44100, 2, 16), 20000, noiseFill},
		{"sine-8-mono", fmtFor(8000, 1, 8), 5000, sineFill},
		{"sine-24-stereo", fmtFor(96000, 2, 24), 20000, sineFill},
		{"noise-32-stereo", fmtFor(48000, 2, 32), 12000, noiseFill},
		{"sine-20-stereo", fmtFor(48000, 2, 20), 9000, sineFill},
		{"sine-4-mono", fmtFor(48000, 1, 4), 5000, sineFill},
		{"ramp-16-5_1", fmtFor(48000, 6, 16), 10000, rampFill},
		{"short-tail", fmtFor(44100, 2, 16), 4096 + 100, sineFill},
		{"tiny", fmtFor(44100, 2, 16), 5, sineFill},
		{"odd-rate-hz", fmtFor(44101, 2, 16), 9000, sineFill},
		{"odd-rate-khz", fmtFor(255000, 2, 16), 9000, sineFill},
		{"odd-rate-dahz", fmtFor(655350, 2, 16), 9000, sineFill},
		{"streaminfo-rate", fmtFor(1048575, 2, 16), 9000, sineFill},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.fill(tc.fmt, tc.frames)
			defer audio.Put(src)
			packets, si, _, _ := encodeAll(t, src, flac.DefaultEncoderLevel)
			decodeAll(t, packets, si, src)
		})
	}
}

func sineFill(f audio.Format, frames int) *audio.Buffer {
	return testutil.Sine(f, frames, 997, 0.8)
}

func noiseFill(f audio.Format, frames int) *audio.Buffer {
	return testutil.Noise(f, frames, 0x5EED)
}

func rampFill(f audio.Format, frames int) *audio.Buffer {
	return testutil.Ramp(f, frames)
}

func TestEncodeRoundTripAllLevels(t *testing.T) {
	f := fmtFor(44100, 2, 16)
	src := testutil.Noise(f, 3*4096+321, 7)
	defer audio.Put(src)
	for level := 0; level <= 8; level++ {
		packets, si, _, _ := encodeAll(t, src, level)
		decodeAll(t, packets, si, src)
		if si.MinBlock != flac.EncoderBlockSize(level) || si.MinBlock != si.MaxBlock {
			t.Errorf("level %d: STREAMINFO blocks %d..%d, want constant %d",
				level, si.MinBlock, si.MaxBlock, flac.EncoderBlockSize(level))
		}
	}
}

func TestEncodeSpecialSignals(t *testing.T) {
	f := fmtFor(44100, 2, 16)
	cases := []struct {
		name string
		fill func(*audio.Buffer)
	}{
		{"silence", func(b *audio.Buffer) {
			for c := 0; c < b.Fmt.Channels; c++ {
				clear(b.ChanI(c))
			}
		}},
		{"constant", func(b *audio.Buffer) {
			for c := 0; c < b.Fmt.Channels; c++ {
				s := b.ChanI(c)
				for i := range s {
					s[i] = -12345
				}
			}
		}},
		// Every sample carries trailing zero bits, the wasted-bits case
		// (an 12-bit signal shipped in a 16-bit stream).
		{"wasted-bits", func(b *audio.Buffer) {
			for c := 0; c < b.Fmt.Channels; c++ {
				s := b.ChanI(c)
				for i := range s {
					s[i] = int32(int8(i*(c+3))) << 4
				}
			}
		}},
		// Full-scale alternation defeats prediction; verbatim must win
		// and still round-trip.
		{"anti-predictable", func(b *audio.Buffer) {
			for c := 0; c < b.Fmt.Channels; c++ {
				s := b.ChanI(c)
				for i := range s {
					if i%2 == 0 {
						s[i] = 32767
					} else {
						s[i] = -32768
					}
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := audio.Get(f, 4096+55)
			defer audio.Put(src)
			src.N = src.Cap()
			tc.fill(src)
			packets, si, _, _ := encodeAll(t, src, flac.DefaultEncoderLevel)
			decodeAll(t, packets, si, src)
		})
	}
}

// TestEncodeMD5 checks the signature against a direct MD5 of the
// interleaved little-endian samples, including a 24-bit case where the
// sample bytes are not a power of two.
func TestEncodeMD5(t *testing.T) {
	for _, bits := range []int{16, 24} {
		f := fmtFor(44100, 2, bits)
		src := testutil.Noise(f, 4096+700, 11)
		defer audio.Put(src)

		bs := (bits + 7) / 8
		raw := make([]byte, 0, src.N*2*bs)
		for i := 0; i < src.N; i++ {
			for c := 0; c < 2; c++ {
				v := src.ChanI(c)[i]
				for b := 0; b < bs; b++ {
					raw = append(raw, byte(v>>(8*b)))
				}
			}
		}
		want := md5.Sum(raw)

		_, _, _, enc := encodeAll(t, src, 5)
		if got := enc.MD5(); got != want {
			t.Errorf("%d-bit MD5 %x, want %x", bits, got, want)
		}
	}
}

func TestEncodeEmptyStream(t *testing.T) {
	f := fmtFor(44100, 2, 16)
	enc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(func(codec.Packet) error { t.Fatal("no packets expected"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if trailer.Samples != 0 {
		t.Fatalf("trailer samples %d, want 0", trailer.Samples)
	}
	if got, want := enc.MD5(), md5.Sum(nil); got != want {
		t.Errorf("empty MD5 %x, want %x", got, want)
	}
}

func TestEncoderMisuse(t *testing.T) {
	f := fmtFor(44100, 2, 16)
	discard := func(codec.Packet) error { return nil }

	if _, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: 9}); err == nil {
		t.Error("level 9 accepted")
	}
	if _, err := flac.NewEncoder(fmtFor(44100, 2, 3), nil); err == nil {
		t.Error("3-bit depth accepted")
	}
	if _, err := flac.NewEncoder(fmtFor(1<<20, 2, 16), nil); err == nil {
		t.Error("rate past the STREAMINFO field accepted")
	}
	floatFmt := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	if _, err := flac.NewEncoder(floatFmt, nil); err == nil {
		t.Error("float format accepted")
	}

	enc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := testutil.Sine(f, enc.FrameSize(), 500, 0.5)
	defer audio.Put(buf)

	// A short chunk is accepted only as the stream's last.
	short := audio.Get(f, 100)
	defer audio.Put(short)
	short.N = 100
	if err := enc.Encode(short, discard); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(buf, discard); err == nil {
		t.Error("full chunk after a short chunk accepted")
	}

	if _, err := enc.Finish(discard); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(buf, discard); err == nil {
		t.Error("Encode after Finish accepted")
	}
	if _, err := enc.Finish(discard); err == nil {
		t.Error("second Finish accepted")
	}

	enc2, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	over := audio.Get(f, enc2.FrameSize()+1)
	defer audio.Put(over)
	over.N = over.Cap()
	if err := enc2.Encode(over, discard); err == nil {
		t.Error("oversized chunk accepted")
	}
	wrong := testutil.Sine(fmtFor(48000, 2, 16), 100, 500, 0.5)
	defer audio.Put(wrong)
	if err := enc2.Encode(wrong, discard); err == nil {
		t.Error("mismatched buffer format accepted")
	}
}

// TestEncodeFrameHeaders spot-checks wire fields the decoder resolves
// silently: frame numbering, block size codes, and the subset-required
// in-header rate and depth codes.
func TestEncodeFrameHeaders(t *testing.T) {
	f := fmtFor(44100, 2, 16)
	src := testutil.Sine(f, 2*4096+300, 997, 0.8)
	defer audio.Put(src)
	packets, si, _, _ := encodeAll(t, src, 5)
	if len(packets) != 3 {
		t.Fatalf("%d packets, want 3", len(packets))
	}
	num := flac.Numbering{}
	for i, pkt := range packets {
		fi, err := flac.ParseFrameHeader(pkt)
		if err != nil {
			t.Fatalf("frame %d header: %v", i, err)
		}
		if fi.Variable {
			t.Fatal("encoder must use the fixed blocking strategy")
		}
		if i == 0 {
			num = si.Numbering(fi)
		}
		if got, want := fi.Coded, uint64(i); got != want {
			t.Errorf("frame %d coded number %d", i, got)
		}
		if fi.Rate != 44100 || fi.Bits != 16 {
			t.Errorf("frame %d does not carry rate and depth in-header (subset): rate %d bits %d", i, fi.Rate, fi.Bits)
		}
		wantBlock := 4096
		if i == 2 {
			wantBlock = 300
		}
		if fi.BlockSize != wantBlock {
			t.Errorf("frame %d block size %d, want %d", i, fi.BlockSize, wantBlock)
		}
	}
	_ = num
}

func TestEncoderVersionAndBlockSize(t *testing.T) {
	if flac.EncoderVersion(0) == flac.EncoderVersion(5) {
		t.Error("versions must distinguish levels")
	}
	if flac.EncoderBlockSize(0) != 1152 || flac.EncoderBlockSize(5) != 4096 {
		t.Errorf("unexpected block sizes: %d, %d", flac.EncoderBlockSize(0), flac.EncoderBlockSize(5))
	}
	if flac.EncoderBlockSize(9) != 0 || flac.EncoderBlockSize(-1) != 0 {
		t.Error("out-of-range levels must report 0")
	}
}

// FuzzEncodeRoundTrip drives the encoder with arbitrary sample data and
// formats, asserting decode(encode(x)) == x always.
func FuzzEncodeRoundTrip(f *testing.F) {
	f.Add(uint8(2), uint8(16), uint8(5), 3000, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add(uint8(1), uint8(8), uint8(0), 100, []byte{0xFF, 0x00})
	f.Add(uint8(2), uint8(32), uint8(8), 500, []byte{0x80, 0x7F, 1, 2})
	f.Fuzz(func(t *testing.T, channels, bits, level uint8, frames int, seed []byte) {
		ch := int(channels)%8 + 1
		depth := int(bits)%29 + 4
		lv := int(level) % 9
		n := frames % 9000
		if n < 0 {
			n = -n
		}
		if n == 0 || len(seed) == 0 {
			t.Skip()
		}
		fm := fmtFor(44100, ch, depth)
		src := audio.Get(fm, n)
		defer audio.Put(src)
		src.N = n
		lim := int64(1)<<(depth-1) - 1
		for c := 0; c < ch; c++ {
			s := src.ChanI(c)
			for i := range s {
				b := int64(seed[(i*ch+c)%len(seed)])
				v := (b - 128) * lim / 127
				if v > lim {
					v = lim
				}
				if v < -lim-1 {
					v = -lim - 1
				}
				s[i] = int32(v)
			}
		}
		packets, si, _, _ := encodeAll(t, src, lv)
		decodeAll(t, packets, si, src)
	})
}
