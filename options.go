package waxflow

import (
	"fmt"
	"log/slog"
	"math"
	"math/bits"
	"strings"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// Option configures an Engine.
type Option func(*Engine)

// WithLogger sets the Engine's logger. Nil (and the default) discards.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// IndexCache persists demuxer-built source indexes across sessions (the
// cacheDir/idx sidecar): MP3 frame tables today, seek tables for later
// formats. The engine restores a cached index when it opens a source
// whose demuxer can use one, and saves fresh snapshots on close. Keying
// blobs by source identity is the implementation's job (the server keys
// by ref plus size plus mtime); the engine stays identity-agnostic.
type IndexCache interface {
	// Load returns the saved index blob for src, or nil.
	Load(src container.Source) []byte
	// Save persists a fresh snapshot for src. Best effort: failures are
	// the implementation's to swallow (a lost sidecar only costs a
	// rebuild).
	Save(src container.Source, blob []byte)
	// Drop removes src's saved blob. The engine calls it when a demuxer
	// rejects a loaded blob, so an invalid one stops being served (and
	// LRU-refreshed) forever.
	Drop(src container.Source)
}

// WithIndexCache wires an index sidecar cache into the Engine.
func WithIndexCache(c IndexCache) Option {
	return func(e *Engine) {
		e.idx = c
	}
}

// TranscodeOptions selects the Transcode output, with the DSP chain
// (resample, mix, gain, dither) between decode and encode. Zero values keep
// the source's properties, so the zero options add no DSP stage and the
// decoder's samples reach the encoder unaltered: a bit-exact container
// rewrite for a lossless source to a lossless output. A lossy source is
// decoded and re-encoded even so, which costs a generation.
//
// Remux is what removes that generation, and the options it accepts are exactly
// the ones described above: zero everywhere but Format and Container. It moves
// the source's own packets rather than decoding them, so it is a bit-exact
// container rewrite for a lossy source too, but only where the codec survives
// the trip (the output format's codec must already be the source's). PlanRemux
// answers whether a given request is one of those, and the server's ladder asks
// it before reaching for a transcode.
//
// An encoder knob (see Knob) is taken only by the formats its field names, and
// Check refuses one set on any other format.
type TranscodeOptions struct {
	// Format is the output format name, one of OutputFormats.
	Format string
	// Container names one of the format's other forms ("adts" for aac, "mka"
	// for flac); empty selects its default. Check refuses a name the format
	// has no form for, and its error lists the ones it has.
	Container string
	// SpliceTrims asks a multi-span cut for exact interior splices, which
	// only Matroska can carry.
	//
	// The default cut snaps every interior edge inward onto the packet grid
	// and gives up under one packet of wanted audio at each, because no
	// per-splice trim is portable (ADR-0011). With this set, each interior
	// span's head is preceded by the codec's pre-roll packets carrying a
	// full-duration DiscardPadding -- decoded so the decoder converges,
	// discarded so nothing is heard -- and each interior tail is exact.
	//
	// The cost is reach. The cut's track then trims inside its run, so
	// PlanRemux declines every destination but mka and webm, and a request
	// for any other container falls through to a re-encode. Firefox rejects
	// a file with more than one DiscardPadding outright; Chromium and
	// ffmpeg-based players take it. Ignored by a single-span cut, which has
	// no interior edge.
	SpliceTrims bool
	// Rate resamples to this sample rate in Hz; 0 keeps the source rate.
	Rate int
	// Channels converts the channel count: a downmix to 1 or 2, or a widening
	// into the conventional layout for a larger count, which places the
	// source's positions and leaves the rest silent. 0 keeps the source
	// layout. A target whose layout has no place for one of the source's
	// positions is refused rather than substituted.
	Channels int
	// BitDepth forces integer output at this depth, dithered when
	// reducing; 0 keeps the source domain and depth.
	BitDepth int
	// GainDB applies a scalar gain, finite within +-120 dB. Positive
	// gain engages the true-peak limiter; tighter policy clamps (the
	// HTTP +12 dB bound) live at the API boundary, not here.
	GainDB float64
	// Dynamics applies a dynamics-processing preset to the post-gain
	// signal: gain.PresetOff (the zero value) applies none, gain.PresetVoice
	// the spoken-word leveller. It is a closed vocabulary rather than raw
	// compressor parameters; see gain.Preset for why.
	//
	// It composes with GainDB rather than replacing it, and the order is
	// load-bearing: the preset's curve has a fixed threshold, so the caller
	// levels the signal to a known point with GainDB first and the preset
	// then shapes it. A caller with a measured loudness (an analyze job's)
	// sends the exact dB alongside the preset. WaxFlow cannot measure a
	// live stream, so it cannot do this for the caller: two-pass is
	// jobs-only.
	//
	// A preset always engages the true-peak limiter.
	Dynamics gain.Preset
	// FromSample starts output at this source-timeline sample, seeking
	// sample-exact before the first chunk. Seconds convert to samples at
	// the API boundary (ADR-0006); 0 starts at the beginning.
	FromSample int64
	// FLACLevel selects the FLAC compression level for flac output: 1
	// through 8 literally, FLACLevelDefault (the zero value) for the
	// encoder default, and FLACLevelFastest for level 0, which needs a
	// sentinel because the zero value cannot mean it without stealing
	// the default. Levels trade encode speed for size and never affect
	// decoded audio.
	FLACLevel int
	// WavPackLevel selects the WavPack compression level for wavpack
	// output: WavPackLevelFast through WavPackLevelVeryHigh literally, and
	// WavPackLevelDefault (the zero value) for the encoder default, which
	// is normal. Levels choose how deep a decorrelation cascade each block
	// runs: they trade encode speed for size and never affect decoded
	// audio.
	WavPackLevel int
	// APELevel selects the Monkey's Audio compression level for ape
	// output: APELevelFast, APELevelNormal, or APELevelHigh literally, and
	// APELevelDefault (the zero value) for the encoder default, which is
	// normal. Levels choose the filter cascade each frame runs through:
	// they trade encode and decode speed for size and never affect decoded
	// audio. The format's two deeper levels decode here but are not
	// written; see ape.MaxEncodeLevel.
	APELevel int
	// Bitrate selects the target bit rate in bits per second for every
	// format with a bit rate (mp3, opus, aac, he-aac); 0 keeps the encoder's
	// default (mp3 128000, opus 96000, aac 128000, he-aac 64000 or 32000 under
	// HEAACv2). A negative value or one above MaxBitrate is refused. An mp3
	// rate must be a legal Layer III rate for the output sample rate; aac
	// holds its long-term mean with a bit reservoir. Vorbis takes none (no
	// ABR).
	Bitrate int
	// VBR selects variable bit rate for mp3 and opus output, sizing each
	// frame to its content around Bitrate (for mp3, the quality anchor). The
	// zero value is constant bit rate.
	VBR bool
	// HEAACv2 selects HE-AAC v2 (parametric stereo over a mono SBR core,
	// an AOT-29 stream) for he-aac output, and drops the zero-Bitrate
	// default to 32000. Selection is explicit rather than
	// bitrate-automatic: an auto threshold would silently switch stereo
	// coding technology across a bitrate boundary. Stereo sources only;
	// a mono source is refused at plan time (encode v1 instead).
	HEAACv2 bool
	// OpusComplexity gates the Opus encoder's analysis depth: 1 through 10
	// literally, OpusComplexityDefault (the zero value) for the encoder
	// default (5), and OpusComplexityLowest for complexity 0, which needs a
	// sentinel because the zero value cannot mean it without stealing the
	// default. Higher is slower and higher quality.
	OpusComplexity int
	// OpusSignal hints the opus encoder about the content type: "voice"
	// biases the speech/music mode decision toward SILK/hybrid (audiobooks,
	// podcasts), "music" toward CELT. The zero value ("" or "auto") lets the
	// encoder's analyser decide per frame.
	OpusSignal string
	// VorbisQuality selects VBR quality for vorbis output on libvorbis's -q
	// scale (-1..10); higher is larger and better. The zero value uses the
	// encoder default (3.0). Vorbis is natively quality-driven, so this is the
	// primary knob; a small nonzero value near 0 reaches the lowest qualities
	// (the zero value cannot, matching the "0 means default" idiom).
	VorbisQuality float64
	// Shaping selects the dither strategy for quantization; the default
	// is flat TPDF.
	Shaping dither.Shaping
	// ResampleProfile selects resampler quality; empty means resample.HQ.
	ResampleProfile resample.Profile
}

// Check refuses what the options alone rule out, in this order: a non-finite
// float (a NaN in the plan cache's key is an entry no lookup reaches again), a
// Bitrate below 0 or above MaxBitrate, a negative FromSample, an unregistered
// Format, a Container the format has no form for, a set knob the format does
// not take, which is CodeUnsupportedFormat wrapping a *KnobError, and a
// level, Opus complexity or Opus signal outside its range. Every engine method
// that takes the options runs it before it plans or encodes, and a boundary
// may run it before it has a track.
func (o TranscodeOptions) Check() error {
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"GainDB", o.GainDB},
		{"VorbisQuality", o.VorbisQuality},
	} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("waxflow: %s is %g, want a finite number", f.name, f.v))
		}
	}
	if o.Bitrate < 0 || o.Bitrate > MaxBitrate {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("waxflow: Bitrate is %d, want 0 for the default or a positive bit rate up to %d",
				o.Bitrate, MaxBitrate))
	}
	if o.FromSample < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "waxflow: negative FromSample")
	}
	row, err := outputRow(o.Format)
	if err != nil {
		return err
	}
	if _, err := row.form(o.Container); err != nil {
		return err
	}
	if off := o.Knobs() &^ row.knobs; off != 0 {
		k := off & -off
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "", &KnobError{
			Knob:     k,
			Format:   row.name,
			Takers:   knobTakers(k),
			Lossless: !row.lossy && k&(KnobBitrate|KnobVBR) != 0,
		})
	}
	// The range of each knob the format takes. An unset knob is its default
	// and passes, and the knob rule above refused a set one the format lacks.
	for _, check := range []func(TranscodeOptions) error{
		func(o TranscodeOptions) error { _, err := flacLevel(o); return err },
		func(o TranscodeOptions) error { _, err := wavpackLevel(o); return err },
		func(o TranscodeOptions) error { _, err := apeLevel(o); return err },
		checkOpusComplexity,
		func(o TranscodeOptions) error { _, err := opusSignal(o); return err },
	} {
		if err := check(o); err != nil {
			return err
		}
	}
	return nil
}

// Knob names an encoder option only some output formats take.
type Knob uint16

// The knobs, one bit each, in the order Check reports them.
const (
	KnobBitrate Knob = 1 << iota
	KnobVBR
	KnobVorbisQuality
	KnobHEAACv2
	KnobFLACLevel
	KnobWavPackLevel
	KnobAPELevel
	KnobOpusComplexity
	KnobOpusSignal
)

// knobNames are the knobs' field names, in bit order.
var knobNames = [...]string{
	"Bitrate", "VBR", "VorbisQuality", "HEAACv2", "FLACLevel",
	"WavPackLevel", "APELevel", "OpusComplexity", "OpusSignal",
}

// String is the option's field name; a value that is not one knob reads
// Knob(n).
func (k Knob) String() string {
	if k != 0 && k&(k-1) == 0 {
		if i := bits.TrailingZeros16(uint16(k)); i < len(knobNames) {
			return knobNames[i]
		}
	}
	return fmt.Sprintf("Knob(%d)", uint16(k))
}

// normalized folds the spellings of one request into one value: OpusSignal
// "auto" is the unset hint. The plan key, the remux test and Knobs read it.
func (o TranscodeOptions) normalized() TranscodeOptions {
	if o.OpusSignal == "auto" {
		o.OpusSignal = ""
	}
	return o
}

// Knobs reports the encoder knobs o sets: each field that differs from its
// zero value, with OpusSignal "auto" counted as unset.
func (o TranscodeOptions) Knobs() Knob {
	o = o.normalized()
	var k Knob
	for _, f := range []struct {
		set  bool
		knob Knob
	}{
		{o.Bitrate != 0, KnobBitrate},
		{o.VBR, KnobVBR},
		{o.VorbisQuality != 0, KnobVorbisQuality},
		{o.HEAACv2, KnobHEAACv2},
		{o.FLACLevel != 0, KnobFLACLevel},
		{o.WavPackLevel != 0, KnobWavPackLevel},
		{o.APELevel != 0, KnobAPELevel},
		{o.OpusComplexity != 0, KnobOpusComplexity},
		{o.OpusSignal != "", KnobOpusSignal},
	} {
		if f.set {
			k |= f.knob
		}
	}
	return k
}

// KnobError is a set knob the named format does not take. Check returns it
// inside a CodeUnsupportedFormat error, so a boundary can word it in its own
// spelling of the knob.
type KnobError struct {
	Knob     Knob
	Format   string
	Takers   []string // the formats that take it, in table order
	Lossless bool     // the format holds no bit rate because it is lossless
}

func (e *KnobError) Error() string {
	takers := strings.Join(e.Takers, ", ")
	if e.Lossless {
		return fmt.Sprintf("waxflow: %s applies to lossy output (%s); %s is lossless", e.Knob, takers, e.Format)
	}
	return fmt.Sprintf("waxflow: %s applies to %s output; this request names %s", e.Knob, takers, e.Format)
}

// knobTakers lists the formats that take k, in table order.
func knobTakers(k Knob) []string {
	var names []string
	for _, o := range outputs {
		if !o.remuxOnly && o.knobs&k != 0 {
			names = append(names, o.name)
		}
	}
	return names
}

// RunOption attaches what rides along with one run and never shapes its plan.
type RunOption func(*runConfig)

// runConfig is what a run's RunOptions set.
type runConfig struct {
	tags     []container.Tag
	chapters []container.Chapter
	art      *container.Picture
	progress func(done, total int64)
}

// newRunConfig applies run in order, so a later option wins.
func newRunConfig(run []RunOption) runConfig {
	var x runConfig
	for _, o := range run {
		if o != nil {
			o(&x)
		}
	}
	return x
}

// WithTags embeds canonical metadata fields (TITLE, ARTIST, ...) in the
// output where the muxer can represent them in its stream form: Ogg
// OpusTags, a FLAC VORBIS_COMMENT block, an MP3 ID3v2 tag, MP4 ilst
// atoms. Formats without stream-form tagging (WAV, AIFF, ADTS)
// ignore them; a finished file gets full metadata from the mapping
// post-pass instead. Tags never change the plan: callers keying
// cached bytes must fold the tag values into their own key.
// WavPack and Monkey's Audio take them too, as the APEv2 block they
// write after the audio.
//
// Every muxer that bounds its tag block refuses a value that does not
// fit, failing the run with waxerr.CodeUnsupportedFormat naming
// the key, rather than writing an output that quietly lacks it. The
// refusal comes before the encode, so it costs nothing to retry with
// the value trimmed. Those bounds sit at each format's own read or
// field limit, so tags read off a source can always be written back;
// Ogg is the exception, capping its comment header at 48 KiB to keep
// the pre-audio headers small. MP4 sets no bound at all.
func WithTags(tags []container.Tag) RunOption {
	return func(x *runConfig) { x.tags = tags }
}

// WithChapters embeds chapter markers. Only the MP4 muxer represents
// them (Nero chpl); the mapping post-pass covers finished files of
// the other formats.
func WithChapters(chapters []container.Chapter) RunOption {
	return func(x *runConfig) { x.chapters = chapters }
}

// WithArt embeds cover art. Only the MP4 muxer represents it (the ilst
// covr atom); art inflates the pre-audio init header, so live
// streams should leave it nil.
func WithArt(art *container.Picture) RunOption {
	return func(x *runConfig) { x.art = art }
}

// WithProgress, given a non-nil fn, calls it after each encoded chunk or
// copied packet with the samples carried so far and the projected total (-1
// unknown). It runs on the run's goroutine, so blocking it pauses the
// pipeline; the job runner's yield-to-live-streams check rides on exactly that.
func WithProgress(fn func(done, total int64)) RunOption {
	return func(x *runConfig) { x.progress = fn }
}

// MaxBitrate is the largest Bitrate, in bits per second, and so fits an int on
// every build. A boundary that takes kbit/s refuses a value above
// MaxBitrate/1000 before it multiplies.
const MaxBitrate = math.MaxInt32

// FLACLevel spellings whose meaning the zero value cannot carry.
const (
	// FLACLevelDefault keeps the encoder's default compression level.
	FLACLevelDefault = 0
	// FLACLevelFastest selects FLAC level 0.
	FLACLevelFastest = -1
)

// WavPackLevel spellings. WavPack's levels are named modes rather than a
// numeric scale, and they are numbered from one, so the zero value means the
// default with no sentinel needed.
const (
	// WavPackLevelDefault keeps the encoder's default level (normal).
	WavPackLevelDefault = 0
	// WavPackLevelFast is the shallowest cascade: fastest, largest.
	WavPackLevelFast = wavpack.LevelFast
	// WavPackLevelNormal is the default cascade.
	WavPackLevelNormal = wavpack.LevelNormal
	// WavPackLevelHigh spends more passes for a smaller file.
	WavPackLevelHigh = wavpack.LevelHigh
	// WavPackLevelVeryHigh is the deepest cascade: smallest, slowest.
	WavPackLevelVeryHigh = wavpack.LevelVeryHigh
)

// APELevel spellings. Monkey's Audio names its levels in thousands, which is
// the vocabulary the format itself uses in the file header, so the option
// carries those numbers rather than a scale of its own; zero is not one of
// them, so the default needs no sentinel.
const (
	// APELevelDefault keeps the encoder's default level (normal).
	APELevelDefault = 0
	// APELevelFast is the shallowest cascade: no filter at all, fastest,
	// largest.
	APELevelFast = ape.LevelFast
	// APELevelNormal is the default cascade, a 16-tap filter.
	APELevelNormal = ape.LevelNormal
	// APELevelHigh runs a 64-tap filter for a smaller file.
	APELevelHigh = ape.LevelHigh
)

// OpusComplexity spellings whose meaning the zero value cannot carry.
const (
	// OpusComplexityDefault keeps the encoder's default complexity.
	OpusComplexityDefault = 0
	// OpusComplexityLowest selects complexity 0.
	OpusComplexityLowest = -1
)

// ProbeOptions configures Engine.Probe.
type ProbeOptions struct {
	// Strict turns tolerated input damage into errors.
	Strict bool
}
