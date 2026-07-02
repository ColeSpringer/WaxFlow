package pcm

import (
	"bytes"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// wireMatrix is every wire configuration M1 supports, exercised by the
// round-trip and marshal tests.
var wireMatrix = []struct {
	name string
	cfg  Config
}{
	{"u8", Config{Encoding: UnsignedInt, Bits: 8}},
	{"s8", Config{Encoding: SignedInt, Bits: 8}},
	{"s16le", Config{Encoding: SignedInt, Bits: 16}},
	{"s16be", Config{Encoding: SignedInt, Bits: 16, BigEndian: true}},
	{"s24le", Config{Encoding: SignedInt, Bits: 24}},
	{"s24be", Config{Encoding: SignedInt, Bits: 24, BigEndian: true}},
	{"s32le", Config{Encoding: SignedInt, Bits: 32}},
	{"s32be", Config{Encoding: SignedInt, Bits: 32, BigEndian: true}},
	{"s24in32le", Config{Encoding: SignedInt, Bits: 32, ValidBits: 24}},
	{"f32le", Config{Encoding: Float, Bits: 32}},
	{"f32be", Config{Encoding: Float, Bits: 32, BigEndian: true}},
	{"f64le", Config{Encoding: Float, Bits: 64}},
}

func TestConfigMarshalRoundTrip(t *testing.T) {
	for _, tt := range wireMatrix {
		t.Run(tt.name, func(t *testing.T) {
			b, err := tt.cfg.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseConfig(b)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.cfg {
				t.Errorf("round trip = %+v, want %+v", got, tt.cfg)
			}
		})
	}
}

func TestConfigValidateRejects(t *testing.T) {
	bad := []Config{
		{Encoding: SignedInt, Bits: 12},
		{Encoding: SignedInt, Bits: 16, ValidBits: 17},
		{Encoding: SignedInt, Bits: 16, ValidBits: -1},
		{Encoding: UnsignedInt, Bits: 16},
		{Encoding: Float, Bits: 16},
		{Encoding: Float, Bits: 32, ValidBits: 24},
		{Encoding: Encoding(9), Bits: 16},
	}
	for _, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", cfg)
		}
	}
	if _, err := ParseConfig([]byte{9, 0, 16, 0, 0}); err == nil {
		t.Error("ParseConfig with wrong version must fail")
	}
	if _, err := ParseConfig(nil); err == nil {
		t.Error("ParseConfig(nil) must fail")
	}
}

// fillTest populates a buffer deterministically, hitting the extremes of
// the bit depth on the first frames.
func fillTest(b *audio.Buffer, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed))
	depth := b.Fmt.BitDepth
	for c := 0; c < b.Fmt.Channels; c++ {
		if b.Fmt.Type == audio.Int {
			s := b.ChanI(c)
			min := int32(-1) << (depth - 1)
			max := -(min + 1) // negating min+1 stays in range even at depth 32
			// uint64(min) sign-extends, so max-min is computed modulo
			// 2^64; the wrapped difference is exactly the span
			// (2^depth - 1), including at depth 32.
			for i := range s {
				s[i] = min + int32(rng.Uint64N(uint64(max)-uint64(min)+1))
			}
			if len(s) >= 3 {
				s[0], s[1], s[2] = min, max, 0
			}
		} else {
			s := b.ChanF(c)
			for i := range s {
				s[i] = float32(rng.Float64()*2 - 1)
			}
			if len(s) >= 3 {
				s[0], s[1], s[2] = -1, 1, 0
			}
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, tt := range wireMatrix {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
			enc, err := NewEncoder(tt.cfg, f)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := NewDecoder(tt.cfg, f)
			if err != nil {
				t.Fatal(err)
			}

			src := audio.Get(f, 1000)
			defer audio.Put(src)
			src.N = 1000
			fillTest(src, 42)

			var wire []byte
			var pts []int64
			err = enc.Encode(src, func(p codec.Packet) error {
				wire = append(wire, p.Data...)
				pts = append(pts, p.PTS)
				if !p.Sync || p.Dur != 1000 {
					t.Errorf("packet sync=%v dur=%d, want true, 1000", p.Sync, p.Dur)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := 1000 * tt.cfg.BytesPerFrame(2); len(wire) != want {
				t.Fatalf("wire = %d bytes, want %d", len(wire), want)
			}
			if len(pts) != 1 || pts[0] != 0 {
				t.Fatalf("pts = %v, want [0]", pts)
			}

			got := audio.Get(f, 1000)
			defer audio.Put(got)
			err = dec.Decode(wire, func(b *audio.Buffer) error {
				for c := 0; c < f.Channels; c++ {
					if f.Type == audio.Int {
						copy(got.ChanI(c)[got.N:got.N+b.N:got.Stride], b.ChanI(c))
					} else {
						copy(got.ChanF(c)[got.N:got.N+b.N:got.Stride], b.ChanF(c))
					}
				}
				got.N += b.N
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.N != 1000 {
				t.Fatalf("decoded %d frames, want 1000", got.N)
			}
			for c := 0; c < f.Channels; c++ {
				if f.Type == audio.Int {
					want, have := src.ChanI(c), got.ChanI(c)
					for i := range want {
						if want[i] != have[i] {
							t.Fatalf("ch%d[%d] = %d, want %d", c, i, have[i], want[i])
						}
					}
				} else {
					want, have := src.ChanF(c), got.ChanF(c)
					for i := range want {
						if math.Float32bits(want[i]) != math.Float32bits(have[i]) {
							t.Fatalf("ch%d[%d] = %v, want %v", c, i, have[i], want[i])
						}
					}
				}
			}

			trailer, err := enc.Finish(func(codec.Packet) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if trailer != (codec.Trailer{Samples: 1000}) {
				t.Errorf("trailer = %+v, want {1000 0 0}", trailer)
			}
		})
	}
}

// TestPackingGoldens pins the exact byte layout per wire format so a
// refactor cannot silently flip endianness or justification.
func TestPackingGoldens(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		samples []int32 // one channel
		want    []byte
	}{
		{"s16le", Config{Encoding: SignedInt, Bits: 16}, []int32{0x0102, -2}, []byte{0x02, 0x01, 0xFE, 0xFF}},
		{"s16be", Config{Encoding: SignedInt, Bits: 16, BigEndian: true}, []int32{0x0102, -2}, []byte{0x01, 0x02, 0xFF, 0xFE}},
		{"s24le", Config{Encoding: SignedInt, Bits: 24}, []int32{0x010203, -2}, []byte{0x03, 0x02, 0x01, 0xFE, 0xFF, 0xFF}},
		{"s24be", Config{Encoding: SignedInt, Bits: 24, BigEndian: true}, []int32{0x010203, -2}, []byte{0x01, 0x02, 0x03, 0xFF, 0xFF, 0xFE}},
		{"u8", Config{Encoding: UnsignedInt, Bits: 8}, []int32{0, -128, 127}, []byte{0x80, 0x00, 0xFF}},
		{"s8", Config{Encoding: SignedInt, Bits: 8}, []int32{0, -128, 127}, []byte{0x00, 0x80, 0x7F}},
		{"s24in32le", Config{Encoding: SignedInt, Bits: 32, ValidBits: 24}, []int32{1, -1}, []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
			enc, err := NewEncoder(tt.cfg, f)
			if err != nil {
				t.Fatal(err)
			}
			src := audio.Get(f, len(tt.samples))
			defer audio.Put(src)
			src.N = len(tt.samples)
			copy(src.ChanI(0), tt.samples)
			var wire []byte
			if err := enc.Encode(src, func(p codec.Packet) error {
				wire = append(wire, p.Data...)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wire, tt.want) {
				t.Errorf("wire = % X, want % X", wire, tt.want)
			}
		})
	}
}

func TestInterleaveOrder(t *testing.T) {
	cfg := Config{Encoding: SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	enc, _ := NewEncoder(cfg, f)
	src := audio.Get(f, 2)
	defer audio.Put(src)
	src.N = 2
	copy(src.ChanI(0), []int32{1, 2}) // left
	copy(src.ChanI(1), []int32{3, 4}) // right
	var wire []byte
	if err := enc.Encode(src, func(p codec.Packet) error {
		wire = append(wire, p.Data...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 0, 3, 0, 2, 0, 4, 0} // L0 R0 L1 R1
	if !bytes.Equal(wire, want) {
		t.Errorf("wire = % X, want % X", wire, want)
	}
}

func TestDecodeChunksLargePacket(t *testing.T) {
	cfg := Config{Encoding: SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	dec, _ := NewDecoder(cfg, f)
	frames := audio.StandardChunk + 100
	pkt := make([]byte, frames*2)
	var sizes []int
	if err := dec.Decode(pkt, func(b *audio.Buffer) error {
		sizes = append(sizes, b.N)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[0] != audio.StandardChunk || sizes[1] != 100 {
		t.Errorf("chunk sizes = %v", sizes)
	}
}

// TestDecoderRelease pins the codec.Releaser contract: the pooled scratch
// goes back on Release instead of waiting for the garbage collector.
func TestDecoderRelease(t *testing.T) {
	cfg := Config{Encoding: SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	dec, _ := NewDecoder(cfg, f)
	if err := dec.Decode(make([]byte, 64), func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if dec.buf == nil {
		t.Fatal("decode must have populated the scratch buffer")
	}
	dec.Release()
	if dec.buf != nil {
		t.Error("Release must return the scratch buffer to the pool")
	}
	dec.Release() // second call must be harmless
}

func TestDecodeRejectsTruncatedFrame(t *testing.T) {
	cfg := Config{Encoding: SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	dec, _ := NewDecoder(cfg, f)
	if err := dec.Decode(make([]byte, 7), func(*audio.Buffer) error { return nil }); err == nil {
		t.Error("Decode of a partial frame must fail")
	}
}

func TestNewRejectsMismatchedFormat(t *testing.T) {
	cfg := Config{Encoding: SignedInt, Bits: 16}
	wrong := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}
	if _, err := NewDecoder(cfg, wrong); err == nil {
		t.Error("NewDecoder with mismatched depth must fail")
	}
	if _, err := NewEncoder(cfg, wrong); err == nil {
		t.Error("NewEncoder with mismatched depth must fail")
	}
}

func TestEncoderRejectsWrongBuffer(t *testing.T) {
	cfg := Config{Encoding: SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	enc, _ := NewEncoder(cfg, f)
	other := audio.Get(audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}, 4)
	defer audio.Put(other)
	other.N = 4
	if err := enc.Encode(other, func(codec.Packet) error { return nil }); err == nil {
		t.Error("Encode with mismatched buffer format must fail")
	}
}
