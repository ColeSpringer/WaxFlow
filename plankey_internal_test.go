package waxflow

import (
	"context"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// optionCase sets one TranscodeOptions field to value on options whose
// Format is format, a format that takes the field.
type optionCase struct {
	format string
	value  any
}

var optionCases = map[string]optionCase{
	"Format":          {format: "wav", value: "flac"},
	"Container":       {format: "flac", value: "mka"},
	"SpliceTrims":     {format: "flac", value: true},
	"Rate":            {format: "flac", value: 48000},
	"Channels":        {format: "flac", value: 1},
	"BitDepth":        {format: "flac", value: 24},
	"GainDB":          {format: "flac", value: -3.0},
	"Dynamics":        {format: "flac", value: gain.PresetVoice},
	"FromSample":      {format: "flac", value: 480},
	"FLACLevel":       {format: "flac", value: 8},
	"WavPackLevel":    {format: "wavpack", value: WavPackLevelHigh},
	"APELevel":        {format: "ape", value: APELevelHigh},
	"Bitrate":         {format: "mp3", value: 192000},
	"VBR":             {format: "mp3", value: true},
	"HEAACv2":         {format: "he-aac", value: true},
	"OpusComplexity":  {format: "opus", value: 8},
	"OpusSignal":      {format: "opus", value: "voice"},
	"VorbisQuality":   {format: "vorbis", value: 5.0},
	"Shaping":         {format: "flac", value: dither.Shaped},
	"ResampleProfile": {format: "flac", value: resample.Fast},
}

// withField returns opts with the named field set to v.
func withField(t *testing.T, opts TranscodeOptions, name string, v any) TranscodeOptions {
	t.Helper()
	f := reflect.ValueOf(&opts).Elem().FieldByName(name)
	f.Set(reflect.ValueOf(v).Convert(f.Type()))
	return opts
}

// TestPlanCacheKeysOnEveryOption: each option set alone keys a plan of its
// own, and a seek shares its source's plan.
func TestPlanCacheKeysOnEveryOption(t *testing.T) {
	track := openWAV(t, New(), sineWAV(t)).Info().Default()
	for f := range reflect.TypeFor[TranscodeOptions]().Fields() {
		c, ok := optionCases[f.Name]
		if !ok {
			t.Errorf("TranscodeOptions.%s has no case: give it a value and a format that takes it", f.Name)
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			e := New()
			base := TranscodeOptions{Format: c.format}
			if _, err := e.PlanTranscode(track, base); err != nil {
				t.Fatalf("base %+v: %v", base, err)
			}
			_, err := e.PlanTranscode(track, withField(t, base, f.Name, c.value))
			want := 2
			switch {
			case err != nil:
				t.Fatalf("%s=%v: %v", f.Name, c.value, err)
			case f.Name == "FromSample":
				want = 1
			}
			if got := len(e.plans); got != want {
				t.Errorf("%s=%v left %d plans cached, want %d", f.Name, c.value, got, want)
			}
		})
	}
}

// TestNonFiniteOptionsAreRefused: a NaN or infinite float option is refused
// in its field's name by Check and by every entry point that takes the
// options, and leaves nothing in the plan cache.
func TestNonFiniteOptionsAreRefused(t *testing.T) {
	raw := sineWAV(t)
	track := openWAV(t, New(), raw).Info().Default()
	floats := 0
	for f := range reflect.TypeFor[TranscodeOptions]().Fields() {
		if k := f.Type.Kind(); k != reflect.Float32 && k != reflect.Float64 {
			continue
		}
		floats++
		c, ok := optionCases[f.Name]
		if !ok {
			t.Errorf("TranscodeOptions.%s has no case: give it a value and a format that takes it", f.Name)
			continue
		}
		base := TranscodeOptions{Format: c.format}
		if _, err := New().PlanTranscode(track, withField(t, base, f.Name, c.value)); err != nil {
			t.Fatalf("%s=%v on %s does not plan: %v", f.Name, c.value, c.format, err)
		}
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			t.Run(fmt.Sprintf("%s=%g", f.Name, v), func(t *testing.T) {
				e := New()
				opts := withField(t, base, f.Name, v)
				ctx := context.Background()
				_, perr := e.PlanTranscode(track, opts)
				_, rerr := e.PlanRemux(track, opts)
				spans := []Span{{From: 0, To: 2048}}
				header := container.Track{Samples: -1}
				emit := func(mp4.Segment) error { return nil }
				_, cerr := e.PlanCut(track, opts, spans, knownGrid(1024))
				_, cserr := e.PlanCutSegments(track, opts, spans, knownGrid(1024), 2)
				_, ierr := e.InitSegment(&SegmentPlan{TranscodePlan: TranscodePlan{Format: track.Fmt}}, opts)
				_, terr := e.TranscodeMedia(ctx, openWAV(t, e, raw), io.Discard, opts)
				_, serr := e.TranscodeSegments(ctx, container.BytesSource(raw), "wav", opts, SegmentedOptions{}, emit)
				_, smerr := e.TranscodeSegmentsMedia(ctx, openWAV(t, e, raw), opts, SegmentedOptions{}, emit)
				_, xerr := e.CutStream(ctx, container.BytesSource(raw), "wav", io.Discard, opts, spans, 1024, header)
				_, xserr := e.CutSegments(ctx, container.BytesSource(raw), "wav", opts, spans, 1024, header,
					SegmentedOptions{SegmentSamples: 4096}, emit)
				for _, got := range []struct {
					what string
					err  error
				}{
					{"Check", opts.Check()}, {"PlanTranscode", perr}, {"PlanRemux", rerr}, {"PlanCut", cerr},
					{"PlanCutSegments", cserr}, {"InitSegment", ierr}, {"TranscodeMedia", terr},
					{"TranscodeSegments", serr}, {"TranscodeSegmentsMedia", smerr}, {"CutStream", xerr},
					{"CutSegments", xserr},
				} {
					if waxerr.CodeOf(got.err) != waxerr.CodeInvalidRequest || !strings.Contains(fmt.Sprint(got.err), f.Name) {
						t.Errorf("%s: %v, want CodeInvalidRequest naming %s", got.what, got.err, f.Name)
					}
				}
				if n := len(e.plans); n != 0 {
					t.Errorf("%d plans cached, want none", n)
				}
			})
		}
	}
	if floats == 0 {
		t.Error("TranscodeOptions has no float field; this test checks nothing")
	}
}

// TestRemuxableDeclinesEveryOtherOption: a remux copies packets, so any option
// but the format, the container, SpliceTrims and the kernel selections asks for
// a transform and declines, while those five set alone still remux, as does
// OpusSignal "auto", the unset hint spelled out.
func TestRemuxableDeclinesEveryOtherOption(t *testing.T) {
	src := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	copies := map[string]bool{
		"Format": true, "Container": true, "SpliceTrims": true, "ResampleProfile": true, "Shaping": true,
	}
	for f := range reflect.TypeFor[TranscodeOptions]().Fields() {
		c, ok := optionCases[f.Name]
		if !ok {
			t.Errorf("TranscodeOptions.%s has no case: give it a value and a format that takes it", f.Name)
			continue
		}
		opts := withField(t, TranscodeOptions{Format: "flac"}, f.Name, c.value)
		if got := remuxable(opts, src); got != copies[f.Name] {
			t.Errorf("remuxable with %s=%v = %v, want %v", f.Name, c.value, got, copies[f.Name])
		}
	}
	if !remuxable(TranscodeOptions{Format: "opus", OpusSignal: "auto"}, src) {
		t.Error(`remuxable with OpusSignal="auto" = false, want true`)
	}
}

// TestOpusSignalAutoSharesTheUnsetPlan: OpusSignal "auto" and "" key one plan.
func TestOpusSignalAutoSharesTheUnsetPlan(t *testing.T) {
	track := openWAV(t, New(), sineWAV(t)).Info().Default()
	e := New()
	for _, signal := range []string{"", "auto"} {
		if _, err := e.PlanTranscode(track, TranscodeOptions{Format: "opus", OpusSignal: signal}); err != nil {
			t.Fatalf("OpusSignal=%q: %v", signal, err)
		}
	}
	if n := len(e.plans); n != 1 {
		t.Errorf("%d plans cached, want 1", n)
	}
}
