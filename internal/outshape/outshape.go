// Package outshape holds the output-shaping part of a request as every server
// surface carries it, the one mapping onto engine options, the wire policy,
// and each surface's wording of a knob its format does not take or a value
// out of the knob's range.
package outshape

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// Params is the output-shaping part of a request as every surface carries
// it: the /stream query, an HLS descriptor, a job body. It must never
// implement json.Marshaler: the method would be promoted onto the types
// that embed it and take over their whole document.
type Params struct {
	Format        string  `json:"format,omitempty"`
	Container     string  `json:"container,omitempty"`
	Rate          int     `json:"rate,omitempty"`
	Channels      int     `json:"ch,omitempty"`
	Bits          int     `json:"bits,omitempty"`
	Bitrate       int     `json:"bitrate,omitempty"` // kbit/s
	HEv2          bool    `json:"hev2,omitempty"`
	VorbisQuality float64 `json:"vorbisQuality,omitempty"`
	FLACLevel     int     `json:"flacLevel,omitempty"`
	WavPackLevel  int     `json:"wavpackLevel,omitempty"`
	APELevel      int     `json:"apeLevel,omitempty"`
}

// MaxKbps is the largest kbit/s value a request may name, refused before it
// is multiplied to bits per second.
const MaxKbps = waxflow.MaxBitrate / 1000

// Options is the one mapping onto engine options. gainDB and dynamics are
// the caller's resolved level, profile the daemon's resampler. A bit rate
// outside 0..MaxKbps maps to -1, which the engine refuses, so no product
// wraps to a rate the request never named.
func (p Params) Options(gainDB float64, dynamics gain.Preset, profile resample.Profile) waxflow.TranscodeOptions {
	bitrate := -1
	if p.Bitrate >= 0 && p.Bitrate <= MaxKbps {
		bitrate = p.Bitrate * 1000
	}
	return waxflow.TranscodeOptions{
		Format:          p.Format,
		Container:       p.Container,
		Rate:            p.Rate,
		Channels:        p.Channels,
		BitDepth:        p.Bits,
		GainDB:          gainDB,
		Dynamics:        dynamics,
		Bitrate:         bitrate,
		HEAACv2:         p.HEv2,
		VorbisQuality:   p.VorbisQuality,
		FLACLevel:       p.FLACLevel,
		WavPackLevel:    p.WavPackLevel,
		APELevel:        p.APELevel,
		ResampleProfile: profile,
	}
}

// CheckWire applies the wire policy: bits 0, 16 or 24; rate, ch and bitrate
// not negative; bitrate at most MaxKbps; vorbisQuality finite. A level and a
// quality may be negative (-1 spells level 0 and the lowest quality).
func (p Params) CheckWire() error {
	bad := func(format string, args ...any) error {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	switch p.Bits {
	case 0, 16, 24:
	default:
		return bad("bits %d: want 16 or 24", p.Bits)
	}
	// A JSON number cannot be absent, so 0 is the default there.
	for _, n := range []struct {
		name, zero string
		v          int
	}{
		{"rate", "the source's value", p.Rate},
		{"ch", "the source's value", p.Channels},
		{"bitrate", "the format's default", p.Bitrate},
	} {
		if n.v < 0 {
			return bad("%s %d: want 0 for %s or a positive integer", n.name, n.v, n.zero)
		}
	}
	if p.Bitrate > MaxKbps {
		return bad("bitrate %d: want at most %d kbit/s", p.Bitrate, MaxKbps)
	}
	if math.IsNaN(p.VorbisQuality) || math.IsInf(p.VorbisQuality, 0) {
		return bad("vorbisQuality %g: want a finite number", p.VorbisQuality)
	}
	return nil
}

// Check is CheckWire, then the engine's Check with a knob the format does
// not take, or one out of range, worded by w. It covers the request's shape,
// what Params carries; what a caller adds beyond it (a seek, a resolved gain)
// is the engine's to check when it plans. Format is already resolved.
func (p Params) Check(w Wording) error {
	if err := p.CheckWire(); err != nil {
		return err
	}
	return w.Reword(p.Options(0, gain.PresetOff, "").Check())
}

// Knobs reports whether p sets any encoder knob: the question direct play
// asks. It asks the options p maps to, so a knob added later counts here.
func (p Params) Knobs() bool {
	return p.Options(0, gain.PresetOff, "").Knobs() != 0
}

// ParseQuery reads the shaping parameters a query-string surface carries
// (format, container, rate, ch, bits, bitrate, hev2). It returns q
// unresolved, an empty one as absent, and reads no level and no quality. The
// format's default is the caller's. It refuses in this order: rate, ch, bits
// and bitrate, each empty then not a positive integer; then q; then hev2;
// then CheckWire.
func ParseQuery(get func(name string) (value string, present bool)) (p Params, q string, err error) {
	bad := func(format string, args ...any) (Params, string, error) {
		return Params{}, "", waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	p.Format, _ = get("format")
	p.Container, _ = get("container")
	// Absent is the only spelling of the default, so present and empty is
	// refused rather than read as zero.
	for _, n := range []struct {
		name, omit string
		dst        *int
	}{
		{"rate", "keep the source's value", &p.Rate},
		{"ch", "keep the source's value", &p.Channels},
		{"bits", "keep the source's value", &p.Bits},
		{"bitrate", "take the format's default", &p.Bitrate},
	} {
		v, ok := get(n.name)
		if !ok {
			continue
		}
		if v == "" {
			return bad("%s is present but empty; omit it to %s", n.name, n.omit)
		}
		x, err := strconv.Atoi(v)
		if err != nil || x <= 0 {
			return bad("%s %q: want a positive integer", n.name, v)
		}
		*n.dst = x
	}
	q, _ = get("q")
	switch {
	case q == "":
	case p.Bitrate != 0:
		return bad("q and bitrate are mutually exclusive")
	default:
		if _, ok := presets[q]; !ok {
			return Params{}, "", badQ(q)
		}
	}
	if v, ok := get("hev2"); ok {
		if v == "" {
			return bad("hev2 is present but empty; omit it to encode HE-AAC v1")
		}
		if p.HEv2, err = strconv.ParseBool(v); err != nil {
			return bad("hev2 %q: want a boolean", v)
		}
	}
	if err := p.CheckWire(); err != nil {
		return Params{}, "", err
	}
	return p, q, nil
}

// preset is what one q value names: a bit rate where the format takes one,
// a Vorbis quality where it takes that instead.
type preset struct {
	kbps    int
	quality float64
}

// presets are the q values. The mp3 encoder clamps a bit rate to one its
// layer holds, so a preset never fails there.
var presets = map[string]preset{
	"low":  {kbps: 96, quality: 2},
	"med":  {kbps: 128, quality: 4},
	"high": {kbps: 192, quality: 6},
}

// qValues spells the q values in every sentence that lists them.
const qValues = "low, med, or high"

func badQ(q string) error {
	return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("q %q: want %s", q, qValues))
}

// qKnobs are the knobs a preset can set.
const qKnobs = waxflow.KnobBitrate | waxflow.KnobVorbisQuality

// Preset resolves q for format: the preset's bit rate where the engine says
// the format takes one, its Vorbis quality where it takes that, and a refusal
// elsewhere. An unknown format is the engine's refusal of it. An empty q
// names nothing.
func Preset(format, q string) (bitrate int, vorbisQuality float64, err error) {
	if q == "" {
		return 0, 0, nil
	}
	p, ok := presets[q]
	if !ok {
		return 0, 0, badQ(q)
	}
	err = waxflow.CheckKnobs(format, waxflow.KnobBitrate)
	ke, isKnob := errors.AsType[*waxflow.KnobError](err)
	switch {
	case err == nil:
		return p.kbps, 0, nil
	case !isKnob:
		return 0, 0, err
	case waxflow.CheckKnobs(format, waxflow.KnobVorbisQuality) == nil:
		return 0, p.quality, nil
	}
	var takers []string
	for _, o := range waxflow.Outputs() {
		if o.Knobs&qKnobs != 0 {
			takers = append(takers, o.Name)
		}
	}
	return 0, 0, refusal("q", takers, ke)
}

// knobParams are the wire names of the knobs Params sets.
var knobParams = map[waxflow.Knob]string{
	waxflow.KnobBitrate:       "bitrate",
	waxflow.KnobHEAACv2:       "hev2",
	waxflow.KnobVorbisQuality: "vorbisQuality",
	waxflow.KnobFLACLevel:     "flacLevel",
	waxflow.KnobWavPackLevel:  "wavpackLevel",
	waxflow.KnobAPELevel:      "apeLevel",
}

// Wording is how one surface words a knob its format does not take, or a
// value out of the knob's range.
type Wording struct {
	// names spells each knob the surface takes; another passes through in
	// the engine's words.
	names map[waxflow.Knob]string
	// formats are the ones the surface produces; nil is every format.
	formats []string
	// quality is what a bit rate on a quality-driven format is pointed to;
	// empty keeps the generic sentence.
	quality string
}

// NewWording is the wording of a surface that produces every format, with
// names spelling its knobs and quality what a bit rate on a quality-driven
// format is pointed to.
func NewWording(names map[waxflow.Knob]string, quality string) Wording {
	return Wording{names: names, quality: quality}
}

// The surfaces' wordings: /stream takes bitrate, with q worded by Preset; an
// HLS mint bitrate or bitrates and only the segmented formats; a job bitrate
// or vorbisQuality.
var (
	Stream = Wording{names: knobParams, quality: "q=" + qValues}
	HLS    = Wording{names: hlsParams(), formats: waxflow.SegmentedFormats()}
	Job    = Wording{names: knobParams, quality: knobParams[waxflow.KnobVorbisQuality]}
)

func hlsParams() map[waxflow.Knob]string {
	m := maps.Clone(knobParams)
	m[waxflow.KnobBitrate] = "bitrate/bitrates"
	return m
}

// Reword words the engine's *waxflow.KnobError in the surface's parameter
// names, naming the parameter, the format and the formats that take it among
// those the surface produces, and its *waxflow.RangeError naming the
// parameter, the value and what it takes. Any other error, nil included, and
// one on a knob the surface does not name, passes through.
func (w Wording) Reword(err error) error {
	if re, ok := errors.AsType[*waxflow.RangeError](err); ok {
		return w.outOfRange(re, err)
	}
	ke, ok := errors.AsType[*waxflow.KnobError](err)
	if !ok {
		return err
	}
	param, ok := w.names[ke.Knob]
	if !ok {
		return err
	}
	// The engine says which formats are quality-driven: those that take a
	// Vorbis quality.
	if ke.Knob == waxflow.KnobBitrate && w.quality != "" && waxflow.CheckKnobs(ke.Format, waxflow.KnobVorbisQuality) == nil {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf(
			"%s does not apply to %s, which is quality-driven; use %s", param, ke.Format, w.quality))
	}
	var takers []string
	for _, f := range ke.Takers {
		if w.formats == nil || slices.Contains(w.formats, f) {
			takers = append(takers, f)
		}
	}
	return refusal(param, takers, ke)
}

// outOfRange words re, which err wraps, in the surface's name for its knob.
func (w Wording) outOfRange(re *waxflow.RangeError, err error) error {
	param, ok := w.names[re.Knob]
	if !ok {
		return err
	}
	v := fmt.Sprint(re.Value)
	if s, ok := re.Value.(string); ok {
		v = strconv.Quote(s)
	}
	if re.Unwritten {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("%s %s decodes but is not written; want %s", param, v, re.Want()))
	}
	return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("%s %s: want %s", param, v, re.Want()))
}

// refusal is the one sentence every surface refuses a parameter its format
// does not take in, naming the formats that take it. A parameter spelled
// "a/b" is two and takes "apply".
func refusal(param string, takers []string, ke *waxflow.KnobError) error {
	verb := "applies"
	if strings.Contains(param, "/") {
		verb = "apply"
	}
	list := strings.Join(takers, ", ")
	if ke.Lossless {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("%s %s to lossy output (%s); %s is lossless", param, verb, list, ke.Format))
	}
	return waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("%s %s to %s output; this request names %s", param, verb, list, ke.Format))
}
