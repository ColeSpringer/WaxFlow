package opus

import (
	"os"
	"testing"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// perfEnforced reports whether the realtime floors below are hard assertions.
// They are measured against the CI baseline machine class (docs/quality-gates.md),
// so a shared runner measures its own scheduling noise as much as the encoder.
// The default suite reports the numbers and the nightly bench job triages them
// against the floors; WAXFLOW_PERF=1 turns them into gates for a dedicated perf
// run, the same switch server's TTFA percentiles use.
func perfEnforced() bool { return os.Getenv("WAXFLOW_PERF") == "1" }

// encodeWarmup is how long each case encodes untimed before it is measured.
// The floors are sustained-throughput numbers, but a cold pass also pays to
// fault in the encoder's scratch and to bring the core up from its idle clock.
// Warming one frame did not cover that, and the cost landed entirely on
// whichever case ran first: complexity 5 measured slower than complexity 10,
// and music slower than speech, despite doing strictly less work each time.
const encodeWarmup = 100 * time.Millisecond

// warmup runs one encode pass at a time until encodeWarmup has elapsed, so the
// pass is never cut mid-stream and the encoder is measured in steady state.
func warmup(pass func()) {
	for start := time.Now(); time.Since(start) < encodeWarmup; {
		pass()
	}
}

// bestOf times pass reps times and returns its fastest run. Interference only
// ever makes a run read slow, so the minimum is the least contaminated
// estimate of what the encoder sustains, where a mean over the reps measures
// the runner's other tenants as much as the code under test.
func bestOf(reps int, pass func()) time.Duration {
	var best time.Duration
	for r := 0; r < reps; r++ {
		start := time.Now()
		pass()
		if d := time.Since(start); best == 0 || d < best {
			best = d
		}
	}
	return best
}

// TestEncodeRealtimeFactor reports the CELT encoder's realtime factor (audio
// seconds encoded per wall-clock second, per core) at the default complexity
// and at complexity 10 (the quality-gate configuration, with theta RDO and
// the second MDCT). The floor is >=30x portable at both
// (docs/quality-gates.md), enforced under WAXFLOW_PERF=1.
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
		pass := func() {
			enc := newCELTEncoder(C)
			enc.complexity = complexity
			for fi := 0; fi < frames; fi++ {
				for c := 0; c < C; c++ {
					pcm[c] = src[c][fi*opusFrameSize : (fi+1)*opusFrameSize]
				}
				enc.celtEncode(pcm, opusFrameSize, opusFrameLM, C, 0, opusCELTBands, 319)
			}
		}
		warmup(pass)

		const reps = 3
		elapsed := bestOf(reps, pass)
		audioSec := float64(frames*opusFrameSize) / sr
		rtf := audioSec / elapsed.Seconds()
		t.Logf("CELT encode complexity %d: %.1fx realtime (best of %d x %d frames, %s)", complexity, rtf, reps, frames, elapsed)
		if rtf < 30 && perfEnforced() {
			t.Errorf("complexity %d: encode realtime factor %.1fx below the 30x floor", complexity, rtf)
		}
	}
}

// TestOpusEncodeRealtimeFactor reports the FULL Opus encoder's realtime
// factor: the tonality analyser, mode decision, and delay plumbing on top of
// the codec cores, over the paths a real stream takes (CELT music at 96k
// stereo, SILK/hybrid speech at 24k mono). Same >=30x portable floor
// (docs/quality-gates.md), enforced under WAXFLOW_PERF=1.
func TestOpusEncodeRealtimeFactor(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped under -short")
	}
	if raceEnabled {
		t.Skip("timing test skipped under -race (the instrumented build is many times slower)")
	}
	const sr = 48000
	music := synthMusic(2, sr)
	speech := synthSpeechFloat(4 * sr)

	for _, tc := range []struct {
		name    string
		pcm     [][]float32
		bitrate int
		signal  Signal
	}{
		{"music-96k-stereo", music, 96000, SignalAuto},
		{"speech-24k-mono", [][]float32{speech}, 24000, SignalVoice},
	} {
		C := len(tc.pcm)
		f := audio.Format{Rate: sr, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}
		frames := len(tc.pcm[0]) / opusFrameSize
		emit := func(p codec.Packet) error { return nil }
		b := audio.Get(f, opusFrameSize)

		pass := func() {
			enc, err := NewEncoder(f, &EncoderOptions{Bitrate: tc.bitrate, Signal: tc.signal})
			if err != nil {
				t.Fatal(err)
			}
			for fi := 0; fi < frames; fi++ {
				for c := 0; c < C; c++ {
					copy(b.ChanF(c), tc.pcm[c][fi*opusFrameSize:(fi+1)*opusFrameSize])
				}
				b.N = opusFrameSize
				if err := enc.Encode(b, emit); err != nil {
					t.Fatal(err)
				}
			}
		}
		warmup(pass)

		const reps = 3
		elapsed := bestOf(reps, pass)
		audio.Put(b)
		rtf := (float64(frames*opusFrameSize) / sr) / elapsed.Seconds()
		t.Logf("opus encode %s: %.1fx realtime (best of %d x %d frames, %s)", tc.name, rtf, reps, frames, elapsed)
		if rtf < 30 && perfEnforced() {
			t.Errorf("%s: encode realtime factor %.1fx below the 30x floor", tc.name, rtf)
		}
	}
}
