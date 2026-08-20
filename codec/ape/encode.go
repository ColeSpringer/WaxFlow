package ape

import (
	"fmt"
	"hash/crc32"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// BlocksPerFrame is the frame length this encoder writes, and so
// Encoder.FrameSize. It is the format's own base length, which is what every
// level below extra high uses; the two deeper levels multiply it, and those
// are not written here.
//
// A frame is a large unit by the format's design (a second and two thirds at
// 44.1 kHz): it re-primes the coder and flushes the predictors, so it is also
// the seek granularity and the pre-roll a seek costs.
const BlocksPerFrame = 73728

// MaxEncodeLevel is the deepest level this encoder writes. Extra high and
// insane decode here but are not produced: their cascades run 256 and 1552
// filter taps a sample in both directions, so a file written at either decodes
// at 62x and 15x realtime against the 100x floor, and buys a fraction of a
// percent for it. Every level still decodes, so a file from another encoder
// plays as it always did.
const MaxEncodeLevel = LevelHigh

// DefaultEncoderLevel is the level a nil EncoderOptions selects: the
// reference tool's own default.
const DefaultEncoderLevel = LevelNormal

// writtenFileVersion is the stream version this encoder's files state. It is a
// separate constant from the decoder's MaxFileVersion on purpose: what we can
// read and what we claim to have written are different facts, and raising the
// first must not silently change the second.
const writtenFileVersion = 3990

// EncoderVersion is the encoder's algorithm revision for cache keys
// (ADR-0004): compressed bytes for the same input must never change without a
// bump. The level is part of it because it changes the bytes while leaving the
// decoded samples untouched.
func EncoderVersion(level int) string { return fmt.Sprintf("apeenc-1-l%d", level) }

// EncoderOptions configures the encoder. A nil pointer selects
// DefaultEncoderLevel; a non-nil one uses Level literally.
type EncoderOptions struct {
	// Level is the compression level, LevelFast through MaxEncodeLevel.
	Level int
}

// Encoder encodes integer PCM into APE frames. It implements codec.Encoder:
// one frame becomes one packet, carrying the same small frame header the
// demuxer prepends, so a packet is the same object on both sides.
//
// Frames are accumulated rather than taken one chunk at a time. A frame states
// neither its length nor its block count, so a reader takes both from the file
// header: every frame but the last must be exactly BlocksPerFrame long, and a
// short chunk in the middle of a stream (which the framer delivers at a
// splice) would otherwise write one that no reader could size.
type Encoder struct {
	fmt audio.Format
	cfg Config

	rc    *rangeEncoder
	state [maxChannels]entropyState
	pred  [maxChannels]*predictor

	// samps holds the frame being filled, interleaved; pend is how much of it
	// is filled.
	samps []int32
	pend  int
	// pack is the rolling scratch the CRC runs over: the frame's samples in
	// the byte form the source file had them, a chunk at a time.
	pack []byte

	pos      int64 // blocks encoded so far, and so the next frame's PTS
	finished bool
}

// NewEncoder returns an Encoder for integer PCM in format f. A nil opts
// selects DefaultEncoderLevel.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	level := DefaultEncoderLevel
	if opts != nil {
		level = opts.Level
	}
	switch {
	case level < LevelFast || level%1000 != 0:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("ape: compression level %d is not one of %d..%d", level, LevelFast, MaxEncodeLevel))
	case level > MaxEncodeLevel && level <= LevelInsane:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("ape: compression level %d decodes but is not written (levels above %d cost more in filter taps than they save)",
				level, MaxEncodeLevel))
	case level > LevelInsane:
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("ape: compression level %d is not one of %d..%d", level, LevelFast, MaxEncodeLevel))
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Int {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ape: cannot encode %v (Monkey's Audio holds integer PCM)", f))
	}
	cfg := Config{
		FileVersion:      writtenFileVersion,
		CompressionLevel: level,
		BlocksPerFrame:   BlocksPerFrame,
		BitsPerSample:    f.BitDepth,
		Channels:         f.Channels,
		Rate:             f.Rate,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if want := cfg.Format(); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ape: cannot encode format %v (nearest encodable is %v)", f, want))
	}
	e := &Encoder{
		fmt:   f,
		cfg:   cfg,
		rc:    newRangeEncoder(),
		samps: make([]int32, BlocksPerFrame*cfg.Channels),
		// The pack scratch holds one block past its flush threshold, so the
		// append that crosses it never reallocates.
		pack: make([]byte, 0, packChunk+8),
	}
	for c := range cfg.Channels {
		e.pred[c] = newPredictor(cfg)
	}
	return e, nil
}

// InputFormat returns the exact PCM format Encode expects.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize returns the frame length the framer should deliver. Encode
// accepts any length, since a splice can shorten a chunk anywhere in a
// stream; asking for the frame length is what keeps the common case a single
// copy.
func (e *Encoder) FrameSize() int { return BlocksPerFrame }

// CodecConfig returns the stream shape a demuxer would otherwise take from the
// file header. APE rides in no container that carries an out-of-band config
// blob, so this is our own canonical form, and it is what the muxer reads the
// header's fields out of.
func (e *Encoder) CodecConfig() []byte {
	b, err := e.cfg.MarshalBinary()
	if err != nil {
		// NewEncoder validated the fields; this cannot happen.
		panic(err)
	}
	return b
}

// Encode buffers a chunk and emits every whole frame it completes. The
// emitted packet is borrowed: Data is valid only during the callback.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	switch {
	case e.finished:
		return waxerr.New(waxerr.CodeInternal, "ape: Encode after Finish")
	case src.Fmt != e.fmt:
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ape: buffer format %v, encoder expects %v", src.Fmt, e.fmt))
	}
	ch := e.cfg.Channels
	for off := 0; off < src.N; {
		n := min(src.N-off, BlocksPerFrame-e.pend)
		for c := range ch {
			dst := e.samps[e.pend*ch+c:]
			for i, v := range src.ChanI(c)[off : off+n] {
				dst[i*ch] = v
			}
		}
		e.pend += n
		off += n
		if e.pend == BlocksPerFrame {
			if err := e.emitFrame(emit); err != nil {
				return err
			}
		}
	}
	return nil
}

// Finish emits the partial frame and reports the stream total. APE has no
// encoder delay and no padding: a frame is complete on its own.
func (e *Encoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	if e.finished {
		return codec.Trailer{}, waxerr.New(waxerr.CodeInternal, "ape: Finish called twice")
	}
	e.finished = true
	if e.pend > 0 {
		if err := e.emitFrame(emit); err != nil {
			return codec.Trailer{}, err
		}
	}
	return codec.Trailer{Samples: e.pos}, nil
}

// emitFrame codes what is buffered and hands it out as one packet.
func (e *Encoder) emitFrame(emit func(codec.Packet) error) error {
	n := e.pend
	e.pend = 0
	data, err := e.packFrame(e.samps[:n*e.cfg.Channels], n)
	if err != nil {
		return err
	}
	pkt := codec.Packet{Data: data, PTS: e.pos, Dur: int64(n), Sync: true}
	e.pos += int64(n)
	return emit(pkt)
}

// packFrame codes n blocks into one whole packet: the frame header, the
// samples' CRC, whatever special code stands in for coding them, and the
// range-coded residuals.
func (e *Encoder) packFrame(samps []int32, n int) ([]byte, error) {
	crc, special, err := e.prepare(samps)
	if err != nil {
		return nil, err
	}

	w := &e.rc.w
	w.reset(FrameHeaderLen)
	// The CRC's top bit is the special-code flag, which is why the encoder
	// dropped its low bit to make room.
	if special != 0 {
		crc |= 1 << 31
	}
	w.putUint32(crc)
	if special != 0 {
		w.putUint32(special)
	}

	for c := range e.cfg.Channels {
		e.pred[c].flush()
		e.state[c].flush()
	}
	e.rc.start()
	e.encodeBlocks(samps, n, special)
	e.rc.finalize()

	// The skip is zero by construction: a frame is written into its own words
	// here, and the muxer repacks the run when it lays the frames down. See
	// frameWriter.
	PutFrameHeader(w.buf, n, 0, w.n)
	return w.buf, nil
}

// encodeBlocks codes the frame's residuals, or nothing where a special code
// stands for the whole shape. It is decodeBlocks read the other way, and the
// branches have to line up with it exactly: which channel is coded first, and
// what each predictor's second input is.
func (e *Encoder) encodeBlocks(samps []int32, n int, special uint32) {
	switch {
	case e.cfg.Channels == 1:
		if special&specialMonoSilence != 0 {
			return
		}
		for i := range n {
			e.rc.encodeValue(e.pred[0].compress(samps[i], 0), &e.state[0])
		}
	case special&specialLeftSilence != 0 && special&specialRightSilence != 0:
		// Both channels are digitally silent; the frame is its header.
	case special&specialPseudoStereo != 0:
		// The difference channel is flat, so only the mid channel is coded.
		for i := range n {
			e.rc.encodeValue(e.pred[0].compress(samps[2*i], 0), &e.state[0])
		}
	default:
		// The difference channel goes first because the mid channel's
		// predictor takes it as its second input, and the difference
		// channel's takes the mid channel of the block before.
		lastX := int32(0)
		for i := range n {
			x, y := samps[2*i], samps[2*i+1]
			e.rc.encodeValue(e.pred[1].compress(y, lastX), &e.state[1])
			e.rc.encodeValue(e.pred[0].compress(x, y), &e.state[0])
			lastX = x
		}
	}
}

// prepare walks the frame's samples once for the CRC and the special codes,
// then applies the mid/side matrix in place, leaving samps as the (x, y) pair
// the predictors code. It is decode.go's unprepare inverted, and the CRC has
// to be taken here rather than after: it covers the samples in the byte form
// the source file had them, which is what the decoder rebuilds and checks.
func (e *Encoder) prepare(samps []int32) (crc, special uint32, err error) {
	stereo := e.cfg.Channels == 2
	lo, hi := sampleRange(e.cfg.BitsPerSample)
	pack := e.pack[:0]
	// The silence tests are "is every sample zero", so the OR of the raw
	// values answers them without an absolute value per sample.
	var or0, or1 uint32
	identical := true

	for i := 0; i < len(samps); i += e.cfg.Channels {
		v0, v1 := samps[i], int32(0)
		if stereo {
			v1 = samps[i+1]
		}
		if v0 < lo || v0 > hi || v1 < lo || v1 > hi {
			bad := v0
			if v0 >= lo && v0 <= hi {
				bad = v1
			}
			return 0, 0, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("ape: sample %d outside the %d-bit range %d..%d",
					bad, e.cfg.BitsPerSample, lo, hi))
		}
		or0 |= uint32(v0)
		or1 |= uint32(v1)
		identical = identical && v0 == v1

		switch e.cfg.BitsPerSample {
		case 8:
			// 8-bit rides unsigned in every source format the format's own
			// encoder reads, and the CRC is over those bytes; the coded value
			// stays signed.
			pack = append(pack, byte(v0+128))
			if stereo {
				pack = append(pack, byte(v1+128))
			}
		case 16:
			pack = append(pack, byte(v0), byte(v0>>8))
			if stereo {
				pack = append(pack, byte(v1), byte(v1>>8))
			}
		default:
			t0 := pack24(v0)
			pack = append(pack, byte(t0), byte(t0>>8), byte(t0>>16))
			if stereo {
				t1 := pack24(v1)
				pack = append(pack, byte(t1), byte(t1>>8), byte(t1>>16))
			}
		}
		if len(pack) >= packChunk {
			crc = crc32.Update(crc, crc32.IEEETable, pack)
			pack = pack[:0]
		}
	}
	crc = crc32.Update(crc, crc32.IEEETable, pack)
	e.pack = pack[:0]
	// Drop the low bit to make room for the special-code flag in the top one.
	crc >>= 1

	// Special codes are a 16-bit facility in the reference encoder, and this
	// follows it: nothing in circulation writes one at another depth, and the
	// silence they stand for costs almost nothing to code anyway.
	if e.cfg.BitsPerSample == 16 {
		switch {
		case !stereo:
			if or0 == 0 {
				special |= specialMonoSilence
			}
		default:
			// The reference names the flags for the channels in the order it
			// reads them, which is the second channel first: a frame whose
			// channel 0 is silent carries the RIGHT bit. Decoders act only
			// when both are set, so nothing about playback can tell this
			// apart from the transposition; what pins it is
			// TestAPEEncodeMatchesTheReferenceOnSpecialFrames, which compares
			// the coded bytes against the reference's on exactly these
			// shapes.
			if or1 == 0 {
				special |= specialLeftSilence
			}
			if or0 == 0 {
				special |= specialRightSilence
			}
			if identical {
				special |= specialPseudoStereo
			}
		}
	}

	if stereo {
		for i := 0; i < len(samps); i += 2 {
			y := samps[i+1] - samps[i]
			samps[i] += y / 2
			samps[i+1] = y
		}
	}
	return crc, special, nil
}

// sampleRange is the inclusive range a sample of the given width may hold.
// The chain quantizes to the stream's depth, so anything outside it is a
// caller's bug rather than audio; refusing beats coding a sample the decoder
// would hand back as a different one.
func sampleRange(bits int) (lo, hi int32) {
	hi = 1<<(bits-1) - 1
	return -hi - 1, hi
}
