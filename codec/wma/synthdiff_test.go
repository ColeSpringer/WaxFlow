//go:build !wmatablesgen

package wma

// Differentials for the hand-built streams, through a container trick: WMA is
// a registered RIFF codec (tag 0x0161), so a WAV whose fmt chunk carries the
// stream's WAVEFORMATEX and whose data chunk is the packets back to back is a
// file ffmpeg decodes exactly as it decodes the same stream in ASF (verified
// bit-identical on a real cell before this landed). That gives the synthetic
// streams an oracle: until now their reconstruction VALUES were pinned only
// by hand-derived relationships, because no encoder writes these paths and a
// raw WMA stream has no container ffmpeg would take.
//
// Windows' own decoder also accepts the wrap, but it garbles hand-built
// streams whose configuration its encoder would not write, so only ffmpeg
// serves as the oracle here.

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// waveBlob is the stream's WAVEFORMATEX as a WAV fmt chunk payload.
//
// flags2 sits at a version-dependent offset in the codec-specific extra bytes
// -- 4 for v2, 2 for v1 -- and writing the v2 layout for a v1 stream is
// invisible on this side of the wrap, since decodeSynth is handed the Config
// struct directly. The oracle reads zeros there instead and decodes the stream
// as LSP exponents with no reservoir and no variable blocks, which is a
// differential comparing two different streams.
func waveBlob(c Config) []byte {
	var b []byte
	le16 := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	le32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	tag, extra, at := uint16(tagV2), 10, 4
	if !c.V2 {
		tag, extra, at = tagV1, 4, 2
	}
	le16(tag)
	le16(uint16(c.Channels))
	le32(uint32(c.Rate))
	le32(uint32(c.BitRate / 8))
	le16(uint16(c.BlockAlign))
	le16(16)
	le16(uint16(extra))
	e := make([]byte, extra)
	binary.LittleEndian.PutUint16(e[at:], c.Flags2)
	return append(b, e...)
}

// TestWaveBlobRoundTrips is a check on the instrument rather than on the
// decoder: the wrap is only an oracle if the header it writes says what the
// Config says, and ParseConfig is the reader that has to agree with it.
func TestWaveBlobRoundTrips(t *testing.T) {
	for _, c := range []Config{
		{V2: true, Rate: 44100, Channels: 2, BitRate: 128000, BlockAlign: 743, Flags2: flagExpVLC | flagVarBlkLen},
		{V2: false, Rate: 22050, Channels: 1, BitRate: 32000, BlockAlign: 185, Flags2: flagExpVLC},
		{V2: false, Rate: 44100, Channels: 1, BitRate: 64000, BlockAlign: 371, Flags2: 0},
	} {
		got, err := ParseConfig(waveBlob(c))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if got != c {
			t.Errorf("wrapped %+v, read back %+v", c, got)
		}
	}
}

// wrapWMAInWAV writes the packets as a WMA-in-WAV file.
func wrapWMAInWAV(t *testing.T, path string, c Config, pkts [][]byte) {
	t.Helper()
	blob := waveBlob(c)
	var data []byte
	for _, p := range pkts {
		data = append(data, p...)
	}
	var b []byte
	le32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	b = append(b, "RIFF"...)
	le32(uint32(4 + 8 + len(blob) + 8 + len(data)))
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	le32(uint32(len(blob)))
	b = append(b, blob...)
	b = append(b, "data"...)
	le32(uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// synthOracle decodes the packets with ffmpeg through the WAV wrap and with
// this decoder, aligns the two (each drops a different whole-frame lead), and
// returns them trimmed to a common origin and length. Skips without ffmpeg.
func synthOracle(t *testing.T, cfg Config, pkts [][]byte, bits []int) (ours, ff []float32, trim int) {
	t.Helper()
	testutil.FFmpeg(t)
	ours = decodeSynth(t, cfg, pkts, bits)
	wav := filepath.Join(t.TempDir(), "stream.wav")
	wrapWMAInWAV(t, wav, cfg, pkts)
	ff = testutil.FFmpegDecodeF32NoSIMD(t, wav)

	frameLen := cfg.FrameLen() * cfg.Channels
	best := math.Inf(1)
	trim = -1
	for _, cand := range []int{0, frameLen, 2 * frameLen, 3 * frameLen} {
		if len(ours) <= cand {
			continue
		}
		n := min(len(ours)-cand, len(ff))
		var s2, sig float64
		for i := range n {
			d := float64(ff[i]) - float64(ours[cand+i])
			s2 += d * d
			sig += float64(ff[i]) * float64(ff[i])
		}
		if sig == 0 {
			continue
		}
		if r := math.Sqrt(s2 / sig); r < best {
			best, trim = r, cand
		}
	}
	if trim < 0 || best > 0.5 {
		t.Fatalf("the decodes do not align at any whole-frame trim (best relative rms %g)", best)
	}
	ours = ours[trim:]
	n := min(len(ours), len(ff))
	return ours[:n], ff[:n], trim
}

// relDiff is rms(a-b)/rms(b) and max|a-b|/peak|b|.
func relDiff(a, b []float32) (rms, maxAbs float64) {
	var s2, sig, peak float64
	for i := range b {
		d := float64(a[i]) - float64(b[i])
		s2 += d * d
		v := float64(b[i])
		sig += v * v
		peak = max(peak, math.Abs(v))
	}
	if sig == 0 {
		return 0, 0
	}
	for i := range b {
		maxAbs = max(maxAbs, math.Abs(float64(a[i])-float64(b[i]))/peak)
	}
	return math.Sqrt(s2 / sig), maxAbs
}

// TestVariableBlockDifferential scores the variable-block-length stream
// against ffmpeg. The float rounding floor is the gate: the two decoders
// agree per sample to about a part in 1e7 of the signal, so anything past
// the bound below is a reconstruction difference, not precision.
func TestVariableBlockDifferential(t *testing.T) {
	cfg := synthConfig(t, flagExpVLC|flagVarBlkLen)
	pkts, bits := varBlockStream(t, cfg, false)
	ours, ff, _ := synthOracle(t, cfg, pkts, bits)
	rms, maxAbs := relDiff(ours, ff)
	t.Logf("variable-block differential: relative rms %.3e max %.3e over %d", rms, maxAbs, len(ours))
	if rms > 4e-6 || maxAbs > 4e-5 {
		t.Errorf("relative rms %g max %g, want <= 4e-6 and 4e-5", rms, maxAbs)
	}
}

// TestLSPDifferential scores LSP-exponent frames with varying codebook
// indices against ffmpeg. Measured while landing this: ffmpeg's curve agrees
// with the exact (p+q)^(-1/4) to ~6e-7 relative with no structure in p+q, so
// the reference does not evaluate the curve through a coarser table and the
// residual here is per-bin float rounding order. The Microsoft-corpus LSP
// cells miss the tight corpus gate by the same relative scale for the same
// reason; see msDeficits.
func TestLSPDifferential(t *testing.T) {
	cfg := synthConfig(t, 0)
	if cfg.expVLC() {
		t.Fatal("want LSP mode")
	}
	frameLen := cfg.FrameLen()
	var sampled []int
	for b := 40; b+6 < frameLen*9/10; b += 64 {
		for k := range 6 {
			sampled = append(sampled, b+k)
		}
	}
	s := &synth{cfg: cfg}
	var pkts [][]byte
	var bits []int
	for f := range 24 {
		s.bw.put(1, 1)
		s.bw.put(20, 7)
		for j := range nbLSPCoefs {
			n, m := 4, 16
			if j == 0 || j == 8 || j == 9 {
				n, m = 3, 8
			}
			s.bw.put(uint32((f*7+j*3+f*j)%m), n)
		}
		s.coefsAt(t, sampled, cfg.coefsEnd(0))
		p, n := s.packet(t)
		pkts = append(pkts, p)
		bits = append(bits, n)
	}
	ours, ff, _ := synthOracle(t, cfg, pkts, bits)
	rms, maxAbs := relDiff(ours, ff)
	t.Logf("LSP differential: relative rms %.3e max %.3e over %d", rms, maxAbs, len(ours))
	if rms > 4e-6 || maxAbs > 4e-5 {
		t.Errorf("relative rms %g max %g, want <= 4e-6 and 4e-5", rms, maxAbs)
	}
}

// TestNoiseReuseCharacterization pins the one reconstruction difference the
// Microsoft corpus surfaced, on a deterministic stream: when a block reuses
// an exponent curve decoded at another block length AND noise coding fills
// bands, ffmpeg reads the resampled curve one source bin behind inside the
// noise regions (both the per-bin fill and the band powers behind the gains),
// while this decoder indexes the reused curve the same way everywhere, which
// is what docs/notes/wma-bitstream.md describes. Everywhere else the two
// decoders agree to float rounding, measured on this same stream: the twelve
// noise-off reuse probes run while landing this matched exactly at every
// ratio, so the divergence is confined to the noise path.
//
// The stream replicates the corpus cell's shape: 22.05 kHz mono, noise on,
// a 128-sample transmit followed by a 256-sample reuse whose noise bands
// cover the curve's last step. Everything is deterministic, including the
// noise (the generator is part of the format's shared lineage), so the
// difference ffmpeg produces is a constant of the stream.
//
// If this test fails with the diff NEAR ZERO inside the reuse span, ffmpeg
// has stopped reading behind: delete the ms-22050 msDeficits entry, re-run
// the Microsoft corpus, and retire this test's lower bound with it.
func TestNoiseReuseCharacterization(t *testing.T) {
	cfg := Config{V2: true, Rate: 22050, Channels: 1, BitRate: 20000, BlockAlign: 372,
		Flags2: flagExpVLC | flagVarBlkLen | 2<<flagBlkDepthSh}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if n := cfg.nbBlockSizes(); n != 4 {
		t.Fatalf("nbBlockSizes = %d, want 4", n)
	}
	if mult, noise := cfg.highFreqMult(); !noise || mult != 0.7 {
		t.Fatalf("mult %v noise %v, want the 0.7 arm", mult, noise)
	}
	frameLen := cfg.FrameLen()

	allOff := make([]bool, 16)
	s := &synth{cfg: cfg, noiseBands: allOff, noiseGain: defaultNoiseGain}
	var pkts [][]byte
	var bits []int
	type blk struct {
		len             int
		coded, transmit bool
		bins            []int
		pattern         []bool
		endsFrame       bool
	}
	var blocks []blk
	for w := range 3 {
		blocks = append(blocks, blk{frameLen, true, true,
			[]int{8 * w, 8*w + 1, 8*w + 2, 8*w + 3, 8*w + 4, 8*w + 5, 8*w + 6, 8*w + 7}, allOff, true})
	}
	blocks = append(blocks,
		blk{128, true, true, nil, allOff, false},
		blk{256, true, false, nil, []bool{false, true, true}, false},
		blk{512, false, false, nil, allOff, false},
		blk{128, false, false, nil, allOff, true},
		blk{frameLen, true, true, nil, allOff, true})
	for i, b := range blocks {
		next := b.len
		if i+1 < len(blocks) {
			next = blocks[i+1].len
		}
		s.noiseBands = b.pattern
		s.writeBlock(t, b.len, next, b.coded, b.transmit, b.bins)
		if b.endsFrame {
			p, n := s.packet(t)
			pkts = append(pkts, p)
			bits = append(bits, n)
		}
	}
	ours, ff, trim := synthOracle(t, cfg, pkts, bits)

	// The reuse block: frame 3 (after three warmups), position 128, 256
	// samples, so its window writes accumulator [512, 1024) of that frame,
	// which frame 3 emits at decoder output 2*frameLen + 512.
	lo := 2*frameLen + 512 - trim
	hi := lo + 512
	span := func(a, b []float32, x, y int) (rms float64) {
		var s2 float64
		n := 0
		for i := x; i < y && i < len(a) && i < len(b); i++ {
			d := float64(a[i]) - float64(b[i])
			s2 += d * d
			n++
		}
		if n > 0 {
			rms = math.Sqrt(s2 / float64(n))
		}
		return
	}
	sig := span(ours, make([]float32, len(ours)), lo, hi)
	before := span(ours, ff, 0, lo)
	inside := span(ours, ff, lo, hi)
	after := span(ours, ff, hi, min(len(ours), hi+4*frameLen))
	t.Logf("signal rms %.3e; diff before %.3e inside %.3e after %.3e", sig, before, inside, after)

	if before > sig/1e4 || after > sig/1e4 {
		t.Errorf("the decoders disagree outside the reuse block (before %.3e after %.3e, signal %.3e): "+
			"the divergence is no longer confined to the noise path of the reuse block", before, after, sig)
	}
	switch {
	case inside < sig/1e4:
		t.Errorf("ffmpeg now agrees inside the reuse block too (diff %.3e, signal %.3e): "+
			"it no longer reads the noise-band curve one source bin behind; delete the ms-22050 "+
			"msDeficits entry and this test's lower bound", inside, sig)
	case inside < sig/40 || inside > sig:
		t.Errorf("the divergence inside the reuse block measures %.3e against signal %.3e, outside the "+
			"characterized shape; re-derive what changed before adjusting anything", inside, sig)
	}
}

// coefsAt writes level-one coefficients at the given sorted bins, escapes
// covering the gaps, then end-of-block (unless the bins fill the span, when
// the reader exits without one).
func (s *synth) coefsAt(t *testing.T, bins []int, nb int) {
	t.Helper()
	book := 2 * s.cfg.coefBookPair()
	offset := 0
	for _, bin := range bins {
		if bin < offset || bin >= nb {
			t.Fatalf("bin %d out of order or past nb %d (offset %d)", bin, nb, offset)
		}
		if gap := bin - offset; gap == 0 {
			s.bw.put(coefCodes[book][2], int(coefBits[book][2]))
		} else {
			s.bw.put(coefCodes[book][0], int(coefBits[book][0]))
			s.bw.put(1, escapeBits(21))
			s.bw.put(uint32(gap), s.cfg.frameLenBits())
		}
		s.bw.put(1, 1)
		offset = bin + 1
	}
	if offset < nb {
		s.bw.put(coefCodes[book][1], int(coefBits[book][1]))
	}
}

// writeBlock is synth.block with caller-chosen coefficient placement and the
// noise fields the differentials need.
func (s *synth) writeBlock(t *testing.T, blockLen, next int, coded, transmit bool, bins []int) {
	t.Helper()
	c := s.cfg
	n := bitsOf(c.nbBlockSizes()-1) + 1
	if !s.started {
		s.bw.put(s.selFor(blockLen), n)
		s.bw.put(s.selFor(blockLen), n)
		s.started = true
	}
	s.bw.put(s.selFor(next), n)
	s.bw.put(boolBit(coded), 1)
	if !coded {
		return
	}
	s.bw.put(20, 7) // total gain 21
	if s.noiseBands != nil {
		s.noise(t, blockSpec{len: blockLen, coded: [2]bool{true, false}})
	}
	if blockLen < c.FrameLen() {
		s.bw.put(boolBit(transmit), 1)
	}
	k := c.frameLenBits() - bitsOf(blockLen)
	if transmit || blockLen == c.FrameLen() {
		s.exponents(k, blockLen)
	}
	nb := c.coefsEnd(k)
	if s.noiseBands != nil {
		for j, hb := range s.highBands(k, blockLen) {
			if s.noiseBands[j] {
				nb -= hb.end - hb.start
			}
		}
	}
	s.coefsAt(t, bins, nb)
}
