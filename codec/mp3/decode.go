package mp3

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// maxReservoir is the deepest backward reference main_data_begin can
// encode (9 bits): the decoder never needs to keep more.
const maxReservoir = 511

// maxFrameLen bounds a packet: the largest compliant frame is 1441 bytes
// (320 kbit/s at 32 kHz, padded); free-format frames may run longer but
// the format caps a frame's slot count at this order of magnitude, and
// container/mpa enforces its own scan bound first.
const maxFrameLen = 8 << 10

// Decoder decodes MP3 frames into planar float32 buffers. It implements
// codec.Decoder: one packet is one whole frame (header included) and
// always emits exactly SamplesPerFrame frames, silent when the frame is
// undecodable. Silence instead of a hard error is the honest damage unit
// for a lossy stream, and the constant emission keeps format.Media's
// sample counting exact, which is what seeking and gapless trims stand
// on. Structural misuse (a packet that is not a frame of this stream)
// still errors: that is a wiring bug, not damage.
//
// The bit reservoir makes frames interdependent: after Reset (a seek),
// frames whose main_data_begin reaches back into unseen bytes emit
// silence while their own bytes prime the reservoir. container/mpa's
// seek backoff covers the reservoir plus the filterbank history, so
// output converges to the linear decode before the seek target.
type Decoder struct {
	f   audio.Format
	buf *audio.Buffer // reusable output, borrowed by emit callbacks

	resv    []byte // rolling main-data reservoir, at most maxReservoir bytes
	main    []byte // assembled main data for the current frame
	silent  bool   // the current frame decodes to silence
	si      sideInfo
	gran    granule
	gr0Ist  [2][40]uint8 // granule 0 raw scalefactors for MPEG-1 scfsi copies
	scratch [576]float32
	store   [2][32][18]float32 // IMDCT overlap-add state
	v       [2][1024]float32   // synthesis filterbank state
}

// NewDecoder returns a Decoder for a track with the given format. The
// format must be what Header.PCMFormat produces for the stream's frames;
// containers build both from the same header, so a mismatch is a wiring
// bug.
func NewDecoder(f audio.Format) (*Decoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp3: track format %v is not a Layer III shape", f))
	}
	return &Decoder{f: f}, nil
}

// Decode decodes one frame and emits one buffer of SamplesPerFrame
// frames. The buffer is borrowed: valid only during the callback.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	h, err := ParseHeader(pkt)
	if err != nil {
		return err
	}
	if got := h.PCMFormat(); got != d.f {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp3: frame format %v disagrees with the track's %v", got, d.f))
	}
	if len(pkt) > maxFrameLen {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "mp3: oversized frame packet")
	}

	spf := h.SamplesPerFrame()
	if d.buf == nil || d.buf.Cap() < spf || d.buf.Fmt != d.f {
		audio.Put(d.buf)
		d.buf = audio.Get(d.f, max(spf, audio.StandardChunk))
	}
	d.buf.N = spf

	d.silent = false
	off := HeaderLen
	if h.Protected {
		off += 2 // CRC-16 over the side info; skipped, not verified
	}
	silen := h.SideInfoLen()
	var body []byte
	if len(pkt) < off+silen {
		d.silent = true
	} else {
		body = pkt[off+silen:]
		if !parseSideInfo(h, pkt[off:off+silen], &d.si) {
			d.silent = true
		}
	}

	if !d.silent {
		// Assemble main data: main_data_begin bytes of reservoir tail,
		// then this frame's own bytes. An unsatisfiable reference (the
		// first frames after a seek) is silence, but the frame's bytes
		// still enter the reservoir below so later frames decode.
		mdb := d.si.mainDataBegin
		if mdb > len(d.resv) {
			d.silent = true
		} else {
			tail := d.resv[len(d.resv)-mdb:]
			d.main = append(d.main[:0], tail...)
			d.main = append(d.main, body...)
		}
	}
	if !d.silent {
		d.decodeFrame(h)
	}
	if d.silent {
		for c := 0; c < d.f.Channels; c++ {
			clear(d.buf.ChanF(c)[:spf])
		}
	}

	// Roll the reservoir forward with this frame's own main data.
	d.resv = append(d.resv, body...)
	if n := len(d.resv) - maxReservoir; n > 0 {
		d.resv = append(d.resv[:0], d.resv[n:]...)
	}

	return emit(d.buf)
}

// decodeFrame reconstructs PCM for all granules from the assembled main
// data, or flags silence when the data is inconsistent.
func (d *Decoder) decodeFrame(h Header) {
	r := bitReader{data: d.main}
	granules := 1
	if h.Version == MPEG1 {
		granules = 2
	}
	msActive := h.Mode == ModeJoint && h.ModeExt&2 != 0

	// A frame whose granule budgets overrun the assembled bytes cannot
	// carry what the side info promises.
	total := 0
	for gi := 0; gi < granules; gi++ {
		for ch := 0; ch < h.Channels; ch++ {
			total += d.si.gr[gi][ch].part23Len
		}
	}
	if total > r.bitLen() {
		d.silent = true
		return
	}

	for gri := 0; gri < granules; gri++ {
		g := &d.gran
		for ch := 0; ch < h.Channels; ch++ {
			gi := &d.si.gr[gri][ch]
			b := bandsFor(h, gi)
			part23End := r.bitPos() + gi.part23Len

			// Scalefactor sharing needs both granules long: the spec
			// requires encoders to clear scfsi around short blocks, and
			// honoring it anyway would reinterpret short-window values
			// as long-band ones.
			var scfsi *[4]bool
			if gri == 1 && gi.blockType != blockShort &&
				d.si.gr[0][ch].blockType != blockShort {
				scfsi = &d.si.scfsi[ch]
			}
			readScalefactors(&r, h, gi, b, g, ch, scfsi, &d.gr0Ist)
			if r.bitPos() > part23End {
				// Scalefactors alone overran the granule budget.
				for i := range g.raw[ch] {
					g.raw[ch][i] = 0
				}
				r.setPos(part23End)
				r.err = false
			} else {
				readSpectrum(&r, gi, b, g, ch, part23End)
				r.err = false // damage is contained per granule
			}
			requantize(gi, b, g, ch, msActive)
		}
		if gri == 0 && h.Version == MPEG1 {
			// Granule 0's raw scalefactors (istPos is never touched by
			// the gain folds) feed granule 1's scfsi sharing.
			d.gr0Ist = g.istPos
		}

		if h.Channels == 2 && h.Mode == ModeJoint {
			// Stereo walks the left granule info's band shape, as the
			// reference decoders do; a hostile frame whose channels
			// disagree on block type gets bounded garbage in the joint
			// bands, nothing worse.
			stereo(h, bandsFor(h, &d.si.gr[gri][0]), g, &d.si.gr[gri][1])
		}

		for ch := 0; ch < h.Channels; ch++ {
			gi := &d.si.gr[gri][ch]
			b := bandsFor(h, gi)
			nLongBands := 0
			if gi.blockType == blockShort {
				nLongBands = b.longSubbands
				reorder(b, g, ch, nLongBands*18, &d.scratch)
				antialias(g, ch, max(nLongBands-1, 0))
			} else {
				antialias(g, ch, 31)
			}
			d.hybrid(gi, g, ch, nLongBands)
			d.synth(g, ch, d.buf.ChanF(ch)[gri*576:gri*576+576])
		}
	}
}

// Drain is a no-op: every packet emits its full frame, and the inherent
// 529-sample codec latency is signaled through the container's gapless
// trims, not buffered here.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset discards the reservoir and filterbank state after a seek.
func (d *Decoder) Reset() {
	d.resv = d.resv[:0]
	d.store = [2][32][18]float32{}
	d.v = [2][1024]float32{}
	d.gr0Ist = [2][40]uint8{}
}

// Release returns pooled scratch (format.Media calls it on Close).
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}
