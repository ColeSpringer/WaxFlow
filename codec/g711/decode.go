package g711

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder expands companded bytes into planar 16-bit buffers. It implements
// codec.Decoder.
type Decoder struct {
	tab *[256]int16
	fmt audio.Format
	buf *audio.Buffer // reusable scratch, borrowed by emit callbacks
}

// NewDecoder returns a Decoder for a law and track format. The format must
// be what Format returns for the track's rate and channels; a demuxer builds
// both from one header, so a mismatch is a wiring bug rather than bad input.
func NewDecoder(law Law, f audio.Format) (*Decoder, error) {
	if err := law.Valid(); err != nil {
		return nil, err
	}
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := Format(f.Rate, f.Channels, f.Layout); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"g711: track format "+f.String()+" is not the one this codec decodes to ("+want.String()+")")
	}
	return &Decoder{tab: law.table(), fmt: f}, nil
}

// Decode expands one packet of interleaved bytes and emits planar buffers of
// at most audio.StandardChunk frames. Emitted buffers are borrowed: valid
// only during the callback.
//
// One byte is one sample, so a packet is a whole number of frames exactly
// when its length divides by the channel count; anything else describes a
// frame the stream does not hold.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	ch := d.fmt.Channels
	if len(pkt)%ch != 0 {
		return waxerr.Malformed("g711: ", "packet of %d bytes is not a whole number of %d-channel frames", len(pkt), ch)
	}
	for len(pkt) > 0 {
		frames := min(len(pkt)/ch, audio.StandardChunk)
		if d.buf == nil || d.buf.Cap() < frames || d.buf.Fmt != d.fmt {
			audio.Put(d.buf)
			d.buf = audio.Get(d.fmt, audio.StandardChunk)
		}
		d.buf.N = frames
		for c := range ch {
			dst := d.buf.ChanI(c)
			for i := range dst {
				dst[i] = int32(d.tab[pkt[i*ch+c]])
			}
		}
		if err := emit(d.buf); err != nil {
			return err
		}
		pkt = pkt[frames*ch:]
	}
	return nil
}

// Drain is a no-op: companding has no decoder latency.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset is a no-op: every byte decodes independently of the last, which is
// also why every packet is a sync point.
func (d *Decoder) Reset() {}

// Release returns the scratch buffer to the pool (codec.Releaser). The
// decoder must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}
