package wmalossless_test

// Every shape this decoder does not cover, named. Two kinds live here and the
// difference is the point: a stream that is inconsistent with the format is
// damage, and a stream that is well formed and uses something no available
// description covers is unsupported. A caller can act on the second and not on
// the first.
//
// The third kind is at the bottom: the shapes that look like damage from the
// inside of a decoder and are not. A packet that is entirely continuation, a
// sequence number that jumps and a landing before the first seekable tile are
// the ordinary consequences of seeking and of a lossy transport, and a decoder
// that reports them as malformed turns a file that plays into one that will
// not seek.

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
	"github.com/colespringer/waxflow/waxerr"
)

const (
	testRate       = 44100
	testBlockAlign = 13375
	testFlags      = 0x01a1 // what every stream any encoder writes carries
)

// config builds a WAVEFORMATEX plus the 18 codec extra bytes.
func config(rate, channels, bits int, mask uint32, align int, flags uint16) []byte {
	b := make([]byte, 36)
	binary.LittleEndian.PutUint16(b, 0x0163)
	binary.LittleEndian.PutUint16(b[2:], uint16(channels))
	binary.LittleEndian.PutUint32(b[4:], uint32(rate))
	binary.LittleEndian.PutUint32(b[8:], 144000)
	binary.LittleEndian.PutUint16(b[12:], uint16(align))
	binary.LittleEndian.PutUint16(b[14:], 16)
	binary.LittleEndian.PutUint16(b[16:], 18)
	binary.LittleEndian.PutUint16(b[18:], uint16(bits))
	binary.LittleEndian.PutUint32(b[20:], mask)
	binary.LittleEndian.PutUint16(b[32:], flags)
	return b
}

func stereoConfig() []byte {
	return config(testRate, 2, 16, 0x03, testBlockAlign, testFlags)
}

// TestConfigRefusals covers what has to be refused before a bit of bitstream
// is read. container/asf validates none of these, so if they are not checked
// here they are not checked at all.
func TestConfigRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  []byte
		want string
		code waxerr.Code
	}{
		{"short-waveformatex", stereoConfig()[:17],
			"WAVEFORMATEX of 17 bytes", waxerr.CodeMalformedInput},
		{"short-extra", stereoConfig()[:30],
			"12 codec extra bytes", waxerr.CodeMalformedInput},
		{"wrong-tag", func() []byte {
			b := stereoConfig()
			binary.LittleEndian.PutUint16(b, 0x0161)
			return b
		}(), "wFormatTag 0x0161 is not WMA Lossless", waxerr.CodeMalformedInput},
		{"depth-32", config(testRate, 2, 32, 0x03, testBlockAlign, testFlags),
			"32 bits per sample", waxerr.CodeUnsupportedFormat},
		{"depth-20", config(testRate, 2, 20, 0x03, testBlockAlign, testFlags),
			"20 bits per sample", waxerr.CodeUnsupportedFormat},
		{"nine-channels", config(testRate, 9, 16, 0, testBlockAlign, testFlags),
			"9 channels", waxerr.CodeUnsupportedFormat},
		{"zero-channels", config(testRate, 0, 16, 0, testBlockAlign, testFlags),
			"0 channels", waxerr.CodeMalformedInput},
		{"block-align-zero", config(testRate, 2, 16, 0x03, 0, testFlags),
			"nBlockAlign 0", waxerr.CodeMalformedInput},
		{"subframe-depth-6", config(testRate, 2, 16, 0x03, testBlockAlign, 0x01a1|0x30),
			"subframe depth 6", waxerr.CodeUnsupportedFormat},
		{"rate-zero", config(0, 2, 16, 0x03, testBlockAlign, testFlags),
			"sample rate 0", waxerr.CodeMalformedInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wmalossless.ParseConfig(tc.cfg)
			if err == nil {
				t.Fatal("accepted a config this build cannot decode")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != tc.code {
				t.Errorf("error code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestChannelMaskIsALayoutNotAChannelCount pins the one field this decoder
// reads more narrowly than the notes describe, and why.
//
// The notes say a non-zero mask overrides nChannels, its population count being
// the channel count. On every stream any encoder writes the two agree, so the
// narrowing is invisible there. They do not always agree: Windows also writes
// SPEAKER_ALL (0x80000000), which names no speaker position and whose
// population count is 1 whatever the stream carries. Overriding from it turns a
// stereo file into a mono track that passes every format check, probes
// playable, and then dies partway through the first frame complaining about a
// padding field. So the count comes from nChannels and the mask supplies only
// the layout, and only when it is positional and covers exactly that count.
func TestChannelMaskIsALayoutNotAChannelCount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		channels   int
		mask       uint32
		wantCh     int
		wantLayout audio.ChannelMask
	}{
		{"stereo", 2, 0x03, 2, audio.ChannelMask(0x03)},
		{"5.1", 6, 0x3f, 6, audio.ChannelMask(0x3f)},
		{"no-mask", 2, 0, 2, audio.DefaultLayout(2)},
		// SPEAKER_ALL: one bit, no position, and it must not become a channel
		// count of one.
		{"speaker-all", 2, 0x80000000, 2, audio.DefaultLayout(2)},
		// A positional mask that covers the wrong number of channels is a
		// disagreement nothing can resolve; the guessed layout is what riff
		// keeps in the same situation.
		{"mask-disagrees", 2, 0x3f, 2, audio.DefaultLayout(2)},
		// A mask with a bit outside the named positions is not a layout.
		{"unnamed-position", 2, 0x03 | 1<<20, 2, audio.DefaultLayout(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := wmalossless.ParseConfig(config(48000, tc.channels, 24, tc.mask, 12288, testFlags))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Channels != tc.wantCh {
				t.Errorf("channels = %d, want %d", cfg.Channels, tc.wantCh)
			}
			if got := cfg.Format().Layout; got != tc.wantLayout {
				t.Errorf("layout = %v (%#x), want %v", got, uint32(got), tc.wantLayout)
			}
			if err := cfg.Format().Valid(); err != nil {
				t.Errorf("the format it builds is invalid: %v", err)
			}
		})
	}
}

// TestDepthComesFromTheExtraBytes pins the other field read from an unobvious
// place: wBitsPerSample is decoration in this format and the depth lives in the
// codec extra bytes. The two agree on every real file, so only a header built
// to disagree can tell a reader that takes the wrong one.
func TestDepthComesFromTheExtraBytes(t *testing.T) {
	raw := config(48000, 2, 24, 0x03, 12288, testFlags)
	binary.LittleEndian.PutUint16(raw[14:], 16) // wBitsPerSample says 16
	cfg, err := wmalossless.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BitsPerSample != 24 {
		t.Errorf("depth = %d, want 24 from the extra bytes", cfg.BitsPerSample)
	}
}

// TestDerivedGeometry pins the rules a wrong reading of nBlockAlign or the
// rate would break silently: the frame length ladder and the header field
// width. Both are measured on the corpus in the oracle note.
func TestDerivedGeometry(t *testing.T) {
	for _, tc := range []struct {
		rate, align                     int
		wantFrame, wantBits, wantMinSub int
	}{
		{44100, 13375, 2048, 17, 128},
		{48000, 12288, 2048, 17, 128},
		{88200, 13375, 4096, 17, 256},
		{96000, 12288, 4096, 17, 256},
		{16000, 1000, 512, 13, 32},
		{22050, 1000, 1024, 13, 64},
		{192000, 4096, 8192, 16, 512},
	} {
		cfg, err := wmalossless.ParseConfig(config(tc.rate, 2, 16, 0x03, tc.align, testFlags))
		if err != nil {
			t.Fatalf("%dHz: %v", tc.rate, err)
		}
		if cfg.SamplesPerFrame() != tc.wantFrame {
			t.Errorf("%dHz: frame %d samples, want %d", tc.rate, cfg.SamplesPerFrame(), tc.wantFrame)
		}
		if cfg.FrameSizeBits() != tc.wantBits {
			t.Errorf("%dHz align %d: header field %d bits, want %d",
				tc.rate, tc.align, cfg.FrameSizeBits(), tc.wantBits)
		}
		if cfg.MinSubframeLen() != tc.wantMinSub {
			t.Errorf("%dHz: min subframe %d, want %d", tc.rate, cfg.MinSubframeLen(), tc.wantMinSub)
		}
	}
}

// bitWriter builds a packet most significant bit first, which is the only way
// to reach the mid-stream refusals: no encoder produces any of them.
type bitWriter struct {
	buf  []byte
	bits int
}

func (w *bitWriter) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.bits&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.buf[w.bits>>3] |= 1 << (7 - uint(w.bits&7))
		}
		w.bits++
	}
}

// packet wraps payload bits in a packet header with a continuation count of
// zero, so the frame in it begins at payload bit 0.
func packet(t *testing.T, cfg wmalossless.Config, seq int, payload *bitWriter) []byte {
	t.Helper()
	var w bitWriter
	w.put(uint32(seq), 4)
	w.put(0, 1) // seekable frame in packet: a hint the decoder confirms itself
	w.put(0, 1) // not spliced
	w.put(0, cfg.FrameSizeBits())
	for i := 0; i < payload.bits; i++ {
		w.put(uint32(payload.buf[i>>3]>>(7-uint(i&7))&1), 1)
	}
	if len(w.buf) > cfg.BlockAlign {
		t.Fatalf("payload of %d bits does not fit a %d-byte packet", payload.bits, cfg.BlockAlign)
	}
	out := make([]byte, cfg.BlockAlign)
	copy(out, w.buf)
	return out
}

// frameHead writes everything before the first subframe: the tile header for a
// single subframe spanning the frame, the discarded gain, and no skips.
func frameHead(cfg wmalossless.Config, w *bitWriter, takes []int) {
	w.put(0, 1) // tile aligned: 0, which is what every real encoder writes
	for _, v := range takes {
		w.put(uint32(v), 1)
	}
	if allTake(takes) {
		w.put(uint32(cfg.MaxSubframes()-1), 4) // ratio: one subframe spans the frame
	}
	w.put(0, 8) // dynamic range gain, read and discarded
	w.put(0, 1) // no skip fields
}

func allTake(takes []int) bool {
	for _, v := range takes {
		if v == 0 {
			return false
		}
	}
	return true
}

// cdlmsDef writes a minimal filter definition: one filter per channel, order
// 16, scaling 12, nothing transmitted.
func cdlmsDef(w *bitWriter, channels int) {
	w.put(0, 1) // no transmitted coefficients
	for c := 0; c < channels; c++ {
		w.put(0, 3) // one filter
		w.put(1, 7) // order (1+1)*8 = 16
		w.put(12, 4)
	}
}

// decodeOne runs one synthetic frame through a decoder. The frame is carried
// by one packet and decoded by the drain, which is the whole deferred model in
// two calls.
func decodeOne(t *testing.T, cfgRaw []byte, payload *bitWriter) error {
	t.Helper()
	cfg, err := wmalossless.ParseConfig(cfgRaw)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	drop := func(*audio.Buffer) error { return nil }
	if err := dec.Decode(packet(t, cfg, 0, payload), drop); err != nil {
		return err
	}
	return dec.Drain(drop)
}

// TestBitstreamRefusals covers the shapes a decoder meets only once it is
// reading, each of which is either undescribed or described only by an
// implementation known to get it wrong.
func TestBitstreamRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(cfg wmalossless.Config, w *bitWriter)
		want  string
		code  waxerr.Code
	}{
		{"arithmetic-coding", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 1})
			w.put(1, 1) // seekable tile
			w.put(1, 1) // arithmetic coding
		}, "arithmetic coding", waxerr.CodeUnsupportedFormat},

		{"transmitted-cdlms", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 1})
			w.put(1, 1) // seekable
			w.put(0, 1) // no arithmetic coding
			w.put(0, 1) // no AC filter
			w.put(0, 1) // no decorrelation
			w.put(0, 1) // no MCLMS
			w.put(1, 1) // CDLMS coefficients transmitted
		}, "transmitted CDLMS coefficients", waxerr.CodeUnsupportedFormat},

		{"cdlms-order-past-256", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 1})
			w.put(1, 1)
			w.put(0, 4)  // no arithmetic, no AC, no decorrelation, no MCLMS
			w.put(0, 1)  // nothing transmitted
			w.put(0, 3)  // one filter
			w.put(32, 7) // order (32+1)*8 = 264
			w.put(12, 4)
		}, "CDLMS order 264", waxerr.CodeMalformedInput},

		{"lpc-filter", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 1})
			w.put(1, 1)
			w.put(0, 4)
			cdlmsDef(w, cfg.Channels)
			w.put(0, 3) // moving-average scaling
			w.put(0, 8) // quantisation step 1
			w.put(0, 1) // not a raw PCM tile
			w.put(3, 2) // both channels coded
			w.put(1, 1) // LPC flag
		}, "the LPC filter", waxerr.CodeUnsupportedFormat},

		{"non-aligned-tiling", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 0})
		}, "tiled differently", waxerr.CodeUnsupportedFormat},

		{"no-channel-takes", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{0, 0})
		}, "no channel takes", waxerr.CodeMalformedInput},

		{"padding-past-depth", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 1})
			w.put(1, 1)
			w.put(0, 4)
			cdlmsDef(w, cfg.Channels)
			w.put(0, 3)
			w.put(0, 8)
			w.put(0, 1)  // not raw PCM
			w.put(3, 2)  // both coded
			w.put(0, 1)  // no LPC
			w.put(1, 1)  // padding present
			w.put(17, 5) // 17 zeroes in a 16-bit stream
		}, "17 padding zeroes", waxerr.CodeMalformedInput},

		{"raw-pcm-all-padding", func(cfg wmalossless.Config, w *bitWriter) {
			frameHead(cfg, w, []int{1, 1})
			w.put(0, 1)  // not seekable
			w.put(1, 1)  // raw PCM tile
			w.put(1, 1)  // padding present
			w.put(16, 5) // leaves no bits per sample
		}, "16 padding zeroes in a 16-bit raw PCM tile", waxerr.CodeMalformedInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := wmalossless.ParseConfig(stereoConfig())
			if err != nil {
				t.Fatal(err)
			}
			var w bitWriter
			tc.build(cfg, &w)
			err = decodeOne(t, stereoConfig(), &w)
			if err == nil {
				t.Fatal("accepted a stream this build cannot decode")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != tc.code {
				t.Errorf("error code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestSplicedPacketIsRefused is its own cell because the bit is in the packet
// header rather than in a frame.
func TestSplicedPacketIsRefused(t *testing.T) {
	cfg, err := wmalossless.ParseConfig(stereoConfig())
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	pkt := make([]byte, cfg.BlockAlign)
	pkt[0] = 0x04 // sequence 0, no seek hint, spliced
	err = dec.Decode(pkt, func(*audio.Buffer) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "spliced") {
		t.Fatalf("error = %v, want a refusal naming the spliced packet", err)
	}
	if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
		t.Errorf("error code = %q, want %q", code, waxerr.CodeUnsupportedFormat)
	}
}

// TestTheseAreNotDamage is the other half of the contract, and the half a
// decoder gets wrong by being careful. Every case here is a file that plays.
func TestTheseAreNotDamage(t *testing.T) {
	cfg, err := wmalossless.ParseConfig(stereoConfig())
	if err != nil {
		t.Fatal(err)
	}
	drop := func(*audio.Buffer) error { return nil }

	t.Run("all-continuation-packet", func(t *testing.T) {
		dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()
		var w bitWriter
		w.put(0, 4)
		w.put(0, 1)
		w.put(0, 1)
		// The encoder writes the packet size in bits, which overshoots the
		// payload by the header width. Clamping is the contract; refusing
		// would reject 115 of the 496 packets in the corpus.
		w.put(uint32(cfg.BlockAlign*8), cfg.FrameSizeBits())
		pkt := make([]byte, cfg.BlockAlign)
		copy(pkt, w.buf)
		if err := dec.Decode(pkt, drop); err != nil {
			t.Fatalf("a packet that is entirely continuation was refused: %v", err)
		}
	})

	t.Run("sequence-jump", func(t *testing.T) {
		dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()
		var w bitWriter
		frameHead(cfg, &w, []int{1, 1})
		w.put(0, 1) // not seekable
		w.put(1, 1) // raw PCM, so it needs no history
		w.put(0, 1) // no padding
		for i := 0; i < 2*cfg.SamplesPerFrame(); i++ {
			w.put(0, 16)
		}
		w.put(0, 1) // trailer: no more frames here
		if err := dec.Decode(packet(t, cfg, 0, &w), drop); err != nil {
			t.Fatal(err)
		}
		// Sequence 5 after 0 means packets were lost. Dropping the carry and
		// waiting is the recovery; reporting damage is not.
		if err := dec.Decode(packet(t, cfg, 5, &w), drop); err != nil {
			t.Fatalf("a sequence jump was reported as damage: %v", err)
		}
	})

	t.Run("landing-before-a-seekable-tile", func(t *testing.T) {
		dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()
		var w bitWriter
		frameHead(cfg, &w, []int{1, 1})
		w.put(0, 1) // not seekable, so no filter has an order
		w.put(0, 1) // not raw PCM either: undecodable, but not damaged
		w.put(3, 2)
		w.put(0, 1)
		w.put(0, 1)
		w.put(0, 1)
		emitted := 0
		count := func(*audio.Buffer) error { emitted++; return nil }
		if err := dec.Decode(packet(t, cfg, 0, &w), count); err != nil {
			t.Fatal(err)
		}
		if err := dec.Drain(count); err != nil {
			t.Fatalf("a landing before the first seekable tile was reported as damage: %v", err)
		}
		if emitted != 0 {
			t.Errorf("emitted %d buffers with no filter state", emitted)
		}
	})
}
