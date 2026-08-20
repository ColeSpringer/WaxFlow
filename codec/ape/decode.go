package ape

import (
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder decodes APE frames into planar int buffers. One packet is one frame,
// every frame is independently decodable, so Drain and Reset are no-ops.
type Decoder struct {
	cfg Config
	fmt audio.Format

	rd    *rangeDecoder
	state [maxChannels]entropyState
	pred  [maxChannels]*predictor

	// samps holds one whole frame, interleaved. A frame is the format's unit
	// of decoding and the deep levels make it long (a quarter of a minute at
	// insane), so it is decoded in full, checked against its CRC, and only
	// then handed out in ordinary chunks.
	samps []int32
	// pack is the rolling scratch the CRC runs over: the frame's samples in
	// the byte form the encoder hashed, a chunk at a time.
	pack []byte
	buf  *audio.Buffer

	// interim records that this stream needs the wider accumulators; see
	// predictor.interim. It sticks once set, as it does in the reference.
	interim bool
}

// packChunk is the CRC scratch's size in bytes. It is a multiple of 12 so
// every sample width divides it evenly at either channel count.
const packChunk = 12 << 8

// NewDecoder returns a Decoder for a stream. The track format must be what
// Config.Format produces; the demuxer builds both from the same header, so a
// mismatch is a wiring bug.
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := cfg.Format(); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"ape: track format "+f.String()+" does not match the stream ("+want.String()+")")
	}
	// The pack scratch holds one block past its flush threshold, so the
	// append that crosses it never reallocates.
	d := &Decoder{cfg: cfg, fmt: f, rd: newRangeDecoder(cfg.FileVersion), pack: make([]byte, 0, packChunk+8)}
	for c := range cfg.Channels {
		d.pred[c] = newPredictor(cfg)
	}
	return d, nil
}

// Decode decodes one frame and emits it.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	blocks, skip, data, err := ParseFrameHeader(pkt)
	if err != nil {
		return err
	}
	if blocks > d.cfg.BlocksPerFrame {
		return malformed("frame of %d blocks exceeds the stream's %d", blocks, d.cfg.BlocksPerFrame)
	}
	need := blocks * d.cfg.Channels
	if cap(d.samps) < need {
		d.samps = make([]int32, need)
	}
	samps := d.samps[:need]

	if err := d.frame(blocks, skip, data, samps); err != nil {
		// A 24-bit file encoded between Monkey's Audio 5.01 and 8.51 decodes
		// with wider accumulators, and the only sign of it is a frame that
		// decodes cleanly and then fails its CRC. Anything else (a collapsed
		// coder, a bitstream that ran out) is damage the retry cannot
		// explain, so it does not pay for one.
		if d.interim || d.cfg.BitsPerSample != 24 || !errors.Is(err, errFrameCRC) {
			return err
		}
		d.setInterim(true)
		if retry := d.frame(blocks, skip, data, samps); retry != nil {
			// The stream is damaged rather than interim. Unlatch, or one bad
			// frame would decode every later one with the wrong arithmetic.
			d.setInterim(false)
			return err
		}
	}
	return d.emit(blocks, samps, emit)
}

// frame decodes one frame's blocks into samps and checks the frame's CRC.
func (d *Decoder) frame(blocks, skip int, data []byte, samps []int32) error {
	d.rd.br.reset(data, skip)

	// The frame header is a CRC with the special-code flag in its top bit.
	// Every version this package covers uses that flag, so there is no
	// version test around it.
	storedCRC := d.rd.br.bits(32)
	special := uint32(0)
	if storedCRC&0x80000000 != 0 {
		special = d.rd.br.bits(32)
	}
	storedCRC &= 0x7fffffff

	for c := range d.cfg.Channels {
		d.pred[c].flush()
		d.state[c].flush()
	}
	d.rd.start()

	if err := d.decodeBlocks(blocks, special, samps); err != nil {
		return err
	}
	crc, err := d.unprepare(samps)
	if err != nil {
		return err
	}
	if crc != storedCRC {
		return fmt.Errorf("%w (%#08x, want %#08x)", errFrameCRC, crc, storedCRC)
	}
	return nil
}

// errFrameCRC is the frame's own integrity failure, told apart from the other
// ways a frame can fail because it is the only one the interim accumulators
// can explain.
var errFrameCRC = malformed("frame fails its CRC")

// decodeBlocks decodes the frame's residuals into samps as the encoder's
// (x, y) pair, or fills in the shape a special code stands for.
func (d *Decoder) decodeBlocks(n int, special uint32, samps []int32) error {
	switch {
	case d.cfg.Channels == 1:
		if special&specialMonoSilence != 0 {
			clear(samps)
			return nil
		}
		for i := range samps {
			v, err := d.rd.decodeValue(&d.state[0])
			if err != nil {
				return err
			}
			samps[i] = d.pred[0].decompress(v, 0)
		}
	case special&specialLeftSilence != 0 && special&specialRightSilence != 0:
		clear(samps)
	case special&specialPseudoStereo != 0:
		// One coded channel, and the difference channel is flat: the two
		// output channels come out identical.
		for i := range n {
			v, err := d.rd.decodeValue(&d.state[0])
			if err != nil {
				return err
			}
			samps[2*i] = d.pred[0].decompress(v, 0)
			samps[2*i+1] = 0
		}
	default:
		// The difference channel is decoded first because the mid channel's
		// predictor takes it as its second input, and the difference
		// channel's takes the mid channel of the block before.
		lastX := int32(0)
		for i := range n {
			ny, err := d.rd.decodeValue(&d.state[1])
			if err != nil {
				return err
			}
			nx, err := d.rd.decodeValue(&d.state[0])
			if err != nil {
				return err
			}
			y := d.pred[1].decompress(ny, lastX)
			x := d.pred[0].decompress(nx, y)
			lastX = x
			samps[2*i], samps[2*i+1] = x, y
		}
	}
	return nil
}

// unprepare undoes the encoder's mid/side matrix in place and returns the
// frame's CRC, computed over the samples in the byte form the encoder hashed
// (the source file's own, which is unsigned for 8-bit and little-endian
// otherwise). Returning the CRC from the same pass is what keeps the frame's
// bytes from having to exist twice.
func (d *Decoder) unprepare(samps []int32) (uint32, error) {
	crc := uint32(0)
	pack := d.pack[:0]
	stereo := d.cfg.Channels == 2
	for i := 0; i < len(samps); i += d.cfg.Channels {
		v0, v1 := samps[i], int32(0)
		if stereo {
			// samps[i] is the mid channel and samps[i+1] the difference,
			// over the pair in the order the source file had them. A sample
			// that lands outside the stream's width is truncated rather than
			// refused: only a damaged stream produces one, and the frame's
			// CRC is what reports it.
			y := samps[i+1]
			v0 -= y / 2
			v1 = v0 + y
		}
		switch d.cfg.BitsPerSample {
		case 8:
			// The coded value is already the signed sample; 8-bit rides as
			// unsigned bytes in every source format the encoder reads, and
			// the CRC is over those, so the bias belongs to the CRC alone.
			pack = append(pack, byte(v0+128))
			samps[i] = int32(int8(v0))
			if stereo {
				pack = append(pack, byte(v1+128))
				samps[i+1] = int32(int8(v1))
			}
		case 16:
			if stereo && (v0 < -32768 || v0 > 32767 || v1 < -32768 || v1 > 32767) {
				// The reference refuses the frame here rather than wrapping.
				// A stream the encoder produced never gets here, since both
				// channels came out of 16-bit samples.
				return 0, malformed("16-bit sample %d outside its range", max(v0, v1))
			}
			s0, s1 := int16(v0), int16(v1)
			pack = append(pack, byte(s0), byte(s0>>8))
			samps[i] = int32(s0)
			if stereo {
				pack = append(pack, byte(s1), byte(s1>>8))
				samps[i+1] = int32(s1)
			}
		default:
			t0 := pack24(v0)
			pack = append(pack, byte(t0), byte(t0>>8), byte(t0>>16))
			samps[i] = int32(t0<<8) >> 8
			if stereo {
				t1 := pack24(v1)
				pack = append(pack, byte(t1), byte(t1>>8), byte(t1>>16))
				samps[i+1] = int32(t1<<8) >> 8
			}
		}
		if len(pack) >= packChunk {
			crc = crc32.Update(crc, crc32.IEEETable, pack)
			pack = pack[:0]
		}
	}
	crc = crc32.Update(crc, crc32.IEEETable, pack)
	d.pack = pack[:0]
	// The encoder drops the CRC's low bit to make room for the special-code
	// flag in the top one.
	return crc >> 1, nil
}

// pack24 is the reference's 24-bit byte form. For a sample inside the range it
// is the low 24 bits either way; outside it the two differ, and following the
// reference is what keeps the same frames failing their CRC in both.
func pack24(v int32) uint32 {
	if v < 0 {
		return uint32(v+0x800000) | 0x800000
	}
	return uint32(v)
}

// emit hands the decoded frame out in ordinary chunks rather than as one
// buffer: a frame runs to a quarter of a minute at the deepest level, and
// nothing downstream wants that in one piece.
func (d *Decoder) emit(blocks int, samps []int32, emit func(*audio.Buffer) error) error {
	if d.buf == nil || d.buf.Fmt != d.fmt {
		audio.Put(d.buf)
		d.buf = audio.Get(d.fmt, audio.StandardChunk)
	}
	ch := d.cfg.Channels
	for off := 0; off < blocks; {
		n := min(blocks-off, d.buf.Cap())
		d.buf.N = n
		for c := range ch {
			dst := d.buf.ChanI(c)
			src := samps[(off*ch)+c:]
			for i := range dst {
				dst[i] = src[i*ch]
			}
		}
		if err := emit(d.buf); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// setInterim switches every predictor between the ordinary accumulators and
// the interim ones.
func (d *Decoder) setInterim(on bool) {
	d.interim = on
	for c := range d.cfg.Channels {
		d.pred[c].setInterim(on)
	}
}

// Drain is a no-op: APE has no decoder latency.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset is a no-op: every frame re-primes the coder and flushes the
// predictors, so each is a sync point on its own.
func (d *Decoder) Reset() {}

// Release returns the output buffer to the pool (codec.Releaser). The decoder
// must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
	d.samps = nil
}
