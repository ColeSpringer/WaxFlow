package wavpack

import (
	"encoding/binary"
	"fmt"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// Compression levels. They select how many candidate decorrelation cascades
// the encoder tries on each block before keeping the smallest, and nothing
// else: every level is lossless and decodes to the same samples, so the choice
// is purely size against encode time. The names are the reference tool's
// (-f, none, -h, -hh).
const (
	LevelFast = iota + 1
	LevelNormal
	LevelHigh
	LevelVeryHigh
)

// DefaultEncoderLevel is the level a nil EncoderOptions selects.
const DefaultEncoderLevel = LevelNormal

// BlockSamples is the encoder's block length, and so Encoder.FrameSize. Every
// block repeats the cascade's state (about a hundred bytes), so short blocks
// pay that over and over; long ones cost decode work on a seek, since a seek
// lands on a block boundary and the rest is pre-rolled. This is a fifth of a
// second at 44.1 kHz.
const BlockSamples = 8192

// packDelta is the rate every pass adapts its weight at, in 1024ths per
// sample. It rides in the top three bits of each stored term.
//
// One, not the two the reference uses, and the two corpora disagree about it by
// enough that the choice is worth stating. The weight takes a fixed step every
// sample, so the step size is also the jitter it carries once it has converged:
// on tonal material that jitter is most of the residual, and two came out 23%
// larger than one on synthesized partials. On the real audio of the official
// suite two is better, by 0.03% to 0.09%. One is kept because the trade is not
// symmetric -- it costs under a tenth of a percent on ordinary music and saves
// a fifth of the file on a sustained tone, which real music does contain.
const packDelta = 1

// maxCodedMagnitude bounds the magnitude the entropy coder sees. Wider samples
// (a genuine 32-bit source) have their low bits held back in the extension
// stream instead: the decorrelation math is 32-bit, so a full-scale 32-bit
// sample wraps applyWeight and the prediction stops predicting, which costs
// far more than the raw bits the extension spends.
const maxCodedMagnitude = 24

// A compression level is a list of candidate decorrelation cascades, and the
// encoder tries all of them on every block and keeps the smallest. Levels
// nest: a deeper level's list contains the shallower one's, so it wins every
// block the shallower one would have. That is what orders the knob, and it is
// not obtainable from a fixed cascade per level, because no single cascade
// wins across content: on synthesized tones the best chain was a different one
// per signal and the wrong one cost up to 15%. On real audio the spread is far
// narrower (the whole eight-candidate search is worth about 0.2% over its own
// first candidate), so the search earns its keep as insurance against content
// that is not ordinary music, not as the main lever. The main lever is depth.
//
// Ordered, not monotone to the byte. The choice is greedy per block over an
// entropy state the blocks share (see switchMargin), so a deeper level can
// still land a hair above a shallower one; docs/quality-gates.md states the
// tolerance that holds.
//
// The lists are ours, not the reference's, since nothing in the bitstream
// fixes which terms an encoder picks. Mono has its own because the
// cross-channel terms have no meaning there and the format rejects them.
type levelSets struct {
	stereo [][]int
	mono   [][]int
}

// The cascades, named for their shape and shared between the levels that use
// them. The names are not level names on purpose: which cascade a level starts
// from is a measured choice, and the shallowest level does not use the
// shallowest chain.
var (
	cascTwoTap  = []int{17, 17}
	cascBase    = []int{18, 18, 2, 3, -2}
	cascMixed   = []int{18, 17, 2, 3, -2}
	cascDeep    = []int{18, 18, 2, 3, -2, 18, 2, 4, 7, 5}
	cascShort   = []int{17, 17, 2, 3, -2}
	cascWide    = []int{18, 18, 2, -2, 3, 4}
	cascLags    = []int{18, 18, 2, 3, 4, 5, 6, 7, -2}
	cascDeepest = []int{18, 18, 2, 3, -2, 18, 2, 4, 7, 5, 3, 6, 8, -1, 18, 2}
	monoTwoTap  = []int{17, 17}
	monoBase    = []int{18, 18, 2, 3, 5}
	monoMixed   = []int{18, 17, 2, 3, 5}
	monoDeep    = []int{18, 18, 2, 3, 5, 18, 2, 4, 7, 6}
	monoShort   = []int{17, 17, 2, 3, 5}
	monoWide    = []int{18, 18, 2, 4, 3, 6}
	monoLags    = []int{18, 18, 2, 3, 4, 5, 6, 7, 8}
	monoDeepest = []int{18, 18, 2, 3, 5, 18, 2, 4, 7, 6, 3, 8, 1, 18, 2, 4}
)

// The ladder, chosen by measurement against the real audio of the official
// WavPack suite and a synthesized tonal set, at every level, against the
// reference encoder at its matching setting.
//
// The two-tap chain is deliberately NOT the fast level's, though it is the
// cheapest: on real audio it never wins, and a level built on it alone came out
// 1% larger than the reference's fast mode while every alternative came out 2%
// smaller. It stays in the ladder from the normal level up, because it is the
// chain that wins on sustained tones, where dropping it costs 12%.
var levels = [5]levelSets{
	LevelFast: {
		stereo: [][]int{cascBase},
		mono:   [][]int{monoBase},
	},
	LevelNormal: {
		stereo: [][]int{cascBase, cascTwoTap, cascDeep},
		mono:   [][]int{monoBase, monoTwoTap, monoDeep},
	},
	LevelHigh: {
		stereo: [][]int{cascBase, cascTwoTap, cascDeep, cascMixed, cascDeepest},
		mono:   [][]int{monoBase, monoTwoTap, monoDeep, monoMixed, monoDeepest},
	},
	LevelVeryHigh: {
		stereo: [][]int{cascBase, cascTwoTap, cascDeep, cascMixed, cascDeepest,
			cascShort, cascWide, cascLags},
		mono: [][]int{monoBase, monoTwoTap, monoDeep, monoMixed, monoDeepest,
			monoShort, monoWide, monoLags},
	},
}

// list is the level's candidate cascades for a coding mode.
func (l levelSets) list(mono bool) [][]int {
	if mono {
		return l.mono
	}
	return l.stereo
}

// EncoderVersion is the encoder's algorithm revision for cache keys
// (ADR-0004): compressed bytes for the same input must never change without a
// bump. The level is part of it because it changes the bytes while leaving the
// decoded samples untouched.
func EncoderVersion(level int) string { return fmt.Sprintf("wvenc-4-l%d", level) }

// EncoderOptions configures the encoder. A nil pointer selects
// DefaultEncoderLevel; a non-nil one uses Level literally.
type EncoderOptions struct {
	// Level is the compression level, LevelFast through LevelVeryHigh.
	Level int
}

// Encoder encodes integer PCM into WavPack blocks. It implements
// codec.Encoder: one input chunk becomes exactly one block packet, header
// included, which is the same unit the demuxer delivers, so a packet is
// interchangeable between the two.
type Encoder struct {
	fmt   audio.Format
	cfg   Config
	level int
	sets  levelSets

	pos      int64 // frames encoded so far, and so the next block's index
	finished bool

	// Cascade and entropy state carry across blocks. Each block stores the
	// state it starts from, quantized to what the stored form decodes to, so
	// blocks stay independently decodable while the encoder avoids paying the
	// adaptation transient more than once. Every candidate is advanced on
	// every block, chosen or not, so whichever one wins the next block starts
	// from the state its own metadata will describe.
	//
	// One set per coding mode, kept side by side rather than rebuilt when the
	// mode changes: the two are not interchangeable (a mono block codes one
	// channel with the mono term list), but a stream that flips modes flips
	// back, and a single set would pay the adaptation transient on both
	// crossings. A digitally silent stereo block is a false-stereo block, so
	// an inter-track gap or a fade to nothing is enough to trigger one.
	state   [2]modeState
	cur     *modeState
	srateIx uint32

	bw        bitWriter
	cw        bitWriter
	xw        bitWriter
	int32Info [4]byte
	samps     []int32
	trial     []int32
	best      []int32
	meta      []byte
	block     []byte
}

// modeState is everything the encoder carries across blocks of one coding
// mode: the candidate cascades and the entropy coder's medians.
type modeState struct {
	casc []cascade
	w    wordEncoder
}

// cascade is one candidate decorrelation chain: its passes, and the metadata
// bytes describing the state it starts the current block from.
type cascade struct {
	passes [maxTerms]decorrPass
	nterm  int
	meta   []byte
	buf    []byte // renderMeta's scratch, kept so a block does not allocate
	cross  bool   // the chain has a cross-channel pass, which the flags announce
}

// NewEncoder returns an Encoder for integer PCM in format f. A nil opts
// selects DefaultEncoderLevel.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	level := DefaultEncoderLevel
	if opts != nil {
		level = opts.Level
	}
	if level < LevelFast || level > LevelVeryHigh {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("wavpack: compression level %d outside %d..%d", level, LevelFast, LevelVeryHigh))
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Int {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("wavpack: cannot encode %v (WavPack holds integer PCM)", f))
	}
	cfg := Config{Rate: f.Rate, Channels: f.Channels, BitDepth: f.BitDepth, ValidBits: f.BitDepth}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if want := cfg.Format(); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("wavpack: cannot encode format %v (nearest encodable is %v)", f, want))
	}
	e := &Encoder{fmt: f, cfg: cfg, level: level, sets: levels[level], srateIx: 15}
	for i, r := range srateTable {
		if r == f.Rate {
			e.srateIx = uint32(i)
			break
		}
	}
	return e, nil
}

// InputFormat returns the exact PCM format Encode expects.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize returns the block length the framer must deliver.
func (e *Encoder) FrameSize() int { return BlockSamples }

// CodecConfig returns the stream shape a demuxer would otherwise derive from
// the first block. WavPack carries no out-of-band configuration of its own, so
// this is the canonical form ParseConfig reads, and it is what the muxer
// checks the track against.
func (e *Encoder) CodecConfig() []byte {
	b, err := e.cfg.MarshalBinary()
	if err != nil {
		// NewEncoder validated the fields; this cannot happen.
		panic(err)
	}
	return b
}

// Finish reports the stream total. WavPack has no encoder delay, no padding,
// and no buffered blocks, so nothing flushes.
func (e *Encoder) Finish(func(codec.Packet) error) (codec.Trailer, error) {
	if e.finished {
		return codec.Trailer{}, waxerr.New(waxerr.CodeInternal, "wavpack: Finish called twice")
	}
	e.finished = true
	return codec.Trailer{Samples: e.pos}, nil
}

// Encode encodes one chunk as one block. The emitted packet is borrowed: Data
// is valid only during the callback.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	switch {
	case e.finished:
		return waxerr.New(waxerr.CodeInternal, "wavpack: Encode after Finish")
	case src.Fmt != e.fmt:
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("wavpack: buffer format %v, encoder expects %v", src.Fmt, e.fmt))
	case src.N == 0:
		return nil
	case src.N > BlockSamples:
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("wavpack: %d-sample chunk exceeds the %d-sample block", src.N, BlockSamples))
	case e.pos+int64(src.N) > MaxSamples:
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("wavpack: stream exceeds the %d-sample maximum", int64(MaxSamples)))
	}

	n, ch := src.N, e.cfg.Channels
	if cap(e.samps) < n*ch {
		e.samps = make([]int32, n*ch)
	}
	samps := e.samps[:n*ch]
	for c := range ch {
		s := src.ChanI(c)
		for i := range n {
			samps[i*ch+c] = s[i]
		}
	}

	pkt := codec.Packet{Data: e.packBlock(samps, n), PTS: e.pos, Dur: int64(n), Sync: true}
	e.pos += int64(n)
	return emit(pkt)
}

// packBlock encodes n frames of interleaved samples into one whole block.
func (e *Encoder) packBlock(samps []int32, n int) []byte {
	flags := uint32(e.cfg.BitDepth/8 - 1)
	flags |= flagInitialBlock | flagFinalBlock
	flags |= e.srateIx << flagSrateLSB

	// A stereo block whose channels are identical codes one of them and says
	// so; the decoder duplicates it. Real dual-mono material is common enough
	// (and halves in size) to be worth the scan.
	coded, mono := samps, e.cfg.Channels == 1
	if mono {
		flags |= flagMono
	} else if falseStereo(samps) {
		mono = true
		flags |= flagFalseStereo
		for i := range n {
			samps[i] = samps[i*2]
		}
		coded = samps[:n]
	}
	e.prime(mono)

	// The inverse of the decoder's fixup, in its order: strip the low bits
	// every sample shares, then hold back whatever still will not fit the
	// coder's working range.
	shift, sent, mag := analyzeBits(coded, e.cfg.BitDepth)
	if shift != 0 {
		for i, v := range coded {
			coded[i] = v >> uint(shift)
		}
		flags |= uint32(shift) << flagShiftLSB
	}
	flags |= uint32(mag) << flagMagLSB
	if sent != 0 {
		flags |= flagInt32Data
		// The extension payload leads with its own CRC over the samples it
		// completes, so the writer starts four bytes in and they are filled
		// once the loop has run.
		e.xw.reset(append(e.xw.buf[:0], 0, 0, 0, 0))
		crc := uint32(0xffffffff)
		mask := int32(1)<<uint(sent) - 1
		for i, v := range coded {
			crc = crcExtension(crc, v)
			e.xw.putBits(uint32(v&mask), sent)
			coded[i] = v >> uint(sent)
		}
		e.xw.buf = e.xw.flush()
		binary.LittleEndian.PutUint32(e.xw.buf, crc)
		e.int32Info = [4]byte{byte(sent)}
	}

	// The block CRC covers the samples as the decoder holds them just before
	// that fixup, which is after it has undone the joint-stereo matrix.
	crc := uint32(0xffffffff)
	if mono {
		for _, v := range coded {
			crc = crcMono(crc, v)
		}
	} else {
		for i := 0; i < len(coded); i += 2 {
			crc = crcStereo(crc, coded[i], coded[i+1])
		}
		if jointStereoPays(coded) {
			flags |= flagJointStereo
			for i := 0; i < len(coded); i += 2 {
				d := coded[i] - coded[i+1]
				coded[i], coded[i+1] = d, coded[i+1]+(d>>1)
			}
		}
	}
	// Run every candidate cascade and keep the bitstream of the cheapest.
	// All of them are advanced whether they win or not, so each stays the
	// chain its own metadata describes.
	e.quantizeEntropy(mono)
	best := e.search(coded, mono)
	if e.cur.casc[best].cross {
		flags |= flagCrossDecorr
	}

	// The metadata states the cascade and entropy state this block starts
	// from, quantized to what the stored form decodes to; search rendered the
	// winner's half of it before running anything. The block is assembled with
	// the header's room reserved in front, so nothing has to be moved once the
	// length is known.
	if cap(e.block) < BlockHeaderLen {
		e.block = make([]byte, BlockHeaderLen, 1<<16)
	}
	body := e.block[:BlockHeaderLen]
	if e.srateIx == 15 {
		// Three bytes for a rate the field holds, four for one it does not.
		// The reference writes the wider form the same way, and its top bit
		// is reserved, which is what MaxRate bounds.
		r := e.cfg.Rate
		rate := []byte{byte(r), byte(r >> 8), byte(r >> 16)}
		if r >= 1<<24 {
			rate = append(rate, byte(r>>24)&0x7f)
		}
		body = appendMeta(body, idSampleRate, rate)
	}
	if sent != 0 {
		body = appendMeta(body, idInt32Info, e.int32Info[:])
	}
	body = append(body, e.cur.casc[best].meta...)
	body = appendMeta(body, idEntropyVars, e.meta)
	body = appendMeta(body, idWVBitstream, e.bw.buf)
	if sent != 0 {
		body = appendMeta(body, idWVXBitstream, e.xw.buf)
	}
	// The checksum covers everything ahead of it, the header included, so its
	// sub-block goes last and its value is filled once the header is written.
	// The slot is the last checksumBytes of the block by construction, so this
	// states where it is rather than walking the chain to find it again.
	var slot [checksumBytes]byte
	body = appendMeta(body, idBlockChecksum, slot[:])
	flags |= flagHasChecksum
	e.block = body
	e.header(body, n, flags, crc)
	writeChecksum(body, len(body)-checksumBytes, checksumBytes)
	return body
}

// prime selects the carried state for a coding mode, building it on first
// use. The mode's own state is picked up where its last block left it, so
// crossing into a false-stereo block and back costs the two blocks that cross
// and nothing after them.
//
// A mono block writes only the first channel's medians, so the second's must
// be what a fresh decoder holds: zero. Nothing in the mono path ever writes
// it, so holding the two modes apart is what keeps that true.
func (e *Encoder) prime(mono bool) {
	ix := 0
	if mono {
		ix = 1
	}
	e.cur = &e.state[ix]
	if e.cur.casc == nil {
		sets := e.sets.list(mono)
		e.cur.casc = make([]cascade, len(sets))
		for i, terms := range sets {
			c := &e.cur.casc[i]
			c.nterm = len(terms)
			for j, t := range terms {
				c.passes[j] = decorrPass{term: t, delta: packDelta}
				c.cross = c.cross || t < 0
			}
		}
	}
	// The carry between adjacent samples does not cross a block boundary.
	e.cur.w.holdingOne, e.cur.w.holdingZero = false, false
}

// falseStereo reports whether an interleaved stereo block's channels are
// identical.
func falseStereo(samps []int32) bool {
	for i := 0; i < len(samps); i += 2 {
		if samps[i] != samps[i+1] {
			return false
		}
	}
	return true
}

// jointStereoPays reports whether the mid/side matrix leaves less to code than
// the two channels do. Comparing summed magnitudes is the cheap proxy for
// comparing coded lengths, and it is the decision the cascade cannot make for
// itself: the cross-channel terms adapt a weight, which handles correlation
// but not the near-identical channels the matrix collapses outright.
// forceNoJoint suppresses the matrix, for the test that has to size a stream
// both ways to show the optimization pays. Nothing else writes it.
var forceNoJoint bool

func jointStereoPays(coded []int32) bool {
	if forceNoJoint {
		return false
	}
	var lr, ms uint64
	for i := 0; i < len(coded); i += 2 {
		l, r := coded[i], coded[i+1]
		d := l - r
		lr += uint64(log2u(magOf(l))) + uint64(log2u(magOf(r)))
		ms += uint64(log2u(magOf(d))) + uint64(log2u(magOf(r+(d>>1))))
	}
	return ms < lr
}

// analyzeBits reports the low bits every sample of a block shares (a source
// narrower than its container, which the block's shift field restores), how
// many more have to ride in the extension stream to bring the magnitude inside
// the coder's working range, and how wide the samples the coder then sees are.
//
// That last one is the header's magnitude field, and it is a fact about the
// block's data rather than about the stream's declared depth. It has to be:
// the reference decoder reads it to decide the samples fit its narrow decode
// path, and rejects a block claiming much past this cap outright -- a stream
// declaring the nominal 32 was refused by `wvunpack -v` while both our decoder
// and ffmpeg's, which ignore the field, read it back perfectly. That is the
// shape of bug only the reference tool can find.
func analyzeBits(coded []int32, depth int) (shift, sent, mag int) {
	var ordata, magdata uint32
	for _, v := range coded {
		ordata |= uint32(v)
		magdata |= magOf(v)
	}
	if ordata != 0 {
		// All-zero samples say nothing about redundant bits, and a shift as
		// wide as the depth would leave the stream claiming zero valid bits.
		shift = min(bits.TrailingZeros32(ordata), depth-1)
	}
	mag = bits.Len32(magdata >> uint(shift))
	if mag > maxCodedMagnitude {
		sent = mag - maxCodedMagnitude
		mag = maxCodedMagnitude
	}
	return shift, sent, mag
}

// writtenStreamVersion is the version this encoder's blocks state, which is
// the reference's own current output version. It is a separate constant from
// the decoder's MaxStreamVersion on purpose: what we can read and what we
// claim to have written are different facts, and raising the first must not
// silently change the second.
const writtenStreamVersion = 0x410

// header fills the reserved 32 bytes at the front of a finished block, which
// is where its own length becomes writable.
func (e *Encoder) header(block []byte, n int, flags, crc uint32) {
	h := block[:BlockHeaderLen]
	copy(h[:4], "wvpk")
	// The size field counts from the version, eight bytes in.
	binary.LittleEndian.PutUint32(h[4:], uint32(len(block)-8))
	binary.LittleEndian.PutUint16(h[8:], writtenStreamVersion)
	h[10] = byte(e.pos >> 32)
	// The length is not known until the stream ends, so every block carries
	// the escape; the muxer patches the first block's field once it is.
	h[11] = 0
	binary.LittleEndian.PutUint32(h[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(h[16:], uint32(e.pos))
	binary.LittleEndian.PutUint32(h[20:], uint32(n))
	binary.LittleEndian.PutUint32(h[24:], flags)
	binary.LittleEndian.PutUint32(h[28:], crc)
}

// appendMeta appends one metadata sub-block: the id, its length in 16-bit
// words, and the payload padded to a whole word.
func appendMeta(dst []byte, id byte, payload []byte) []byte {
	n := len(payload)
	words := (n + 1) / 2
	if n&1 != 0 {
		id |= idOddSize
	}
	if words > 0xff {
		id |= idLarge
	}
	dst = append(dst, id, byte(words))
	if id&idLarge != 0 {
		dst = append(dst, byte(words>>8), byte(words>>16))
	}
	dst = append(dst, payload...)
	if n&1 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

// search runs every candidate cascade over the block and leaves the winner's
// residuals in e.best, returning its index. Every candidate is advanced, so
// their carried state stays interchangeable from one block to the next.
//
// The score is the reference's own proxy: the summed log magnitude of the
// residuals, which tracks coded length closely enough to choose between chains
// and costs a table lookup a sample instead of an entropy pass. The metadata
// each chain would write is scored with them, since a sixteen-pass chain
// carries a hundred bytes a two-pass chain does not.
func (e *Encoder) search(coded []int32, mono bool) int {
	if cap(e.trial) < len(coded) {
		e.trial = make([]int32, len(coded))
		e.best = make([]int32, len(coded))
	}
	n := len(coded)
	if !mono {
		n /= 2
	}
	start := e.cur.w
	best, bestCost, bestState := 0, 0, start
	for i := range e.cur.casc {
		c := &e.cur.casc[i]
		// Quantizing first is not an optimization: the decoder starts from
		// what the block stores, so the chain has to run from that too.
		c.renderMeta(mono)
		trial := e.trial[:len(coded)]
		copy(trial, coded)
		for j := range c.passes[:c.nterm] {
			if mono {
				packMonoPass(&c.passes[j], trial)
			} else {
				packStereoPass(&c.passes[j], trial)
			}
		}
		e.cur.w = start
		e.cw.reset(e.cw.buf[:0])
		e.cur.w.putWords(&e.cw, trial, n, mono)
		e.cw.buf = e.cw.flush()
		cost := len(e.cw.buf) + len(c.meta)
		if i == 0 || cost < bestCost {
			best, bestCost, bestState = i, cost, e.cur.w
			e.best, e.trial = e.trial, e.best
			e.bw.buf, e.cw.buf = e.cw.buf, e.bw.buf
		}
	}
	e.cur.w = bestState
	return best
}

// renderMeta quantizes the cascade's carried state to what its stored form
// decodes to and renders the three sub-blocks describing it. The encoder then
// runs from the quantized values, which is what keeps it in step with a
// decoder that has only ever seen the stored ones.
func (c *cascade) renderMeta(mono bool) {
	c.meta = c.meta[:0]
	buf := c.buf[:0]
	for i := range c.passes[:c.nterm] {
		dpp := &c.passes[i]
		buf = append(buf, byte(dpp.term+5)|byte(dpp.delta)<<5)
	}
	c.meta = appendMeta(c.meta, idDecorrTerms, buf)

	buf = buf[:0]
	for i := range c.passes[:c.nterm] {
		dpp := &c.passes[i]
		w := storeWeight(dpp.weightA)
		dpp.weightA = restoreWeight(w)
		buf = append(buf, byte(w))
		if !mono {
			w = storeWeight(dpp.weightB)
			dpp.weightB = restoreWeight(w)
			buf = append(buf, byte(w))
		}
	}
	c.meta = appendMeta(c.meta, idDecorrWeights, buf)

	buf = buf[:0]
	put := func(v *int32) {
		q, log := quantizeSample(*v)
		*v = q
		buf = binary.LittleEndian.AppendUint16(buf, uint16(int16(log)))
	}
	for i := range c.passes[:c.nterm] {
		dpp := &c.passes[i]
		switch {
		case dpp.term > maxTerm:
			put(&dpp.samplesA[0])
			put(&dpp.samplesA[1])
			if !mono {
				put(&dpp.samplesB[0])
				put(&dpp.samplesB[1])
			}
		case dpp.term < 0:
			put(&dpp.samplesA[0])
			put(&dpp.samplesB[0])
		default:
			for m := range dpp.term {
				put(&dpp.samplesA[m])
				if !mono {
					put(&dpp.samplesB[m])
				}
			}
		}
	}
	c.meta = appendMeta(c.meta, idDecorrSamples, buf)
	c.buf = buf
}

// quantizeEntropy rounds the carried medians to what the block's stored log
// form decodes to and renders that sub-block's payload. It runs before the
// candidate search, not after it, because the search costs candidates by
// coding them for real: the medians it starts from have to be the ones the
// decoder will start from.
func (e *Encoder) quantizeEntropy(mono bool) {
	e.meta = e.meta[:0]
	chans := 2
	if mono {
		chans = 1
	}
	for ch := range chans {
		for m := range 3 {
			log := log2u(e.cur.w.c[ch].median[m])
			e.cur.w.c[ch].median[m] = uint32(exp2s(log))
			e.meta = binary.LittleEndian.AppendUint16(e.meta, uint16(log))
		}
	}
}
