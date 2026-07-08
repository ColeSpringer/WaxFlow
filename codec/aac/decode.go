package aac

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Syntactic element types (ISO 14496-3 Table 4.85).
const (
	elSCE = 0
	elCPE = 1
	elCCE = 2
	elLFE = 3
	elDSE = 4
	elPCE = 5
	elFIL = 6
	elEND = 7
)

const (
	maxWindowGroups = 8
	maxSFBCount     = 64
	sfOffset        = 100 // scalefactor gain offset (2^0.25 steps)
)

// icsInfo is the per-channel window and grouping description.
type icsInfo struct {
	windowSequence  int
	windowShape     int
	maxSfb          int
	numWindows      int
	numWindowGroups int
	windowGroupLen  [maxWindowGroups]int
	swb             []uint16 // scalefactor-band offsets for this window type
	numSwb          int
}

// channelData holds one channel's parsed ICS and decoded spectrum.
type channelData struct {
	info       icsInfo
	globalGain int
	sfbCb      [maxWindowGroups][maxSFBCount]uint8
	sf         [maxWindowGroups][maxSFBCount]int
	spec       [1024]float64
	tns        tnsInfo
	hasTNS     bool
	hasPulse   bool
	pulse      pulseInfo
	pnsSeed    uint32
}

// Decoder decodes AAC-LC access units into planar float buffers. Each
// packet is one 1024-sample frame; the IMDCT's overlap makes frame N depend
// on frame N-1, so Reset clears the overlap after a seek and the container
// pre-rolls one frame.
type Decoder struct {
	cfg      Config
	fmt      audio.Format
	rateIdx  int
	channels int
	frameLen int

	buf      *audio.Buffer
	ch       [audio.MaxChannels]channelData
	overlap  [audio.MaxChannels][1024]float64
	prevWin  [audio.MaxChannels]int // previous window_shape per output channel
	pnsState uint32                 // perceptual-noise PRNG state
}

// NewDecoder returns a Decoder for a stream. The track format must be what
// Config.Format produces.
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if cfg.ObjectType != aotAACLC {
		return nil, malformed("object type %d is not AAC-LC", cfg.ObjectType)
	}
	if cfg.FrameLength != 1024 {
		return nil, malformed("frame length %d unsupported (only 1024)", cfg.FrameLength)
	}
	rateIdx := samplingIndex(cfg.SampleRate)
	if rateIdx < 0 || rateIdx >= len(swbOffsetLong) {
		return nil, malformed("sample rate %d has no scalefactor-band table", cfg.SampleRate)
	}
	d := &Decoder{cfg: cfg, fmt: f, rateIdx: rateIdx, channels: f.Channels,
		frameLen: int(cfg.FrameLength), pnsState: 0x1f2e3d4c}
	return d, nil
}

// Decode decodes one access unit and emits one 1024-frame buffer.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if d.buf == nil || d.buf.Cap() < d.frameLen || d.buf.Fmt != d.fmt {
		audio.Put(d.buf)
		d.buf = audio.Get(d.fmt, d.frameLen)
	}
	d.buf.N = d.frameLen
	for c := 0; c < d.channels; c++ {
		clear(d.buf.ChanF(c)[:d.frameLen])
	}

	r := newBitReader(pkt)
	outCh := 0
	for {
		if r.left() < 3 {
			break
		}
		tag := r.read(3)
		switch tag {
		case elSCE, elLFE:
			r.read(4) // element_instance_tag
			if outCh >= d.channels {
				return malformed("more channels in bitstream than configured")
			}
			cd := &d.ch[0]
			if err := d.decodeChannelData(r, cd, false); err != nil {
				return err
			}
			d.dequant(cd)
			d.applyPNS(cd)
			d.finishChannel(cd, outCh)
			outCh++
		case elCPE:
			r.read(4) // element_instance_tag
			if outCh+2 > d.channels {
				return malformed("channel pair exceeds configured channels")
			}
			if err := d.decodePair(r, outCh); err != nil {
				return err
			}
			outCh += 2
		case elDSE:
			skipDSE(r)
		case elPCE:
			skipPCE(r)
		case elFIL:
			skipFIL(r)
		case elEND:
			r.byteAlign()
			goto done
		default: // CCE
			return malformed("unsupported element type %d", tag)
		}
		if r.overrun() {
			return malformed("access unit overruns packet")
		}
		if outCh >= d.channels {
			// Remaining elements (fill, padding) do not add audio.
			break
		}
	}
done:
	return emit(d.buf)
}

// decodePair decodes a channel-pair element, applying the shared window and
// M/S stereo before the per-channel filterbank.
func (d *Decoder) decodePair(r *bitReader, outCh int) error {
	common := r.bit() != 0
	var shared icsInfo
	msMask := 0
	var msUsed [maxWindowGroups][maxSFBCount]bool
	if common {
		if !d.parseICSInfo(r, &shared) {
			return malformed("bad shared ics_info")
		}
		msMask = int(r.read(2))
		if msMask == 1 {
			for g := 0; g < shared.numWindowGroups; g++ {
				for sfb := 0; sfb < shared.maxSfb; sfb++ {
					msUsed[g][sfb] = r.bit() != 0
				}
			}
		}
	}
	left, right := &d.ch[0], &d.ch[1]
	if common {
		left.info = shared
		right.info = shared
	}
	if err := d.decodeChannelData(r, left, common); err != nil {
		return err
	}
	if err := d.decodeChannelData(r, right, common); err != nil {
		return err
	}
	d.dequant(left)
	d.dequant(right)
	// PNS fills noise bands before the stereo tools so intensity, which
	// copies the left channel, sees the filled spectrum (ISO 14496-3 order:
	// dequant, PNS, M/S, intensity). M/S and intensity both skip PNS bands.
	d.applyPNS(left)
	d.applyPNS(right)
	if common && msMask != 0 {
		applyMS(left, right, msMask, &msUsed)
	}
	applyIntensity(left, right, msMask, &msUsed)
	d.finishChannel(left, outCh)
	d.finishChannel(right, outCh+1)
	return nil
}

// Drain is a no-op: each access unit emits its full 1024-sample frame, and
// the trailing filterbank overlap belongs to no further frame.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset clears the filterbank overlap and window history after a seek.
func (d *Decoder) Reset() {
	for c := range d.overlap {
		d.overlap[c] = [1024]float64{}
		d.prevWin[c] = shapeSine
	}
}

// Release returns the output buffer to the pool (codec.Releaser).
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}
