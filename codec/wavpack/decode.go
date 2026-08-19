package wavpack

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder decodes WavPack blocks into planar int buffers. One packet is one
// whole block, header included, and every block is independently decodable, so
// Drain and Reset are no-ops.
type Decoder struct {
	cfg   Config
	fmt   audio.Format
	buf   *audio.Buffer // reusable output, borrowed by emit callbacks
	state blockState
	samps []int32 // interleaved decode scratch
}

// NewDecoder returns a Decoder for a stream. The track format must be what
// Config.Format produces; the demuxer builds both from the same first block,
// so a mismatch is a wiring bug.
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := cfg.Format(); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"wavpack: track format "+f.String()+" does not match the stream ("+want.String()+")")
	}
	return &Decoder{cfg: cfg, fmt: f}, nil
}

// Decode decodes one block and emits one buffer. The buffer is borrowed:
// valid only during the callback. A block carrying no samples (a trailing MD5
// or wrapper block) emits nothing.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	h, err := ParseBlockHeader(pkt)
	if err != nil {
		return err
	}
	if !h.Audio() {
		return nil
	}
	if err := h.supported(); err != nil {
		return err
	}
	if err := d.consistent(h); err != nil {
		return err
	}
	need := h.BlockSamples * d.cfg.Channels
	if cap(d.samps) < need {
		d.samps = make([]int32, need)
	}
	got, err := d.state.unpackBlock(h, pkt, d.samps[:need])
	if err != nil {
		return err
	}
	if d.buf == nil || d.buf.Cap() < got || d.buf.Fmt != d.fmt {
		audio.Put(d.buf)
		d.buf = audio.Get(d.fmt, max(got, audio.StandardChunk))
	}
	d.buf.N = got
	for c := 0; c < d.cfg.Channels; c++ {
		dst := d.buf.ChanI(c)
		for i := range dst {
			dst[i] = d.samps[i*d.cfg.Channels+c]
		}
	}
	return emit(d.buf)
}

// consistent rejects a block whose shape disagrees with the track. WavPack
// permits a mid-stream change of channel count or width; the fixed-format
// pipeline does not, so it is an error rather than damage.
func (d *Decoder) consistent(h BlockHeader) error {
	if h.Channels() != d.cfg.Channels || h.BytesPerSample()*8 != d.cfg.BitDepth {
		return malformed("block at sample %d changes format mid-stream", h.BlockIndex)
	}
	return nil
}

// Drain is a no-op: WavPack has no decoder latency.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset is a no-op: every block decodes independently.
func (d *Decoder) Reset() {}

// Release returns the output buffer to the pool (codec.Releaser). The decoder
// must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}
