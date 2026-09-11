//go:build !wmaprotablesgen

package wmapro_test

// Every shape this decoder does not cover, named. Two kinds live here and the
// difference is the point: a stream that is inconsistent with the format is
// damage, and a stream that is well formed and uses something no available
// description covers is unsupported. A caller can act on the second and not on
// the first.
//
// The third kind is at the bottom: the shapes that look like damage from the
// inside of a decoder and are not. A continuation count of zero over a pending
// carry, a count that claims more than the packet holds, a sequence number
// that jumps, a group of no channels and a negative quantisation step are all
// things real streams or lossy transports produce, and a decoder that reports
// them as malformed turns a file that plays into one that will not.

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmapro"
	"github.com/colespringer/waxflow/waxerr"
)

const (
	testRate       = 44100
	testBlockAlign = 5945
	testFlags      = 0x00e0 // what every decodable stream Windows writes carries
)

// config builds a WAVEFORMATEX plus the 18 codec extra bytes.
func config(rate, channels, bits int, mask uint32, align int, flags, word16 uint16) []byte {
	b := make([]byte, 36)
	binary.LittleEndian.PutUint16(b, 0x0162)
	binary.LittleEndian.PutUint16(b[2:], uint16(channels))
	binary.LittleEndian.PutUint32(b[4:], uint32(rate))
	binary.LittleEndian.PutUint32(b[8:], 16002)
	binary.LittleEndian.PutUint16(b[12:], uint16(align))
	binary.LittleEndian.PutUint16(b[14:], 16)
	binary.LittleEndian.PutUint16(b[16:], 18)
	binary.LittleEndian.PutUint16(b[18:], uint16(bits))
	binary.LittleEndian.PutUint32(b[20:], mask)
	binary.LittleEndian.PutUint16(b[32:], flags)
	binary.LittleEndian.PutUint16(b[34:], word16)
	return b
}

func stereoConfig() []byte {
	return config(testRate, 2, 16, 0x03, testBlockAlign, testFlags, 0)
}

// smallConfig is a short frame in a small packet with one subframe per frame,
// so a hand-built frame reads no tiling bits and stays legible: 512 samples,
// a 64-byte packet, a length prefix and the DRC byte.
func smallConfig() []byte { return config(16000, 2, 16, 0x03, 64, 0x00c0, 0) }

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
		}(), "wFormatTag 0x0161 is not WMA Pro", waxerr.CodeMalformedInput},
		{"depth-32", config(testRate, 2, 32, 0x03, testBlockAlign, testFlags, 0),
			"32 bits per sample", waxerr.CodeUnsupportedFormat},
		{"depth-20", config(testRate, 2, 20, 0x03, testBlockAlign, testFlags, 0),
			"20 bits per sample", waxerr.CodeUnsupportedFormat},
		{"nine-channels", config(testRate, 9, 16, 0, testBlockAlign, testFlags, 0),
			"9 channels", waxerr.CodeUnsupportedFormat},
		{"zero-channels", config(testRate, 0, 16, 0, testBlockAlign, testFlags, 0),
			"0 channels", waxerr.CodeMalformedInput},
		{"block-align-zero", config(testRate, 2, 16, 0x03, 0, testFlags, 0),
			"nBlockAlign 0", waxerr.CodeMalformedInput},
		{"rate-zero", config(0, 2, 16, 0x03, testBlockAlign, testFlags, 0),
			"sample rate 0", waxerr.CodeMalformedInput},
		{"subframe-depth-6", config(testRate, 2, 16, 0x03, testBlockAlign, 0x00c0|6<<3, 0),
			"subframe depth 6", waxerr.CodeUnsupportedFormat},
		// 192 kHz sits on the rule's top arm at 2^13, and the flags' +1
		// adjustment names a frame nothing can lay out.
		{"frame-above-8192", config(192000, 2, 24, 0x03, testBlockAlign, testFlags|0x02, 0),
			"2^14 samples", waxerr.CodeUnsupportedFormat},
		// 512-sample frames at a depth of 32 leave 16-sample subframes.
		{"subframe-below-64", config(16000, 2, 16, 0x03, testBlockAlign, 0x00c0|5<<3, 0),
			"16 samples each", waxerr.CodeMalformedInput},
		// The low-bit-rate tool, by the exact predicate the notes settle on:
		// any non-zero word, not a bit test.
		{"low-bit-rate-tool", config(32000, 2, 16, 0x03, 1536, 0x0060, 0x20c6),
			"low-bit-rate tool", waxerr.CodeUnsupportedFormat},
		{"low-bit-rate-tool-other-word", config(testRate, 2, 16, 0x03, 2973, testFlags, 0xc042),
			"low-bit-rate tool", waxerr.CodeUnsupportedFormat},
		// The unprefixed frame layer has no fixture anywhere. Refused at the
		// config so the container declines the track, rather than probing it
		// as playable and failing every packet.
		{"no-length-prefix", config(16000, 2, 16, 0x03, 64, 0x0080, 0),
			"no length prefix", waxerr.CodeUnsupportedFormat},
		// Above the ceiling the frame-length rule still answers, and the
		// refusal exists so that what probes as playable is what decodes.
		{"rate-above-ceiling", config(400000, 2, 24, 0x03, testBlockAlign, testFlags, 0),
			"sample rate 400000", waxerr.CodeUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wmapro.ParseConfig(tc.cfg)
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
// reads more narrowly than the notes describe, and why: the notes have a
// positional mask's population count override nChannels, and Windows also
// writes SPEAKER_ALL, whose population count is 1 whatever the stream carries.
// codec/wmalossless learned it first; the rule is the same here.
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
		{"speaker-all", 2, 0x80000000, 2, audio.DefaultLayout(2)},
		{"mask-disagrees", 2, 0x3f, 2, audio.DefaultLayout(2)},
		{"unnamed-position", 2, 0x03 | 1<<20, 2, audio.DefaultLayout(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := wmapro.ParseConfig(config(48000, tc.channels, 24, tc.mask, 16384, testFlags, 0))
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
// codec extra bytes, where it feeds both the quantisation base and the
// transform's normalisation.
func TestDepthComesFromTheExtraBytes(t *testing.T) {
	raw := config(48000, 2, 24, 0x03, 16384, testFlags, 0)
	binary.LittleEndian.PutUint16(raw[14:], 16) // wBitsPerSample says 16
	cfg, err := wmapro.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BitsPerSample != 24 {
		t.Errorf("depth = %d, want 24 from the extra bytes", cfg.BitsPerSample)
	}
	if f := cfg.Format(); f.Type != audio.Float || f.BitDepth != 32 {
		t.Errorf("format %v, want float32: the depth is a codec parameter, not an output one", f)
	}
}

// TestDerivedGeometry pins the rules a wrong reading of nBlockAlign, the rate
// or the flags would break silently: the frame-length ladder with its
// adjustment, the header field width and the shortest subframe. All measured
// on the corpus in the oracle note; 32 kHz is the arm the v1/v2 rule has and
// this one does not.
func TestDerivedGeometry(t *testing.T) {
	for _, tc := range []struct {
		rate, align                     int
		flags                           uint16
		wantFrame, wantBits, wantMinSub int
	}{
		{44100, 5945, testFlags, 2048, 16, 128},
		{48000, 16384, testFlags, 2048, 18, 128},
		{32000, 1536, 0x0060, 2048, 14, 128},
		{88200, 17833, testFlags, 4096, 18, 256},
		{96000, 16384, testFlags, 4096, 18, 256},
		{22050, 1000, testFlags, 1024, 13, 64},
		{16000, 1000, 0x00d0, 512, 13, 128},
		{192000, 4096, testFlags, 8192, 16, 512},
		// The adjustment: -1 at 44.1 kHz gives 1024, -2 gives 512.
		{44100, 5945, 0x00c4, 1024, 16, 1024},
		{44100, 5945, 0x00c6, 512, 16, 512},
	} {
		cfg, err := wmapro.ParseConfig(config(tc.rate, 2, 16, 0x03, tc.align, tc.flags, 0))
		if err != nil {
			t.Fatalf("%dHz flags %#04x: %v", tc.rate, tc.flags, err)
		}
		if cfg.SamplesPerFrame() != tc.wantFrame {
			t.Errorf("%dHz flags %#04x: frame %d samples, want %d", tc.rate, tc.flags, cfg.SamplesPerFrame(), tc.wantFrame)
		}
		if cfg.FrameSizeBits() != tc.wantBits {
			t.Errorf("%dHz align %d: header field %d bits, want %d", tc.rate, tc.align, cfg.FrameSizeBits(), tc.wantBits)
		}
		if cfg.MinSubframeLen() != tc.wantMinSub {
			t.Errorf("%dHz flags %#04x: min subframe %d, want %d", tc.rate, tc.flags, cfg.MinSubframeLen(), tc.wantMinSub)
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

// code writes one book symbol's codeword.
func (w *bitWriter) code(book string, sym int) {
	c, n := wmapro.CodeForTest(book, sym)
	w.put(c, n)
}

func (w *bitWriter) append(o *bitWriter) {
	for i := 0; i < o.bits; i++ {
		w.put(uint32(o.buf[i>>3]>>(7-uint(i&7))&1), 1)
	}
}

// signed writes a two's complement field.
func (w *bitWriter) signed(v, n int) { w.put(uint32(v)&(1<<uint(n)-1), n) }

// packet wraps payload bits in a packet header: sequence seq and continuation
// count cont, zero padded to nBlockAlign. fill is what pads the rest of the
// packet; a 0xff fill makes a field escape run into the end of the packet.
func packet(t testing.TB, cfg wmapro.Config, seq, cont int, payload *bitWriter, fill byte) []byte {
	t.Helper()
	var w bitWriter
	w.put(uint32(seq), 4)
	w.put(2, 2) // the two skipped bits, as the encoder writes them
	w.put(uint32(cont), cfg.FrameSizeBits())
	w.append(payload)
	if len(w.buf) > cfg.BlockAlign {
		t.Fatalf("payload of %d bits does not fit a %d-byte packet", payload.bits, cfg.BlockAlign)
	}
	out := make([]byte, cfg.BlockAlign)
	for i := range out {
		out[i] = fill
	}
	copy(out, w.buf)
	if fill != 0 && w.bits&7 != 0 {
		// Keep the fill out of the bits the payload already owns.
		out[w.bits>>3] = w.buf[w.bits>>3] | fill>>uint(w.bits&7)
	}
	return out
}

// frame wraps a frame body in its length prefix, one padding bit and the
// trailer bit, which is what the length prefix accounts for. extraGap widens
// the padding, which a decoder must tolerate.
func frame(cfg wmapro.Config, body *bitWriter, more bool, extraGap int) *bitWriter {
	var w bitWriter
	w.put(uint32(cfg.FrameSizeBits()+body.bits+2+extraGap), cfg.FrameSizeBits())
	w.append(body)
	w.put(0, 1+extraGap)
	if more {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
	return &w
}

// tilingOne writes the tiling of one full-frame subframe per channel, which
// reads nothing at a depth of zero and a uniform bit plus one zero shift
// otherwise.
func tilingOne(cfg wmapro.Config, w *bitWriter) {
	if cfg.MaxSubframes() == 1 {
		return
	}
	w.put(1, 1) // uniform
	if m := cfg.MaxSubframes(); m == 4 || m == 16 {
		w.put(0, 1)
	} else {
		w.put(0, floorLog2(m)+1)
	}
}

func floorLog2(x int) int {
	n := -1
	for ; x > 0; x >>= 1 {
		n++
	}
	return n
}

// frameHead writes everything before the first subframe: the tiling, the
// post-processing bit, the discarded gain, and no trims.
func frameHead(cfg wmapro.Config, w *bitWriter) {
	tilingOne(cfg, w)
	if cfg.Channels > 1 {
		w.put(0, 1) // no post-processing transform
	}
	if cfg.DecodeFlags&0x80 != 0 {
		w.put(0, 8) // dynamic range gain, read and discarded
	}
	w.put(0, 1) // no trim fields
}

// groupsNoTransform writes the channel group section with every channel in
// one group and no transform: for two channels the explicit "no transform"
// answer, for more the group bits then a clear transform bit.
func groupsNoTransform(cfg wmapro.Config, w *bitWriter) {
	if cfg.Channels < 2 {
		return
	}
	w.put(0, 1) // the bit that must be clear
	if cfg.Channels == 2 {
		w.put(1, 1)
		w.put(0, 1)
		return
	}
	for i := 0; i < cfg.Channels; i++ {
		w.put(1, 1)
	}
	w.put(0, 1)
}

// subframeHead writes the two leading bits and the group section.
func subframeHead(cfg wmapro.Config, w *bitWriter) {
	w.put(0, 1) // no extended payload
	w.put(0, 1) // reserved
	groupsNoTransform(cfg, w)
}

// silentSubframe is a subframe in which no channel transmits.
func silentSubframe(cfg wmapro.Config, w *bitWriter) {
	subframeHead(cfg, w)
	w.put(0, cfg.Channels)
}

// silentFrame is a frame of one silent subframe.
func silentFrame(cfg wmapro.Config, w *bitWriter) {
	frameHead(cfg, w)
	silentSubframe(cfg, w)
}

// quantHead writes the quantisation section up to the scale factors: no
// vector limit, the step delta, and no per-channel modifiers.
func quantHead(cfg wmapro.Config, w *bitWriter, delta int) {
	w.put(0, 1)
	w.signed(delta, 6)
	if cfg.Channels > 1 {
		w.put(0, 3)
		w.put(0, cfg.Channels)
	}
}

// dpcm writes one channel's first scale factors: resolution 1 and a zero
// delta on every band.
func dpcm(w *bitWriter, nb int) {
	w.put(0, 2)
	for i := 0; i < nb; i++ {
		w.code("scaleDelta", 60)
	}
}

// zeroBlock is a coefficient block coding nothing: book 0, one all-zero
// vector, which trips the zero-run switch, and end of block.
func zeroBlock(w *bitWriter) {
	w.put(0, 1)
	w.code("vec4", 1)
	w.code("coef0", 1)
}

// transmitFrame is a frame of one subframe in which channel 0 transmits an
// all-zero block at the given step delta, with tail as the block's tail in
// place of the default end-of-block.
func transmitFrame(t testing.TB, cfg wmapro.Config, w *bitWriter, delta int, tail func(w *bitWriter)) {
	t.Helper()
	frameHead(cfg, w)
	subframeHead(cfg, w)
	w.put(1, 1)
	w.put(0, cfg.Channels-1)
	quantHead(cfg, w, delta)
	nb := wmapro.BandCountForTest(cfg, 0)
	if nb <= 0 {
		t.Fatal("no band layout")
	}
	for i := 0; i < cfg.Channels; i++ {
		dpcm(w, nb)
	}
	w.put(0, 1) // book 0
	w.code("vec4", 1)
	if tail == nil {
		w.code("coef0", 1)
		return
	}
	tail(w)
}

// decodeOne runs one hand-built frame through a decoder.
func decodeOne(t testing.TB, cfgRaw []byte, body *bitWriter) error {
	t.Helper()
	cfg, err := wmapro.ParseConfig(cfgRaw)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := wmapro.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	return dec.Decode(packet(t, cfg, 0, 0, frame(cfg, body, false, 0), 0), drop)
}

func drop(*audio.Buffer) error { return nil }

func parse(t testing.TB, raw []byte) wmapro.Config {
	t.Helper()
	cfg, err := wmapro.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestBitstreamRefusals covers the shapes a decoder meets only once it is
// reading. Each is either a tool nothing here can describe, or a frame that
// contradicts the format.
func TestBitstreamRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   []byte
		build func(cfg wmapro.Config, w *bitWriter)
		want  string
		code  waxerr.Code
	}{
		{"extended-payload", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			w.put(1, 1) // the low-bit-rate tool's payload
		}, "low-bit-rate tool's payload", waxerr.CodeUnsupportedFormat},

		{"reserved-subframe-bit", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			w.put(0, 1)
			w.put(1, 1)
		}, "reserved subframe bit", waxerr.CodeMalformedInput},

		{"channel-transform-leading-bit", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			w.put(0, 2)
			w.put(1, 1)
		}, "unknown channel transform", waxerr.CodeUnsupportedFormat},

		{"two-channel-unknown-transform", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			w.put(0, 3)
			w.put(1, 1)
			w.put(1, 1)
		}, "unknown two-channel channel transform", waxerr.CodeUnsupportedFormat},

		// Eight channels in one group asking for the built-in matrix, which
		// stops at six.
		{"built-in-matrix-for-eight", config(16000, 8, 16, 0, 256, 0x00c0, 0), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			w.put(0, 3)
			w.put(0xff, 8) // every channel joins the first group
			w.put(1, 1)    // transform on
			w.put(0, 1)    // the built-in matrix
		}, "built-in decorrelation matrix for a group of 8", waxerr.CodeUnsupportedFormat},

		// A depth of 32 has a raw three-bit shift field that can name 7.
		{"subframe-shift-too-deep", config(testRate, 2, 16, 0x03, testBlockAlign, 0x00c0|5<<3, 0), func(cfg wmapro.Config, w *bitWriter) {
			w.put(1, 1) // uniform
			w.put(7, 3)
		}, "subframe length shift of 7", waxerr.CodeMalformedInput},

		// At a depth of 4: a 256 then a 512 in a 512-sample frame.
		{"subframe-overruns-frame", config(16000, 2, 16, 0x03, 64, 0x00c0|2<<3, 0), func(cfg wmapro.Config, w *bitWriter) {
			w.put(1, 1) // uniform
			w.put(1, 1) // shift 1 + ...
			w.put(0, 1) // ... 0: a 256
			w.put(0, 1) // shift 0: a 512 at frontier 256
		}, "a subframe of 512 samples at frontier 256", waxerr.CodeMalformedInput},

		{"vector-limit-above-length", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			subframeHead(cfg, w)
			w.put(2, 2)   // channel 0 transmits
			w.put(1, 1)   // explicit vector limits
			w.put(129, 8) // 129 << 2 = 516 in a 512-sample subframe
		}, "vector coefficient limit of 516", waxerr.CodeMalformedInput},

		{"step-escape-runs-out", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			frameHead(cfg, w)
			subframeHead(cfg, w)
			w.put(2, 2)
			w.put(0, 1)
			w.signed(31, 6) // the escape, and the packet's fill continues it
		}, "quantisation step escape runs out of bits", waxerr.CodeMalformedInput},

		{"band-exponent-past-bound", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			// 90 + 31, then nine full chunks and a 5: a step of 405.
			frameHead(cfg, w)
			subframeHead(cfg, w)
			w.put(2, 2)
			w.put(0, 1)
			w.signed(31, 6)
			for i := 0; i < 9; i++ {
				w.put(31, 5)
			}
			w.put(5, 5)
			w.put(0, 3)
			w.put(0, 2)
			nb := wmapro.BandCountForTest(cfg, 0)
			dpcm(w, nb)
			dpcm(w, nb)
			zeroBlock(w)
		}, "band exponent of 405", waxerr.CodeMalformedInput},

		{"run-level-escape-three-bits", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			transmitFrame(t, cfg, w, 0, func(w *bitWriter) {
				w.code("coef0", 0) // escape
				w.put(0, 1)        // an 8-bit level
				w.put(1, 8)
				w.put(7, 3) // all three run bits
			})
		}, "run-level escape sets all three run bits", waxerr.CodeMalformedInput},

		{"run-level-run-past-length", smallConfig(), func(cfg wmapro.Config, w *bitWriter) {
			transmitFrame(t, cfg, w, 0, func(w *bitWriter) {
				w.code("coef0", 0)
				w.put(0, 1)
				w.put(1, 8)
				w.put(6, 3)   // 1, 1, 0: the wide run
				w.put(504, 9) // from index 4: 4 + 504 + 4 = 512
			})
		}, "run-level run reaches 512 in a subframe of 512", waxerr.CodeMalformedInput},

		// Two subframes; the second transmits scale factors as differences
		// whose run skips every band.
		{"scale-factor-run-past-bands", config(16000, 2, 16, 0x03, 64, 0x00c8, 0), func(cfg wmapro.Config, w *bitWriter) {
			w.put(1, 1) // uniform
			w.put(1, 1) // shift 1: two subframes of 256
			w.put(0, 1) // no post-processing
			w.put(0, 8) // gain
			w.put(0, 1) // no trims
			nb := wmapro.BandCountForTest(cfg, 1)
			for i := 0; i < 2; i++ {
				subframeHead(cfg, w)
				w.put(2, 2)
				quantHead(cfg, w, 0)
				if i == 0 {
					dpcm(w, nb)
					dpcm(w, nb)
					zeroBlock(w)
					continue
				}
				w.put(1, 1)                // channel 0 transmits scale factors
				w.code("scaleRunLevel", 0) // the 14-bit escape
				w.put(1<<6|31<<1, 14)      // level 1, run 31, negative
			}
		}, "scale factor run skips past the last of", waxerr.CodeMalformedInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parse(t, tc.cfg)
			var w bitWriter
			tc.build(cfg, &w)
			dec, err := wmapro.NewDecoder(cfg, cfg.Format())
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Release()
			var fill byte
			if tc.name == "step-escape-runs-out" {
				fill = 0xff
			}
			fr := frame(cfg, &w, false, 0)
			if fill != 0 {
				// The frame claims the whole packet, so the escape's chunks
				// are inside the frame right up to the end of the payload.
				fr = &bitWriter{}
				fr.put(uint32(cfg.BlockAlign*8-6-cfg.FrameSizeBits()), cfg.FrameSizeBits())
				fr.append(&w)
			}
			err = dec.Decode(packet(t, cfg, 0, 0, fr, fill), drop)
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

// TestPacketRefusals covers the refusals taken at the packet and frame
// layers rather than inside a subframe.
func TestPacketRefusals(t *testing.T) {
	cfg := parse(t, smallConfig())
	var silent bitWriter
	silentFrame(cfg, &silent)
	newDec := func(t *testing.T) *wmapro.Decoder {
		t.Helper()
		dec, err := wmapro.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dec.Release)
		return dec
	}
	expect := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeMalformedInput {
			t.Errorf("error code = %q, want %q", code, waxerr.CodeMalformedInput)
		}
	}

	t.Run("over-length-packet", func(t *testing.T) {
		expect(t, newDec(t).Decode(make([]byte, cfg.BlockAlign+1), drop), "longer than nBlockAlign")
	})

	t.Run("carried-frame-too-short-for-its-prefix", func(t *testing.T) {
		// A frame that crosses into the next packet and, once completed
		// there, declares a length its own prefix does not fit in: the carry
		// holds only zero bits.
		dec := newDec(t)
		if err := dec.Decode(packet(t, cfg, 0, 0, frame(cfg, &silent, true, 0), 0), drop); err != nil {
			t.Fatal(err)
		}
		expect(t, dec.Decode(packet(t, cfg, 1, 32, &bitWriter{}, 0), drop), "cannot hold its own length prefix")
	})

	t.Run("frame-too-short-for-its-prefix", func(t *testing.T) {
		var w bitWriter
		w.put(5, cfg.FrameSizeBits())
		w.put(0, 40)
		expect(t, newDec(t).Decode(packet(t, cfg, 0, 0, &w, 0), drop), "cannot hold its own length prefix")
	})

	t.Run("frame-overruns-its-declared-length", func(t *testing.T) {
		// The prefix claims three bits fewer than the body reads. The walk
		// then ends past the trailer bit, and seeking back to it would accept
		// a frame that contradicts itself.
		whole := frame(cfg, &silent, false, 0)
		var w bitWriter
		w.put(uint32(whole.bits-3), cfg.FrameSizeBits())
		w.append(&silent)
		expect(t, newDec(t).Decode(packet(t, cfg, 0, 0, &w, 0), drop), "past its declared length")
	})
}

// TestEveryValidConfigHasABandLayout is the proof behind the sample rate
// ceiling: a config Validate accepts is one NewDecoder can lay bands out for
// at every subframe size, so a track that probes as playable decodes. Every
// arm of the frame-length rule, every depth and every adjustment, at rates
// from one hertz to the ceiling.
func TestEveryValidConfigHasABandLayout(t *testing.T) {
	rates := []int{1, 7, 8000, 11025, 16000, 16001, 22050, 22051, 32000, 44100,
		48000, 48001, 88200, 96000, 96001, 176400, 192000, 384000}
	for _, rate := range rates {
		for depth := 0; depth <= 5; depth++ {
			for adjust := 0; adjust <= 3; adjust++ {
				flags := uint16(0x00c0 | depth<<3 | adjust<<1)
				cfg, err := wmapro.ParseConfig(config(rate, 2, 16, 0x03, 4096, flags, 0))
				if err != nil {
					continue // refused, so never a track
				}
				for k := 0; k <= depth; k++ {
					if nb := wmapro.BandCountForTest(cfg, k); nb < 1 {
						t.Errorf("%d Hz depth %d adjust %d: size index %d lays out %d bands", rate, depth, adjust, k, nb)
					}
				}
				dec, err := wmapro.NewDecoder(cfg, cfg.Format())
				if err != nil {
					t.Errorf("%d Hz depth %d adjust %d: probes as playable and NewDecoder refuses: %v", rate, depth, adjust, err)
					continue
				}
				dec.Release()
			}
		}
	}
}

// splitFrame builds a packet payload that ends partway through a frame: one
// whole frame with the trailer bit set, then the head of a second frame padded
// out so that exactly tailBits of it are left over. The packet's fill is
// payload as far as any reader can tell, so a frame that crosses into the
// next packet has to overrun this one for real.
func splitFrame(cfg wmapro.Config, body *bitWriter, tailBits int) (first, tail *bitWriter) {
	lead := frame(cfg, body, true, 0)
	room := cfg.BlockAlign*8 - 6 - cfg.FrameSizeBits() - lead.bits
	whole := frame(cfg, body, false, 0)
	whole = frame(cfg, body, false, room+tailBits-whole.bits)
	first, tail = &bitWriter{}, &bitWriter{}
	first.append(lead)
	for i := 0; i < whole.bits; i++ {
		bit := uint32(whole.buf[i>>3] >> (7 - uint(i&7)) & 1)
		if i < room {
			first.put(bit, 1)
		} else {
			tail.put(bit, 1)
		}
	}
	return first, tail
}

// TestTheseAreNotDamage is the other half of the contract, and the half a
// decoder gets wrong by being careful. Every case here is a stream that plays.
func TestTheseAreNotDamage(t *testing.T) {
	cfg := parse(t, smallConfig())
	newDec := func(t *testing.T, cfg wmapro.Config) *wmapro.Decoder {
		t.Helper()
		dec, err := wmapro.NewDecoder(cfg, cfg.Format())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dec.Release)
		return dec
	}
	var silent bitWriter
	silentFrame(cfg, &silent)

	t.Run("sequence-jump", func(t *testing.T) {
		dec := newDec(t, cfg)
		if err := dec.Decode(packet(t, cfg, 0, 0, frame(cfg, &silent, false, 0), 0), drop); err != nil {
			t.Fatal(err)
		}
		// Sequence 5 after 0 means packets were lost. Dropping the carry and
		// owing a lead-in is the recovery; reporting damage is not.
		if err := dec.Decode(packet(t, cfg, 5, 0, frame(cfg, &silent, false, 0), 0), drop); err != nil {
			t.Fatalf("a sequence jump was reported as damage: %v", err)
		}
	})

	t.Run("continuation-zero-over-a-carry", func(t *testing.T) {
		dec := newDec(t, cfg)
		// A frame whose declared length overruns the packet is the carry;
		// the next packet says nothing continues, which discards it. Measured
		// on ten of thirteen corpus files. The packet's fill is payload as
		// far as any reader can tell, so the overrun has to be real: a frame
		// longer than what the packet has left.
		first, _ := splitFrame(cfg, &silent, 100)
		if err := dec.Decode(packet(t, cfg, 0, 0, first, 0), drop); err != nil {
			t.Fatal(err)
		}
		got := 0
		count := func(b *audio.Buffer) error { got += b.N; return nil }
		if err := dec.Decode(packet(t, cfg, 1, 0, frame(cfg, &silent, false, 0), 0), count); err != nil {
			t.Fatalf("a zero continuation count over a carry was reported as damage: %v", err)
		}
		if got != cfg.SamplesPerFrame() {
			t.Errorf("%d samples after the discarded carry, want the next packet's frame (%d)", got, cfg.SamplesPerFrame())
		}
	})

	t.Run("continuation-exceeds-payload", func(t *testing.T) {
		dec := newDec(t, cfg)
		// The head of a frame in one packet and a count in the next that
		// claims more than the packet holds: the count saturates, and the
		// carried frame decodes.
		first, tail := splitFrame(cfg, &silent, 100)
		if err := dec.Decode(packet(t, cfg, 0, 0, first, 0), drop); err != nil {
			t.Fatal(err)
		}
		got := 0
		count := func(b *audio.Buffer) error { got += b.N; return nil }
		if err := dec.Decode(packet(t, cfg, 1, 1<<uint(cfg.FrameSizeBits())-1, tail, 0), count); err != nil {
			t.Fatalf("a saturating continuation count was reported as damage: %v", err)
		}
		if got != cfg.SamplesPerFrame() {
			t.Errorf("%d samples, want the carried frame (%d)", got, cfg.SamplesPerFrame())
		}
	})

	t.Run("skipped-header-bits-set", func(t *testing.T) {
		dec := newDec(t, cfg)
		pkt := packet(t, cfg, 0, 0, frame(cfg, &silent, false, 0), 0)
		pkt[0] |= 0x0c // both skipped bits
		if err := dec.Decode(pkt, drop); err != nil {
			t.Fatalf("set header bits were reported as damage: %v", err)
		}
	})

	t.Run("negative-quantisation-step", func(t *testing.T) {
		// 90 - 32 - 124: a step of -66, a gain below one.
		var w bitWriter
		frameHead(cfg, &w)
		subframeHead(cfg, &w)
		w.put(2, 2)
		w.put(0, 1)
		w.signed(-32, 6)
		for i := 0; i < 4; i++ {
			w.put(31, 5)
		}
		w.put(0, 5)
		w.put(0, 3)
		w.put(0, 2)
		nb := wmapro.BandCountForTest(cfg, 0)
		dpcm(&w, nb)
		dpcm(&w, nb)
		zeroBlock(&w)
		if err := decodeOne(t, smallConfig(), &w); err != nil {
			t.Fatalf("a negative quantisation step was reported as damage: %v", err)
		}
	})

	t.Run("group-of-no-channels", func(t *testing.T) {
		three := parse(t, config(16000, 3, 16, 0x07, 64, 0x00c0, 0))
		var w bitWriter
		frameHead(three, &w)
		w.put(0, 3)
		w.put(0, 3) // nobody joins the first group
		w.put(7, 3) // everybody joins the second
		w.put(0, 1) // no transform
		w.put(0, 3) // nobody transmits
		if err := decodeOne(t, config(16000, 3, 16, 0x07, 64, 0x00c0, 0), &w); err != nil {
			t.Fatalf("a group of no channels was reported as damage: %v", err)
		}
	})

	t.Run("run-level-block-without-end", func(t *testing.T) {
		var w bitWriter
		transmitFrame(t, cfg, &w, 0, func(w *bitWriter) {
			w.code("coef0", 0)
			w.put(0, 1)
			w.put(1, 8)
			w.put(6, 3)
			w.put(503, 9) // lands on index 511, the last
			w.put(1, 1)   // positive
		})
		if err := decodeOne(t, smallConfig(), &w); err != nil {
			t.Fatalf("a block that runs out without end-of-block was reported as damage: %v", err)
		}
	})

	t.Run("wider-padding-before-the-trailer", func(t *testing.T) {
		dec := newDec(t, cfg)
		got := 0
		count := func(b *audio.Buffer) error { got += b.N; return nil }
		var w bitWriter
		w.append(frame(cfg, &silent, true, 3))
		w.append(frame(cfg, &silent, false, 0))
		if err := dec.Decode(packet(t, cfg, 0, 0, &w, 0), count); err != nil {
			t.Fatalf("a frame with three padding bits was reported as damage: %v", err)
		}
		if got != cfg.SamplesPerFrame() {
			t.Errorf("%d samples, want the second frame (%d): the trailer bit was not found through the gap", got, cfg.SamplesPerFrame())
		}
	})

	t.Run("short-and-empty-packets", func(t *testing.T) {
		dec := newDec(t, cfg)
		full := packet(t, cfg, 0, 0, frame(cfg, &silent, false, 0), 0)
		if err := dec.Decode(full[:len(full)/2], drop); err != nil {
			t.Fatalf("a short packet was refused: %v", err)
		}
		if err := dec.Decode(nil, drop); err != nil {
			t.Fatalf("an empty packet was refused: %v", err)
		}
	})
}
