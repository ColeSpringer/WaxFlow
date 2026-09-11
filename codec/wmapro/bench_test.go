//go:build !wmaprotablesgen

package wmapro_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmapro"
)

// BenchmarkDecode records the realtime floor docs/quality-gates.md carries.
//
// Two cells, chosen by what they cost: the 16-bit stereo cell, which is what
// anyone ships, and the 48 kHz 5.1 cell at 24 bits and 384 kbit/s, which is
// the most coefficients per second any committed cell carries and runs six
// transforms per subframe.
//
// Everything takes the *testing.B rather than a zero-value one, so a broken
// fixture fails visibly instead of leaving a benchmark that measures nothing
// and passes.
func BenchmarkDecode(b *testing.B) {
	for _, c := range committedCells() {
		if c.name != "pro-44100-2ch-16-128k" && c.name != "pro-48000-6ch-24-384k" {
			continue
		}
		b.Run(c.name, func(b *testing.B) {
			track, pkts := demux(b, corpusPath(b, c.name))
			cfg, err := wmapro.ParseConfig(track.CodecConfig)
			if err != nil {
				b.Fatal(err)
			}
			dec, err := wmapro.NewDecoder(cfg, track.Fmt)
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
