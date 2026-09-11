//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
)

// BenchmarkDecode records the realtime floor docs/quality-gates.md carries.
//
// Two cells, chosen by what they cost. The 8 kHz cell at 4 kbit/s is the
// cheapest shape in the envelope and the one a low-rate archive carries; the
// 22.05 kHz cell at 20 kbit/s is the binding one, with the most superframes
// per second of audio and sixteen LSPs, so its postfilter runs the four
// transforms most often. Everything else in the envelope sits between them.
//
// Everything takes the *testing.B rather than a zero-value one, so a broken
// fixture fails visibly instead of leaving a benchmark that measures nothing
// and passes.
func BenchmarkDecode(b *testing.B) {
	for _, name := range []string{"voice-8000-4k", "voice-22050-20k"} {
		b.Run(name, func(b *testing.B) {
			track, pkts := demux(b, corpusPath(b, name))
			cfg, err := wmavoice.ParseConfig(track.CodecConfig)
			if err != nil {
				b.Fatal(err)
			}
			dec, err := wmavoice.NewDecoder(cfg, track.Fmt)
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
