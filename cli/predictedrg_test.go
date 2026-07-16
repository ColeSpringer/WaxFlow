package cli

import (
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestPredictedRGClampsWhenLimited pins the OGG/MP4 predicted-peak clamp: the
// analyze fold reports a raw true peak that can exceed the encode's ceiling, so
// when the chain limits (a downmix, or positive gain) the predicted RG peak
// must be capped at the ceiling; without a limiter it follows the gain.
func TestPredictedRGClampsWhenLimited(t *testing.T) {
	src := &waxflow.AnalyzeResult{IntegratedLUFS: -15, TruePeakDB: 3.0}
	const gainDB = -1.0
	outLUFS := src.IntegratedLUFS + gainDB

	limited, _ := predictedRG(src, gainDB, true)
	wantLimited := meta.ReplayGainTags(outLUFS, gain.DefaultCeilingDB)
	if limited[1].Value != wantLimited[1].Value {
		t.Errorf("limited peak tag = %s, want ceiling-clamped %s", limited[1].Value, wantLimited[1].Value)
	}

	unlimited, _ := predictedRG(src, gainDB, false)
	wantUnlimited := meta.ReplayGainTags(outLUFS, src.TruePeakDB+gainDB)
	if unlimited[1].Value != wantUnlimited[1].Value {
		t.Errorf("unlimited peak tag = %s, want gain-shifted %s", unlimited[1].Value, wantUnlimited[1].Value)
	}

	// The two must actually differ, or the clamp is untested.
	if limited[1].Value == unlimited[1].Value {
		t.Errorf("clamp had no effect: limited and unlimited peak tags both %s", limited[1].Value)
	}
}
