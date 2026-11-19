package testutil

import (
	"math"
	"testing"
)

// failRecorder is a testing.TB that notes a failure and carries on.
type failRecorder struct {
	testing.TB
	failed bool
}

func (r *failRecorder) Helper()               {}
func (r *failRecorder) Errorf(string, ...any) { r.failed = true }
func (r *failRecorder) Logf(string, ...any)   {}

// TestHoldToSteadyStateFailsOnLevelsItCannotJudge: a stretch with no whole
// window, a NaN sample and a silent reference are failures, not passes.
func TestHoldToSteadyStateFailsOnLevelsItCannotJudge(t *testing.T) {
	src := [][]float32{make([]float32, 960)}
	dec := [][]float32{make([]float32, 960)}
	for i := range dec[0] {
		dec[0][i] = 0.01
	}
	good := ErrorLevelsOf(src, dec, 0, 960)
	nan := [][]float32{append([]float32(nil), dec[0]...)}
	nan[0][5] = float32(math.NaN())
	for name, c := range map[string][2]ErrorLevels{
		"no whole window":    {ErrorLevelsOf(src, dec, 0, 100), good},
		"no steady window":   {good, ErrorLevelsOf(src, dec, 0, 100)},
		"a NaN sample":       {ErrorLevelsOf(src, nan, 0, 960), good},
		"a NaN steady state": {good, ErrorLevelsOf(src, nan, 0, 960)},
		"a silent reference": {good, ErrorLevelsOf(src, src, 0, 960)},
	} {
		r := &failRecorder{TB: t}
		HoldToSteadyState(r, name, c[0], c[1])
		if !r.failed {
			t.Errorf("%s passed", name)
		}
	}
	r := &failRecorder{TB: t}
	if HoldToSteadyState(r, "equal levels", good, good); r.failed {
		t.Error("equal levels failed")
	}
}
