package pcm

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder unpacks interleaved wire PCM into planar buffers. It implements
// codec.Decoder.
type Decoder struct {
	cfg Config
	fmt audio.Format
	buf *audio.Buffer // reusable scratch, borrowed by emit callbacks
}

// NewDecoder returns a Decoder for the given wire configuration and track
// format. The track format must be what cfg.PCMFormat produces; demuxers
// construct both from the same header, so a mismatch is a wiring bug.
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := cfg.PCMFormat(f.Rate, f.Channels, f.Layout); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("pcm: track format %v does not match wire config (want %v)", f, want))
	}
	return &Decoder{cfg: cfg, fmt: f}, nil
}

// Decode unpacks one packet of interleaved frames and emits planar
// buffers of at most audio.StandardChunk frames. Emitted buffers are
// borrowed: valid only during the callback. A packet that does not hold a
// whole number of frames is malformed.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	fb := d.cfg.BytesPerFrame(d.fmt.Channels)
	if len(pkt)%fb != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("pcm: packet of %d bytes is not a whole number of %d-byte frames", len(pkt), fb))
	}
	for len(pkt) > 0 {
		frames := len(pkt) / fb
		if frames > audio.StandardChunk {
			frames = audio.StandardChunk
		}
		if d.buf == nil || d.buf.Cap() < frames || d.buf.Fmt != d.fmt {
			audio.Put(d.buf)
			d.buf = audio.Get(d.fmt, audio.StandardChunk)
		}
		d.buf.N = frames
		d.unpack(pkt[:frames*fb])
		if err := emit(d.buf); err != nil {
			return err
		}
		pkt = pkt[frames*fb:]
	}
	return nil
}

// Drain is a no-op: PCM has no decoder latency.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset is a no-op: PCM decoding is stateless across packets.
func (d *Decoder) Reset() {}

// Release returns the scratch buffer to the pool (codec.Releaser). The
// decoder must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}

// unpack de-interleaves data (a whole number of frames) into d.buf,
// which already has N set.
func (d *Decoder) unpack(data []byte) {
	ch := d.fmt.Channels
	step := d.cfg.Bits / 8 * ch
	shift := d.cfg.shift()
	for c := 0; c < ch; c++ {
		off := c * d.cfg.Bits / 8
		switch {
		case d.cfg.Encoding == Float && d.cfg.Bits == 32:
			dst := d.buf.ChanF(c)
			for i := range dst {
				dst[i] = math.Float32frombits(d.u32(data[off+i*step:]))
			}
		case d.cfg.Encoding == Float:
			dst := d.buf.ChanF(c)
			for i := range dst {
				dst[i] = float32(math.Float64frombits(d.u64(data[off+i*step:])))
			}
		case d.cfg.Encoding == UnsignedInt:
			dst := d.buf.ChanI(c)
			for i := range dst {
				dst[i] = int32(int(data[off+i*step])-128) >> shift
			}
		case d.cfg.Bits == 8:
			dst := d.buf.ChanI(c)
			for i := range dst {
				dst[i] = int32(int8(data[off+i*step])) >> shift
			}
		case d.cfg.Bits == 16:
			dst := d.buf.ChanI(c)
			for i := range dst {
				dst[i] = int32(int16(d.u16(data[off+i*step:]))) >> shift
			}
		case d.cfg.Bits == 24:
			dst := d.buf.ChanI(c)
			for i := range dst {
				dst[i] = d.s24(data[off+i*step:]) >> shift
			}
		default: // 32
			dst := d.buf.ChanI(c)
			for i := range dst {
				dst[i] = int32(d.u32(data[off+i*step:])) >> shift
			}
		}
	}
}

func (d *Decoder) u16(b []byte) uint16 {
	if d.cfg.BigEndian {
		return binary.BigEndian.Uint16(b)
	}
	return binary.LittleEndian.Uint16(b)
}

func (d *Decoder) u32(b []byte) uint32 {
	if d.cfg.BigEndian {
		return binary.BigEndian.Uint32(b)
	}
	return binary.LittleEndian.Uint32(b)
}

func (d *Decoder) u64(b []byte) uint64 {
	if d.cfg.BigEndian {
		return binary.BigEndian.Uint64(b)
	}
	return binary.LittleEndian.Uint64(b)
}

// s24 assembles a sign-extended 24-bit sample.
func (d *Decoder) s24(b []byte) int32 {
	var v uint32
	if d.cfg.BigEndian {
		v = uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	} else {
		v = uint32(b[2])<<16 | uint32(b[1])<<8 | uint32(b[0])
	}
	return int32(v<<8) >> 8
}
