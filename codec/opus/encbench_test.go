package opus

import (
	"testing"
	"time"
)

// TestEncodeRealtimeFactor reports the CELT encoder's realtime factor (audio
// seconds encoded per wall-clock second, per core) at the default complexity
// and at complexity 10 (the quality-gate configuration, with theta RDO and
// the second MDCT). The floor is >=15x portable at both
// (docs/quality-gates.md).
func TestEncodeRealtimeFactor(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped under -short")
	}
	if raceEnabled {
		t.Skip("timing test skipped under -race (the instrumented build is many times slower)")
	}
	const (
		C  = 2
		sr = 48000
	)
	src := synthMusic(C, sr)
	total := len(src[0])
	frames := total / opusFrameSize
	pcm := make([][]float32, C)

	for _, complexity := range []int{DefaultComplexity, 10} {
		// Warm up plans.
		enc := newCELTEncoder(C)
		enc.complexity = complexity
		for c := 0; c < C; c++ {
			pcm[c] = src[c][:opusFrameSize]
		}
		enc.celtEncode(pcm, opusFrameSize, opusFrameLM, C, 0, opusCELTBands, 319)

		const reps = 3
		start := time.Now()
		encoded := 0
		for r := 0; r < reps; r++ {
			enc = newCELTEncoder(C)
			enc.complexity = complexity
			for fi := 0; fi < frames; fi++ {
				for c := 0; c < C; c++ {
					pcm[c] = src[c][fi*opusFrameSize : (fi+1)*opusFrameSize]
				}
				enc.celtEncode(pcm, opusFrameSize, opusFrameLM, C, 0, opusCELTBands, 319)
				encoded += opusFrameSize
			}
		}
		elapsed := time.Since(start)
		audioSec := float64(encoded) / sr
		rtf := audioSec / elapsed.Seconds()
		t.Logf("CELT encode complexity %d: %.1fx realtime (%d frames x %d reps, %s)", complexity, rtf, frames, reps, elapsed)
		if rtf < 15 {
			t.Errorf("complexity %d: encode realtime factor %.1fx below the 15x floor", complexity, rtf)
		}
	}
}
