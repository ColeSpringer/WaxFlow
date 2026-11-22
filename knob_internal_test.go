package waxflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
					_, cerr := e.PlanCut(track, opts, []Span{{From: 0, To: 2048}}, 1024)
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
		Codec:   codec.HEAAC,
		Fmt:     audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: 44100,
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
	_, cerr := e.PlanCut(track, opts, []Span{{From: 0, To: 2048}}, 1024)
	for _, got := range []struct {
		what string
		err  error
	}{{"PlanTranscode", perr}, {"PlanRemux", rerr}, {"PlanCut", cerr}} {
		if waxerr.CodeOf(got.err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: %v, want an invalid request", got.what, got.err)
		}
	}
}
