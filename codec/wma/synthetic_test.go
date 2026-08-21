//go:build !wmatablesgen

package wma

// Hand-built streams for the paths no ffmpeg-produced file can reach.
//
// `flags2` is 1 in every file either ffmpeg encoder writes, so a generated
// corpus never carries LSP-coded exponents, variable block lengths, exponent
// reuse, or the three tabulated v2 exponent-band tables -- and those tables
// apply only below the largest block sizes, so all three ship gated on a file
// no corpus can contain. The differential says nothing about any of it.
//
// What a hand-built stream can establish without an oracle is that the reader
// walks the layout the notes describe: it consumes EXACTLY the bits that were
// written, which pins every field width and every conditional field, and it
// produces finite audio that goes to silence when the stream does. Whether
// the layout is the RIGHT one is what the real-world fixture in decode_test.go
// is for, and synthdiff_test.go now also scores several of these streams
// against ffmpeg directly, through the WMA-in-WAV wrap.

import (
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// synth writes a synthetic stream a packet at a time, mirroring the decoder's
// own block-length walk so the selectors it emits are the ones the decoder
// expects to read.
type synth struct {
	cfg     Config
	bw      synthBits
	started bool
	// expSym, when non-zero, overrides the alternating exponent deltas with
	// one symbol repeated, which is how a stream is driven off the end of the
	// exponent table.
	expSym int
	// noiseBands, when set, is the per-band fill pattern to write for a
	// noise-coded stream. It is indexed by high-band position and shared by
	// both channels.
	noiseBands []bool
	// noiseGain is the first filled band's gain in dB; the rest are deltas
	// from it, so changing it moves every noise gain in the stream together.
	// Every caller that writes noise sets it: the zero value is a legal gain
	// rather than "unset", so leaving it out changes the stream silently.
	noiseGain int
	// filled counts the bands actually written, so a test can refuse to pass
	// on a stream that exercised nothing.
	filled int
}

// noise writes the high-band flags and gains: TWO passes over the channels,
// every coded channel's flags and then every coded channel's gains, which is
// the ordering a reader that takes them together per channel gets wrong.
func (s *synth) noise(t testing.TB, b blockSpec) {
	t.Helper()
	k := s.cfg.frameLenBits() - bitsOf(b.len)
	bands := s.highBands(k, b.len)
	if len(bands) > len(s.noiseBands) {
		t.Fatalf("the block has %d high bands, the pattern covers %d", len(bands), len(s.noiseBands))
	}
	for ch := range s.cfg.Channels {
		if !b.coded[ch] {
			continue
		}
		for j := range bands {
			s.bw.put(boolBit(s.noiseBands[j]), 1)
		}
	}
	maxLen := uint8(0)
	for _, e := range hgainHuff {
		maxLen = max(maxLen, e[1])
	}
	for ch := range s.cfg.Channels {
		if !b.coded[ch] {
			continue
		}
		first := true
		for j := range bands {
			if !s.noiseBands[j] {
				continue
			}
			s.filled++
			if first {
				// The first filled band of a channel reads 7 bits minus 19.
				s.bw.put(uint32(19+s.noiseGain), 7)
				first = false
				continue
			}
			// Later ones add a delta from the noise-gain book, whose codes come
			// from the LISTED order rather than a canonical sort. Entry 30 is
			// symbol 14, a three-bit code, and the sorted build would give it a
			// different one.
			var acc uint32
			for i, e := range hgainHuff {
				code := acc >> (maxLen - e[1])
				acc += 1 << (maxLen - e[1])
				if i == 30 {
					s.bw.put(code, int(e[1]))
					break
				}
			}
		}
	}
}

// highBands is the block's high-band list, the same clipping the decoder does.
func (s *synth) highBands(k, blockLen int) []bandRange {
	mult, on := s.cfg.highFreqMult()
	if !on {
		return nil
	}
	highStart := int(math.Round(float64(blockLen) * mult))
	end := s.cfg.coefsEnd(k)
	var out []bandRange
	at := 0
	for _, w := range s.cfg.expBandWidths(k, blockLen) {
		bs, be := at, at+w
		at = be
		if lo, hi := max(bs, highStart), min(be, end); lo < hi {
			out = append(out, bandRange{lo, hi})
		}
	}
	return out
}

type synthBits struct {
	b []byte
	n int
}

func (w *synthBits) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n&7 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.b[w.n>>3] |= 1 << (7 - w.n&7)
		}
		w.n++
	}
}

func (w *synthBits) reset() { w.b, w.n = nil, 0 }

// selFor is the selector value for a block length: blockLenBits = frameLenBits
// - v.
func (s *synth) selFor(blockLen int) uint32 {
	return uint32(s.cfg.frameLenBits() - bitsOf(blockLen))
}

// blockSpec is one block to write.
type blockSpec struct {
	len, next int
	ms        bool
	// coded is per channel. Both false is the block that carries nothing.
	coded [2]bool
	// transmit sends exponents rather than reusing the previous block's; a
	// full-length block always transmits and the field is not written.
	transmit bool
	// zeroCoefs, per channel, writes that channel as CODED with all-zero
	// coefficients, which is the same all-zero spectrum by a different route
	// through the reader.
	zeroCoefs [2]bool
}

// block writes one block.
func (s *synth) block(t testing.TB, b blockSpec) {
	t.Helper()
	c := s.cfg
	if c.varBlockLen() {
		n := bitsOf(c.nbBlockSizes()-1) + 1
		if !s.started {
			// The first block after a reset reads two selectors for the
			// previous and current lengths, then one more for the next.
			s.bw.put(s.selFor(b.len), n)
			s.bw.put(s.selFor(b.len), n)
			s.started = true
		}
		s.bw.put(s.selFor(b.next), n)
	}
	if c.Channels == 2 {
		s.bw.put(boolBit(b.ms), 1)
	}
	any := false
	for ch := range c.Channels {
		s.bw.put(boolBit(b.coded[ch]), 1)
		any = any || b.coded[ch]
	}
	if !any {
		// The block stops the bitstream reading after the flags.
		return
	}
	s.bw.put(20, 7) // total gain 21, so the coefficient escape is 12 bits wide
	if s.noiseBands != nil {
		s.noise(t, b)
	}
	if b.len < c.FrameLen() {
		s.bw.put(boolBit(b.transmit), 1)
	}
	k := c.frameLenBits() - bitsOf(b.len)
	for ch := range c.Channels {
		if b.coded[ch] && (b.transmit || b.len == c.FrameLen()) {
			s.exponents(k, b.len)
		}
	}
	for ch := range c.Channels {
		if b.coded[ch] {
			s.coefficients(b.zeroCoefs[ch], b.ms && ch == 1)
		}
		// v1 aligns after each channel slot of a stereo block, coded or not.
		if !c.V2 && c.Channels == 2 {
			for s.bw.n&7 != 0 {
				s.bw.put(0, 1)
			}
		}
	}
}

func boolBit(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}

// exponents writes an exponent curve that alternates by band. A FLAT curve
// would make the block-size ratio in exponent reuse unobservable, since every
// index into it gives the same answer.
func (s *synth) exponents(k, blockLen int) {
	bands := s.cfg.expBandWidths(k, blockLen)
	first := 0
	if !s.cfg.V2 {
		s.bw.put(26, 5) // the running index seeds at this plus 10
		first = 1
	}
	if !s.cfg.expVLC() {
		// LSP: ten coefficients, three bits for 0, 8 and 9 and four for the
		// rest, each indexing its own codebook row.
		for j := range nbLSPCoefs {
			n, idx := 4, uint32(j%16)
			if j == 0 || j == 8 || j == 9 {
				n, idx = 3, uint32(j%8)
			}
			s.bw.put(idx, n)
		}
		return
	}
	// Symbols 63 and 57 are the plus-three and minus-three deltas, so the
	// running index alternates between two values a factor 1.6 apart.
	for i := range len(bands) - first {
		sym := 63
		if i%2 == 1 {
			sym = 57
		}
		if s.expSym != 0 {
			sym = s.expSym
		}
		s.bw.put(expScaleCodes[sym], int(expScaleBits[sym]))
	}
}

// coefficients writes three run-level pairs and an end-of-block, or just the
// end-of-block when the block is to carry nothing. side picks the second book
// of the pair, which codes channel 1 of a mid/side block.
func (s *synth) coefficients(none, side bool) {
	book := 2 * s.cfg.coefBookPair()
	if side {
		book++
	}
	if !none {
		for range 3 {
			// Index 2 is the first ladder entry: run 0, level 1.
			s.bw.put(coefCodes[book][2], int(coefBits[book][2]))
			s.bw.put(1, 1) // a sign bit of 1 is POSITIVE
		}
		// One escape, so the two widths it reads are exercised: the level in
		// escapeBits (12 at this block gain) and the run in frameLenBits,
		// which is the FRAME length even on a short block.
		s.bw.put(coefCodes[book][0], int(coefBits[book][0]))
		s.bw.put(9, escapeBits(21))
		s.bw.put(7, s.cfg.frameLenBits())
		s.bw.put(1, 1)
	}
	s.bw.put(coefCodes[book][1], int(coefBits[book][1])) // end of block
}

// packet finishes the current frame run into a blockAlign-sized packet and
// reports how many bits were written.
func (s *synth) packet(t testing.TB) ([]byte, int) {
	t.Helper()
	n := s.bw.n
	if (n+7)/8 > s.cfg.BlockAlign {
		t.Fatalf("the synthetic frame is %d bits, past the %d-byte superframe", n, s.cfg.BlockAlign)
	}
	pkt := make([]byte, s.cfg.BlockAlign)
	copy(pkt, s.bw.b)
	s.bw.reset()
	return pkt, n
}

// decodeSynth runs a synthetic stream and checks that every frame consumed
// exactly the bits it was written with.
func decodeSynth(t testing.TB, cfg Config, pkts [][]byte, bits []int) []float32 {
	t.Helper()
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer d.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		for i := range b.N {
			for ch := range b.Fmt.Channels {
				got = append(got, b.ChanF(ch)[i])
			}
		}
		return nil
	}
	for i, p := range pkts {
		if err := d.Decode(p, emit); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		// The frame read exactly what was written: no field is a bit wide,
		// no conditional field was read that should not have been, and the
		// walk ended where the writer ended.
		if d.r.pos != bits[i] {
			t.Fatalf("frame %d consumed %d bits, %d were written", i, d.r.pos, bits[i])
		}
	}
	if err := d.Drain(emit); err != nil {
		t.Fatal(err)
	}
	return got
}

// synthConfig is a v2 44.1 kHz mono stream at 64 kbit/s: the noise ladder is
// off there, which keeps a hand-built frame down to the fields under test.
func synthConfig(t testing.TB, flags2 uint16) Config {
	t.Helper()
	c := Config{V2: true, Rate: 44100, Channels: 1, BitRate: 64000, Flags2: flags2}
	c.BlockAlign = c.BitRate * c.FrameLen() / (c.Rate * 8)
	if err := c.Validate(); err != nil {
		t.Fatalf("synthetic config: %v", err)
	}
	if _, noise := c.highFreqMult(); noise {
		t.Fatal("the synthetic config has noise coding on; the frames below do not write its fields")
	}
	return c
}

func finite(t testing.TB, s []float32, what string) (peak float64) {
	t.Helper()
	for i, v := range s {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			t.Fatalf("%s: sample %d is %v", what, i, f)
		}
		peak = max(peak, math.Abs(f))
	}
	return peak
}

// TestVariableBlockLengths walks a frame that changes block size twice, which
// no corpus file does. It reaches, in one stream: the block-length selectors
// and their reset-time triple read, exponent reuse across a size change (which
// indexes the previous block's curve through the ratio between the two
// lengths), and the tabulated v2 exponent-band tables, which apply only to the
// three shortest block sizes and so ship gated on a file ffmpeg cannot write.
// varBlockLayouts alternates two frame layouts. The short one's last block is
// 1024 at block position 1024, so it writes through accumulator index 3584 and
// leaves the last 512 untouched, while the long one writes all of it.
//
// The short layout's first block is the one that carries nothing, chosen as
// the most exposed placement there is: it is short, and its right half lands
// in the accumulator's lower half, over samples the long frame before it left
// non-zero. It is still not enough to make the uncoded block's zero-store
// observable, measured by deleting the store and running the suite. That
// matches what decodeBlock's own comment says and why: the window transitions
// make each block's non-zero span end exactly where the next one's store
// begins, so the region it clears is already zero in any decode that reached
// it. The placement stays because it is the one worth having if the argument
// ever stops holding, not because a test here depends on it.
var varBlockLayouts = [][]int{{2048}, {256, 256, 512, 1024}}

// varBlockShort says what each block of the short layout carries. The two
// reuses are the point of the table and they reuse at DIFFERENT lengths, which
// is the only thing that makes the ratio in resampleExponents observable:
// block 1 indexes the 2048-sample curve the frame before it transmitted, and
// block 3 indexes the 512-sample one from earlier in its own frame. A reuse at
// the same length would decode identically with the ratio deleted.
var varBlockShort = []struct {
	coded, transmit bool
}{
	{false, false}, // carries nothing, and is where its zero-store is visible
	{true, false},  // reuses at 2048, so the curve is read one bin in eight
	{true, true},   // transmits at 512
	{true, false},  // reuses at 512, so the curve is read two bins per one
}

const varBlockContent, varBlockSilent = 6, 5

// varBlockStream builds the variable-block-length stream. codeSilent writes
// every block that carries nothing as a CODED block with an immediate
// end-of-block instead of as an uncoded one: the same all-zero spectrum by a
// different route through the reader.
func varBlockStream(t *testing.T, cfg Config, codeSilent bool) (pkts [][]byte, bits []int) {
	t.Helper()
	type blk struct {
		len             int
		coded, transmit bool
	}
	var blocks []blk
	var frameEnd []int
	for f := range varBlockContent + varBlockSilent {
		layout := varBlockLayouts[f%len(varBlockLayouts)]
		for i, b := range layout {
			// The long layout is one coded block; the short one follows
			// varBlockShort, whose first block carries nothing while its
			// neighbours do. A frame that is entirely coded or entirely silent
			// cannot separate the two things that keep the accumulator clean.
			coded, transmit := true, true
			if len(layout) > 1 {
				coded, transmit = varBlockShort[i].coded, varBlockShort[i].transmit
			}
			blocks = append(blocks, blk{b, coded && f < varBlockContent, transmit})
		}
		frameEnd = append(frameEnd, len(blocks))
	}
	s := &synth{cfg: cfg}
	fi := 0
	for i, b := range blocks {
		next := b.len
		if i+1 < len(blocks) {
			next = blocks[i+1].len
		}
		coded, zero := b.coded, false
		if !coded && codeSilent {
			coded, zero = true, true
		}
		s.block(t, blockSpec{len: b.len, next: next, coded: [2]bool{coded, coded},
			transmit: b.transmit, zeroCoefs: [2]bool{zero, zero}})
		if i+1 == frameEnd[fi] {
			fi++
			p, n := s.packet(t)
			pkts = append(pkts, p)
			bits = append(bits, n)
		}
	}
	return pkts, bits
}

// TestVariableBlockLengths walks a frame that changes block size twice, which
// no corpus file does. It reaches, in one stream: the block-length selectors
// and their reset-time triple read, exponent reuse across a size change (which
// indexes the previous block's curve through the ratio between the two
// lengths), and the tabulated v2 exponent-band tables, which apply only to the
// three shortest block sizes and so ship gated on a file ffmpeg cannot write.
//
// What it establishes is that the reader walks the layout: every frame
// consumes exactly the bits written, which pins each field width and each
// conditional field, the audio stays finite, and it goes to exact silence when
// the stream does. The reconstruction VALUES on this path are scored against
// ffmpeg by TestVariableBlockDifferential, which reaches the same stream
// through the WMA-in-WAV wrap.
func TestVariableBlockLengths(t *testing.T) {
	// Variable block lengths, VLC exponents, no reservoir.
	cfg := synthConfig(t, flagExpVLC|flagVarBlkLen)
	if n := cfg.nbBlockSizes(); n != 4 {
		t.Fatalf("nbBlockSizes = %d, want 4 (2048/1024/512/256)", n)
	}
	// Two of the three block sizes below take a tabulated band row; the test
	// is worth little if they do not.
	for _, b := range []int{512, 256} {
		k := cfg.frameLenBits() - bitsOf(b)
		if row := cfg.frameLenBits() - 7 - k; row >= 3 {
			t.Fatalf("a %d-sample block takes computed bands, not row %d", b, row)
		}
	}

	pkts, bits := varBlockStream(t, cfg, false)
	got := decodeSynth(t, cfg, pkts, bits)
	if peak := finite(t, got, "variable block lengths"); peak == 0 {
		t.Fatal("the decode is silent; the coefficients went nowhere")
	}

	// Frame by frame: the head trim drops one, so output frame f is packet
	// f+1's. The frames the coded packets produce carry audio; the FIRST
	// silent one still does, because it completes the last coded block's
	// overlap tail, and every one after it must be exactly zero.
	n := cfg.FrameLen()
	for f := 0; f*n < len(got); f++ {
		p := 0.0
		for _, v := range got[f*n : min((f+1)*n, len(got))] {
			p = max(p, math.Abs(float64(v)))
		}
		switch {
		case f <= varBlockContent-1:
			if p == 0 {
				t.Errorf("output frame %d is silent inside the coded run", f)
			}
		default:
			if p != 0 {
				t.Errorf("output frame %d peaks at %g after the stream went silent", f, p)
			}
		}
	}
}

// TestUncodedBlockIsAZeroSpectrum states the notes' claim as an equivalence a
// test can check: a block with no coded channel "stops the bitstream reading
// and nothing else", and still runs the transform stage with an all-zero
// spectrum, because that is what completes the previous block's overlap tail.
//
// So it must decode identically to a coded block whose coefficients are all
// zero, which reaches the same spectrum by a route the reader cannot skip.
// A decoder that returns early for an uncoded block passes every "is it
// silent" check -- the missing tail is silence too -- and fails this.
func TestUncodedBlockIsAZeroSpectrum(t *testing.T) {
	cfg := synthConfig(t, flagExpVLC|flagVarBlkLen)
	pkts, bits := varBlockStream(t, cfg, false)
	uncoded := decodeSynth(t, cfg, pkts, bits)
	pkts, bits = varBlockStream(t, cfg, true)
	zeroed := decodeSynth(t, cfg, pkts, bits)
	if len(uncoded) != len(zeroed) {
		t.Fatalf("%d samples against %d", len(uncoded), len(zeroed))
	}
	for i := range uncoded {
		if uncoded[i] != zeroed[i] {
			t.Fatalf("sample %d: an uncoded block gave %v where a zero-coefficient one gave %v",
				i, uncoded[i], zeroed[i])
		}
	}
}

// TestLSPExponents walks a stream with `flags2` bit 0 clear. Half of the
// format's exponent coding, the lspCodebook table and the larger noise
// multiplier are reachable no other way: neither ffmpeg encoder ever clears
// that bit.
func TestLSPExponents(t *testing.T) {
	cfg := synthConfig(t, 0) // LSP exponents, no reservoir, no variable blocks
	if cfg.expVLC() {
		t.Fatal("the config still asks for VLC exponents")
	}
	s := &synth{cfg: cfg}
	var pkts [][]byte
	var bits []int
	for range 4 {
		s.block(t, blockSpec{len: cfg.FrameLen(), next: cfg.FrameLen(),
			coded: [2]bool{true, true}, transmit: true})
		p, n := s.packet(t)
		pkts = append(pkts, p)
		bits = append(bits, n)
	}
	got := decodeSynth(t, cfg, pkts, bits)
	if peak := finite(t, got, "LSP exponents"); peak == 0 {
		t.Fatal("the decode is silent")
	}

	// The curve itself: the Vorbis LSP evaluation is a resonant envelope, so
	// every bin must be positive and finite, and the whole point of it is that
	// it is not flat.
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	if err := d.Decode(pkts[0], func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	exp := d.exp[0][:cfg.FrameLen()]
	lo, hi := math.Inf(1), 0.0
	for i, v := range exp {
		f := float64(v)
		if !(f > 0) || math.IsInf(f, 0) {
			t.Fatalf("exponent %d is %v", i, f)
		}
		lo, hi = min(lo, f), max(hi, f)
	}
	if hi/lo < 2 {
		t.Errorf("the LSP curve spans %g..%g, which is flat enough to be a bug", lo, hi)
	}
	if got, want := float64(d.maxExp[0]), hi; got != want {
		t.Errorf("maxExp = %v, the curve peaks at %v", got, want)
	}
	// The noise multiplier is a property of the exponent strategy, and LSP
	// takes the larger one.
	if got := cfg.noiseMult(); got != 0.04 {
		t.Errorf("noiseMult = %v with LSP exponents, want 0.04", got)
	}
}

// stereoSynthConfig is a v2 44.1 kHz stereo stream at 128 kbit/s: noise coding
// is off there, and v2 does not byte-align, so a hand-built stereo block is
// just the flags, the gain, the exponents and the coefficients.
func stereoSynthConfig(t *testing.T) Config {
	t.Helper()
	c := Config{V2: true, Rate: 44100, Channels: 2, BitRate: 128000, Flags2: flagExpVLC}
	c.BlockAlign = c.BitRate * c.FrameLen() / (c.Rate * 8)
	if err := c.Validate(); err != nil {
		t.Fatalf("synthetic config: %v", err)
	}
	if _, noise := c.highFreqMult(); noise {
		t.Fatal("the synthetic config has noise coding on")
	}
	return c
}

// splitChannels de-interleaves a stereo decode.
func splitChannels(s []float32) (l, r []float32) {
	for i := 0; i+1 < len(s); i += 2 {
		l = append(l, s[i])
		r = append(r, s[i+1])
	}
	return l, r
}

// TestMidSideOneSided covers the two asymmetric mid/side cases, which no
// ffmpeg corpus reaches: measured across every stereo cell and on duplicated,
// near-silent and digitally silent sources, that encoder sets ms_stereo in
// 100% of blocks and codes BOTH channels in 100% of them, so a one-sided block
// never occurs in anything it writes.
//
// They are worth reaching anyway, because each has an exact relationship a
// test can demand and because the notes flag them as easy to get backwards.
// With only the mid coded the side is absent, the butterfly does NOT run, and
// both outputs are the mid -- a decoder that clears every uncoded channel's
// buffer silences the right channel on exactly the near-mono material where a
// listener would notice. With only the side coded the mid is zeroed, the
// butterfly DOES run, and the outputs are S and -S.
//
// Each phase is preceded by a both-coded run, which is what leaves a stale
// spectrum in the channel the next phase leaves out: without it a decoder that
// wrongly ran the butterfly would butterfly against zeroes and look right.
func TestMidSideOneSided(t *testing.T) {
	cfg := stereoSynthConfig(t)
	n := cfg.FrameLen()
	const warm, phase = 3, 4

	build := func(t *testing.T, coded [2]bool) []float32 {
		t.Helper()
		s := &synth{cfg: cfg}
		var pkts [][]byte
		var bits []int
		emit := func(c [2]bool) {
			s.block(t, blockSpec{len: n, next: n, ms: true, coded: c, transmit: true})
			p, b := s.packet(t)
			pkts = append(pkts, p)
			bits = append(bits, b)
		}
		for range warm {
			emit([2]bool{true, true})
		}
		for range phase {
			emit(coded)
		}
		return decodeSynth(t, cfg, pkts, bits)
	}

	t.Run("only the mid is coded", func(t *testing.T) {
		l, r := splitChannels(build(t, [2]bool{true, false}))
		// Skip the transition frame, whose overlap still carries a both-coded
		// block; from there the two channels must be identical sample for
		// sample.
		from := warm * n
		if from >= len(l) {
			t.Fatalf("only %d frames of output", len(l)/n)
		}
		nonzero := false
		for i := from; i < len(l); i++ {
			if l[i] != r[i] {
				t.Fatalf("sample %d: left %v, right %v; both outputs are the mid", i, l[i], r[i])
			}
			nonzero = nonzero || l[i] != 0
		}
		if !nonzero {
			t.Fatal("both channels are silent, which proves nothing")
		}
	})

	t.Run("only the side is coded", func(t *testing.T) {
		l, r := splitChannels(build(t, [2]bool{false, true}))
		from := warm * n
		nonzero := false
		for i := from; i < len(l); i++ {
			if l[i] != -r[i] {
				t.Fatalf("sample %d: left %v, right %v; the mid is zero so L = S and R = -S", i, l[i], r[i])
			}
			nonzero = nonzero || l[i] != 0
		}
		if !nonzero {
			t.Fatal("both channels are silent, which proves nothing")
		}
	})
}

// TestNoiseCodedBands reaches the noise model, which no ffmpeg-produced file
// does: measured over every noise-coded corpus cell, 0 of 2317 high bands were
// ever flagged for noise fill, so the flags' effect, the gains, the noise-gain
// book, the per-band power ratio and the whole mult1 scale are unexercised by
// the differential.
//
// What is checkable without an oracle: the reader consumes exactly the bits
// written -- which pins the TWO-PASS ordering, since taking flags and gains
// together per channel reads the same number of bits in a different order and
// desynchronises the stereo block that follows -- the audio stays finite, and
// the noise-filled bands actually carry energy rather than silence.
func TestNoiseCodedBands(t *testing.T) {
	// v2 44.1 kHz stereo at 24 kbit/s: bps1 is 0.435, under the 0.61 that
	// turns noise coding off, so the high band starts at 0.4 of the block.
	cfg := Config{V2: true, Rate: 44100, Channels: 2, BitRate: 24000, Flags2: flagExpVLC}
	cfg.BlockAlign = cfg.BitRate * cfg.FrameLen() / (cfg.Rate * 8)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	mult, on := cfg.highFreqMult()
	if !on || mult != 0.4 {
		t.Fatalf("the config has noise coding %v at %v; this test needs it on", on, mult)
	}
	build := func(gain int) []float32 {
		s := &synth{cfg: cfg, noiseGain: gain}
		// Fill every other high band, so both the filled and the coded arms of
		// the reconstruction run and the gain book is read more than once per
		// channel.
		s.noiseBands = make([]bool, 32)
		for i := range s.noiseBands {
			s.noiseBands[i] = i%2 == 0
		}
		n := cfg.FrameLen()
		var pkts [][]byte
		var bits []int
		for range 5 {
			s.block(t, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
			p, b := s.packet(t)
			pkts = append(pkts, p)
			bits = append(bits, b)
		}
		if s.filled < 20 {
			t.Fatalf("only %d bands were noise-filled; the stream exercises nothing", s.filled)
		}
		got := decodeSynth(t, cfg, pkts, bits)
		if peak := finite(t, got, "noise-coded bands"); peak == 0 {
			t.Fatal("the decode is silent")
		}
		return got
	}

	// The gains have to reach the samples, and the only way to say that
	// without an oracle is to move them and watch the output move with them.
	// The stream is decoded three times at gains a fixed step apart -- every
	// later band is a delta from the first, so one field moves all of them --
	// and only the filled bands change, because the three streams have the
	// same bit layout and so draw the same noise at the same indices.
	//
	// Differencing is what makes that measurable. The coded coefficients here
	// carry about forty times the energy of the fill, so a ratio of total
	// energies barely moves; the difference between two decodes is the fill's
	// change alone. A reconstruction that zeroed the noise-flagged bands while
	// still consuming their flags and gains leaves every difference zero, and
	// the coded coefficients keep all three decodes audibly non-silent, which
	// is what an "is it louder than nothing" check cannot see.
	//
	// What this pins is that the fill reaches the samples and that the gain is
	// a decibel amplitude. The per-band power ratio cancels in a ratio of
	// differences, so it is NOT pinned here; nothing reaches it, since no
	// ffmpeg-produced file flags a band for fill.
	const step = 12
	loud, mid, quiet := build(8), build(8-step), build(8-2*step)
	d1, d2 := rmsDiff(t, loud, mid), rmsDiff(t, mid, quiet)
	if d1 == 0 {
		t.Fatal("moving every noise gain changed nothing; the fill is not reaching the samples")
	}
	want := math.Pow(10, -step/20.0)
	if got := d2 / d1; math.Abs(got-want) > 1e-3 {
		t.Errorf("a %d dB step scaled the noise fill by %.5f, want %.5f", step, got, want)
	}
}

// rmsDiff is the root mean square of a minus b.
func rmsDiff(t *testing.T, a, b []float32) float64 {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%d samples against %d", len(a), len(b))
	}
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(a)))
}

// TestExponentIndexRefusal: the exponent curve is a 156-entry table over index
// -60..95 and the table IS the definition of the value, so a stream that
// indexes past it is one no decoder agrees with. Saturating would be the
// forgiving choice and the wrong one, and it is also how a fuzzer reads off
// the end of an array. No corpus file goes near the bound -- ffmpeg's encoder
// emits a flat curve -- so the refusal needs a stream built to trip it.
func TestExponentIndexRefusal(t *testing.T) {
	cfg := synthConfig(t, flagExpVLC)
	s := &synth{cfg: cfg}
	// Symbol 63 is the plus-three delta. The index seeds at 36 and the table
	// ends at 95, so twenty bands of it run off the end; the block has 25.
	s.expSym = 63
	s.block(t, blockSpec{len: cfg.FrameLen(), next: cfg.FrameLen(),
		coded: [2]bool{true, true}, transmit: true})
	pkt, _ := s.packet(t)
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	err = d.Decode(pkt, func(*audio.Buffer) error { return nil })
	if err == nil {
		t.Fatal("accepted an exponent index past the table")
	}
	if !strings.Contains(err.Error(), "exponent index") {
		t.Errorf("error %q does not name the exponent index", err)
	}
	// And a stream that stays inside the table decodes.
	s.expSym = 60 // the delta-zero code
	s.block(t, blockSpec{len: cfg.FrameLen(), next: cfg.FrameLen(),
		coded: [2]bool{true, true}, transmit: true})
	pkt, _ = s.packet(t)
	d2, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Release()
	if err := d2.Decode(pkt, func(*audio.Buffer) error { return nil }); err != nil {
		t.Fatalf("a flat curve well inside the table: %v", err)
	}
}

// TestMidStreamRefusals covers the malformed shapes that need a stream built
// to produce them. Each is a named refusal the implementability checklist in
// docs/notes/wma-bitstream.md asks for, and none is reachable from a corpus
// file, which is by construction well formed.
func TestMidStreamRefusals(t *testing.T) {
	t.Run("exponents reused before any were transmitted", func(t *testing.T) {
		cfg := synthConfig(t, flagExpVLC|flagVarBlkLen)
		s := &synth{cfg: cfg}
		// A short first block that reuses: there is nothing to reuse.
		s.block(t, blockSpec{len: 512, next: 512, coded: [2]bool{true, true}, transmit: false})
		pkt, _ := s.packet(t)
		mustRefuse(t, cfg, pkt, "reuses exponents that were never transmitted")
	})

	t.Run("run-level coding past the channel", func(t *testing.T) {
		cfg := synthConfig(t, flagExpVLC)
		s := &synth{cfg: cfg}
		s.bw.put(1, 1)  // channel coded
		s.bw.put(20, 7) // total gain 21
		s.exponents(0, cfg.FrameLen())
		// An escape whose run lands past the codeable span.
		book := 2 * cfg.coefBookPair()
		s.bw.put(coefCodes[book][0], int(coefBits[book][0]))
		s.bw.put(1, escapeBits(21))
		s.bw.put(uint32(cfg.coefsEnd(0)+16), cfg.frameLenBits())
		s.bw.put(1, 1)
		s.bw.put(coefCodes[book][1], int(coefBits[book][1]))
		pkt, _ := s.packet(t)
		mustRefuse(t, cfg, pkt, "past the channel")
	})

	t.Run("a block that overruns the frame", func(t *testing.T) {
		cfg := synthConfig(t, flagExpVLC|flagVarBlkLen)
		s := &synth{cfg: cfg}
		// A half-length block followed by a full-length one: the second has
		// only half a frame of room left.
		s.block(t, blockSpec{len: cfg.FrameLen() / 2, next: cfg.FrameLen(),
			coded: [2]bool{true, true}, transmit: true})
		s.block(t, blockSpec{len: cfg.FrameLen(), next: cfg.FrameLen(),
			coded: [2]bool{true, true}, transmit: true})
		pkt, _ := s.packet(t)
		mustRefuse(t, cfg, pkt, "overruns")
	})

	t.Run("a block-length selector out of range", func(t *testing.T) {
		// 16 kHz frames 512 samples, so the block-size depth caps at two and
		// the stream has three sizes: a two-bit selector can name a fourth
		// that does not exist.
		cfg := Config{V2: true, Rate: 16000, Channels: 1, BitRate: 64000,
			Flags2: flagExpVLC | flagVarBlkLen}
		cfg.BlockAlign = cfg.BitRate * cfg.FrameLen() / (cfg.Rate * 8)
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		n := bitsOf(cfg.nbBlockSizes()-1) + 1
		if cfg.nbBlockSizes() != 3 || n != 2 {
			t.Fatalf("the config has %d block sizes and %d selector bits, want 3 and 2",
				cfg.nbBlockSizes(), n)
		}
		s := &synth{cfg: cfg}
		s.bw.put(3, n) // one past the last block size
		s.bw.put(0, n)
		s.bw.put(0, n)
		pkt, _ := s.packet(t)
		mustRefuse(t, cfg, pkt, "selector")
	})
}

func mustRefuse(t *testing.T, cfg Config, pkt []byte, want string) {
	t.Helper()
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	err = d.Decode(pkt, func(*audio.Buffer) error { return nil })
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
}

// TestMidOnlyBlockEqualsAZeroSideChannel is TestMidSideOneSided's relationship
// made into a value. That test demands the two outputs be identical, which
// they are even when a decoder wrongly runs the butterfly: with only the mid
// coded, channel 1's samples are COPIED from channel 0 downstream, so
// corrupting the mid corrupts both equally and the relationship survives.
//
// What separates them is that a mid/side block with the side coded to all
// zeroes reaches the same audio by the other route -- the butterfly runs, over
// a side that really is zero, and gives (a+0, a-0). So the two streams must
// decode identically, and a butterfly that ran against the PREVIOUS block's
// leftover side channel does not.
func TestMidOnlyBlockEqualsAZeroSideChannel(t *testing.T) {
	cfg := stereoSynthConfig(t)
	n := cfg.FrameLen()
	const warm, phase = 3, 4

	build := func(t *testing.T, zeroSide bool) []float32 {
		t.Helper()
		s := &synth{cfg: cfg}
		var pkts [][]byte
		var bits []int
		emit := func(b blockSpec) {
			s.block(t, b)
			p, n := s.packet(t)
			pkts = append(pkts, p)
			bits = append(bits, n)
		}
		// A both-coded run first: it is what leaves a side channel behind for
		// a wrongly-run butterfly to pick up.
		for range warm {
			emit(blockSpec{len: n, next: n, ms: true, coded: [2]bool{true, true}, transmit: true})
		}
		for range phase {
			b := blockSpec{len: n, next: n, ms: true, coded: [2]bool{true, false}, transmit: true}
			if zeroSide {
				b.coded[1] = true
				b.zeroCoefs[1] = true
			}
			emit(b)
		}
		return decodeSynth(t, cfg, pkts, bits)
	}

	got, want := build(t, false), build(t, true)
	if len(got) != len(want) {
		t.Fatalf("%d samples against %d", len(got), len(want))
	}
	from := warm * n * cfg.Channels
	if from >= len(got) {
		t.Fatalf("only %d samples of output", len(got))
	}
	nonzero := false
	for i := from; i < len(got); i++ {
		if got[i] != want[i] {
			t.Fatalf("sample %d: an uncoded side gave %v where a zero-coded one gave %v",
				i, got[i], want[i])
		}
		nonzero = nonzero || got[i] != 0
	}
	if !nonzero {
		t.Fatal("the compared region is silent, which proves nothing")
	}
}

// BenchmarkLSPExponentCurve measures the half of the exponent coding no
// benchmark could reach before: every corpus cell is VLC-coded, because
// `flags2` is 1 in every file either ffmpeg encoder writes, so a benchmark
// picked from that corpus profiles the other path exclusively.
//
// It matters because the LSP curve costs one transcendental per BIN per coded
// channel, which is one per output sample per channel, where the VLC path
// costs one table lookup per band. The realtime floor in docs/quality-gates.md
// is a per-stream number, so the worst shape it has to hold for is the one
// below: the highest rate, stereo, LSP.
func BenchmarkLSPExponentCurve(b *testing.B) {
	cfg := Config{V2: true, Rate: 48000, Channels: 2, BitRate: 128000, Flags2: 0}
	cfg.BlockAlign = cfg.BitRate * cfg.FrameLen() / (cfg.Rate * 8)
	if err := cfg.Validate(); err != nil {
		b.Fatal(err)
	}
	if cfg.expVLC() {
		b.Fatal("the config still asks for VLC exponents")
	}
	s := &synth{cfg: cfg}
	n := cfg.FrameLen()
	var pkts [][]byte
	var bits []int
	for range 8 {
		s.block(b, blockSpec{len: n, next: n, coded: [2]bool{true, true}, transmit: true})
		p, k := s.packet(b)
		pkts = append(pkts, p)
		bits = append(bits, k)
	}
	_ = bits
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		b.Fatal(err)
	}
	defer d.Release()
	var frames int64
	emit := func(buf *audio.Buffer) error {
		frames += int64(buf.N)
		return nil
	}
	b.ResetTimer()
	for b.Loop() {
		d.Reset()
		for _, p := range pkts {
			if err := d.Decode(p, emit); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(frames)/float64(cfg.Rate)/b.Elapsed().Seconds(), "x-realtime")
}

// defaultNoiseGain is the first filled band's gain every noise-coded synthetic
// here writes unless it is varying the gain on purpose.
const defaultNoiseGain = 8
