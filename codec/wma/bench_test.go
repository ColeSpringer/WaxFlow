//go:build !wmatablesgen

package wma_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wma"
)

// benchCells is what the realtime floor is measured over, by what each one
// costs rather than by index: the shortest frame at the lowest rate, where the
// per-block overhead is worst relative to the samples it produces, and a
// full-rate stereo stream, which is what a server actually transcodes.
func benchCells() []cell {
	var out []cell
	for _, c := range corpusCells {
		switch {
		case !c.v2 && c.rate == 8000, c.v2 && c.rate == 44100 && c.channels == 2 && c.kbps == 128:
			out = append(out, c)
		}
	}
	return out
}

// BenchmarkDecode records the realtime floor.
//
// It used to be driven from a zero-value &testing.T{}, which cannot report
// anything: the corpus builder's Skip on a machine without ffmpeg killed the
// benchmark goroutine before the timer ever started, and the run PASSED with
// no ns/op and no x-realtime metric. `make bench` then recorded nothing while
// reporting success. Everything here takes the *testing.B, so a missing oracle
// skips visibly and a broken corpus fails.
func BenchmarkDecode(b *testing.B) {
	for _, c := range benchCells() {
		b.Run(c.name(), func(b *testing.B) {
			track, pkts := demux(b, corpusFile(b, c))
			cfg, err := wma.ParseConfig(track.CodecConfig)
			if err != nil {
				b.Fatal(err)
			}
			dec, err := wma.NewDecoder(cfg, track.Fmt)
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
			// Realtime multiple: decoded seconds per second of CPU.
			secs := float64(frames) / float64(cfg.Rate)
			b.ReportMetric(secs/b.Elapsed().Seconds(), "x-realtime")
		})
	}
}
