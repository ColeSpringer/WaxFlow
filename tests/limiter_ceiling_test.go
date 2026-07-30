package waxflow_test

// End-to-end mirror of the harness the WaxTap v3.0 E2E report used to attribute
// its F2 finding to WaxFlow: Engine.Transcode with a positive GainDB, then
// Engine.Analyze, with nothing but waxflow in the path. That is what made the
// attribution stick, and what makes this a regression test rather than a unit
// test of the limiter (dsp/gain has those). The report's table is in the plan.

import (
	"context"
	"math"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/gain"
)

// crestWAV synthesizes the report's source shape: sparse short transients over a
// quiet tonal bed, stereo, at a crest factor around 27 dB.
func crestWAV(t *testing.T, seconds float64) ([]byte, float64) {
	t.Helper()
	const rate, ch = 48000, 2
	f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
	frames := int(seconds * rate)
	inter := make([]float32, frames*ch)
	for i := 0; i < frames; i++ {
		bed := 0.062 * math.Sin(2*math.Pi*220*float64(i)/rate)
		inter[i*ch] = float32(bed)
		inter[i*ch+1] = float32(bed * 0.9)
	}
	for at := rate / 8; at < frames; at += rate / 2 {
		for k := 0; k < 4 && at+k < frames; k++ {
			v := float32(math.Cos(math.Pi * float64(k) / 4))
			inter[(at+k)*ch] += v
			inter[(at+k)*ch+1] += v * 0.9
		}
	}
	var peak, sq float64
	for _, v := range inter {
		a := math.Abs(float64(v))
		if a > peak {
			peak = a
		}
		sq += a * a
	}
	crest := 20 * math.Log10(peak/math.Sqrt(sq/float64(len(inter))))
	return synthWAVFromSamples(t, f, inter), crest
}

// TestTranscodeGainTruePeakCeiling drives the report's sweep and requires the
// analyzed output to stay at or under the ceiling. The tolerance is the
// engine's, not the limiter kernel's: Analyze measures with a different 4x
// detector, a few hundredths of a dB apart. The logged shortfall is the
// deliverable for WaxTap, since holding the peak can only cost loudness.
func TestTranscodeGainTruePeakCeiling(t *testing.T) {
	e := waxflow.New()
	wav, crest := crestWAV(t, 15)
	t.Logf("source crest factor %.1f dB", crest)

	src, err := e.Analyze(context.Background(), container.BytesSource(wav), "wav", waxflow.AnalyzeOptions{})
	if err != nil {
		t.Fatalf("analyze source: %v", err)
	}
	t.Logf("source %.3f LUFS, %+.3f dBTP", src.IntegratedLUFS, src.TruePeakDB)

	const allow = 0.20
	for _, gainDB := range []float64{3.39, 7.39, 11.39, 13.39, 17.39, 22.39} {
		out := &memWS{}
		if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
			waxflow.TranscodeOptions{Format: "wav", GainDB: gainDB}); err != nil {
			t.Fatalf("transcode at %+.2f dB: %v", gainDB, err)
		}
		res, err := e.Analyze(context.Background(), container.BytesSource(out.b), "wav", waxflow.AnalyzeOptions{})
		if err != nil {
			t.Fatalf("analyze at %+.2f dB: %v", gainDB, err)
		}
		// F1's quantity: how far under srcLUFS+GainDB the limiter leaves the
		// output. Unlike raw LUFS it carries across sources.
		shortfall := (src.IntegratedLUFS + gainDB) - res.IntegratedLUFS
		t.Logf("GainDB %+.2f: out %.3f LUFS, %+.3f dBTP, %+.3f dBFS (over ceiling %+.2f dB, loudness shortfall %.3f LU)",
			gainDB, res.IntegratedLUFS, res.TruePeakDB, res.SamplePeakDB,
			res.TruePeakDB-gain.DefaultCeilingDB, shortfall)
		if res.TruePeakDB > gain.DefaultCeilingDB+allow {
			t.Errorf("GainDB %+.2f: output true peak %+.3f dBTP is %+.2f dB over the %.2f dBTP ceiling",
				gainDB, res.TruePeakDB, res.TruePeakDB-gain.DefaultCeilingDB, gain.DefaultCeilingDB)
		}
	}
}
