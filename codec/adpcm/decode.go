package adpcm

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder turns ADPCM blocks into planar 16-bit buffers. It implements
// codec.Decoder.
type Decoder struct {
	cfg Config
	fmt audio.Format

	// ima and ms are the per-channel coder state, one slice per family. All
	// three layouts reload it from every block header except the QuickTime
	// one, which carries it across blocks; see decodeIMAQuickTime.
	ima []imaState
	ms  []msState

	// scratch holds one decoded block per channel. A block is decoded whole
	// because the layouts write their channels in three different orders, and
	// only then chunked into buffers.
	scratch [][]int32
	buf     *audio.Buffer // reusable output, borrowed by emit callbacks
}

// malformed reports bytes that deviate from the format.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("adpcm: ", format, args...)
}

// NewDecoder returns a Decoder for a block geometry and track format. The
// format must be what Config.Format returns; a demuxer builds both from one
// header, so a mismatch is a wiring bug rather than bad input.
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := cfg.Format(f.Rate, f.Layout); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"adpcm: track format "+f.String()+" is not the one this config decodes to ("+want.String()+")")
	}
	d := &Decoder{cfg: cfg, fmt: f, scratch: make([][]int32, cfg.Channels)}
	for c := range d.scratch {
		d.scratch[c] = make([]int32, cfg.SamplesPerBlock)
	}
	if cfg.Layout == MS {
		d.ms = make([]msState, cfg.Channels)
	} else {
		d.ima = make([]imaState, cfg.Channels)
	}
	return d, nil
}

// Decode consumes whole blocks and emits planar buffers of at most
// audio.StandardChunk frames. Emitted buffers are borrowed: valid only
// during the callback.
//
// Whole blocks, because a block is the unit every container addresses these
// codecs by: the demuxers index the payload in blocks and hand over a run of
// them, so a packet that is not a whole number of blocks describes a block
// boundary the stream does not have.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	size := d.cfg.BlockBytes()
	if len(pkt)%size != 0 {
		return malformed("packet of %d bytes is not a whole number of %d-byte blocks", len(pkt), size)
	}
	for len(pkt) > 0 {
		var err error
		switch d.cfg.Layout {
		case IMAQuickTime:
			err = d.decodeIMAQuickTime(pkt[:size])
		case MS:
			err = d.decodeMS(pkt[:size])
		default:
			err = d.decodeIMAWav(pkt[:size])
		}
		if err != nil {
			return err
		}
		if err := d.emitBlock(emit); err != nil {
			return err
		}
		pkt = pkt[size:]
	}
	return nil
}

// emitBlock hands the decoded block over in pipeline-sized pieces.
func (d *Decoder) emitBlock(emit func(*audio.Buffer) error) error {
	for off := 0; off < d.cfg.SamplesPerBlock; {
		frames := min(d.cfg.SamplesPerBlock-off, audio.StandardChunk)
		if d.buf == nil || d.buf.Cap() < frames || d.buf.Fmt != d.fmt {
			audio.Put(d.buf)
			d.buf = audio.Get(d.fmt, audio.StandardChunk)
		}
		d.buf.N = frames
		for c := range d.cfg.Channels {
			copy(d.buf.ChanI(c), d.scratch[c][off:off+frames])
		}
		if err := emit(d.buf); err != nil {
			return err
		}
		off += frames
	}
	return nil
}

// Drain is a no-op: a block decodes entirely within one Decode call.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset discards the coder state. It matters for exactly one layout: the
// QuickTime one carries its predictor across blocks, so a decode resumed
// elsewhere must start from a clean state rather than from wherever the
// previous run left off. The other two reload from every block header and
// are unaffected.
func (d *Decoder) Reset() {
	clear(d.ima)
	clear(d.ms)
}

// Release returns the scratch buffer to the pool (codec.Releaser). The
// decoder must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}

// clip16 saturates to the 16-bit domain both predictors reconstruct in. It
// takes the wider type because MS ADPCM computes in it; the IMA predictor's
// own arithmetic is bounded by the step table and stays in 32 bits.
func clip16(v int64) int32 { return int32(min(max(v, -32768), 32767)) }

func clamp32(v, lo, hi int32) int32 { return min(max(v, lo), hi) }

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// trunc256 divides by 256 rounding toward zero, which is what the MS
// specification's C expression does and what an arithmetic shift does not.
//
// Written branchlessly: a shift alone floors, which for a negative value is
// one too low, and adding the divisor's mask back first rounds it the other
// way. The sign test the obvious form needs costs more than the arithmetic,
// and this sits in the inner loop of every MS block.
func trunc256(v int64) int64 { return (v + (v >> 63 & 255)) >> 8 }

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
