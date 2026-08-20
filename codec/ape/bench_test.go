package ape_test

import (
	"os"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/ape"
)

// The performance floor is 100x realtime per core for APE decode
// (docs/quality-gates.md); `make bench` reports the factor as the
// "x-realtime" metric. The committed noise fixture is the harder case (dense
// residuals, the entropy coder's escape paths); the sine is the
// predictor-friendly one, so real music lands between.
//
// APE is the slowest lossless decoder here by design: a compression level buys
// its size with filter taps, and the deepest one runs 1552 of them per sample
// against the default level's 16. The shared fixtures these run on are at the
// default level, which is what almost every .ape in the world uses, so the
// floor is stated for that; the deep levels are proportionally slower and no
// ratchet claims otherwise.

func benchDecode(b *testing.B, name string) {
	raw, err := os.ReadFile(repoPath("testdata", name))
	if err != nil {
		b.Fatal(err)
	}
	cfg, packets := frames(b, raw)
	dec, err := ape.NewDecoder(cfg, cfg.Format())
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

func BenchmarkDecodeSine(b *testing.B)  { benchDecode(b, "sine-s16.ape") }
func BenchmarkDecodeNoise(b *testing.B) { benchDecode(b, "noise-s16.ape") }
