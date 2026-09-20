package waxflow_test

// The interior-splice policy, end to end. A cut of more than one span has to
// keep audio from the spans the caller asked for and none from the ones it
// asked to remove, and the two policies differ only in what happens within a
// packet of each join. See ADR-0011.

import (
	"context"
	"math"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

// spliceTones are the three tones the fixture below puts in its three
// thirds. They are far apart and none is a harmonic of another, so energy at
// one of them in the output names the third it came from.
var spliceTones = [3]float64{440, 1500, 3700}

// tonedFixture is a 3-second source whose thirds each carry one of
// spliceTones, encoded to format at 48 kHz stereo.
func tonedFixture(t *testing.T, format string, seconds int) ([]byte, int) {
	t.Helper()
	const rate = 48000
	frames := rate * seconds
	raw, _ := makeWAVOf(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, func(b *audio.Buffer) {
		seg := frames / len(spliceTones)
		for i := range frames {
			f := spliceTones[min(i/seg, len(spliceTones)-1)]
			v := float32(0.4 * math.Sin(2*math.Pi*f*float64(i)/rate))
			for ch := range 2 {
				b.ChanI(ch)[i] = int32(v * 32000)
			}
		}
	})
	e := waxflow.New()
	ws := &memWS{}
	if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "wav", ws,
		waxflow.TranscodeOptions{Format: format}); err != nil {
		t.Fatalf("building the %s fixture: %v", format, err)
	}
	return ws.Buf, frames
}

// toneLevel is the RMS of pcm at freq, by a single-bin correlation. Channel
// 0 of an interleaved stereo buffer.
func toneLevel(pcm []float32, rate int, freq float64) float64 {
	var re, im float64
	n := len(pcm) / 2
	for i := range n {
		th := 2 * math.Pi * freq * float64(i) / float64(rate)
		re += float64(pcm[i*2]) * math.Cos(th)
		im += float64(pcm[i*2]) * math.Sin(th)
	}
	if n == 0 {
		return 0
	}
	return 2 * math.Hypot(re, im) / float64(n)
}

// TestCutInteriorSpliceKeepsOnlyWhatWasAskedFor is the default policy's
// promise, measured: the removed third is not in the output at all, and both
// kept thirds are.
func TestCutInteriorSpliceKeepsOnlyWhatWasAskedFor(t *testing.T) {
	for _, tc := range []struct {
		format, container, hint string
		// ADTS has no gapless signalling of any kind, so a cut whose head
		// trim is nonzero -- which is every cut of a source with a decoder
		// delay -- has nowhere to put it and falls through to a re-encode.
		// The span count is not what declines it; cutTrimsExpressible is.
		declines bool
	}{
		{format: "opus", hint: "opus"},
		{format: "opus", container: "mka", hint: "mka"},
		{format: "opus", container: "webm", hint: "webm"},
		{format: "aac", hint: "m4a"},
		{format: "aac", container: "adts", hint: "aac", declines: true},
		{format: "aac", container: "mka", hint: "mka"},
	} {
		name := tc.format + "-" + tc.hint
		t.Run(name, func(t *testing.T) {
			src, frames := tonedFixture(t, tc.format, 3)
			seg := int64(frames / 3)
			spans := []waxflow.Span{{From: 0, To: seg}, {From: 2 * seg, To: waxflow.ToEnd}}
			opts := waxflow.TranscodeOptions{Format: tc.format, Container: tc.container}
			out, plan := runCut(t, src, tc.format, opts, spans)
			if tc.declines {
				if out != nil {
					t.Error("this destination cannot signal the cut's head trim, so it must decline")
				}
				return
			}
			if out == nil {
				t.Fatal("PlanCut declined a two-span cut of an allowlisted codec")
			}
			for i, s := range plan.Landed {
				want := spans[i]
				if want.To == waxflow.ToEnd {
					want.To = math.MaxInt64
				}
				if s.From < want.From || s.To > want.To {
					t.Errorf("Landed[%d] = %v reaches outside the requested %v", i, s, spans[i])
				}
			}
			got := decodeWhole(t, out, tc.hint)
			lo, mid, hi := toneLevel(got, 48000, spliceTones[0]),
				toneLevel(got, 48000, spliceTones[1]), toneLevel(got, 48000, spliceTones[2])
			t.Logf("levels: kept %.4f / removed %.3e / kept %.4f over %d frames (plan %d)",
				lo, mid, hi, len(got)/2, plan.Samples)
			if lo < 0.05 || hi < 0.05 {
				t.Errorf("a kept third is missing: levels %.4f / %.4f", lo, hi)
			}
			// The bound has to sit under what the regression produces, not
			// merely under nothing. A pre-roll of the removed third landing
			// at the join contributes its own amplitude times its share of
			// the run: 1024 AAC samples in ~190000 is 0.4*1024/190000 =
			// 0.0022, and Opus's 3840 is 0.0081. lo/200 is 9.8e-4, under
			// both and a good ten times over what a clean splice measures.
			if mid > lo/200 {
				t.Errorf("the removed third is audible at %.3e against a kept %.4f", mid, lo)
			}
			if n := probeTrack(t, out, tc.hint).Samples; n != plan.Samples {
				t.Errorf("the output declares %d samples, the plan promised %d", n, plan.Samples)
			}
		})
	}
}

// TestCutSpliceTrimsAreExactOnMatroska is the opt-in: the interior tail lands
// exactly where it was asked for, the pre-roll it walks is discarded rather
// than heard, and every other destination declines.
func TestCutSpliceTrimsAreExactOnMatroska(t *testing.T) {
	src, frames := tonedFixture(t, "opus", 3)
	seg := int64(frames / 3)
	spans := []waxflow.Span{{From: 0, To: seg}, {From: 2 * seg, To: waxflow.ToEnd}}

	for _, cont := range []string{"mka", "webm"} {
		t.Run(cont, func(t *testing.T) {
			opts := waxflow.TranscodeOptions{Format: "opus", Container: cont, SpliceTrims: true}
			out, plan := runCut(t, src, "opus", opts, spans)
			if out == nil {
				t.Fatal("PlanCut declined SpliceTrims on Matroska, which is the one destination that carries it")
			}
			if plan.Landed[0].To != seg {
				t.Errorf("Landed[0].To = %d, want the requested %d exactly", plan.Landed[0].To, seg)
			}
			if plan.Track.MidPadding <= 0 {
				t.Error("MidPadding = 0: the pre-roll and the tail slop were not recorded as trims")
			}
			got := decodeWhole(t, out, cont)
			lo, mid, hi := toneLevel(got, 48000, spliceTones[0]),
				toneLevel(got, 48000, spliceTones[1]), toneLevel(got, 48000, spliceTones[2])
			t.Logf("levels: kept %.4f / removed %.3e / kept %.4f over %d frames (plan %d)",
				lo, mid, hi, len(got)/2, plan.Samples)
			if lo < 0.05 || hi < 0.05 {
				t.Errorf("a kept third is missing: levels %.4f / %.4f", lo, hi)
			}
			// Tighter than the default policy's bound for the same reason
			// it is a bound at all: here the pre-roll really is delivered to
			// the decoder, and the trims are the only thing keeping it out
			// of the output.
			if mid > lo/200 {
				t.Errorf("the discarded pre-roll is audible at %.3e against a kept %.4f", mid, lo)
			}
			if n := int64(len(got) / 2); n != plan.Samples {
				t.Errorf("the output decodes to %d frames, the plan promised %d", n, plan.Samples)
			}
		})
	}

	// Everything else falls through to a re-encode, because the cut's track
	// now trims inside its run.
	aac, aacFrames := tonedFixture(t, "aac", 3)
	aacSeg := int64(aacFrames / 3)
	aacSpans := []waxflow.Span{{From: 0, To: aacSeg}, {From: 2 * aacSeg, To: waxflow.ToEnd}}
	for _, tc := range []struct{ format, container, hint string }{
		{"opus", "", "opus"},   // Ogg-Opus: one pre-skip, one end granule
		{"aac", "", "m4a"},     // fragmented MP4: one edit list
		{"aac", "adts", "aac"}, // no gapless signalling at all
	} {
		t.Run(tc.format+"-"+tc.hint, func(t *testing.T) {
			e := waxflow.New()
			raw, sp := src, spans
			if tc.format == "aac" {
				raw, sp = aac, aacSpans
			}
			info, err := e.Probe(container.BytesSource(raw), tc.format, nil)
			if err != nil {
				t.Fatal(err)
			}
			grid, err := e.PacketGrid(container.BytesSource(raw), tc.format)
			if err != nil {
				t.Fatal(err)
			}
			opts := waxflow.TranscodeOptions{Format: tc.format, Container: tc.container, SpliceTrims: true}
			plan, err := e.PlanCut(info.Default(), opts, sp, grid)
			if err != nil {
				t.Fatal(err)
			}
			if plan != nil {
				t.Error("this destination planned a spliced cut; only Matroska states a trim per packet")
			}
		})
	}
}
