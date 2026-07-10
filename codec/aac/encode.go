package aac

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/dsp/psy"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*Encoder)(nil)

// EncoderVersion identifies the encode algorithm revision for cache keys
// (ADR-0004). It composes the psychoacoustic model's revision: retuning
// dsp/psy changes these streams too.
const EncoderVersion = "aac-enc-1+" + psy.Version

// EncoderDelay is the codec priming in output samples: one frame of
// zeros ahead of the first real sample, so frame 0's MDCT window (which
// reaches one frame into the past) sees defined history. Carried in
// Trailer.Delay and the container's edit list.
const EncoderDelay = 1024

// frameLen is the AAC-LC long frame in samples per channel.
const frameLen = 1024

// DefaultBitrate is used when EncoderOptions.Bitrate is zero.
const DefaultBitrate = 128000

// thrCalib maps psy thresholds (FFT energy of unit-full-scale input)
// onto the encoder's MDCT energy scale: the analytic window/scale ratio
// (8/3: Hann FFT energy 3N/8 against the scale-2 sine-window MDCT's N)
// times the 32768 PCM scaling, squared.
const thrCalib = (8.0 / 3.0) * 32768 * 32768

// psyOffsetDB is the model's SNR-demand offset, the encoder's master
// quality tuning constant (positive demands lower thresholds and so
// more bits per band before the rate loop pushes back).
const psyOffsetDB = 0.0

// EncoderOptions configures NewEncoder. The zero value is the default.
type EncoderOptions struct {
	// Bitrate is the target in bits per second for the whole stream
	// (all channels), 0 for DefaultBitrate. AAC frames are inherently
	// variable-size; the encoder holds the long-term mean at the target
	// with a bit reservoir (ABR), which is what both fMP4 and ADTS
	// carry naturally.
	Bitrate int
}

// Encoder is an AAC-LC encoder producing raw access units (one packet
// per 1024-sample frame). The fMP4 muxer stores CodecConfig's
// AudioSpecificConfig; the ADTS muxer derives its header fields from it.
type Encoder struct {
	fmt      audio.Format
	channels int
	rate     int
	rateIdx  int
	bitrate  int
	asc      [2]byte

	swbLong     []uint16
	swbShort    []uint16
	numSwbLong  int
	numSwbShort int
	maxSfbLong  int
	maxSfbShort int

	// Input pipeline: pending holds not-yet-complete source blocks;
	// hist slides three whole blocks (m-2, m-1, m). AU m windows blocks
	// m-1 and m (hist[1024:3072]): decoder output block m is the FIRST
	// half of AU m's window, which puts the first real sample at output
	// position 1024, the declared EncoderDelay. Block m doubles as the
	// window-decision lookahead.
	pending   [2][]float32
	hist      [2][3 * frameLen]float32
	inSamples int64
	outFrames int64

	// Window decision state.
	det        [2]*psy.AttackDetector
	attackPrev [2]attackInfo // attack in the previous source block
	attackCur  [2]attackInfo // attack in the just-arrived source block
	prevSeq    int

	// Psychoacoustics, per channel.
	psyLong  [2]*psy.Model
	psyShort [2]*psy.Model

	// Rate control.
	meanBits  float64
	reservoir float64
	avgPE     float64

	// Per-frame scratch.
	spec  [2][1024]float64
	cq    [2]chanQuant
	tns   [2]tnsEnc
	thr   [2][maxWindowGroups][maxSFBCount]float64
	msUse [maxWindowGroups][maxSFBCount]bool
	w     bitWriter
}

type attackInfo struct {
	attack bool
	pos    int
}

// NewEncoder returns an Encoder for f, which must be float32 with 1 or
// 2 channels at one of the 13 AAC sampling rates.
func NewEncoder(f audio.Format, opts *EncoderOptions) (*Encoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if f.Type != audio.Float || f.BitDepth != 32 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "aac: encoder input must be float32")
	}
	if f.Channels < 1 || f.Channels > 2 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: %d channels unsupported (mono or stereo)", f.Channels))
	}
	rateIdx := samplingIndex(f.Rate)
	if rateIdx < 0 || rateIdx >= len(swbOffsetLong) {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: sample rate %d is not an AAC rate", f.Rate))
	}

	var o EncoderOptions
	if opts != nil {
		o = *opts
	}
	if o.Bitrate == 0 {
		o.Bitrate = DefaultBitrate
	}
	// Floor keeps the rate loop meaningful; the ceiling is the spec's
	// 6144-bit-per-channel decoder buffer drained at frame rate.
	minRate := 8000 * f.Channels
	maxRate := 6 * f.Rate * f.Channels
	bitrate := min(max(o.Bitrate, minRate), maxRate)

	e := &Encoder{
		fmt:      f,
		channels: f.Channels,
		rate:     f.Rate,
		rateIdx:  rateIdx,
		bitrate:  bitrate,
		prevSeq:  onlyLong,
	}
	e.asc[0] = byte(aotAACLC<<3 | rateIdx>>1)
	e.asc[1] = byte(rateIdx<<7 | f.Channels<<3)

	e.swbLong = swbOffsetLong[rateIdx]
	e.swbShort = swbOffsetShort[rateIdx]
	e.numSwbLong = swbCountLong(rateIdx)
	e.numSwbShort = swbCountShort(rateIdx)

	// Bandwidth cutoff: spending the budget below the cutoff beats
	// coding shaped noise at the top; the offsets scale with the
	// per-channel rate. maxSfb is the first band wholly past cutoff.
	cutoff := 3000.0 + float64(bitrate)/float64(f.Channels)/5
	cutoff = math.Min(cutoff, 0.94*float64(f.Rate)/2)
	e.maxSfbLong = coveringSfb(e.swbLong, e.numSwbLong, cutoff, f.Rate, 2048)
	e.maxSfbShort = coveringSfb(e.swbShort, e.numSwbShort, cutoff, f.Rate, 256)

	longBands := make([]int, e.numSwbLong+1)
	for i := range longBands {
		longBands[i] = int(e.swbLong[i])
	}
	shortBands := make([]int, e.numSwbShort+1)
	for i := range shortBands {
		shortBands[i] = int(e.swbShort[i])
	}
	for c := 0; c < f.Channels; c++ {
		var err error
		e.psyLong[c], err = psy.New(psy.Config{
			Rate: f.Rate, Lines: 1024, FFTSize: 2048,
			BandOffsets: longBands, OffsetDB: psyOffsetDB,
		})
		if err != nil {
			return nil, err
		}
		e.psyShort[c], err = psy.New(psy.Config{
			Rate: f.Rate, Lines: 128, FFTSize: 256,
			BandOffsets: shortBands, NoPredict: true, FixedC: 0.4,
			OffsetDB: psyOffsetDB,
		})
		if err != nil {
			return nil, err
		}
		e.det[c] = psy.NewAttackDetector(0)
	}

	e.meanBits = float64(bitrate) * frameLen / float64(f.Rate)
	e.avgPE = e.meanBits * 0.4 // settles onto real content within a few frames
	return e, nil
}

// coveringSfb returns the smallest max_sfb whose bands reach cutoff Hz
// (at least 1, at most numSwb). n is the full transform length.
func coveringSfb(swb []uint16, numSwb int, cutoff float64, rate, n int) int {
	lineHz := float64(rate) / float64(n)
	for sfb := 1; sfb <= numSwb; sfb++ {
		if float64(swb[sfb])*lineHz >= cutoff {
			return sfb
		}
	}
	return numSwb
}

// InputFormat implements codec.Encoder.
func (e *Encoder) InputFormat() audio.Format { return e.fmt }

// FrameSize implements codec.Encoder: 1024 samples per frame.
func (e *Encoder) FrameSize() int { return frameLen }

// Bitrate reports the clamped target bit rate the plan advertises.
func (e *Encoder) Bitrate() int { return e.bitrate }

// Delay reports the encoder priming in output samples.
func (e *Encoder) Delay() int { return EncoderDelay }

// CodecConfig returns the two-byte AudioSpecificConfig (AAC-LC, this
// stream's rate index and channel configuration).
func (e *Encoder) CodecConfig() []byte { return e.asc[:] }

// maxSample bounds accepted input magnitudes (nominal full scale is 1;
// the bound is far above any legitimate pipeline level). Non-finite
// samples become 0 and larger magnitudes clamp: unbounded spectra would
// leave the rate loop no fitting solution and break the 6144-bit-per-
// channel access-unit ceiling.
const maxSample = 8.0

// Encode buffers src and emits an access unit for every whole source
// block that becomes available.
func (e *Encoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	for c := 0; c < e.channels; c++ {
		e.pending[c] = appendSanitized(e.pending[c], src.ChanF(c)[:src.N])
	}
	e.inSamples += int64(src.N)
	for len(e.pending[0]) >= frameLen {
		if err := e.pushBlock(emit); err != nil {
			return err
		}
	}
	return nil
}

// appendSanitized appends src to dst with non-finite samples zeroed and
// magnitudes clamped to maxSample.
func appendSanitized(dst, src []float32) []float32 {
	for _, v := range src {
		switch {
		case math.IsNaN(float64(v)) || math.IsInf(float64(v), 0):
			v = 0
		case v > maxSample:
			v = maxSample
		case v < -maxSample:
			v = -maxSample
		}
		dst = append(dst, v)
	}
	return dst
}

// pushBlock consumes one whole source block from the FIFO and encodes
// the AU it completes: AU m needs blocks m-2 and m-1 for its window and
// block m's attack status for the LONG_START lookahead.
func (e *Encoder) pushBlock(emit func(codec.Packet) error) error {
	for c := 0; c < e.channels; c++ {
		h := &e.hist[c]
		copy(h[:2*frameLen], h[frameLen:])
		copy(h[2*frameLen:], e.pending[c][:frameLen])
		e.pending[c] = append(e.pending[c][:0], e.pending[c][frameLen:]...)
		e.attackPrev[c] = e.attackCur[c]
		a, pos := e.det[c].Scan(h[2*frameLen:], 8)
		e.attackCur[c] = attackInfo{attack: a, pos: pos}
	}
	return e.encodeFrame(emit)
}

// windowSeq runs the window-sequence state machine for the AU being
// encoded: shortNow reflects an attack inside its output block,
// shortNext one inside the next.
func (e *Encoder) windowSeq(shortNow, shortNext bool) int {
	switch {
	case shortNow:
		return eightShort
	case e.prevSeq == eightShort && shortNext:
		// Bridging short: the left overlap is short and the right must
		// be too; a plain long window cannot sit between two shorts.
		return eightShort
	case e.prevSeq == eightShort:
		return longStop
	case shortNext:
		return longStart
	default:
		return onlyLong
	}
}

// grouping maps a transient position (8ths of the previous source
// block, which is the first half of the AU's window) onto the
// short-window grouping: windows before the attack, the attack window
// alone, and the tail. Short window i spans window offsets
// [448+128i, 704+128i); an attack at block offset 128p+64 lands there
// around i = p-3.
func grouping(pos int) []int {
	win := pos - 3
	if win < 0 {
		win = 0
	}
	if win > 7 {
		win = 7
	}
	switch {
	case win == 0:
		return []int{1, 7}
	case win == 7:
		return []int{7, 1}
	default:
		return []int{win, 1, 7 - win}
	}
}

var longGroup = []int{1}

// encodeFrame encodes one access unit from the history window.
func (e *Encoder) encodeFrame(emit func(codec.Packet) error) error {
	shortNow := false
	shortNext := false
	attackPos := 0
	for c := 0; c < e.channels; c++ {
		if e.attackPrev[c].attack {
			if !shortNow {
				attackPos = e.attackPrev[c].pos
			}
			shortNow = true
		}
		shortNext = shortNext || e.attackCur[c].attack
	}
	seq := e.windowSeq(shortNow, shortNext)

	groupLen := longGroup
	swb := e.swbLong
	maxSfb := e.maxSfbLong
	if seq == eightShort {
		groupLen = grouping(attackPos)
		swb = e.swbShort
		maxSfb = e.maxSfbShort
	}

	// Psychoacoustics. The long model runs every frame to keep its
	// prediction history continuous; PE feeds the bit reservoir.
	pe := 0.0
	for c := 0; c < e.channels; c++ {
		rl, err := e.psyLong[c].Analyze(e.hist[c][frameLen : frameLen+2048])
		if err != nil {
			return err
		}
		pe = math.Max(pe, rl.PE)
		if seq != eightShort {
			for sfb := 0; sfb < e.numSwbLong; sfb++ {
				e.thr[c][0][sfb] = rl.Thr[sfb] * thrCalib
			}
			continue
		}
		// Short thresholds accumulate over each group's windows.
		var wThr [8][maxSFBCount]float64
		for i := 0; i < 8; i++ {
			off := frameLen + 448 + i*128
			rs, err := e.psyShort[c].Analyze(e.hist[c][off : off+256])
			if err != nil {
				return err
			}
			for sfb := 0; sfb < e.numSwbShort; sfb++ {
				wThr[i][sfb] = rs.Thr[sfb] * thrCalib
			}
		}
		win := 0
		for g, L := range groupLen {
			for sfb := 0; sfb < e.numSwbShort; sfb++ {
				t := 0.0
				for w := 0; w < L; w++ {
					t += wThr[win+w][sfb]
				}
				e.thr[c][g][sfb] = t
			}
			win += L
		}
	}

	// MDCT on the 32768-scaled block (the decoder normalizes by 1/32768).
	for c := 0; c < e.channels; c++ {
		var tblk [2048]float64
		for i := range tblk {
			tblk[i] = float64(e.hist[c][frameLen+i]) * 32768
		}
		mdctFrame(&tblk, seq, &e.spec[c])
	}

	// TNS per channel (long windows), before the stereo transform: the
	// decoder recombines M/S first and inverse-filters L/R after.
	for c := 0; c < e.channels; c++ {
		e.tns[c] = tnsEnc{}
		if seq != eightShort {
			e.tns[c] = analyzeTNS(&e.spec[c], e.swbLong, e.numSwbLong, maxSfb, e.rateIdx, e.rate)
		}
	}

	// M/S decision and transform (stereo only).
	msMask := 0
	if e.channels == 2 {
		msMask = e.decideMS(groupLen, swb, maxSfb)
	}

	// Band tables and thresholds feed the two-loop quantizer.
	for c := 0; c < e.channels; c++ {
		ch := c
		e.cq[c].buildBands(&e.spec[c], groupLen, swb, maxSfb,
			func(g, sfb int) float64 { return e.thr[ch][g][sfb] }, seq == eightShort)
	}

	// Frame bit budget: reservoir-smoothed, difficulty-modulated.
	e.avgPE = 0.95*e.avgPE + 0.05*pe
	difficulty := 1.0
	if e.avgPE > 0 {
		difficulty = min(max(pe/e.avgPE, 0.65), 1.7)
	}
	target := e.meanBits * difficulty
	target = math.Min(target, e.meanBits+math.Max(e.reservoir, 0)*0.5)
	target = math.Max(target, e.meanBits*0.3)
	target = math.Min(target, float64(6144*e.channels)*0.93)

	overhead := e.overheadBits(seq, maxSfb, msMask, len(groupLen))
	spectral := int(target) - overhead
	if spectral < 0 {
		spectral = 0
	}

	// Split the spectral budget by perceptual demand.
	if e.channels == 2 {
		dl, dr := e.cq[0].demand, e.cq[1].demand
		frac := 0.5
		if dl+dr > 0 {
			frac = min(max(dl/(dl+dr), 0.2), 0.8)
		}
		e.cq[0].quantizeChannel(int(float64(spectral) * frac))
		e.cq[1].quantizeChannel(spectral - int(float64(spectral)*frac))
	} else {
		e.cq[0].quantizeChannel(spectral)
	}

	// Assemble the access unit.
	e.w.reset()
	if e.channels == 2 {
		e.writeCPE(seq, groupLen, maxSfb, msMask)
	} else {
		e.writeSCE(seq, groupLen, maxSfb)
	}
	e.w.writeBits(3, elEND)
	e.w.align()

	e.reservoir += e.meanBits - float64(e.w.bitLen())
	if e.reservoir > float64(6144*e.channels) {
		e.reservoir = float64(6144 * e.channels)
	}
	if e.reservoir < -2*e.meanBits {
		e.reservoir = -2 * e.meanBits
	}

	e.prevSeq = seq
	e.outFrames++
	// Packets are borrowed (valid during emit only), so the writer's
	// buffer goes out directly.
	return emit(codec.Packet{Data: e.w.buf, PTS: (e.outFrames - 1) * frameLen, Dur: frameLen, Sync: true})
}

// decideMS chooses the per-band M/S mask by comparing the perceptual
// bit demand of L/R against M/S coding under the conservative shared
// threshold, transforms the chosen bands in place, and rewrites both
// channels' thresholds. Returns ms_mask_present (0, 1, or 2).
func (e *Encoder) decideMS(groupLen []int, swb []uint16, maxSfb int) int {
	all, none := true, true
	winBase := 0
	for g, L := range groupLen {
		for sfb := 0; sfb < maxSfb; sfb++ {
			lo, hi := int(swb[sfb]), int(swb[sfb+1])
			var eL, eR, eM, eS float64
			for w := 0; w < L; w++ {
				base := (winBase + w) * 128
				for k := lo; k < hi; k++ {
					l, r := e.spec[0][base+k], e.spec[1][base+k]
					eL += l * l
					eR += r * r
					m, s := (l+r)/2, (l-r)/2
					eM += m * m
					eS += s * s
				}
			}
			thrL, thrR := e.thr[0][g][sfb], e.thr[1][g][sfb]
			thrMS := math.Min(thrL, thrR) / 2
			w := float64(hi - lo)
			costLR := demandOf(eL, thrL, w) + demandOf(eR, thrR, w)
			costMS := demandOf(eM, thrMS, w) + demandOf(eS, thrMS, w)
			if costMS < costLR {
				e.msUse[g][sfb] = true
				none = false
			} else {
				e.msUse[g][sfb] = false
				all = false
			}
		}
		winBase += L
	}
	if none {
		return 0
	}
	// Transform the chosen bands and install the shared thresholds.
	winBase = 0
	for g, L := range groupLen {
		for sfb := 0; sfb < maxSfb; sfb++ {
			if !e.msUse[g][sfb] {
				continue
			}
			thrMS := math.Min(e.thr[0][g][sfb], e.thr[1][g][sfb]) / 2
			e.thr[0][g][sfb] = thrMS
			e.thr[1][g][sfb] = thrMS
			lo, hi := int(swb[sfb]), int(swb[sfb+1])
			for w := 0; w < L; w++ {
				base := (winBase + w) * 128
				for k := lo; k < hi; k++ {
					l, r := e.spec[0][base+k], e.spec[1][base+k]
					e.spec[0][base+k] = (l + r) / 2
					e.spec[1][base+k] = (l - r) / 2
				}
			}
		}
		winBase += L
	}
	if all {
		return 2
	}
	return 1
}

// demandOf is the perceptual bit demand of one band: information above
// the masking threshold.
func demandOf(energy, thr, width float64) float64 {
	if thr <= 0 || energy <= thr {
		return 0
	}
	return width * math.Log2(energy/thr)
}

// overheadBits counts every non-spectral bit of the frame so the rate
// loop budgets exactly: element headers, ics_info, the M/S mask, TNS
// presence and data, and the END element with byte alignment slack.
func (e *Encoder) overheadBits(seq, maxSfb, msMask, groups int) int {
	ics := 1 + 2 + 1 // reserved + sequence + shape
	if seq == eightShort {
		ics += 4 + 7
	} else {
		ics += 6 + 1
	}
	perChan := 8 + 1 + 1 + 1 // global_gain + pulse + tns present + gain control
	total := 3 + 4           // element id + instance tag
	if e.channels == 2 {
		total += 1 + ics + 2 // common_window + shared ics_info + ms_mask_present
		if msMask == 1 {
			total += groups * maxSfb
		}
		total += 2 * perChan
	} else {
		total += ics + perChan
	}
	for c := 0; c < e.channels; c++ {
		total += e.tns[c].sideBits()
	}
	total += 3 + 7 // END + worst-case alignment
	return total
}

// writeICSBody emits one channel's individual_channel_stream:
// global_gain, then ics_info when the window is not shared (SCE), then
// sections, scalefactors, pulse/TNS/gain flags, and spectra. The order
// matches decodeChannelData: global_gain comes FIRST.
func (e *Encoder) writeICSBody(c int, info func()) {
	cq := &e.cq[c]
	w := &e.w
	w.writeBits(8, uint64(cq.globalGain))
	if info != nil {
		info()
	}
	// section_data
	lenEsc := uint64(1)<<uint(cq.lenBits) - 1
	for g := 0; g < cq.nGroups; g++ {
		k := 0
		for k < cq.maxSfb {
			cb := cq.bands[g*cq.maxSfb+k].cb
			run := 1
			for k+run < cq.maxSfb && cq.bands[g*cq.maxSfb+k+run].cb == cb {
				run++
			}
			w.writeBits(4, uint64(cb))
			l := run
			for l >= int(lenEsc) {
				w.writeBits(uint(cq.lenBits), lenEsc)
				l -= int(lenEsc)
			}
			w.writeBits(uint(cq.lenBits), uint64(l))
			k += run
		}
	}
	// scale_factor_data
	prev := cq.globalGain
	for g := 0; g < cq.nGroups; g++ {
		for k := 0; k < cq.maxSfb; k++ {
			b := &cq.bands[g*cq.maxSfb+k]
			if b.cb == 0 {
				continue
			}
			e.w.writeSFDelta(b.sf - prev)
			prev = b.sf
		}
	}
	w.writeBits(1, 0) // pulse_data_present
	if e.tns[c].present {
		w.writeBits(1, 1)
		e.tns[c].write(w)
	} else {
		w.writeBits(1, 0)
	}
	w.writeBits(1, 0) // gain_control_data_present
	// spectral_data: per group, tuples across each equal-codebook section.
	var vbuf [1024]int
	for g := 0; g < cq.nGroups; g++ {
		for k := 0; k < cq.maxSfb; {
			b := &cq.bands[g*cq.maxSfb+k]
			run := 1
			for k+run < cq.maxSfb && cq.bands[g*cq.maxSfb+k+run].cb == b.cb {
				run++
			}
			if b.cb != 0 {
				n := 0
				for j := 0; j < run; j++ {
					bb := &cq.bands[g*cq.maxSfb+k+j]
					for i := 0; i < bb.n; i++ {
						vbuf[n] = cq.q[bb.off+i]
						n++
					}
				}
				w.writeSpecRun(b.cb, vbuf[:n])
			}
			k += run
		}
	}
}

// writeICSInfo emits ics_info for the frame's window configuration.
func (e *Encoder) writeICSInfo(seq, maxSfb int, groupLen []int) {
	w := &e.w
	w.writeBits(1, 0) // ics_reserved
	w.writeBits(2, uint64(seq))
	w.writeBits(1, shapeSine)
	if seq == eightShort {
		w.writeBits(4, uint64(maxSfb))
		// scale_factor_grouping: bit i set means window i+1 shares
		// window i's group.
		bits := uint64(0)
		win := 0
		for _, L := range groupLen {
			for j := 1; j < L; j++ {
				bits |= 1 << uint(6-(win+j-1))
			}
			win += L
		}
		w.writeBits(7, bits)
	} else {
		w.writeBits(6, uint64(maxSfb))
		w.writeBits(1, 0) // predictor_data_present
	}
}

func (e *Encoder) writeSCE(seq int, groupLen []int, maxSfb int) {
	e.w.writeBits(3, elSCE)
	e.w.writeBits(4, 0) // element_instance_tag
	e.writeICSBody(0, func() { e.writeICSInfo(seq, maxSfb, groupLen) })
}

func (e *Encoder) writeCPE(seq int, groupLen []int, maxSfb, msMask int) {
	e.w.writeBits(3, elCPE)
	e.w.writeBits(4, 0)
	e.w.writeBits(1, 1) // common_window
	e.writeICSInfo(seq, maxSfb, groupLen)
	e.w.writeBits(2, uint64(msMask))
	if msMask == 1 {
		for g := range groupLen {
			for sfb := 0; sfb < maxSfb; sfb++ {
				v := uint64(0)
				if e.msUse[g][sfb] {
					v = 1
				}
				e.w.writeBits(1, v)
			}
		}
	}
	e.writeICSBody(0, nil)
	e.writeICSBody(1, nil)
}

// Finish pads the tail to a whole block, encodes it, then encodes one
// final block so every real sample is covered by two overlapping
// windows, and reports the gapless trailer.
func (e *Encoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	if n := len(e.pending[0]); n > 0 {
		for c := 0; c < e.channels; c++ {
			e.pending[c] = append(e.pending[c], make([]float32, frameLen-n)...)
		}
		if err := e.pushBlock(emit); err != nil {
			return codec.Trailer{}, err
		}
	}
	for c := 0; c < e.channels; c++ {
		e.pending[c] = append(e.pending[c][:0], make([]float32, frameLen)...)
	}
	if err := e.pushBlock(emit); err != nil {
		return codec.Trailer{}, err
	}
	for c := 0; c < e.channels; c++ {
		e.pending[c] = e.pending[c][:0]
	}
	delay := int64(EncoderDelay)
	padding := e.outFrames*frameLen - e.inSamples - delay
	if padding < 0 {
		padding = 0
	}
	return codec.Trailer{Samples: e.inSamples, Delay: delay, Padding: padding}, nil
}
