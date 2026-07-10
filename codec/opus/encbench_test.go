package opus

import (
	"testing"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
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

// TestOpusEncodeRealtimeFactor reports the FULL Opus encoder's realtime
// factor: the tonality analyser, mode decision, and delay plumbing on top of
// the codec cores, over the paths a real stream takes (CELT music at 96k
// stereo, SILK/hybrid speech at 24k mono). Same >=15x portable floor
// (docs/quality-gates.md).
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

		const reps = 3
		start := time.Now()
		encoded := 0
		for r := 0; r < reps; r++ {
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
				encoded += opusFrameSize
			}
		}
		elapsed := time.Since(start)
		audio.Put(b)
		rtf := (float64(encoded) / sr) / elapsed.Seconds()
		t.Logf("opus encode %s: %.1fx realtime (%d frames x %d reps, %s)", tc.name, rtf, frames, reps, elapsed)
		if rtf < 15 {
			t.Errorf("%s: encode realtime factor %.1fx below the 15x floor", tc.name, rtf)
		}
	}
}
