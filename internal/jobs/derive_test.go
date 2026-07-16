package jobs

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
)

// TestDeriveOutputLoudnessClampsWhenLimited pins the downmix half of the
// derive: a downmix's analyze true peak is the raw fold with no limiter, so it
// can sit above the encode's ceiling. When the encode limits (a downmix, or
// positive gain), the projected peak must be capped at the ceiling; when it
// does not, the peak follows the gain untouched even above the ceiling.
func TestDeriveOutputLoudnessClampsWhenLimited(t *testing.T) {
	// Fold overshoot: a +3 dBTP source and a mild attenuating gain that still
	// leaves the projection (+2 dBTP) above the -1 dBTP ceiling.
	src := &waxflow.AnalyzeResult{IntegratedLUFS: -15, TruePeakDB: 3.0}
	const gainDB = -1.0

	if _, tp := deriveOutputLoudness(src, gainDB, true); tp != gain.DefaultCeilingDB {
		t.Errorf("limited peak = %.3f dBTP, want the ceiling %.3f", tp, gain.DefaultCeilingDB)
	}
	if _, tp := deriveOutputLoudness(src, gainDB, false); tp != src.TruePeakDB+gainDB {
		t.Errorf("unlimited peak = %.3f dBTP, want %.3f (gain-shifted, no clamp)", tp, src.TruePeakDB+gainDB)
	}
	// A projection already under the ceiling is never lifted to it, even when
	// limited: the clamp is a min, not an assignment.
	quiet := &waxflow.AnalyzeResult{IntegratedLUFS: -30, TruePeakDB: -20}
	if _, tp := deriveOutputLoudness(quiet, gainDB, true); tp != quiet.TruePeakDB+gainDB {
		t.Errorf("under-ceiling peak = %.3f, want %.3f (min is a no-op below the ceiling)", tp, quiet.TruePeakDB+gainDB)
	}
	// Silence keeps -inf loudness regardless of the clamp.
	sil := &waxflow.AnalyzeResult{IntegratedLUFS: math.Inf(-1), TruePeakDB: math.Inf(-1)}
	if lufs, _ := deriveOutputLoudness(sil, gainDB, true); !math.IsInf(lufs, -1) {
		t.Errorf("silence loudness = %.3f, want -inf", lufs)
	}
}
