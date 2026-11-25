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

// IndexCache persists demuxer-built source indexes, such as MP3 frame
// tables, across sessions (the cacheDir/idx sidecar): the engine restores one
// at open where the demuxer can use it and saves a fresh one on close. Keying
// by source identity is the implementation's (the server's: ref, size, mtime).
type IndexCache interface {
	// Load returns the saved index blob for src, or nil.
	Load(src container.Source) []byte
	// Save persists a fresh snapshot for src, best effort: a failure is the
	// implementation's to swallow, since a lost sidecar costs a rebuild.
	Save(src container.Source, blob []byte)
	// Drop removes src's saved blob. The engine calls it when a demuxer
	// rejects a loaded blob, so an invalid one stops being served.
	Drop(src container.Source)
}

// WithIndexCache wires an index sidecar cache into the Engine.
func WithIndexCache(c IndexCache) Option {
	return func(e *Engine) {
		e.idx = c
	}
}

// TranscodeOptions selects the Transcode output and the DSP chain (resample,
// mix, gain, dither) between decode and encode. Zero values keep the source's
// properties, so the zero options add only the stages the output row needs. A
// lossy source to a lossy output is re-encoded at the cost of a generation;
// where the row takes the source's format as it is, a lossless source to a
// lossless output is a bit-exact rewrite. Remux moves the source's packets
// instead where the output codec is the source's and the options name no
// change: every field zero but Format, Container, SpliceTrims,
// ResampleProfile and Shaping, or stating the source's own Rate, Channels or
// integer BitDepth (PlanRemux answers). An encoder knob (Knob) is taken only
// by the formats its field names, and Check refuses one set on any other.
type TranscodeOptions struct {
	// Format is the output format name, one of OutputFormats.
	Format string
	// Container names one of the format's other forms ("adts" for aac, "mka"
	// for flac); empty selects its default. Check refuses a name the format
	// has no form for, and its error lists the ones it has.
	Container string
	// SpliceTrims asks a multi-span cut for exact interior splices, which
	// only Matroska carries (ADR-0011). The default snaps every interior edge
	// inward onto the packet grid, giving up under one packet of wanted audio
	// at each. With this set, each interior head is preceded by the codec's
	// pre-roll packets with a full-duration DiscardPadding (decoded, never
	// heard) and each interior tail is exact. PlanRemux then declines all but
	// mka and webm (others re-encode), and Firefox rejects a file with more
	// than one DiscardPadding. A single-span cut ignores it.
	SpliceTrims bool
	// Rate resamples to this sample rate in Hz; 0 keeps the source rate.
	Rate int
	// Channels converts the channel count: a downmix to 1 or 2, or a widening
	// into a larger count's conventional layout, placing the source's
	// positions and leaving the rest silent. 0 keeps the source layout. A
	// target with no place for one of the source's positions is refused.
	Channels int
	// BitDepth forces integer output at this depth, dithered when
	// reducing; 0 keeps the source domain and depth.
	BitDepth int
	// GainDB applies a scalar gain, finite within +-120 dB. Positive
	// gain engages the true-peak limiter; tighter policy clamps (the
	// HTTP +12 dB bound) live at the API boundary, not here.
	GainDB float64
	// Dynamics applies a dynamics preset (a closed vocabulary, see
	// gain.Preset) after GainDB: gain.PresetOff, the zero value, none;
	// gain.PresetVoice the spoken-word leveller. Its curve has a fixed
	// threshold, so the caller levels the signal with GainDB first, from a
	// measured loudness in a two-pass job; WaxFlow cannot measure a live
	// stream, so it does not level one for the caller. A preset always
	// engages the true-peak limiter.
	Dynamics gain.Preset
	// FromSample starts output at this source-timeline sample, seeking
	// sample-exact before the first chunk. Seconds convert to samples at
	// the API boundary (ADR-0006); 0 starts at the beginning.
	FromSample int64
	// FLACLevel selects the flac compression level: 1 through 8 literally,
	// FLACLevelDefault (the zero value) the encoder default, FLACLevelFastest
	// level 0. A level trades encode speed for size, never decoded audio.
	FLACLevel int
	// WavPackLevel selects the wavpack compression level, WavPackLevelFast
	// through WavPackLevelVeryHigh; the zero value is the default, normal. A
	// level trades encode speed for size, never decoded audio.
	WavPackLevel int
	// APELevel selects the ape compression level, APELevelFast,
	// APELevelNormal or APELevelHigh; the zero value is the default, normal.
	// A level trades encode and decode speed for size, never decoded audio.
	// The format's two deeper levels decode but are not written
	// (ape.MaxEncodeLevel).
	APELevel int
	// Bitrate is the target in bits per second for mp3, opus, aac and he-aac
	// (vorbis takes none: no ABR); 0 keeps the encoder default (mp3 128000,
	// opus 96000, aac 128000, he-aac 64000 or 32000 under HEAACv2). A negative
	// value or one above MaxBitrate is refused. An mp3 rate must be whole
	// kbit/s, and one the layer lacks at the output rate clamps to the
	// highest it holds at or below it (its lowest, below that); aac holds its
	// long-term mean with a bit reservoir.
	Bitrate int
	// VBR selects variable bit rate for mp3 and opus output, sizing each
	// frame to its content around Bitrate (for mp3, the quality anchor). The
	// zero value is constant bit rate.
	VBR bool
	// HEAACv2 selects HE-AAC v2 (parametric stereo over a mono SBR core, an
	// AOT-29 stream) for he-aac output and drops the zero-Bitrate default to
	// 32000. It is explicit so no bit rate threshold switches stereo coding
	// silently. A mono source is refused at plan time (encode v1).
	HEAACv2 bool
	// OpusComplexity sets the opus encoder's analysis depth: 1 through 10
	// literally, OpusComplexityDefault (the zero value) the default 5,
	// OpusComplexityLowest complexity 0. Higher is slower and better.
	OpusComplexity int
	// OpusSignal hints the content type to the opus encoder: "voice" biases
	// its mode decision toward SILK/hybrid, "music" toward CELT, and "" or
	// "auto" (alike) let its analyser decide per frame.
	OpusSignal string
	// VorbisQuality is the vorbis VBR quality on libvorbis's -q scale
	// (-1..10), higher larger and better; the zero value is the encoder
	// default 3.0, so a small nonzero value stands in for quality 0.
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
// not take (CheckKnobs), which is CodeUnsupportedFormat wrapping a *KnobError,
// and a level, Opus complexity, Opus signal or Vorbis quality outside its
// range, CodeInvalidRequest wrapping a *RangeError. Every engine method that
// takes the options runs it before it plans or encodes, and a boundary may run
// it before it has a track.
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
	if err := CheckKnobs(row.name, o.Knobs()); err != nil {
		return err
	}
	// The range of each knob the format takes. An unset knob is its default
	// and passes, and the knob rule above refused a set one the format lacks.
	for _, check := range []func(TranscodeOptions) error{
		func(o TranscodeOptions) error { _, err := flacLevel(o); return err },
		func(o TranscodeOptions) error { _, err := wavpackLevel(o); return err },
		func(o TranscodeOptions) error { _, err := apeLevel(o); return err },
		checkOpusComplexity,
		func(o TranscodeOptions) error { _, err := opusSignal(o); return err },
		checkVorbisQuality,
	} {
		if err := check(o); err != nil {
			return err
		}
	}
	return nil
}

// CheckKnobs refuses a knob in set that format does not take, as Check does:
// CodeUnsupportedFormat wrapping a *KnobError for the lowest such knob. A bit
// that names no knob is CodeInvalidRequest, before the format is asked. An
// unregistered format is Check's refusal of it, and nil means format takes
// every knob in set.
func CheckKnobs(format string, set Knob) error {
	if bad := set &^ (Knob(1)<<len(knobNames) - 1); bad != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("waxflow: %s is not a knob", bad&-bad))
	}
	row, err := outputRow(format)
	if err != nil {
		return err
	}
	if off := set &^ row.knobs; off != 0 {
		k := off & -off
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "", &KnobError{
			Knob:     k,
			Format:   row.name,
			Takers:   knobTakers(k),
			Lossless: !row.lossy && k&(KnobBitrate|KnobVBR) != 0,
		})
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

// RangeError is a knob set to a value it does not take. Check returns it
// inside a CodeInvalidRequest error, so a boundary can word it in its own
// spelling of the knob, as it does a *KnobError.
type RangeError struct {
	Knob  Knob
	Value any // as given: an int, a float64 or a string
	// Min and Max bound a numeric knob; Values lists the ones a knob that
	// takes a set accepts.
	Min, Max float64
	Values   []string
	// Unwritten is a level the format decodes and the encoder does not write.
	Unwritten bool
}

// Want is what the knob takes, "-1 to 8" or "auto, voice, or music".
func (e *RangeError) Want() string {
	if len(e.Values) == 0 {
		return fmt.Sprintf("%g to %g", e.Min, e.Max)
	}
	switch n := len(e.Values); n {
	case 1:
		return e.Values[0]
	case 2:
		return e.Values[0] + " or " + e.Values[1]
	default:
		return strings.Join(e.Values[:n-1], ", ") + ", or " + e.Values[n-1]
	}
}

// rangeSubjects name each knob with a range in Check's sentences.
var rangeSubjects = map[Knob]string{
	KnobFLACLevel:      "FLAC level",
	KnobWavPackLevel:   "WavPack level",
	KnobAPELevel:       "APE level",
	KnobOpusComplexity: "Opus complexity",
	KnobVorbisQuality:  "Vorbis quality",
}

func (e *RangeError) Error() string {
	subject, ok := rangeSubjects[e.Knob]
	if !ok {
		subject = e.Knob.String()
	}
	switch {
	case e.Knob == KnobOpusSignal:
		return fmt.Sprintf("opus signal hint %q is not %s", e.Value, e.Want())
	case e.Unwritten:
		return fmt.Sprintf("waxflow: %s %v decodes but is not written; want %s", subject, e.Value, e.Want())
	case len(e.Values) > 0:
		return fmt.Sprintf("waxflow: %s %v is not one of %s", subject, e.Value, e.Want())
	}
	return fmt.Sprintf("waxflow: %s %v outside %g..%g", subject, e.Value, e.Min, e.Max)
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
	measured *container.Track
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

// WithMeasured hands a copy the measurement of its source's track that the
// caller planned from (a measure's or a strict probe's), so a run that would
// walk the source for what a walk finds reads it there. A track nothing
// measured is ignored. It rides along: the plan is the caller's already.
func WithMeasured(track container.Track) RunOption {
	return func(x *runConfig) { x.measured = &track }
}

// WithTags embeds canonical metadata fields (TITLE, ARTIST, ...) where the
// muxer has a stream form: Ogg OpusTags, FLAC VORBIS_COMMENT, MP3 ID3v2, MP4
// ilst, and WavPack's and Monkey's Audio's trailing APEv2. WAV, AIFF, ADTS
// and Matroska ignore them (the mapping post-pass tags a finished file).
// Tags never change the plan: a caller keying cached bytes keys them too.
// Every muxer that bounds its tag block (at 48 KiB; MP4 sets none) refuses
// a value that does not fit with waxerr.CodeUnsupportedFormat naming the
// key, before the encode; TagsFit asks the same question before the run.
func WithTags(tags []container.Tag) RunOption {
	return func(x *runConfig) { x.tags = tags }
}

// WithChapters embeds chapter markers. Only the MP4 muxer represents them
// (Nero chpl); the mapping post-pass covers the other formats' files.
func WithChapters(chapters []container.Chapter) RunOption {
	return func(x *runConfig) { x.chapters = chapters }
}

// WithArt embeds cover art. Only the MP4 muxer represents it (ilst covr);
// it inflates the pre-audio init header, so live streams leave it nil.
func WithArt(art *container.Picture) RunOption {
	return func(x *runConfig) { x.art = art }
}

// WithProgress, given a non-nil fn, calls it after each encoded chunk or
// copied packet with the samples carried so far and the projected total (-1
// unknown). It runs on the run's goroutine, so blocking it pauses the run.
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

// WavPackLevel spellings. WavPack's levels are named modes numbered from
// one, so the zero value is the default with no sentinel.
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

// APELevel spellings: the thousands Monkey's Audio's file header names its
// levels by. Zero is none of them, so the default needs no sentinel.
const (
	// APELevelDefault keeps the encoder's default level (normal).
	APELevelDefault = 0
	// APELevelFast runs no filter at all: fastest, largest.
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
