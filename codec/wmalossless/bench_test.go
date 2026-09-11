package wmalossless_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
)

// BenchmarkDecode records the realtime floor docs/quality-gates.md carries.
//
// Two cells, chosen by what they cost rather than by index: the 16-bit stereo
// cell, whose CDLMS order is 16 and so does twice the filter work per sample
// of the others, and the 5.1 cell, where six channels each run their own
// cascade and the per-frame overhead is spread thinnest.
//
// Everything takes the *testing.B rather than a zero-value one, so a broken
// fixture fails visibly instead of leaving a benchmark that measures nothing
// and passes.
func BenchmarkDecode(b *testing.B) {
	for _, c := range cells {
		if c.name != "ll-44100-2ch-16" && c.name != "ll-48000-6ch-24" {
			continue
		}
		b.Run(c.name, func(b *testing.B) {
			track, pkts := demux(b, corpusPath(b, c.name))
			cfg, err := wmalossless.ParseConfig(track.CodecConfig)
			if err != nil {
				b.Fatal(err)
			}
			dec, err := wmalossless.NewDecoder(cfg, track.Fmt)
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Release()
			var frames int64
			emit := func(buf *audio.Buffer) error {
				frames += int64(buf.N)
				return nil
			}
			b.ResetTimer()
			for b.Loop() {
				dec.Reset()
				for _, p := range pkts {
					if err := dec.Decode(p, emit); err != nil {
						b.Fatal(err)
					}
				}
				if err := dec.Drain(emit); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if frames == 0 {
				b.Fatal("decoded nothing")
			}
			// Realtime multiple: decoded seconds per second of CPU.
			secs := float64(frames) / float64(cfg.Rate)
			b.ReportMetric(secs/b.Elapsed().Seconds(), "x-realtime")
		})
	}
}
