package opus

// Top-level Opus decoder: it splits each packet into frames (opus.go), then runs
// each frame through the SILK, CELT, or hybrid path on one shared range decoder,
// mirroring libopus src/opus_decoder.c opus_decode_frame.
// SILK fills the low band, CELT the high band (start band 17) for hybrid, and
// their outputs sum. Output is planar float32 at 48 kHz.

import (
	"math"
	"math/bits"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// maxFrameSamples is the largest single Opus frame at 48 kHz (SILK 60 ms).
// Version is the decoder's cache-key version constant (ADR-0004): bump on
// any change that alters decoded samples. Revision 2: the CELT inverse
// MDCT's DFT moved from a float64 direct form to the float32 dsp/fft
// kernel (M18).
const Version = "opus-dec-2"

const maxFrameSamples = 2880

// CELT sub-frame sizes at 48 kHz used by the redundancy/transition logic
// (libopus opus_decoder.c F5/F2_5): a redundant frame is 5 ms, a crossfade
// spans 2.5 ms.
const (
	celtF5   = 240 // 5 ms redundant frame
	celtF2_5 = 120 // 2.5 ms crossfade overlap (== celtOverlap)
)

// Decoder decodes an Opus stream to 48 kHz planar float32 (codec.Decoder).
type Decoder struct {
	cfg            Config
	fmt            audio.Format
	celt           *celtDecoder
	silk           *silkDecoder
	prevMode       int
	prevRedundancy bool // previous frame ended with a SILK->CELT redundant frame
	gainMult       float32

	silkBuf [2][]int16   // per API channel, internal SILK output before float
	redCh   [][]float32  // redundant-frame CELT output views, per API channel
	redBuf  [2][]float32 // backing for redCh, celtF5 samples each
	out     *audio.Buffer
	outCh   [][]float32
}

// NewDecoder returns a decoder for a parsed OpusHead Config. The track format
// must be what Config.Format produces (48 kHz float32).
func NewDecoder(cfg Config, f audio.Format) (*Decoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.Channels != cfg.Channels || f.Rate != SampleRate {
		return nil, malformed("track format %v does not match Opus %dch", f, cfg.Channels)
	}
	// One elementary Opus stream carries at most two channels; more requires
	// multistream de-framing (RFC 7845 family 1 with several streams), which
	// this decoder does not implement. The per-channel state below is sized
	// for exactly this bound.
	if cfg.Channels > 2 {
		return nil, malformed("%d-channel Opus needs multistream decoding (mono and stereo only)", cfg.Channels)
	}
	d := &Decoder{
		cfg:      cfg,
		fmt:      f,
		celt:     newCELTDecoder(cfg.Channels),
		silk:     newSILKDecoder(),
		prevMode: -1,
		gainMult: float32(math.Exp2(6.48814081e-4 * float64(cfg.Gain))),
	}
	for c := 0; c < cfg.Channels; c++ {
		d.silkBuf[c] = make([]int16, maxFrameSamples)
		d.redBuf[c] = make([]float32, celtF5)
	}
	d.out = audio.Get(f, maxFrameSamples)
	d.outCh = make([][]float32, cfg.Channels)
	d.redCh = make([][]float32, cfg.Channels)
	return d, nil
}

// Decode decodes one Opus packet, emitting one buffer per contained frame.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	frames, err := splitPacket(pkt)
	if err != nil {
		return err
	}
	for i := range frames {
		fr := frames[i]
		n := fr.cfg.frameSize
		d.out.N = n
		for c := 0; c < d.cfg.Channels; c++ {
			d.outCh[c] = d.out.ChanF(c)
		}
		if err := d.decodeFrame(fr, d.outCh); err != nil {
			return err
		}
		if d.gainMult != 1 {
			for c := 0; c < d.cfg.Channels; c++ {
				ch := d.outCh[c]
				for j := 0; j < n; j++ {
					ch[j] *= d.gainMult
				}
			}
		}
		if err := emit(d.out); err != nil {
			return err
		}
	}
	return nil
}

// endBandFor maps a bandwidth to the CELT end band (libopus opus_decoder.c).
func endBandFor(bw int) int {
	switch bw {
	case bandNB:
		return 13
	case bandMB, bandWB:
		return 17
	case bandSWB:
		return 19
	default:
		return 21
	}
}

// decodeFrame decodes one Opus frame into out[ch][0:frameSize] at 48 kHz,
// mirroring libopus opus_decode_frame (opus_decoder.c). It stitches the SILK low
// band, the CELT high/full band, and the short redundant CELT frames used to
// smooth mode transitions. The FLAG_DECODE_NORMAL path only: PLC/FEC/DTX are out
// of scope (RFC 6716 file decode), so the CELT-only<->SILK "transition" crossfade
// (which libopus fills from a PLC frame) is not synthesized; redundancy handling,
// which is mutually exclusive with it and fully in-band, is.
func (d *Decoder) decodeFrame(fr frame, out [][]float32) error {
	mode := fr.cfg.mode
	bw := fr.cfg.bandwidth
	audiosize := fr.cfg.frameSize
	end := endBandFor(bw)
	C := 1
	if fr.stereo {
		C = 2
	}
	CC := d.cfg.Channels

	for c := 0; c < CC; c++ {
		clear(out[c][:audiosize])
	}

	dec := newRangeDecoder(fr.data)

	// SILK / hybrid low band, decoded into silkBuf (summed into out below, after
	// any CELT overwrite, matching libopus's pcm_silk accumulation order).
	if mode != modeCELT {
		if d.prevMode == modeCELT {
			d.silk.reset()
		}
		d.silk.channel[0].nFramesDecoded = 0
		d.silk.channel[1].nFramesDecoded = 0
		internalRate := 16000
		if mode == modeSILK {
			switch bw {
			case bandNB:
				internalRate = 8000
			case bandMB:
				internalRate = 12000
			}
		}
		payloadMs := max(10, 1000*audiosize/SampleRate)
		ctrl := silkControl{payloadMs, internalRate, C, CC, SampleRate}
		decoded := 0
		for decoded < audiosize {
			subOut := make([][]int16, CC)
			for c := 0; c < CC; c++ {
				subOut[c] = d.silkBuf[c][decoded:]
			}
			n := d.silk.decode(dec, ctrl, subOut)
			if n <= 0 {
				return malformed("silk produced no samples")
			}
			decoded += n
		}
	}

	// Redundancy signalling (opus_decode_frame): after SILK, a flag may reserve a
	// short CELT "redundancy" frame at the tail of the packet, used to bridge a
	// mode transition. celtToSilk picks which end of this frame it crossfades.
	celtLen := len(fr.data)
	redundancy := false
	celtToSilk := false
	redundancyBytes := 0
	startBand := 0
	if mode != modeCELT {
		extra := 0
		if mode == modeHybrid {
			extra = 20
		}
		if dec.tell()+17+extra <= 8*celtLen {
			if mode == modeHybrid {
				redundancy = dec.decodeBitLogp(12) != 0
			} else {
				redundancy = true
			}
			if redundancy {
				celtToSilk = dec.decodeBitLogp(1) != 0
				if mode == modeHybrid {
					redundancyBytes = int(dec.decodeUint(256)) + 2
				} else {
					redundancyBytes = celtLen - (dec.tell()+7)/8
				}
				celtLen -= redundancyBytes
				// Sanity check (opus_decode_frame): behaviour is non-normative
				// for an invalid packet, but must not read out of bounds.
				if celtLen*8 < dec.tell() || celtLen < 0 || redundancyBytes < 0 ||
					celtLen+redundancyBytes > len(fr.data) {
					celtLen = 0
					redundancyBytes = 0
					redundancy = false
				} else {
					dec.storage -= redundancyBytes
				}
			}
		}
		startBand = 17
	}
	redData := fr.data[celtLen : celtLen+redundancyBytes]

	// 5 ms redundant frame for a CELT->SILK transition is decoded first, using
	// the (possibly stale) CELT state, so its content can crossfade the START of
	// this frame's output.
	if redundancy && celtToSilk {
		d.redViews()
		if err := d.celt.celtDecode(redData, 1, C, 0, end, d.redCh); err != nil {
			return err
		}
	}

	// CELT high band (hybrid) or full band (CELT-only), overwriting out. The
	// previous CELT state is discarded on a mode change UNLESS the previous
	// frame ended with a SILK->CELT redundant frame, whose decode primed the
	// CELT state this frame continues from (opus_decode_frame).
	if mode != modeSILK {
		if mode != d.prevMode && d.prevMode != -1 && !d.prevRedundancy {
			d.celt.Reset()
		}
		LM := bits.Len(uint(audiosize/celtShortMDCTSize)) - 1
		if err := d.celt.celtDecodeInner(dec, fr.data[:celtLen], LM, C, startBand, end, out); err != nil {
			return err
		}
	} else if d.prevMode == modeHybrid && !(redundancy && celtToSilk && d.prevRedundancy) {
		// Hybrid->SILK: run a 2.5 ms CELT silence frame so the previous hybrid
		// high band fades out through the MDCT overlap (not PLC; decodes 0xFFFF).
		d.celt.celtDecode(silenceCELT[:], 0, C, 0, end, out)
	}

	// Sum the SILK low band onto the CELT output (opus_decode_frame).
	if mode != modeCELT {
		for c := 0; c < CC; c++ {
			oc := out[c]
			sc := d.silkBuf[c]
			for i := 0; i < audiosize; i++ {
				oc[i] += float32(sc[i]) * (1.0 / 32768.0)
			}
		}
	}

	// 5 ms redundant frame for a SILK->CELT transition is decoded fresh (reset
	// CELT state) after the main frame, and crossfaded onto its END.
	if redundancy && !celtToSilk {
		d.celt.Reset()
		d.redViews()
		if err := d.celt.celtDecode(redData, 1, C, 0, end, d.redCh); err != nil {
			return err
		}
		for c := 0; c < CC; c++ {
			smoothFade(out[c][audiosize-celtF2_5:], d.redCh[c][celtF2_5:], out[c][audiosize-celtF2_5:], d.celt.window)
		}
	}

	// Crossfade the CELT->SILK redundant frame onto the START of the output. It
	// is skipped (but was still decoded, for state) when the previous frame gave
	// the CELT decoder no valid history to continue from.
	if redundancy && celtToSilk && (d.prevMode != modeSILK || d.prevRedundancy) {
		for c := 0; c < CC; c++ {
			copy(out[c][:celtF2_5], d.redCh[c][:celtF2_5])
			smoothFade(d.redCh[c][celtF2_5:], out[c][celtF2_5:], out[c][celtF2_5:], d.celt.window)
		}
	}

	d.prevMode = mode
	d.prevRedundancy = redundancy && !celtToSilk
	return nil
}

// silenceCELT is the two-byte "silence" CELT frame libopus decodes to fade a
// hybrid high band out through the MDCT overlap on a hybrid->SILK transition.
var silenceCELT = [2]byte{0xFF, 0xFF}

// redViews points redCh at the per-channel redundant-frame backing slices.
func (d *Decoder) redViews() {
	for c := 0; c < d.cfg.Channels; c++ {
		d.redCh[c] = d.redBuf[c]
	}
}

// smoothFade crossfades from in1 to in2 across one CELT overlap window into out
// (libopus smooth_fade; single channel, 48 kHz so the window is read directly).
// out may alias in2. w rises 0->1 so the result fades in1 out and in2 in.
func smoothFade(in1, in2, out []float32, window []float64) {
	for i := 0; i < celtF2_5; i++ {
		w := float32(window[i] * window[i])
		out[i] = w*in2[i] + (1-w)*in1[i]
	}
}

// Drain flushes decoder latency. Opus carries no cross-packet decoder delay
// beyond what the container's pre-skip/end-trim already handle, so there is
// nothing to flush at end of stream.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset discards inter-frame state after a seek so the next packet primes.
func (d *Decoder) Reset() {
	d.celt.Reset()
	d.silk.reset()
	d.prevMode = -1
}

// Release returns the pooled output buffer.
func (d *Decoder) Release() {
	if d.out != nil {
		audio.Put(d.out)
		d.out = nil
	}
}

// reset re-initializes the SILK decoder (silk_ResetDecoder).
func (d *silkDecoder) reset() {
	d.channel[0].reset()
	d.channel[1].reset()
	d.sStereo = stereoState{}
	d.prevDecodeOnlyMiddle = 0
	d.nChannelsAPI = 0
	d.nChannelsInternal = 0
}

var _ codec.Decoder = (*Decoder)(nil)
