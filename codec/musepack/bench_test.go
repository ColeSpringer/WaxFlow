package musepack_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/musepack"
)

// The performance floor is 300x realtime per core for Musepack decode
// (docs/quality-gates.md); `make bench` reports the factor as the "x-realtime"
// metric. The committed noise fixture is the harder case (every band coded,
// the widest quantisers); the sine is the sparse one, so real music lands
// between. Both stream versions are benchmarked because the frame readers
// differ; the requantisation and synthesis behind them are shared.

func benchDecode(b *testing.B, name string) {
	cfg, _, packets := demuxAll(b, readFile(b, repoPath("testdata", name)))
	dec, err := musepack.NewDecoder(cfg, cfg.Format())
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Release()

	var samples int64
	emit := func(buf *audio.Buffer) error {
		samples += int64(buf.N)
		return nil
	}
	b.ResetTimer()
	for b.Loop() {
		dec.Reset()
		for _, p := range packets {
			if err := dec.Decode(p, emit); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	seconds := float64(samples) / float64(cfg.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkDecodeSineSV8(b *testing.B)  { benchDecode(b, "sine-s16.mpc") }
func BenchmarkDecodeNoiseSV7(b *testing.B) { benchDecode(b, "noise-s16.mpc") }

// The SV7 sine and SV8 noise shapes come from the codec's own fixtures, so
// each reader is measured on both signal classes.
func benchCodecFixture(b *testing.B, name string) {
	cfg, _, packets := demuxAll(b, readFile(b, codecFixture(name)))
	dec, err := musepack.NewDecoder(cfg, cfg.Format())
	if err != nil {
		b.Fatal(err)
	}
	defer dec.Release()
	var samples int64
	emit := func(buf *audio.Buffer) error {
		samples += int64(buf.N)
		return nil
	}
	b.ResetTimer()
	for b.Loop() {
		dec.Reset()
		for _, p := range packets {
			if err := dec.Decode(p, emit); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(samples)/float64(cfg.Rate)/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkDecodeSineSV7(b *testing.B)  { benchCodecFixture(b, "sv7-48k.mpc") }
func BenchmarkDecodeNoiseSV8(b *testing.B) { benchCodecFixture(b, "sv8-q10.mpc") }
