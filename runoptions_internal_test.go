package waxflow

import (
	"bytes"
	"context"
	"io"
	"math"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestRunOptionsReachEveryEntryPoint holds each of the five run entry points to
// its run options: the output carries the tag, and progress ends on the total
// the plan promised. A forgotten pass-through compiles and drops both silently.
func TestRunOptionsReachEveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	// One second, so a cut has whole packets to keep.
	tone := make([]float32, 48000)
	for i := range tone {
		tone[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	raw := testutil.PCM16WAVBytes(t, 48000, [][]float32{tone})
	// An Opus source, so the remux and cut rungs have packets to move.
	var opusSrc bytes.Buffer
	if _, err := New().Transcode(ctx, container.BytesSource(raw), "wav", &opusSrc, TranscodeOptions{Format: "opus"}); err != nil {
		t.Fatal(err)
	}
	const title = "Every Entry Point"
	trackOf := func(t *testing.T, b []byte, hint string) container.Track {
		t.Helper()
		info, err := format.Probe(ctx, container.BytesSource(b), hint, nil)
		if err != nil {
			t.Fatal(err)
		}
		return info.Default()
	}
	transcodeOpts := TranscodeOptions{Format: "opus"}
	remuxOpts := TranscodeOptions{Format: "opus"}
	cutOpts := TranscodeOptions{Format: "opus"}
	spans := []Span{{From: 4800, To: 19200}}

	for _, tc := range []struct {
		name string
		// plan is the total progress must end on.
		plan func(t *testing.T, e *Engine) int64
		run  func(e *Engine, dst *bytes.Buffer, run []RunOption) (*TranscodeResult, error)
	}{
		{"Transcode", func(t *testing.T, e *Engine) int64 {
			p, err := e.PlanTranscode(trackOf(t, raw, "wav"), transcodeOpts)
			if err != nil {
				t.Fatal(err)
			}
			return p.Samples
		}, func(e *Engine, dst *bytes.Buffer, run []RunOption) (*TranscodeResult, error) {
			return e.Transcode(ctx, container.BytesSource(raw), "wav", dst, transcodeOpts, run...)
		}},
		{"TranscodeMedia", func(t *testing.T, e *Engine) int64 {
			p, err := e.PlanTranscode(trackOf(t, raw, "wav"), transcodeOpts)
			if err != nil {
				t.Fatal(err)
			}
			return p.Samples
		}, func(e *Engine, dst *bytes.Buffer, run []RunOption) (*TranscodeResult, error) {
			med, err := e.OpenStream(container.BytesSource(raw), "wav")
			if err != nil {
				return nil, err
			}
			defer med.Close()
			return e.TranscodeMedia(ctx, med, dst, transcodeOpts, run...)
		}},
		{"Remux", func(t *testing.T, e *Engine) int64 {
			p, err := e.PlanRemux(trackOf(t, opusSrc.Bytes(), "opus"), remuxOpts)
			if err != nil || p == nil {
				t.Fatalf("PlanRemux = %v, %v", p, err)
			}
			return p.Samples
		}, func(e *Engine, dst *bytes.Buffer, run []RunOption) (*TranscodeResult, error) {
			return e.Remux(ctx, container.BytesSource(opusSrc.Bytes()), "opus", dst, remuxOpts, run...)
		}},
		{"RemuxDemuxer", func(t *testing.T, e *Engine) int64 {
			p, err := e.PlanRemux(trackOf(t, opusSrc.Bytes(), "opus"), remuxOpts)
			if err != nil || p == nil {
				t.Fatalf("PlanRemux = %v, %v", p, err)
			}
			return p.Samples
		}, func(e *Engine, dst *bytes.Buffer, run []RunOption) (*TranscodeResult, error) {
			demux, info, err := format.OpenDemuxer(container.BytesSource(opusSrc.Bytes()), "opus", nil)
			if err != nil {
				return nil, err
			}
			if c, ok := demux.(io.Closer); ok {
				defer c.Close()
			}
			return e.RemuxDemuxer(ctx, demux, info.Default(), dst, remuxOpts, run...)
		}},
		{"CutStream", func(t *testing.T, e *Engine) int64 {
			grid, err := e.PacketGrid(ctx, container.BytesSource(opusSrc.Bytes()), "opus")
			if err != nil {
				t.Fatal(err)
			}
			p, err := e.PlanCut(trackOf(t, opusSrc.Bytes(), "opus"), cutOpts, spans, grid)
			if err != nil || p == nil {
				t.Fatalf("PlanCut = %v, %v", p, err)
			}
			return p.Samples
		}, func(e *Engine, dst *bytes.Buffer, run []RunOption) (*TranscodeResult, error) {
			grid, err := e.PacketGrid(ctx, container.BytesSource(opusSrc.Bytes()), "opus")
			if err != nil {
				return nil, err
			}
			return e.CutStream(ctx, container.BytesSource(opusSrc.Bytes()), "opus", dst, cutOpts,
				spans, grid, container.Track{Samples: -1}, run...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New()
			want := tc.plan(t, e)
			var calls, done, total int64
			run := []RunOption{
				WithTags([]container.Tag{{Key: "TITLE", Value: title}}),
				WithProgress(func(d, tot int64) { calls, done, total = calls+1, d, tot }),
			}
			var out bytes.Buffer
			if _, err := tc.run(e, &out, run); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out.Bytes(), []byte(title)) {
				t.Error("the output does not carry the tag")
			}
			if calls == 0 {
				t.Fatal("progress never fired")
			}
			if total != want {
				t.Errorf("progress total = %d, want the planned %d", total, want)
			}
			if done <= 0 {
				t.Errorf("progress ended at %d done", done)
			}
		})
	}
}
