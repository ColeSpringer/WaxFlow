package waxflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// knobTakes is each row's knob set, written out apart from the table.
var knobTakes = map[string]Knob{
	"wav": 0, "aiff": 0, "alac": 0,
	"flac":    KnobFLACLevel,
	"wavpack": KnobWavPackLevel,
	"ape":     KnobAPELevel,
	"mp3":     KnobBitrate | KnobVBR,
	"aac":     KnobBitrate,
	"he-aac":  KnobBitrate | KnobHEAACv2,
	"opus":    KnobBitrate | KnobVBR | KnobOpusComplexity | KnobOpusSignal,
	"vorbis":  KnobVorbisQuality,
}

// losslessRows are the rows whose encoder keeps every sample.
var losslessRows = map[string]bool{"wav": true, "aiff": true, "flac": true, "alac": true, "wavpack": true, "ape": true}

// knobValues are values every format that takes the knob accepts, the
// sentinels for a level or complexity of 0 among them.
var knobValues = map[Knob][]any{
	KnobBitrate:        {64000},
	KnobVBR:            {true},
	KnobVorbisQuality:  {5.0},
	KnobHEAACv2:        {true},
	KnobFLACLevel:      {8, FLACLevelFastest},
	KnobWavPackLevel:   {WavPackLevelHigh},
	KnobAPELevel:       {APELevelHigh},
	KnobOpusComplexity: {8, OpusComplexityLowest},
	KnobOpusSignal:     {"voice"},
}

// TestKnobsAreRefusedOffTheirRows: every knob on every row, through Check,
// the three plans and a run. A row that lacks the knob gets
// CodeUnsupportedFormat wrapping a *KnobError that names the knob, the format
// and its takers in table order; a row that takes it plans and encodes.
func TestKnobsAreRefusedOffTheirRows(t *testing.T) {
	raw := sineWAV(t)
	e := New()
	track := openWAV(t, e, raw).Info().Default()
	if len(knobTakes) != len(outputs) {
		t.Fatalf("knobTakes names %d rows, the table has %d", len(knobTakes), len(outputs))
	}
	for bit := range len(knobNames) {
		k := Knob(1) << bit
		if len(knobValues[k]) == 0 {
			t.Errorf("%s has no value to set", k)
		}
		var takers []string
		for _, row := range outputs {
			if knobTakes[row.name]&k != 0 {
				takers = append(takers, row.name)
			}
		}
		for i := range outputs {
			row := &outputs[i]
			takes, ok := knobTakes[row.name]
			if !ok {
				t.Fatalf("knobTakes has no row %s", row.name)
			}
			for _, v := range knobValues[k] {
				t.Run(fmt.Sprintf("%s=%v/%s", k, v, row.name), func(t *testing.T) {
					opts := withField(t, TranscodeOptions{Format: row.name}, k.String(), v)
					if got := opts.Knobs(); got != k {
						t.Fatalf("Knobs() = %v, want %v", got, k)
					}
					ctx := context.Background()
					dst, err := os.Create(filepath.Join(t.TempDir(), "out"))
					if err != nil {
						t.Fatal(err)
					}
					defer dst.Close()
					_, perr := e.PlanTranscode(track, opts)
					_, rerr := e.PlanRemux(track, opts)
					_, cerr := e.PlanCut(track, opts, []Span{{From: 0, To: 2048}}, knownGrid(1024))
					_, terr := e.TranscodeMedia(ctx, openWAV(t, e, raw), dst, opts)
					for _, got := range []struct {
						what string
						err  error
					}{
						{"Check", opts.Check()}, {"PlanTranscode", perr}, {"PlanRemux", rerr}, {"PlanCut", cerr},
						{"TranscodeMedia", terr},
					} {
						ke, refused := errors.AsType[*KnobError](got.err)
						switch {
						case takes&k != 0 && got.err != nil:
							t.Errorf("%s: %v, want %s taken", got.what, got.err, k)
						case takes&k != 0:
						case !refused || waxerr.CodeOf(got.err) != waxerr.CodeUnsupportedFormat:
							t.Errorf("%s: %v, want CodeUnsupportedFormat wrapping a *KnobError", got.what, got.err)
						case ke.Knob != k || ke.Format != row.name || !slices.Equal(ke.Takers, takers):
							t.Errorf("%s: %+v, want knob %s, format %s, takers %v", got.what, ke, k, row.name, takers)
						case ke.Lossless != (losslessRows[row.name] && k&(KnobBitrate|KnobVBR) != 0):
							t.Errorf("%s: Lossless = %v on %s for %s", got.what, ke.Lossless, row.name, k)
						}
					}
				})
			}
		}
	}
}

// TestCheckKnobsAnswersAsCheck: CheckKnobs gives every format, for each knob
// alone and for all of them at once, the answer Check gives the options that
// set those knobs, and refuses an unknown format as Check does.
func TestCheckKnobsAnswersAsCheck(t *testing.T) {
	same := func(what string, got, want error) {
		t.Helper()
		gk, _ := errors.AsType[*KnobError](got)
		wk, _ := errors.AsType[*KnobError](want)
		if (got == nil) != (want == nil) || got != nil && (got.Error() != want.Error() ||
			waxerr.CodeOf(got) != waxerr.CodeOf(want) || !reflect.DeepEqual(gk, wk)) {
			t.Errorf("%s: CheckKnobs = %v, Check = %v", what, got, want)
		}
	}
	for _, row := range outputs {
		all := TranscodeOptions{Format: row.name}
		var set Knob
		for bit := range len(knobNames) {
			k := Knob(1) << bit
			v := knobValues[k][0]
			opts := withField(t, TranscodeOptions{Format: row.name}, k.String(), v)
			got := CheckKnobs(row.name, k)
			same(fmt.Sprintf("%s %s=%v", row.name, k, v), got, opts.Check())
			if (got == nil) != (knobTakes[row.name]&k != 0) {
				t.Errorf("CheckKnobs(%s, %s) = %v, want it taken: %v", row.name, k, got, knobTakes[row.name]&k != 0)
			}
			all = withField(t, all, k.String(), v)
			set |= k
		}
		same(row.name+" every knob", CheckKnobs(row.name, set), all.Check())
		same(row.name+" no knob", CheckKnobs(row.name, 0), TranscodeOptions{Format: row.name}.Check())
	}
	for _, f := range []string{"opsu", ""} {
		same(fmt.Sprintf("format %q", f), CheckKnobs(f, KnobBitrate), TranscodeOptions{Format: f, Bitrate: 64000}.Check())
	}
}

// TestCheckKnobsRefusesABitThatNamesNoKnob: a set holding a bit past the
// knobs is CodeInvalidRequest naming the lowest such bit, before the format or
// its knobs are asked.
func TestCheckKnobsRefusesABitThatNamesNoKnob(t *testing.T) {
	stray := Knob(1) << len(knobNames)
	for _, c := range []struct {
		format string
		set    Knob
	}{
		{"flac", Knob(1) << 12},
		{"flac", KnobBitrate | Knob(1)<<12 | Knob(1)<<14},
		{"opus", KnobBitrate | stray},
		{"opsu", stray},
	} {
		err := CheckKnobs(c.format, c.set)
		want := fmt.Sprintf("waxflow: %s is not a knob", (c.set&^(stray-1))&-(c.set&^(stray-1)))
		if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || err.Error() != want {
			t.Errorf("CheckKnobs(%q, %d) = %v (%s), want %s %q", c.format, uint16(c.set), err,
				waxerr.CodeOf(err), waxerr.CodeInvalidRequest, want)
		}
	}
}

// TestARangeRefusalIsTyped: Check refuses a knob out of range as
// CodeInvalidRequest wrapping a *RangeError that names the knob, the value as
// given and what the knob takes, in the sentence it always gave.
func TestARangeRefusalIsTyped(t *testing.T) {
	written := []string{"1000", "2000", "3000"}
	for _, c := range []struct {
		opts TranscodeOptions
		want RangeError
		text string
		take string
	}{
		{TranscodeOptions{Format: "flac", FLACLevel: 9}, RangeError{Knob: KnobFLACLevel, Value: 9, Min: -1, Max: 8},
			"waxflow: FLAC level 9 outside -1..8", "-1 to 8"},
		{TranscodeOptions{Format: "wavpack", WavPackLevel: 7}, RangeError{Knob: KnobWavPackLevel, Value: 7, Min: 1, Max: 4},
			"waxflow: WavPack level 7 outside 1..4", "1 to 4"},
		{TranscodeOptions{Format: "ape", APELevel: 4000}, RangeError{Knob: KnobAPELevel, Value: 4000, Values: written, Unwritten: true},
			"waxflow: APE level 4000 decodes but is not written; want 1000, 2000, or 3000", "1000, 2000, or 3000"},
		{TranscodeOptions{Format: "ape", APELevel: 2500}, RangeError{Knob: KnobAPELevel, Value: 2500, Values: written},
			"waxflow: APE level 2500 is not one of 1000, 2000, or 3000", "1000, 2000, or 3000"},
		{TranscodeOptions{Format: "opus", OpusComplexity: 11}, RangeError{Knob: KnobOpusComplexity, Value: 11, Min: -1, Max: 10},
			"waxflow: Opus complexity 11 outside -1..10", "-1 to 10"},
		{TranscodeOptions{Format: "opus", OpusSignal: "foo"}, RangeError{Knob: KnobOpusSignal, Value: "foo", Values: []string{"auto", "voice", "music"}},
			`opus signal hint "foo" is not auto, voice, or music`, "auto, voice, or music"},
		{TranscodeOptions{Format: "vorbis", VorbisQuality: -1.5}, RangeError{Knob: KnobVorbisQuality, Value: -1.5, Min: -1, Max: 10},
			"waxflow: Vorbis quality -1.5 outside -1..10", "-1 to 10"},
	} {
		err := c.opts.Check()
		re, ok := errors.AsType[*RangeError](err)
		switch {
		case waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !ok:
			t.Errorf("%+v: %v (%s), want an invalid request wrapping a *RangeError", c.opts, err, waxerr.CodeOf(err))
		case !reflect.DeepEqual(*re, c.want):
			t.Errorf("%+v: %+v, want %+v", c.opts, *re, c.want)
		case err.Error() != c.text || re.Error() != c.text:
			t.Errorf("%+v: %q, want %q", c.opts, err.Error(), c.text)
		case re.Want() != c.take:
			t.Errorf("%+v: Want() = %q, want %q", c.opts, re.Want(), c.take)
		}
	}
	// What a caller builds itself reads as English: two values take no comma,
	// and a knob with no subject of its own goes by its field name.
	if got := (&RangeError{Knob: KnobOpusSignal, Values: []string{"voice", "music"}}).Want(); got != "voice or music" {
		t.Errorf("two values: Want() = %q, want %q", got, "voice or music")
	}
	if got := (&RangeError{Knob: KnobBitrate, Value: 5, Min: 1, Max: 2}).Error(); got != "waxflow: Bitrate 5 outside 1..2" {
		t.Errorf("a knob with no subject: %q, want %q", got, "waxflow: Bitrate 5 outside 1..2")
	}
}

// TestOutputsNameEachFormatsLossAndKnobs: Outputs reports each row's Lossy and
// Knobs as written out apart from the table, and Check refuses exactly the
// knobs a format's Knobs leave out.
func TestOutputsNameEachFormatsLossAndKnobs(t *testing.T) {
	outs := Outputs()
	if len(outs) != len(knobTakes) {
		t.Fatalf("Outputs lists %d formats, knobTakes %d", len(outs), len(knobTakes))
	}
	for _, o := range outs {
		if o.Lossy == losslessRows[o.Name] {
			t.Errorf("%s: Lossy = %v", o.Name, o.Lossy)
		}
		if want, ok := knobTakes[o.Name]; !ok || o.Knobs != want {
			t.Errorf("%s: Knobs = %v, want %v", o.Name, o.Knobs, want)
		}
		for bit := range len(knobNames) {
			k := Knob(1) << bit
			err := withField(t, TranscodeOptions{Format: o.Name}, k.String(), knobValues[k][0]).Check()
			if refused := err != nil; refused != (o.Knobs&k == 0) {
				t.Errorf("%s with %s: Check = %v, but Knobs = %v", o.Name, k, err, o.Knobs)
			}
		}
	}
}

// TestOpusSignalAutoIsUnset: "auto" is OpusSignal's default spelled out, so
// no row refuses it.
func TestOpusSignalAutoIsUnset(t *testing.T) {
	track := openWAV(t, New(), sineWAV(t)).Info().Default()
	for _, row := range outputs {
		opts := TranscodeOptions{Format: row.name, OpusSignal: "auto"}
		if k := opts.Knobs(); k != 0 {
			t.Errorf("%s: Knobs() = %v, want none", row.name, k)
		}
		if err := opts.Check(); err != nil {
			t.Errorf("%s: Check: %v", row.name, err)
		}
		if _, err := New().PlanTranscode(track, opts); err != nil {
			t.Errorf("%s: PlanTranscode: %v", row.name, err)
		}
	}
}

// TestRemuxKnobsFollowTheNamedRow: format=aac on an HE-AAC track is checked
// against aac before the remux takes the he-aac row, so HEAACv2 is refused,
// while a Bitrate, which aac takes, declines the copy for a transcode.
func TestRemuxKnobsFollowTheNamedRow(t *testing.T) {
	track := container.Track{
		Codec: codec.HEAAC,
		// Explicit SBR over a 22.05 kHz stereo core.
		CodecConfig: []byte{0x2B, 0x92, 0x08, 0x00},
		Fmt:         audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples:     44100,
	}
	e := New()
	if rp, err := e.PlanRemux(track, TranscodeOptions{Format: "aac"}); err != nil || rp == nil {
		t.Fatalf("a bare aac request on an HE-AAC track = %v, %v, want a remux", rp, err)
	}
	if rp, err := e.PlanRemux(track, TranscodeOptions{Format: "aac", HEAACv2: true}); waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Errorf("aac with HEAACv2 = %v, %v, want the knob refused", rp, err)
	}
	if rp, err := e.PlanRemux(track, TranscodeOptions{Format: "aac", Bitrate: 64000}); err != nil || rp != nil {
		t.Errorf("aac with a Bitrate = %v, %v, want a decline", rp, err)
	}
}

// TestNegativeFromSampleIsRefusedByEveryPlan: the remux and cut plans refuse a
// negative FromSample as an invalid request, as the transcode plan does.
func TestNegativeFromSampleIsRefusedByEveryPlan(t *testing.T) {
	track := openWAV(t, New(), sineWAV(t)).Info().Default()
	opts := TranscodeOptions{Format: "flac", FromSample: -1}
	e := New()
	_, perr := e.PlanTranscode(track, opts)
	_, rerr := e.PlanRemux(track, opts)
	_, cerr := e.PlanCut(track, opts, []Span{{From: 0, To: 2048}}, knownGrid(1024))
	for _, got := range []struct {
		what string
		err  error
	}{{"PlanTranscode", perr}, {"PlanRemux", rerr}, {"PlanCut", cerr}} {
		if waxerr.CodeOf(got.err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: %v, want an invalid request", got.what, got.err)
		}
	}
}
