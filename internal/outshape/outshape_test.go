package outshape

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// wantRefused fails unless err carries code and exactly msg.
func wantRefused(t *testing.T, what string, err error, code waxerr.Code, msg string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted, want %q", what, msg)
		return
	}
	if got := waxerr.CodeOf(err); got != code {
		t.Errorf("%s: code %s, want %s (%v)", what, got, code, err)
	}
	if got := waxerr.Message(err); got != msg {
		t.Errorf("%s: %q, want %q", what, got, msg)
	}
}

// TestParamsCheckWire: level -1 and quality -1 are legal; bits outside 16 and
// 24, a negative rate, ch or bitrate, a non-finite quality and a bit rate past
// MaxKbps are refused.
func TestParamsCheckWire(t *testing.T) {
	for _, p := range []Params{
		{},
		{FLACLevel: -1},
		{VorbisQuality: -1},
		{Bits: 16},
		{Bits: 24},
		{Bitrate: MaxKbps},
	} {
		if err := p.CheckWire(); err != nil {
			t.Errorf("%+v: %v, want it accepted", p, err)
		}
	}
	for _, c := range []struct {
		p    Params
		want string
	}{
		{Params{Bits: 20}, "bits 20: want 16 or 24"},
		{Params{Bits: -16}, "bits -16: want 16 or 24"},
		{Params{Rate: -1}, "rate -1: want 0 for the source's value or a positive integer"},
		{Params{Channels: -2}, "ch -2: want 0 for the source's value or a positive integer"},
		{Params{Bitrate: -1}, "bitrate -1: want 0 for the format's default or a positive integer"},
		{Params{Rate: -1, Bitrate: -1}, "rate -1: want 0 for the source's value or a positive integer"},
		{Params{VorbisQuality: math.NaN()}, "vorbisQuality NaN: want a finite number"},
		{Params{VorbisQuality: math.Inf(1)}, "vorbisQuality +Inf: want a finite number"},
		{Params{Bitrate: MaxKbps + 1}, fmt.Sprintf("bitrate %d: want at most %d kbit/s", MaxKbps+1, MaxKbps)},
	} {
		wantRefused(t, fmt.Sprintf("%+v", c.p), c.p.CheckWire(), waxerr.CodeInvalidRequest, c.want)
	}
}

// wraps is a kbit/s value whose product by 1000 wraps the build's int to a
// positive bit rate the engine accepts: 128000 on 64 bits, 704 on 32.
func wraps() int {
	if bits.UintSize == 32 {
		return 4294968
	}
	var w uint64 = 2305843009213694080
	return int(w)
}

// mapped sets every field of Params to a value other than its zero.
var mapped = Params{
	Format: "opus", Container: "webm", Rate: 44100, Channels: 2, Bits: 24,
	Bitrate: 96, HEv2: true, VorbisQuality: 4.5, FLACLevel: -1, WavPackLevel: 3, APELevel: 2000,
}

// TestOptionsIsTheOneMapper: every field lands on its option, the bit rate
// multiplied from kbit/s, beside the caller's gain, dynamics and profile. A
// bit rate outside 0..MaxKbps maps to -1, which the engine refuses.
func TestOptionsIsTheOneMapper(t *testing.T) {
	v := reflect.ValueOf(mapped)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("the fixture leaves %s unset", v.Type().Field(i).Name)
		}
	}
	got := mapped.Options(-3.5, gain.PresetVoice, resample.Fast)
	want := waxflow.TranscodeOptions{
		Format: "opus", Container: "webm", Rate: 44100, Channels: 2, BitDepth: 24,
		GainDB: -3.5, Dynamics: gain.PresetVoice, ResampleProfile: resample.Fast,
		Bitrate: 96000, HEAACv2: true, VorbisQuality: 4.5,
		FLACLevel: -1, WavPackLevel: 3, APELevel: 2000,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Options = %+v\nwant      %+v", got, want)
	}
	// A field that reaches no option leaves the options as they were.
	for i := range v.NumField() {
		p := reflect.New(v.Type()).Elem()
		p.Set(v)
		p.Field(i).SetZero()
		if reflect.DeepEqual(p.Interface().(Params).Options(-3.5, gain.PresetVoice, resample.Fast), got) {
			t.Errorf("%s reaches no option", v.Type().Field(i).Name)
		}
	}
	for _, c := range []struct{ kbps, want int }{
		{MaxKbps, MaxKbps * 1000},
		{MaxKbps + 1, -1},
		{wraps(), -1},
		{math.MaxInt, -1},
		{1 << (bits.UintSize - 3), -1},
		{-1, -1},
		{math.MinInt, -1},
	} {
		if got := (Params{Bitrate: c.kbps}).Options(0, gain.PresetOff, "").Bitrate; got != c.want {
			t.Errorf("bitrate %d kbit/s maps to %d bit/s, want %d", c.kbps, got, c.want)
		}
	}
}

// TestKnobsAsksTheOptions: each knob field counts, a bit rate past the bound
// among them, and no shaping field does.
func TestKnobsAsksTheOptions(t *testing.T) {
	if (Params{Format: "flac", Container: "mka", Rate: 48000, Channels: 2, Bits: 16}).Knobs() {
		t.Error("shaping alone reports a knob")
	}
	for _, p := range []Params{
		{Bitrate: 96}, {HEv2: true}, {VorbisQuality: -1},
		{FLACLevel: -1}, {WavPackLevel: 1}, {APELevel: 1000},
		{Bitrate: MaxKbps + 1}, {Bitrate: wraps()}, {Bitrate: math.MaxInt}, {Bitrate: -1},
		{Bitrate: 1 << (bits.UintSize - 3)}, {Bitrate: math.MinInt}, // times 1000 wraps to 0
	} {
		if !p.Knobs() {
			t.Errorf("%+v reports no knob", p)
		}
	}
}

// TestCheckWordsEachSurface: Check runs the wire policy, then words a knob
// the format lacks in the surface's parameter names, naming the takers among
// the formats the surface produces, and a value out of a knob's range by the
// parameter, on the engine's scale.
func TestCheckWordsEachSurface(t *testing.T) {
	for _, c := range []struct {
		p    Params
		w    Wording
		code waxerr.Code
		want string
	}{
		{Params{Format: "flac", Bits: 20, Bitrate: 128}, Job, waxerr.CodeInvalidRequest, "bits 20: want 16 or 24"},
		{Params{Format: "flac", Bitrate: 128}, Stream, waxerr.CodeUnsupportedFormat,
			"bitrate applies to lossy output (opus, mp3, aac, he-aac); flac is lossless"},
		{Params{Format: "flac", Bitrate: 128}, HLS, waxerr.CodeUnsupportedFormat,
			"bitrate/bitrates apply to lossy output (opus, aac, he-aac); flac is lossless"},
		{Params{Format: "flac", Bitrate: 128}, Job, waxerr.CodeUnsupportedFormat,
			"bitrate applies to lossy output (opus, mp3, aac, he-aac); flac is lossless"},
		{Params{Format: "vorbis", Bitrate: 128}, Stream, waxerr.CodeUnsupportedFormat,
			"bitrate does not apply to vorbis, which is quality-driven; use q=low, med, or high"},
		{Params{Format: "vorbis", Bitrate: 128}, Job, waxerr.CodeUnsupportedFormat,
			"bitrate does not apply to vorbis, which is quality-driven; use vorbisQuality"},
		{Params{Format: "aac", HEv2: true}, HLS, waxerr.CodeUnsupportedFormat,
			"hev2 applies to he-aac output; this request names aac"},
		{Params{Format: "opus", VorbisQuality: 4}, Job, waxerr.CodeUnsupportedFormat,
			"vorbisQuality applies to vorbis output; this request names opus"},
		{Params{Format: "opus", FLACLevel: 5}, Job, waxerr.CodeUnsupportedFormat,
			"flacLevel applies to flac output; this request names opus"},
		{Params{Format: "opus", WavPackLevel: 3}, Job, waxerr.CodeUnsupportedFormat,
			"wavpackLevel applies to wavpack output; this request names opus"},
		{Params{Format: "opus", APELevel: 3000}, Job, waxerr.CodeUnsupportedFormat,
			"apeLevel applies to ape output; this request names opus"},
		{Params{Format: "vorbis", VorbisQuality: 50}, Job, waxerr.CodeInvalidRequest, "vorbisQuality 50: want -1 to 10"},
		{Params{Format: "flac", FLACLevel: 9}, Job, waxerr.CodeInvalidRequest, "flacLevel 9: want -1 to 8"},
		{Params{Format: "wavpack", WavPackLevel: 7}, Job, waxerr.CodeInvalidRequest, "wavpackLevel 7: want 1 to 4"},
		{Params{Format: "ape", APELevel: 4000}, Job, waxerr.CodeInvalidRequest,
			"apeLevel 4000 decodes but is not written; want 1000, 2000, or 3000"},
		{Params{Format: "ape", APELevel: 2500}, Job, waxerr.CodeInvalidRequest, "apeLevel 2500: want 1000, 2000, or 3000"},
	} {
		wantRefused(t, fmt.Sprintf("%+v", c.p), c.p.Check(c.w), c.code, c.want)
	}
	if err := (Params{Format: "he-aac", Bitrate: 48, HEv2: true}).Check(Stream); err != nil {
		t.Errorf("he-aac v2 at 48 kbit/s: %v", err)
	}
	if err := (Params{Format: "ogg"}).Check(Job); waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Errorf("an unknown format: %v, want the engine's refusal", err)
	}
}

// TestJobBitrateHintAsksTheEngine: a job's bit rate on a format that takes a
// Vorbis quality points to vorbisQuality, and on any other format that lacks a
// bit rate reads the generic sentence. Which formats those are is the
// engine's answer, so the test names none.
func TestJobBitrateHintAsksTheEngine(t *testing.T) {
	hinted := 0
	for _, f := range waxflow.OutputFormats() {
		err := Params{Format: f, Bitrate: 128}.Check(Job)
		if waxflow.CheckKnobs(f, waxflow.KnobBitrate) == nil {
			continue
		}
		if waxflow.CheckKnobs(f, waxflow.KnobVorbisQuality) == nil {
			hinted++
			wantRefused(t, f, err, waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("bitrate does not apply to %s, which is quality-driven; use vorbisQuality", f))
		} else if err == nil || strings.Contains(err.Error(), "quality-driven") {
			t.Errorf("%s: %v, want the generic refusal", f, err)
		}
	}
	if hinted == 0 {
		t.Error("no format takes a Vorbis quality, so the hint went unchecked")
	}
}

// TestRewordPassesTheRestThrough: nil, an error that is no knob refusal, and a
// knob refusal or a range on a knob the surface does not name come back as
// they went in.
func TestRewordPassesTheRestThrough(t *testing.T) {
	if err := Stream.Reword(nil); err != nil {
		t.Errorf("nil reworded to %v", err)
	}
	plain := waxerr.New(waxerr.CodeInvalidRequest, "plain")
	if err := Stream.Reword(plain); err != plain {
		t.Errorf("a plain error reworded to %v", err)
	}
	unnamed := waxerr.Wrap(waxerr.CodeUnsupportedFormat, "",
		&waxflow.KnobError{Knob: waxflow.KnobOpusComplexity, Format: "mp3", Takers: []string{"opus"}})
	if err := Job.Reword(unnamed); !errors.Is(err, unnamed) {
		t.Errorf("an unnamed knob reworded to %v", err)
	}
	signal := waxflow.TranscodeOptions{Format: "opus", OpusSignal: "loud"}.Check()
	if err := Job.Reword(signal); !errors.Is(err, signal) {
		t.Errorf("a range on an unnamed knob reworded to %v", err)
	}
}

// TestRewordWordsARangeInFlagNames: a surface built by NewWording names the
// knob its own way, a string value quoted.
func TestRewordWordsARangeInFlagNames(t *testing.T) {
	w := NewWording(map[waxflow.Knob]string{waxflow.KnobOpusSignal: "--opus-signal"}, "")
	err := w.Reword(waxflow.TranscodeOptions{Format: "opus", OpusSignal: "loud"}.Check())
	wantRefused(t, "opus signal", err, waxerr.CodeInvalidRequest, `--opus-signal "loud": want auto, voice, or music`)
}

// query reads raw as a query-string surface does.
func query(t *testing.T, raw string) func(string) (string, bool) {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return func(name string) (string, bool) { return q.Get(name), q.Has(name) }
}

// TestParseQueryReads: the shaping parameters land, an empty q is absent, and
// no level or quality is read.
func TestParseQueryReads(t *testing.T) {
	for _, c := range []struct {
		raw   string
		want  Params
		wantQ string
	}{
		{"", Params{}, ""},
		{"format=aac&container=adts&rate=44100&ch=2&bits=16&bitrate=128&hev2=true",
			Params{Format: "aac", Container: "adts", Rate: 44100, Channels: 2, Bits: 16, Bitrate: 128, HEv2: true}, ""},
		{"q=", Params{}, ""},
		{"q=&bitrate=96", Params{Bitrate: 96}, ""},
		{"q=high", Params{}, "high"},
		{"hev2=false", Params{}, ""},
		{"flacLevel=5&wavpackLevel=2&apeLevel=1000&vorbisQuality=4", Params{}, ""},
	} {
		p, q, err := ParseQuery(query(t, c.raw))
		if err != nil || p != c.want || q != c.wantQ {
			t.Errorf("%q = %+v, %q, %v; want %+v, %q", c.raw, p, q, err, c.want, c.wantQ)
		}
	}
}

// TestParseQueryRefuses: each rule refuses in its words.
func TestParseQueryRefuses(t *testing.T) {
	keep := "; omit it to keep the source's value"
	for _, c := range []struct{ raw, want string }{
		{"rate=", "rate is present but empty" + keep},
		{"ch=", "ch is present but empty" + keep},
		{"bits=", "bits is present but empty" + keep},
		{"bitrate=", "bitrate is present but empty; omit it to take the format's default"},
		{"rate=x", `rate "x": want a positive integer`},
		{"rate=0", `rate "0": want a positive integer`},
		{"ch=-2", `ch "-2": want a positive integer`},
		{"ch=1.5", `ch "1.5": want a positive integer`},
		{"bits=0", `bits "0": want a positive integer`},
		{"bitrate=-5", `bitrate "-5": want a positive integer`},
		{"bitrate=99999999999999999999", `bitrate "99999999999999999999": want a positive integer`},
		{"q=low&bitrate=96", "q and bitrate are mutually exclusive"},
		{"q=loud", `q "loud": want low, med, or high`},
		{"hev2=", "hev2 is present but empty; omit it to encode HE-AAC v1"},
		{"hev2=maybe", `hev2 "maybe": want a boolean`},
		{"bits=20", "bits 20: want 16 or 24"},
		// The order: the integers, then q, then hev2, then the wire policy.
		{"rate=&q=loud", "rate is present but empty" + keep},
		{"rate=&ch=abc", "rate is present but empty" + keep},
		{"bits=20&q=loud", `q "loud": want low, med, or high`},
		{"q=loud&hev2=", `q "loud": want low, med, or high`},
		{fmt.Sprintf("bits=20&bitrate=%d&hev2=", MaxKbps+1), "hev2 is present but empty; omit it to encode HE-AAC v1"},
		{fmt.Sprintf("bits=20&bitrate=%d", MaxKbps+1), "bits 20: want 16 or 24"},
		{fmt.Sprintf("bitrate=%d", MaxKbps+1), fmt.Sprintf("bitrate %d: want at most %d kbit/s", MaxKbps+1, MaxKbps)},
	} {
		_, _, err := ParseQuery(query(t, c.raw))
		wantRefused(t, c.raw, err, waxerr.CodeInvalidRequest, c.want)
	}
}

// TestPresetResolvesPerFormat: q names 96, 128 or 192 kbit/s where the
// format takes a bit rate and quality 2, 4 or 6 on vorbis; an empty q names
// nothing.
func TestPresetResolvesPerFormat(t *testing.T) {
	for _, c := range []struct {
		format, q string
		kbps      int
		quality   float64
	}{
		{"mp3", "low", 96, 0}, {"opus", "med", 128, 0}, {"aac", "high", 192, 0}, {"he-aac", "low", 96, 0},
		{"vorbis", "low", 0, 2}, {"vorbis", "med", 0, 4}, {"vorbis", "high", 0, 6},
		{"mp3", "", 0, 0}, {"flac", "", 0, 0},
	} {
		kbps, quality, err := Preset(c.format, c.q)
		if kbps != c.kbps || quality != c.quality || err != nil {
			t.Errorf("Preset(%q, %q) = %d, %g, %v; want %d, %g, nil", c.format, c.q, kbps, quality, err, c.kbps, c.quality)
		}
	}
}

// TestPresetRefuses: an unknown q is malformed, a format that takes neither a
// bit rate nor a quality refuses q naming the formats that take it, and an
// unknown format is the engine's refusal of it.
func TestPresetRefuses(t *testing.T) {
	_, _, err := Preset("mp3", "loud")
	wantRefused(t, "q=loud", err, waxerr.CodeInvalidRequest, `q "loud": want low, med, or high`)
	for _, f := range []string{"flac", "wav", "ape"} {
		_, _, err := Preset(f, "high")
		wantRefused(t, f, err, waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("q applies to lossy output (opus, vorbis, mp3, aac, he-aac); %s is lossless", f))
	}
	_, _, err = Preset("nosuch", "high")
	if want := (Params{Format: "nosuch"}).Check(Stream); err == nil || err.Error() != want.Error() {
		t.Errorf("an unknown format: %v, want %v", err, want)
	}
}

// TestPresetTakersAskTheEngine: q is refused exactly where the engine says a
// format takes neither a bit rate nor a Vorbis quality, and the refusal names
// every format that takes one.
func TestPresetTakersAskTheEngine(t *testing.T) {
	var takers []string
	for _, o := range waxflow.Outputs() {
		if o.Knobs&qKnobs != 0 {
			takers = append(takers, o.Name)
		}
	}
	for _, o := range waxflow.Outputs() {
		_, _, err := Preset(o.Name, "med")
		if waxflow.CheckKnobs(o.Name, waxflow.KnobBitrate) == nil || waxflow.CheckKnobs(o.Name, waxflow.KnobVorbisQuality) == nil {
			if err != nil {
				t.Errorf("%s: %v, want q accepted", o.Name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), strings.Join(takers, ", ")) {
			t.Errorf("%s: %v, want a refusal naming %v", o.Name, err, takers)
		}
	}
}

// TestParamsHasNoJSONMethods: a type that embeds Params keeps its own JSON
// document, so Params implements no marshaling method.
func TestParamsHasNoJSONMethods(t *testing.T) {
	for _, v := range []any{Params{}, &Params{}} {
		switch v.(type) {
		case json.Marshaler, json.Unmarshaler, encoding.TextMarshaler, encoding.TextUnmarshaler:
			t.Errorf("%T implements a marshaling method", v)
		}
	}
}
