package musepack

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder decodes Musepack packets into planar float32 buffers, one frame of
// 1152 samples per emit.
//
// Nothing in a packet re-anchors a reader that has drifted (SV7 frames chain
// their scalefactors forever, an SV8 block states no frame boundaries), so a
// decode failure is latched until Reset: a caller retrying after an error
// would otherwise read half-updated state as a stream's.
type Decoder struct {
	cfg Config
	fmt audio.Format

	st  frameState
	y   [2][36][32]float32
	syn [2]synthesis
	r   bitReader
	buf *audio.Buffer

	err error

	// onFrame, when set, sees every frame's state before it is emitted. Test
	// seams use it to look at what the real decode path read.
	onFrame func(*frameState)
}

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
			"musepack: track format "+f.String()+" does not match the stream ("+want.String()+")")
	}
	d := &Decoder{cfg: cfg, fmt: f}
	d.st.init()
	return d, nil
}

// Decode decodes one packet: an SV7 frame or an SV8 block, with the state
// block a seek landing carries in front of it. Any failure latches, an emit
// error included: the frames already emitted have advanced the state, so a
// retry of the same packet could only decode it against the wrong generator.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if d.err != nil {
		return d.err
	}
	if err := d.decode(pkt, emit); err != nil {
		d.err = err
		return err
	}
	return nil
}

func (d *Decoder) decode(pkt []byte, emit func(*audio.Buffer) error) error {
	h, state, payload, err := ParsePacketHeader(pkt, d.cfg)
	if err != nil {
		return err
	}
	if h.HasState {
		st, err := ParseState(state, d.cfg.StreamVersion)
		if err != nil {
			return err
		}
		d.st.load(&st, d.cfg.StreamVersion)
	}
	if d.cfg.StreamVersion == 7 {
		d.r.reset(payload, h.BitLen)
		maxUsed, err := d.st.readSV7Header(&d.r, &d.cfg)
		if err != nil {
			return err
		}
		if err := d.st.readSV7Samples(&d.r, maxUsed); err != nil {
			return err
		}
		// The reference validates that a frame consumed exactly what its
		// length field declared; a mismatch is a drifted reader.
		if d.r.pos != h.BitLen {
			return malformed("frame consumed %d bits of the %d it declared", d.r.pos, h.BitLen)
		}
		return d.emitFrame(emit)
	}
	d.r.reset(payload, len(payload)*8)
	for i := 0; i < h.Frames; i++ {
		if err := d.st.readSV8(&d.r, &d.cfg, i == 0); err != nil {
			return err
		}
		if err := d.emitFrame(emit); err != nil {
			return err
		}
	}
	if rest := d.r.remaining(); rest >= 8 {
		return malformed("block has %d bits past its last frame, more than padding", rest)
	}
	return nil
}

// emitFrame requantises and synthesizes the frame the state holds.
func (d *Decoder) emitFrame(emit func(*audio.Buffer) error) error {
	if d.onFrame != nil {
		d.onFrame(&d.st)
	}
	d.requantize()
	if d.buf == nil || d.buf.Fmt != d.fmt {
		audio.Put(d.buf)
		d.buf = audio.Get(d.fmt, FrameLength)
	}
	d.buf.N = FrameLength
	for c := range d.cfg.Channels {
		d.syn[c].frame(&d.y[c], d.buf.ChanF(c))
	}
	return emit(d.buf)
}

// Drain emits nothing: the synthesis delay is a container trim (Track.Delay),
// not latency the decoder holds back.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset discards the cross-packet state after a seek and clears a latched
// failure. The decoder then assumes stream-start state; a landing anywhere
// else hands the state over in its first packet.
func (d *Decoder) Reset() {
	d.st.init()
	for c := range d.syn {
		d.syn[c].reset()
	}
	d.err = nil
}

// Release returns the output buffer to the pool (codec.Releaser). The decoder
// must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}
