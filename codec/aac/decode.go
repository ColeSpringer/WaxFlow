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

// Decoder decodes AAC-LC and HE-AAC v1 access units into planar float
// buffers. Each packet is one frame: 1024 samples, or 2048 at the doubled
// rate when SBR decode is active. The IMDCT's overlap (and the SBR QMF
// state) makes frame N depend on frame N-1, so Reset clears that state
// after a seek and the container pre-rolls.
type Decoder struct {
	cfg      Config
	fmt      audio.Format
	rateIdx  int
	frameLen int
	// slots routes each element in the frame's channel sequence to its
	// output channel (waveSlots). Its length is the output channel count,
	// which is why there is no separate field for that: one source for
	// both cannot drift from the other. elems is the element type expected
	// at each of those positions, or nil where the routing is the identity
	// and the order cannot matter.
	slots []int
	elems []int

	buf      *audio.Buffer
	ch       [audio.MaxChannels]channelData
	overlap  [audio.MaxChannels][1024]float64
	prevWin  [audio.MaxChannels]int // previous window_shape per output channel
	pnsState uint32                 // perceptual-noise PRNG state

	// SBR state, nil/unused for plain LC. Core samples land in stage
	// (per output channel) instead of the output buffer; each SCE/CPE
	// gets a persistent sbrElement, keyed by its position in the frame's
	// element sequence, that upsamples staging into the output buffer.
	sbrActive bool
	outLen    int
	stage     [audio.MaxChannels][]float32
	sbrEls    []*sbrElement
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
	slots := waveSlots(cfg.ChannelConfig, f.Channels)
	if len(slots) != f.Channels {
		return nil, malformed("channel configuration %d with %d channels has no known element order",
			cfg.ChannelConfig, f.Channels)
	}
	elems := channelElements(cfg.ChannelConfig)
	if elems != nil && len(elems) != len(slots) {
		return nil, malformed("channel configuration %d maps %d elements onto %d channels",
			cfg.ChannelConfig, len(elems), len(slots))
	}
	d := &Decoder{cfg: cfg, fmt: f, rateIdx: rateIdx,
		frameLen: int(cfg.FrameLength), slots: slots, elems: elems, pnsState: 0x1f2e3d4c}
	d.sbrActive = cfg.heDecode()
	d.outLen = cfg.OutputSamplesPerAU()
	if d.sbrActive {
		for c := 0; c < f.Channels; c++ {
			d.stage[c] = make([]float32, d.frameLen)
		}
	}
	return d, nil
}

// sbrElem returns the persistent SBR element at a position in the frame's
// element sequence, rebuilding it if the stream changes its element shape.
func (d *Decoder) sbrElem(idx, chans int) *sbrElement {
	for len(d.sbrEls) <= idx {
		d.sbrEls = append(d.sbrEls, nil)
	}
	if el := d.sbrEls[idx]; el != nil && el.chans == chans {
		return el
	}
	el := newSBRElement(chans, d.cfg.ExtensionRate)
	d.sbrEls[idx] = el
	return el
}

// checkElement rejects an element that is not the one Table 1.19 puts at
// this position in the channel sequence. Positions are routed by index
// (see channelElements), so a deviant order would write every channel
// somewhere plausible and wrong rather than fail.
func (d *Decoder) checkElement(pos int, tag uint32) error {
	if d.elems == nil {
		// Mono and stereo route through the identity, so their element
		// order is unconstrained here on purpose. An LFE is not an order
		// question though: these layouts carry no low-frequency channel,
		// so there is nowhere for one to land but a full-range front
		// channel, at the full-range level.
		if tag == elLFE {
			return malformed("channel configuration %d has no LFE channel", d.cfg.ChannelConfig)
		}
		return nil
	}
	if d.elems[pos] == int(tag) {
		return nil
	}
	return malformed("channel configuration %d codes %s at channel %d, found %s",
		d.cfg.ChannelConfig, elementName(d.elems[pos]), pos, elementName(int(tag)))
}

// Decode decodes one access unit and emits one frame buffer: 1024 samples,
// or 2048 at the extension rate when SBR decode is active.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	if d.buf == nil || d.buf.Cap() < d.outLen || d.buf.Fmt != d.fmt {
		audio.Put(d.buf)
		d.buf = audio.Get(d.fmt, d.outLen)
	}
	d.buf.N = d.outLen
	for c := 0; c < len(d.slots); c++ {
		clear(d.buf.ChanF(c)[:d.outLen])
	}

	r := newBitReader(pkt)
	if d.sbrActive {
		if err := d.decodeSBRFrame(r); err != nil {
			return err
		}
		return emit(d.buf)
	}
	// elem walks the frame's channel sequence; d.slots turns that position
	// into the output channel, which past stereo is not the same number.
	elem := 0
	for {
		if r.left() < 3 {
			break
		}
		tag := r.read(3)
		switch tag {
		case elSCE, elLFE:
			r.read(4) // element_instance_tag
			if elem >= len(d.slots) {
				return malformed("more channels in bitstream than configured")
			}
			if err := d.checkElement(elem, tag); err != nil {
				return err
			}
			cd := &d.ch[0]
			if err := d.decodeChannelData(r, cd, false); err != nil {
				return err
			}
			d.dequant(cd)
			d.applyPNS(cd)
			d.finishChannel(cd, d.slots[elem])
			elem++
		case elCPE:
			r.read(4) // element_instance_tag
			if elem+2 > len(d.slots) {
				return malformed("channel pair exceeds configured channels")
			}
			if err := d.checkElement(elem, tag); err != nil {
				return err
			}
			// The pair's two output slots are passed explicitly: a coupled
			// pair is adjacent in the bitstream but need not be adjacent in
			// WAVE order (configuration 6 sends L/R to 0 and 1, then Ls/Rs to
			// 4 and 5, straddling the LFE).
			if err := d.decodePair(r, d.slots[elem], d.slots[elem+1]); err != nil {
				return err
			}
			elem += 2
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
		if elem >= len(d.slots) {
			// Remaining elements (fill, padding) do not add audio.
			break
		}
	}
done:
	return emit(d.buf)
}

// decodePair decodes a channel-pair element, applying the shared window and
// M/S stereo before the per-channel filterbank. leftCh and rightCh are the
// output channels the pair's two members land in, which need not be
// adjacent.
func (d *Decoder) decodePair(r *bitReader, leftCh, rightCh int) error {
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
	d.finishChannel(left, leftCh)
	d.finishChannel(right, rightCh)
	return nil
}

// decodeSBRFrame is the SBR-configured element loop. Unlike the LC loop it
// runs past the filled channel slots, consuming fill, data, and end
// elements, because the SBR payload rides in a fill element after the
// channel element it extends. Core samples land in staging; each element's
// SBR machine then writes the doubled-rate output, with a zero high band
// when its fill is missing or damaged (concealment keeps the AU shape).
func (d *Decoder) decodeSBRFrame(r *bitReader) error {
	elem, elIdx := 0, 0
	var pend *sbrElement
	var pendFill bool
	var pendIn, pendOut [2][]float32
	flush := func() {
		if pend == nil {
			return
		}
		pend.process(pendIn[:pend.chans], pendOut[:pend.chans])
		pend = nil
	}
	for {
		if r.left() < 3 {
			break
		}
		tag := r.read(3)
		// Once every channel slot is filled the LC loop stops reading, so
		// its acceptance envelope includes AUs with trailing junk. This loop
		// must keep reading (the last element's SBR payload is still ahead),
		// so it matches that envelope the other way: junk past the last slot
		// ends the frame rather than failing an AU LC would have emitted.
		full := elem >= len(d.slots)
		switch tag {
		case elSCE, elLFE:
			flush()
			if full {
				return nil
			}
			r.read(4) // element_instance_tag
			if err := d.checkElement(elem, tag); err != nil {
				return err
			}
			cd := &d.ch[0]
			if err := d.decodeChannelData(r, cd, false); err != nil {
				return err
			}
			d.dequant(cd)
			d.applyPNS(cd)
			outCh := d.slots[elem]
			d.finishChannel(cd, outCh)
			el := d.sbrElem(elIdx, 1)
			el.beginFrame()
			pend = el
			// The LFE cannot carry SBR data, so its element permanently
			// conceals (a pure QMF upsample) and a fill after it is not its
			// payload: routing one in would activate a high band the spec
			// says the LFE does not have.
			pendFill = tag == elSCE
			pendIn[0] = d.stage[outCh]
			pendOut[0] = d.buf.ChanF(outCh)[:d.outLen]
			elem++
			elIdx++
		case elCPE:
			flush()
			if full {
				return nil
			}
			r.read(4) // element_instance_tag
			if elem+2 > len(d.slots) {
				return malformed("channel pair exceeds configured channels")
			}
			if err := d.checkElement(elem, tag); err != nil {
				return err
			}
			leftCh, rightCh := d.slots[elem], d.slots[elem+1]
			if err := d.decodePair(r, leftCh, rightCh); err != nil {
				return err
			}
			el := d.sbrElem(elIdx, 2)
			el.beginFrame()
			pend = el
			pendFill = true
			pendIn[0], pendIn[1] = d.stage[leftCh], d.stage[rightCh]
			pendOut[0] = d.buf.ChanF(leftCh)[:d.outLen]
			pendOut[1] = d.buf.ChanF(rightCh)[:d.outLen]
			elem += 2
			elIdx++
		case elDSE:
			skipDSE(r)
		case elPCE:
			skipPCE(r)
		case elFIL:
			count := filCount(r)
			if pend != nil && pendFill {
				pend.parseFill(r, count)
			} else {
				r.skip(count * 8)
			}
		case elEND:
			r.byteAlign()
			flush()
			return nil
		default: // CCE
			if full {
				flush()
				return nil
			}
			return malformed("unsupported element type %d", tag)
		}
		if r.overrun() {
			if full {
				flush()
				return nil
			}
			return malformed("access unit overruns packet")
		}
	}
	flush()
	return nil
}

// Drain is a no-op: each access unit emits its full frame, and the
// trailing filterbank overlap belongs to no further frame.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset clears the filterbank overlap, window history, and SBR channel
// state after a seek. Parsed SBR headers and band tables survive: they
// repeat only every half second or so, and dropping them would mute the
// high band until the next one.
func (d *Decoder) Reset() {
	for c := range d.overlap {
		d.overlap[c] = [1024]float64{}
		d.prevWin[c] = shapeSine
	}
	for _, el := range d.sbrEls {
		if el != nil {
			el.resetForSeek()
		}
	}
}

// Release returns the output buffer to the pool (codec.Releaser).
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}
