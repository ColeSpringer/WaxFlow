package waxflow_test

import (
	"fmt"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/internal/testutil"
)

// decodeOpusSegments decodes the packets of consecutive media segments as
// one stream and drops the pre-skip, so output sample i is source sample i.
func decodeOpusSegments(t *testing.T, segs []mp4.Segment, chans int) [][]float32 {
	t.Helper()
	cfg := opus.Config{Channels: chans, PreSkip: opus.EncoderDelay}
	dec, err := opus.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]float32, chans)
	for _, s := range segs {
		for _, pkt := range parseSegment(t, s.Data).packets {
			err := dec.Decode(pkt, func(b *audio.Buffer) error {
				for c := range chans {
					out[c] = append(out[c], b.ChanF(c)...)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("segment %d: %v", s.Index, err)
			}
		}
	}
	for c := range out {
		out[c] = out[c][opus.EncoderDelay:]
	}
	return out
}

// TestARestartedOpusSegmentRunJoinsAtItsSteadyStateError plays the segments
// of a run restarted mid-stream behind those of the run it replaces, as a
// client meets them after a worker restart, and holds the 400 ms behind the
// join to the error the continuous run has there.
func TestARestartedOpusSegmentRunJoinsAtItsSteadyStateError(t *testing.T) {
	const segSamples = 48000
	const tail = 48000 * 2 / 5
	inputs := []struct {
		name string
		gen  func(frames, chans int) [][]float32
	}{{"tone", testutil.StartupTone}, {"plucked", testutil.StartupPlucked}}
	e := waxflow.New()
	for _, in := range inputs {
		for _, chans := range []int{1, 2} {
			pcm := in.gen(2*testutil.StartupPeriod+48000, chans)
			raw := testutil.FloatWAVBytes(t, 48000, pcm)
			for _, kbps := range []int{24, 32, 48} {
				t.Run(fmt.Sprintf("%s/%dch/%dk", in.name, chans, kbps), func(t *testing.T) {
					opts := waxflow.TranscodeOptions{Format: "opus", Bitrate: kbps * 1000}
					full, _ := collectSegments(t, e, raw, opts, segSamples, 0)
					steady := decodeOpusSegments(t, full, chans)
					for _, at := range []int64{2, 3, 5} {
						fresh, _ := collectSegments(t, e, raw, opts, segSamples, at)
						joined := append(append([]mp4.Segment(nil), full[:at]...), fresh...)
						got := decodeOpusSegments(t, joined, chans)
						from := int(at) * segSamples
						testutil.HoldToSteadyState(t, fmt.Sprintf("400 ms behind a restart at segment %d", at),
							testutil.ErrorLevelsOf(pcm, got, from, from+tail),
							testutil.ErrorLevelsOf(pcm, steady, from, from+tail))
					}
				})
			}
		}
	}
}

// TestAnOpusRestartIntoAQuietPassageStartsTheSoundAtItsSteadyStateError
// restarts a segment run inside two seconds of dither between two stretches
// of the tone, and holds the 500 ms where the tone comes back to the error
// of the same samples before the passage, in the restarted run and in the
// continuous one.
func TestAnOpusRestartIntoAQuietPassageStartsTheSoundAtItsSteadyStateError(t *testing.T) {
	const segSamples, head = 48000, 24000
	const quietFrom, quietTo = 3 * 48000, 5 * 48000
	e := waxflow.New()
	for _, chans := range []int{1, 2} {
		pcm := testutil.StartupTone(8*48000, chans)
		seed := uint32(7)
		for c := range pcm {
			copy(pcm[c][quietTo:], pcm[c]) // the tone again, from its top
			for i := quietFrom; i < quietTo; i++ {
				seed = seed*1664525 + 1013904223
				pcm[c][i] = float32(float64(seed>>8)/(1<<23)-1) / 32768
			}
		}
		raw := testutil.FloatWAVBytes(t, 48000, pcm)
		for _, kbps := range []int{24, 32, 48} {
			t.Run(fmt.Sprintf("%dch/%dk", chans, kbps), func(t *testing.T) {
				opts := waxflow.TranscodeOptions{Format: "opus", Bitrate: kbps * 1000}
				full, _ := collectSegments(t, e, raw, opts, segSamples, 0)
				whole := decodeOpusSegments(t, full, chans)
				steady := testutil.ErrorLevelsOf(pcm, whole, 2*48000, 2*48000+head)
				testutil.HoldToSteadyState(t, "500 ms behind the passage, continuous",
					testutil.ErrorLevelsOf(pcm, whole, quietTo, quietTo+head), steady)
				fresh, _ := collectSegments(t, e, raw, opts, segSamples, 4)
				joined := decodeOpusSegments(t, append(append([]mp4.Segment(nil), full[:4]...), fresh...), chans)
				testutil.HoldToSteadyState(t, "500 ms behind the passage, restarted inside it",
					testutil.ErrorLevelsOf(pcm, joined, quietTo, quietTo+head), steady)
			})
		}
	}
}
