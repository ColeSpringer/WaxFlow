package aac

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

var _ codec.Encoder = (*HEEncoder)(nil)

// HEEncoderVersion identifies the HE-AAC encode revision for cache keys
// (ADR-0004). It composes the LC core's full revision (EncoderVersion,
// which itself carries the psychoacoustic model's: an aac-enc bump
// changes these streams too, and omitting it would serve stale HE bytes
// after core-encoder work rotates only the aac keys) and the resampler's
// (the 2:1 core feed is part of the signal path).
var HEEncoderVersion = "aac-heenc-1+" + EncoderVersion + "+" + resample.HQ.Version()

// HEV2EncoderVersion is the HE-AAC v2 revision. It composes the whole v1
// revision because a v2 stream is the v1 machinery over the downmix: any
// change that rotates the v1 key changes v2 bytes too.
var HEV2EncoderVersion = "aac-hev2enc-1+" + HEEncoderVersion

// HEEncoderDelay is the encoder priming in output samples: the core's
// one-frame priming (1024 core samples, 2048 here) plus the decoder-side
// SBR chain, which is architectural rather than tunable: the envelope
// timeline trails the core by six QMF slots (6*64) and the QMF
// analysis+synthesis pair adds its pinned 578 samples. The delay
// measurement test freezes this against both our decoder and ffmpeg's.
const HEEncoderDelay = 2048 + 6*64 + 578

// HEDefaultBitrate is used when EncoderOptions.Bitrate is zero: 64 kb/s,
// the classic HE-AAC v1 stereo operating point.
const HEDefaultBitrate = 64000

// HEV2DefaultBitrate is the zero-Bitrate default under ParametricStereo:
// 32 kb/s, the classic HE-AAC v2 operating point.
const HEV2DefaultBitrate = 32000

// heOutputRates are the accepted output rates: the ones whose half rate
// has scalefactor-band tables and whose SBR band structure derives
// cleanly. The engine's he-aac row snaps other source rates onto this
// set (HESnapRate); only an explicit conflicting rate request reaches
// the refusal.
var heOutputRates = [...]int{32000, 44100, 48000}

// HESnapRate maps a source rate onto the accepted HE-AAC output rate a
// rateless request converts to. Accepted rates pass through. Low rates
// take their smallest accepted integer multiple and high rates their
// 44100-family member when they have one, keeping the resample ratio in
// the source's own rational family (22050 -> 44100, 16000 -> 32000,
// 88200 -> 44100); everything else takes 48000, the widest band.
func HESnapRate(rate int) int {
	for _, r := range heOutputRates {
		if r == rate {
			return rate
		}
	}
	if rate > 48000 {
		if rate%44100 == 0 {
			return 44100
		}
		return 48000
	}
	for _, r := range heOutputRates {
		if rate > 0 && r%rate == 0 {
			return r
		}
	}
	return 48000
}

// hePlanParams is the shared plan-time half of NewHEEncoder: every
// validation and derived parameter, none of the allocations (the
// analysis slot rings alone are ~190 KB for a stereo v1 element, ~290 KB
// with the v2 front end, and the plan path runs per request).
func hePlanParams(f audio.Format, opts *EncoderOptions) (bitrate int, hdr sbrHeader, tbl sbrFreqTables, err error) {
	if err := f.Valid(); err != nil {
		return 0, hdr, tbl, err
	}
	if f.Type != audio.Float || f.BitDepth != 32 {
		return 0, hdr, tbl, waxerr.New(waxerr.CodeUnsupportedFormat, "aac: encoder input must be float32")
	}
	if f.Channels < 1 || f.Channels > 2 {
		return 0, hdr, tbl, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: %d channels unsupported (mono or stereo)", f.Channels))
	}
	ok := false
	for _, r := range heOutputRates {
		ok = ok || r == f.Rate
	}
	if !ok {
		return 0, hdr, tbl, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: sample rate %d has no HE-AAC form (32000, 44100, 48000)", f.Rate))
	}
	var o EncoderOptions
	if opts != nil {
		o = *opts
	}
	// The message names the encode, not the source: f is the resolved
	// chain format, so mono here can as easily be a stereo source under a
	// requested channel downmix as a mono file.
	if o.ParametricStereo && f.Channels != 2 {
		return 0, hdr, tbl, waxerr.New(waxerr.CodeUnsupportedFormat,
			"aac: HE-AAC v2 codes a stereo pair and this encode resolves to mono; drop the mono conversion or encode HE-AAC v1 instead")
	}
	if o.Bitrate == 0 {
		o.Bitrate = HEDefaultBitrate
		if o.ParametricStereo {
			o.Bitrate = HEV2DefaultBitrate
		}
	}
	// The same clamp the core applies at its (half) rate. Under v2 the
	// core is a mono element, so the clamp and the crossover budget run
	// single-channel whatever the input width. The crossover deliberately
	// sees the GROSS bitrate, ps_data cost included: netting a nominal
	// 3 kb/s out of it was tried and measured WORSE at both gate points
	// (the lower crossover starves the parametric band everywhere the
	// high band matters, bright-tonal fell from above fdk to saturation,
	// while the core-bound transient cells barely moved).
	coreRate := f.Rate / 2
	coreCh := heCoreChans(f, o)
	bitrate = min(max(o.Bitrate, 8000*coreCh), 6*coreRate*coreCh)
	hdr, tbl, err = chooseSBRHeader(f.Rate, bitrate, coreCh)
	return bitrate, hdr, tbl, err
}

// heCoreChans is the SBR/core element's channel count for an encode:
// mono under ParametricStereo (the PS front end folds the pair), the
// input width otherwise.
func heCoreChans(f audio.Format, o EncoderOptions) int {
	if o.ParametricStereo {
		return 1
	}
	return f.Channels
}

// HEPlanBitrate validates an HE-AAC encode configuration and reports the
// clamped target bit rate, guaranteeing NewHEEncoder succeeds on the
// same inputs, without paying its allocations. This is what an engine
// plan calls per request.
func HEPlanBitrate(f audio.Format, opts *EncoderOptions) (int, error) {
	bitrate, _, _, err := hePlanParams(f, opts)
	return bitrate, err
}

// HEEncoder is an HE-AAC v1 or v2 encoder producing raw access units of
// 2048 output samples: an AAC-LC core running at half rate on a delay-
// compensated 2:1 resampled feed, with the SBR envelope data measured
// from the full-rate input in the decoder's own 64-band QMF domain and
// carried as an in-band fill extension before each AU's END element
// (which is also ADTS's implicit signalling). Under ParametricStereo the
// stereo input first passes a delay-compensated PS front end (analysis,
// ps_data extraction, phase-aligned active downmix, synthesis back to
// time), and the whole v1 chain then runs mono over the downmix, so v2
// shares v1's timing contract and its declared delay. The fMP4 muxer
// stores the hierarchical AOT-5 (or AOT-29) AudioSpecificConfig; the
// ADTS muxer derives its header from the core layer.
type HEEncoder struct {
	fmt  audio.Format
	core *Encoder
	sbr  *sbrEnc
	res  *resample.Resampler
	asc  []byte

	inSamples int64
	outFrames int64

	slotStage [2][]float32 // per-channel staging for whole analysis slots
	clean     [2][]float32 // sanitized input scratch
	coreBuf   *audio.Buffer

	// v2 front-end state. psIn counts the source samples accepted (the
	// trailer's Samples; inSamples counts the aligned downmix fed to the
	// core path, which trails by the QMF pair delay until Finish evens
	// them up). dmSkip is the alignment discard still owed, dmStage the
	// per-channel slot staging, dmBuf the aligned mono awaiting the core.
	psIn    int64
	dmSkip  int
	dmStage [2][]float32
	dmBuf   []float32

	// finished guards the one-shot contract: Encode after Finish would
	// feed a drained resampler (a panic on the v1 path) and a second
	// Finish would report a trailer padded past the stream, so both
	// answer with a misuse error instead.
	finished bool
}

// NewHEEncoder returns an HEEncoder for f, which must be float32 with 1
// or 2 channels at one of the accepted output rates (exactly 2 under
// ParametricStereo).
func NewHEEncoder(f audio.Format, opts *EncoderOptions) (*HEEncoder, error) {
	bitrate, hdr, tbl, err := hePlanParams(f, opts)
	if err != nil {
		return nil, err
	}
	var o EncoderOptions
	if opts != nil {
		o = *opts
	}
	v2 := o.ParametricStereo
	coreRate := f.Rate / 2
	coreCh := heCoreChans(f, o)
	sbr := newSBREncFrom(coreCh, hdr, tbl)
	coreFmt := f
	coreFmt.Rate = coreRate
	if v2 {
		coreFmt.Channels = 1
		coreFmt.Layout = audio.DefaultLayout(1)
	}
	core, err := NewEncoder(coreFmt, &EncoderOptions{Bitrate: bitrate})
	if err != nil {
		return nil, err
	}
	// The core codes exactly up to the SBR crossover: the patch takes
	// over at kx, so spending core bits past it would code a band the
	// decoder replaces (kx is in the 64-band grid at the output rate,
	// whose band width is coreRate/64).
	xoverHz := float64(sbr.tbl.kx) * float64(coreRate) / 64
	core.maxSfbLong = coveringSfb(core.swbLong, core.numSwbLong, xoverHz, coreRate, 2048)
	core.maxSfbShort = coveringSfb(core.swbShort, core.numSwbShort, xoverHz, coreRate, 256)
	core.sbr = sbr

	res, err := resample.New(f.Rate, coreRate, coreCh, resample.HQ)
	if err != nil {
		return nil, err
	}
	asc, err := BuildHEASC(coreRate, coreCh, v2)
	if err != nil {
		return nil, err
	}
	e := &HEEncoder{fmt: f, core: core, sbr: sbr, res: res, asc: asc}
	if v2 {
		sbr.ps = &psEnc{}
		e.dmSkip = qmfEncPairDelay
	}
	return e, nil
}

// InputFormat implements codec.Encoder: the full-rate float32 format.
func (e *HEEncoder) InputFormat() audio.Format { return e.fmt }

// FrameSize implements codec.Encoder: 2048 output samples per frame.
func (e *HEEncoder) FrameSize() int { return 2 * frameLen }

// Bitrate reports the clamped target bit rate the plan advertises.
func (e *HEEncoder) Bitrate() int { return e.core.bitrate }

// Delay reports the encoder priming in output samples.
func (e *HEEncoder) Delay() int { return HEEncoderDelay }

// CodecConfig returns the hierarchical AudioSpecificConfig: AOT-5 for
// v1, AOT-29 over the mono core under ParametricStereo.
func (e *HEEncoder) CodecConfig() []byte { return e.asc }

// emitWrap rescales the core's packet timing into the output timeline:
// one AU is 2048 samples at the extension rate.
func (e *HEEncoder) emitWrap(emit func(codec.Packet) error) func(codec.Packet) error {
	return func(p codec.Packet) error {
		e.outFrames++
		p.PTS *= 2
		p.Dur *= 2
		return emit(p)
	}
}

// Encode buffers src, analyzes it into the SBR slot ring, and feeds the
// 2:1 resampled core, emitting an access unit per completed core block.
//
// Input of any length is accepted: it is processed internally in
// frame-sized steps, because the analysis slot rings hold a bounded
// lookback (the live spans and their derivation live on sbrEncRing and
// psEncRing) and analyzing a large buffer in one go before the core
// drained it would overwrite slots a pending AU's extraction still
// needs; the readers' ringGuard panics if that invariant ever breaks.
// The engine's framer happens to deliver exactly frame-sized chunks, but
// this is an exported method and the bound is enforced here, not assumed.
func (e *HEEncoder) Encode(src *audio.Buffer, emit func(codec.Packet) error) error {
	if e.finished {
		return waxerr.New(waxerr.CodeInternal, "aac: Encode after Finish on an HE-AAC encoder")
	}
	if src.Fmt != e.fmt {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("aac: encode input %v disagrees with %v", src.Fmt, e.fmt))
	}
	ch := e.fmt.Channels
	for off := 0; off < src.N; {
		n := min(2*frameLen, src.N-off)
		for c := 0; c < ch; c++ {
			e.clean[c] = appendSanitized(e.clean[c][:0], src.ChanF(c)[off:off+n])
		}
		if e.sbr.ps != nil {
			e.psIn += int64(n)
			if err := e.frontEnd(emit); err != nil {
				return err
			}
		} else if err := e.consume(e.clean[:ch], emit); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// consume runs one chunk of the core-feed signal (per-channel slices of
// equal length, at most a frame plus a slot) through the slot ring and
// the resampled core: the shared back half of the v1 path and the v2
// front end's output.
func (e *HEEncoder) consume(chans [][]float32, emit func(codec.Packet) error) error {
	ch := e.sbr.chans
	e.inSamples += int64(len(chans[0]))

	// QMF analysis first: by the time the resampled core completes
	// block m (which needs input past sample 2048(m+1)), every slot
	// the AU's SBR payload reads (below abs slot 32m) has been pushed.
	for c := 0; c < ch; c++ {
		e.slotStage[c] = append(e.slotStage[c], chans[c]...)
	}
	pos := 0
	for pos+64 <= len(e.slotStage[0]) {
		for c := 0; c < ch; c++ {
			e.sbr.pushSlot(c, e.slotStage[c][pos:pos+64])
		}
		pos += 64
	}
	for c := 0; c < ch; c++ {
		e.slotStage[c] = append(e.slotStage[c][:0], e.slotStage[c][pos:]...)
	}
	return e.feedCore(chans, emit)
}

// pushDM runs one whole slot through the PS analysis and downmix and
// appends whatever cleared the alignment discard to the mono FIFO.
func (e *HEEncoder) pushDM(l, r []float32) {
	var tmp [64]float32
	e.sbr.ps.pushSlot(l, r, tmp[:])
	if e.dmSkip >= 64 {
		e.dmSkip -= 64
		return
	}
	e.dmBuf = append(e.dmBuf, tmp[e.dmSkip:]...)
	e.dmSkip = 0
}

// frontEnd pushes the sanitized stereo chunk in e.clean through the PS
// analysis and downmix, trims the QMF pair delay off the synthesized
// mono, and feeds whatever aligned samples came out to the core path.
func (e *HEEncoder) frontEnd(emit func(codec.Packet) error) error {
	for c := range 2 {
		e.dmStage[c] = append(e.dmStage[c], e.clean[c]...)
	}
	pos := 0
	for pos+64 <= len(e.dmStage[0]) {
		e.pushDM(e.dmStage[0][pos:pos+64], e.dmStage[1][pos:pos+64])
		pos += 64
	}
	for c := range 2 {
		e.dmStage[c] = append(e.dmStage[c][:0], e.dmStage[c][pos:]...)
	}
	return e.drainDM(emit)
}

// drainDM feeds the aligned mono FIFO to the core path in frame-sized
// steps (the slot ring holds bounded lookback, the same rule Encode
// states for its own input). Consumed samples leave the FIFO on the
// error path too, so an emit failure cannot leave them queued for a
// second feed.
func (e *HEEncoder) drainDM(emit func(codec.Packet) error) error {
	for len(e.dmBuf) > 0 {
		n := min(2*frameLen, len(e.dmBuf))
		mono := [1][]float32{e.dmBuf[:n]}
		err := e.consume(mono[:], emit)
		e.dmBuf = e.dmBuf[:copy(e.dmBuf, e.dmBuf[n:])]
		if err != nil {
			return err
		}
	}
	return nil
}

// flushFrontEnd realigns the tail: the front end's synthesis trails the
// input by the QMF pair delay, so Finish pushes the staged partial slot
// and then zero slots until every accepted source sample has come out
// the aligned side, trimming any overshoot.
func (e *HEEncoder) flushFrontEnd(emit func(codec.Packet) error) error {
	if n := len(e.dmStage[0]); n > 0 {
		for c := range 2 {
			e.dmStage[c] = append(e.dmStage[c], make([]float32, 64-n)...)
		}
	}
	var zero [64]float32
	for e.inSamples+int64(len(e.dmBuf)) < e.psIn {
		if len(e.dmStage[0]) >= 64 {
			e.pushDM(e.dmStage[0][:64], e.dmStage[1][:64])
			for c := range 2 {
				e.dmStage[c] = e.dmStage[c][:0]
			}
		} else {
			e.pushDM(zero[:], zero[:])
		}
	}
	if over := e.inSamples + int64(len(e.dmBuf)) - e.psIn; over > 0 {
		e.dmBuf = e.dmBuf[:int64(len(e.dmBuf))-over]
	}
	return e.drainDM(emit)
}

// feedCore runs the resampler over src (nil to drain) and encodes every
// produced core chunk.
func (e *HEEncoder) feedCore(src [][]float32, emit func(codec.Packet) error) error {
	ch := e.sbr.chans
	if e.coreBuf == nil {
		e.coreBuf = audio.Get(e.core.fmt, audio.StandardChunk)
	}
	wrap := e.emitWrap(emit)
	var dst [2][]float32
	consumed := 0
	for {
		for c := 0; c < ch; c++ {
			dst[c] = e.coreBuf.ChanF(c)[:e.coreBuf.Cap()]
		}
		var produced, took int
		if src == nil {
			produced = e.res.Drain(dst[:ch])
		} else {
			var in [2][]float32
			for c := 0; c < ch; c++ {
				in[c] = src[c][consumed:]
			}
			produced, took = e.res.Process(dst[:ch], in[:ch])
			consumed += took
		}
		if produced > 0 {
			e.coreBuf.N = produced
			if err := e.core.Encode(e.coreBuf, wrap); err != nil {
				return err
			}
		}
		if src == nil {
			if produced == 0 {
				return nil
			}
			continue
		}
		if consumed >= len(src[0]) && produced == 0 {
			return nil
		}
		if produced == 0 && took == 0 {
			return nil
		}
	}
}

// Finish drains the resampler, pads the stream so the declared delay and
// the trailing SBR chain memory are fully covered by emitted AUs, and
// reports the gapless trailer in the output timeline.
func (e *HEEncoder) Finish(emit func(codec.Packet) error) (codec.Trailer, error) {
	if e.finished {
		return codec.Trailer{}, waxerr.New(waxerr.CodeInternal, "aac: Finish called twice on an HE-AAC encoder")
	}
	e.finished = true
	if e.sbr.ps != nil {
		if err := e.flushFrontEnd(emit); err != nil {
			return codec.Trailer{}, err
		}
	}
	ch := e.sbr.chans
	if n := len(e.slotStage[0]); n > 0 {
		for c := 0; c < ch; c++ {
			e.slotStage[c] = append(e.slotStage[c], make([]float32, 64-n)...)
			e.sbr.pushSlot(c, e.slotStage[c][:64])
			e.slotStage[c] = e.slotStage[c][:0]
		}
	}
	if err := e.feedCore(nil, emit); err != nil {
		return codec.Trailer{}, err
	}
	// The core's own Finish pads its tail and pushes two zero blocks
	// (the second only flushes its deferred AU and enters no emitted
	// window), which covers a 2048-sample delay; this encoder declares
	// more (the SBR chain), so the last real samples may need one more
	// flush AU to exist in the decode at all. The aus estimate below is
	// the core's emitted count: blocks fed plus one, unchanged by the
	// deferral (the first fed block completes no AU).
	nCore := resample.OutputLen(e.inSamples, e.fmt.Rate, e.core.rate)
	aus := (nCore+frameLen-1)/frameLen + 1
	if aus*2*frameLen-e.inSamples-HEEncoderDelay < 0 {
		zero := audio.Get(e.core.fmt, frameLen)
		zero.N = frameLen
		for c := 0; c < ch; c++ {
			clear(zero.ChanF(c)[:frameLen])
		}
		err := e.core.Encode(zero, e.emitWrap(emit))
		audio.Put(zero)
		if err != nil {
			return codec.Trailer{}, err
		}
	}
	if _, err := e.core.Finish(e.emitWrap(emit)); err != nil {
		return codec.Trailer{}, err
	}
	padding := e.outFrames*2*frameLen - e.inSamples - HEEncoderDelay
	if padding < 0 {
		padding = 0
	}
	// The engine has no encoder-release hook on the happy path, so the
	// pooled scratch goes back here, at the end of every completed encode;
	// Release covers callers that stop early.
	audio.Put(e.coreBuf)
	e.coreBuf = nil
	return codec.Trailer{Samples: e.inSamples, Delay: HEEncoderDelay, Padding: padding}, nil
}

// Release returns pooled scratch (codec.Releaser).
func (e *HEEncoder) Release() {
	audio.Put(e.coreBuf)
	e.coreBuf = nil
}
