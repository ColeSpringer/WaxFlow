package waxflow

import (
	"bytes"
	"context"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// sineWAV is a short 16-bit stereo 44.1 kHz WAV.
func sineWAV(t *testing.T) []byte {
	t.Helper()
	chans := [][]float32{make([]float32, 4410), make([]float32, 4410)}
	for i := range chans[0] {
		v := float32(0.25 * math.Sin(2*math.Pi*440*float64(i)/44100))
		chans[0][i], chans[1][i] = v, v
	}
	return testutil.PCM16WAVBytes(t, 44100, chans)
}

func openWAV(t *testing.T, e *Engine, raw []byte) format.Media {
	t.Helper()
	med, err := e.OpenStream(container.BytesSource(raw), "wav")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { med.Close() })
	return med
}

// TestABadContainerIsRefusedFirst: a pair no form takes is refused in the
// container's words by a plan, a remux plan and a run alike, ahead of a bad
// encoder, knob or chain option. Only a non-finite float, a negative Bitrate or
// FromSample and an unknown format are refused before it.
func TestABadContainerIsRefusedFirst(t *testing.T) {
	raw := sineWAV(t)
	e := New()
	for _, tc := range []struct {
		name string
		opts TranscodeOptions
		want string
	}{
		{"bad encoder option", TranscodeOptions{Format: "flac", Container: "webm", FLACLevel: 99},
			`flac container "webm": want mka or ogg`},
		{"bad chain option", TranscodeOptions{Format: "wav", Container: "webm", Rate: -5},
			`wav container "webm": want mka`},
		{"knob the format lacks", TranscodeOptions{Format: "wav", Container: "webm", FLACLevel: 5},
			`wav container "webm": want mka`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			med := openWAV(t, e, raw)
			track := med.Info().Default()
			_, perr := e.PlanTranscode(track, tc.opts)
			_, rperr := e.PlanRemux(track, tc.opts)
			_, err := e.TranscodeMedia(context.Background(), med, io.Discard, tc.opts)
			for _, got := range []struct {
				what string
				err  error
			}{{"PlanTranscode", perr}, {"PlanRemux", rperr}, {"TranscodeMedia", err}} {
				if got.err == nil || !strings.Contains(got.err.Error(), tc.want) {
					t.Errorf("%s: %v, want the container refusal %q", got.what, got.err, tc.want)
				}
			}
		})
	}
}

// TestSegmentedPlansRefuseAsTheirRuns: a segmented plan refuses a request
// with the code and the words its run and InitSegment refuse it with. The
// remux rung declines a seek, which leaves the refusal to the transcode rung.
// The segmented cut refuses a format with no HLS form as the others do, and
// every segmented entry point refuses a Container.
func TestSegmentedPlansRefuseAsTheirRuns(t *testing.T) {
	raw := sineWAV(t)
	e := New()
	track := openWAV(t, e, raw).Info().Default()
	ctx := context.Background()
	emit := func(mp4.Segment) error { return nil }
	for _, tc := range []struct {
		name          string
		opts          TranscodeOptions
		remuxDeclines bool
	}{
		{name: "wav in webm", opts: TranscodeOptions{Format: "wav", Container: "webm"}},
		{name: "mp3 in mka", opts: TranscodeOptions{Format: "mp3", Container: "mka"}},
		{name: "a knob wav lacks", opts: TranscodeOptions{Format: "wav", FLACLevel: 5}},
		{name: "no segmented form", opts: TranscodeOptions{Format: "wav"}},
		{name: "a seek", opts: TranscodeOptions{Format: "flac", FromSample: 480}, remuxDeclines: true},
		{name: "aac in adts", opts: TranscodeOptions{Format: "aac", Container: "adts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, want := e.TranscodeSegments(ctx, container.BytesSource(raw), "wav", tc.opts, SegmentedOptions{}, emit)
			if want == nil {
				t.Fatal("TranscodeSegments accepted the request")
			}
			_, merr := e.TranscodeSegmentsMedia(ctx, openWAV(t, e, raw), tc.opts, SegmentedOptions{}, emit)
			_, ierr := e.InitSegment(&SegmentPlan{TranscodePlan: TranscodePlan{Format: track.Fmt}}, tc.opts)
			_, perr := e.PlanSegments(track, tc.opts, 0)
			type refusal struct {
				what string
				err  error
			}
			got := []refusal{{"TranscodeSegmentsMedia", merr}, {"InitSegment", ierr}, {"PlanSegments", perr}}
			rp, rerr := e.PlanRemuxSegments(track, tc.opts, 0, knownGrid(1024))
			_, rserr := e.RemuxSegments(ctx, container.BytesSource(raw), "wav", tc.opts,
				SegmentedOptions{SegmentSamples: 4096}, emit)
			switch {
			case !tc.remuxDeclines:
				got = append(got, refusal{"PlanRemuxSegments", rerr}, refusal{"RemuxSegments", rserr})
			case rp != nil || rerr != nil:
				t.Errorf("PlanRemuxSegments = %v, %v, want a decline", rp, rerr)
			}
			for _, got := range got {
				if got.err == nil || waxerr.CodeOf(got.err) != waxerr.CodeOf(want) || got.err.Error() != want.Error() {
					t.Errorf("%s: %v (%s), want %v (%s)", got.what, got.err, waxerr.CodeOf(got.err), want, waxerr.CodeOf(want))
				}
			}
		})
	}

	// The cut rung needs a source whose packets it can copy: an Opus one.
	var opus bytes.Buffer
	if _, err := e.TranscodeMedia(ctx, openWAV(t, e, raw), &opus, TranscodeOptions{Format: "opus"}); err != nil {
		t.Fatal(err)
	}
	med, err := e.OpenStream(container.BytesSource(opus.Bytes()), "opus")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	opusTrack := med.Info().Default()
	grid, err := e.PacketGrid(ctx, container.BytesSource(opus.Bytes()), "opus")
	if err != nil {
		t.Fatal(err)
	}
	g := int64(grid)
	segOpts := SegmentedOptions{SegmentSamples: 2 * grid}
	for _, tc := range []struct {
		name      string
		format    string
		container string
		spans     []Span
	}{
		{"a cut to mp3", "mp3", "", []Span{{From: g, To: 3 * g}}},
		{"a cut to vorbis", "vorbis", "", []Span{{From: g, To: 3 * g}}},
		{"a cut to wav", "wav", "", []Span{{From: g, To: 3 * g}}},
		{"a cut to mp3 over an inverted span", "mp3", "", []Span{{From: 3 * g, To: g}}},
		{"a cut to opus in webm", "opus", "webm", []Span{{From: g, To: 3 * g}}},
	} {
		opts, spans := TranscodeOptions{Format: tc.format, Container: tc.container}, tc.spans
		t.Run(tc.name, func(t *testing.T) {
			_, want := e.TranscodeSegments(ctx, container.BytesSource(opus.Bytes()), "opus", opts, segOpts, emit)
			if want == nil {
				t.Fatal("TranscodeSegments accepted the request")
			}
			_, perr := e.PlanCutSegments(opusTrack, opts, spans, knownGrid(grid), 2)
			_, cerr := e.CutSegments(ctx, container.BytesSource(opus.Bytes()), "opus", opts, spans, grid,
				container.Track{Samples: -1}, segOpts, emit)
			for what, err := range map[string]error{"PlanCutSegments": perr, "CutSegments": cerr} {
				if err == nil || waxerr.CodeOf(err) != waxerr.CodeOf(want) || err.Error() != want.Error() {
					t.Errorf("%s: %v (%s), want %v (%s)", what, err, waxerr.CodeOf(err), want, waxerr.CodeOf(want))
				}
			}
		})
	}
}

// TestMatroskaFormsWriteTheirDocType: the webm form writes the webm DocType
// and the mka form the matroska one, on every row that has them.
func TestMatroskaFormsWriteTheirDocType(t *testing.T) {
	raw := sineWAV(t)
	e := New()
	docTypes := map[string][]byte{
		"mka":  append([]byte{0x42, 0x82, 0x88}, "matroska"...),
		"webm": append([]byte{0x42, 0x82, 0x84}, "webm"...),
	}
	checked := 0
	for i := range outputs {
		row := &outputs[i]
		for _, f := range row.forms {
			want, ok := docTypes[f.name]
			if !ok {
				continue
			}
			var out bytes.Buffer
			opts := TranscodeOptions{Format: row.name, Container: f.name}
			if _, err := e.TranscodeMedia(context.Background(), openWAV(t, e, raw), &out, opts); err != nil {
				t.Errorf("%s/%s: %v", row.name, f.name, err)
				continue
			}
			if !bytes.Contains(out.Bytes()[:min(64, out.Len())], want) {
				t.Errorf("%s/%s: the EBML header does not name DocType %q", row.name, f.name, want[3:])
			}
			checked++
		}
	}
	if checked < 8 {
		t.Errorf("checked %d Matroska forms, want at least 8", checked)
	}
}
