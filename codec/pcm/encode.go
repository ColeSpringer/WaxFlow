package pcm

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// Encoder packs planar buffers into interleaved wire PCM. It implements
// codec.Encoder. Integer input samples must be in range for the format's
// bit depth (the pipeline guarantees this); out-of-range values wrap.
type Encoder struct {
	cfg Config
	fmt audio.Format
	out []byte // reusable packet backing, borrowed by emit callbacks
	pos int64  // running sample count, feeds PTS and the Trailer
}

// NewEncoder returns an Encoder producing the given wire configuration
// from pipeline buffers in format f (which must be cfg.PCMFormat for the
// track's rate and layout).
func NewEncoder(cfg Config, f audio.Format) (*Encoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := cfg.PCMFormat(f.Rate, f.Channels, f.Layout); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("pcm: input format %v does not match wire config (want %v)", f, want))
	}
	return &Encoder{cfg: cfg, fmt: f}, nil
}

// InputFormat returns the exact PCM format Encode expects.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize returns 0: PCM accepts any chunk length.
func (e *Encoder) FrameSize() int { return 0 }

// CodecConfig returns the marshaled wire Config for Track.CodecConfig.
func (e *Encoder) CodecConfig() []byte {
	b, err := e.cfg.MarshalBinary()
	if err != nil {
		// NewEncoder validated the config; this cannot happen.
		panic(err)
	}
	return b
}

// Encode interleaves src into one packet. The packet is borrowed: Data is
// valid only during the callback.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("pcm: buffer format %v, encoder expects %v", src.Fmt, e.fmt))
	}
	if src.N == 0 {
		return nil
	}
	n := src.N * e.cfg.BytesPerFrame(e.fmt.Channels)
	if cap(e.out) < n {
		e.out = make([]byte, n)
	}
	e.out = e.out[:n]
	e.pack(src)
	pkt := codec.Packet{Data: e.out, PTS: e.pos, Dur: int64(src.N), Sync: true}
	e.pos += int64(src.N)
	return emit(pkt)
}

// Finish reports the stream total. PCM has no encoder delay or padding.
func (e *Encoder) Finish(func(codec.Packet) error) (codec.Trailer, error) {
	return codec.Trailer{Samples: e.pos}, nil
}

func (e *Encoder) pack(src *audio.Buffer) {
	ch := e.fmt.Channels
	step := e.cfg.Bits / 8 * ch
	shift := e.cfg.shift()
	for c := 0; c < ch; c++ {
		off := c * e.cfg.Bits / 8
		switch {
		case e.cfg.Encoding == Float && e.cfg.Bits == 32:
			s := src.ChanF(c)
			for i, v := range s {
				e.put32(e.out[off+i*step:], math.Float32bits(v))
			}
		case e.cfg.Encoding == Float:
			s := src.ChanF(c)
			for i, v := range s {
				e.put64(e.out[off+i*step:], math.Float64bits(float64(v)))
			}
		case e.cfg.Encoding == UnsignedInt:
			s := src.ChanI(c)
			for i, v := range s {
				e.out[off+i*step] = byte((v << shift) + 128)
			}
		case e.cfg.Bits == 8:
			s := src.ChanI(c)
			for i, v := range s {
				e.out[off+i*step] = byte(v << shift)
			}
		case e.cfg.Bits == 16:
			s := src.ChanI(c)
			for i, v := range s {
				e.put16(e.out[off+i*step:], uint16(v<<shift))
			}
		case e.cfg.Bits == 24:
			s := src.ChanI(c)
			for i, v := range s {
				e.put24(e.out[off+i*step:], uint32(v<<shift))
			}
		default: // 32
			s := src.ChanI(c)
			for i, v := range s {
				e.put32(e.out[off+i*step:], uint32(v<<shift))
			}
		}
	}
}

func (e *Encoder) put16(b []byte, v uint16) {
	if e.cfg.BigEndian {
		binary.BigEndian.PutUint16(b, v)
	} else {
		binary.LittleEndian.PutUint16(b, v)
	}
}

func (e *Encoder) put24(b []byte, v uint32) {
	if e.cfg.BigEndian {
		b[0], b[1], b[2] = byte(v>>16), byte(v>>8), byte(v)
	} else {
		b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16)
	}
}

func (e *Encoder) put32(b []byte, v uint32) {
	if e.cfg.BigEndian {
		binary.BigEndian.PutUint32(b, v)
	} else {
		binary.LittleEndian.PutUint32(b, v)
	}
}

func (e *Encoder) put64(b []byte, v uint64) {
	if e.cfg.BigEndian {
		binary.BigEndian.PutUint64(b, v)
	} else {
		binary.LittleEndian.PutUint64(b, v)
	}
}
