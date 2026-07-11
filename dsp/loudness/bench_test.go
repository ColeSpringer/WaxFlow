package loudness

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// BenchmarkMeterProcess measures full-meter throughput (K-weighting,
// gating accumulation, true peak) on one second of 48 kHz stereo per
// iteration; the reported time per op is therefore the inverse realtime
// factor.
func BenchmarkMeterProcess(b *testing.B) {
	const rate, channels = 48000, 2
	chans := make([][]float32, channels)
	for c := range chans {
		chans[c] = make([]float32, rate)
		for i := range chans[c] {
			chans[c][i] = float32(0.25 * math.Sin(2*math.Pi*997*float64(i)/rate))
		}
	}
	m, err := NewMeter(rate, channels, audio.DefaultLayout(channels))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(rate * channels * 4))
	b.ResetTimer()
	for range b.N {
		if err := m.Process(chans); err != nil {
			b.Fatal(err)
		}
	}
}
