package vorbis_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The Vorbis encode floor is 40x realtime per core (docs/quality-gates.md), a
// long-block-plus-psy pipeline. Noise is the bit-hungry worst case (fewer
// partitions mask, so more residue is coded), the sine the sparse best case.
func benchEncode(b *testing.B, src *audio.Buffer) {
	defer audio.Put(src)
	enc, err := vorbis.NewEncoder(src.Fmt, nil)
	if err != nil {
		b.Fatal(err)
	}
	const fs = 1024
	chunk := audio.Get(src.Fmt, fs)
	defer audio.Put(chunk)
	emit := func(codec.Packet) error { return nil }

	var samples int64
	b.ResetTimer()
	for b.Loop() {
		for off := 0; off+fs <= src.N; off += fs {
			audio.CopyFrames(chunk, 0, src, off, fs)
			chunk.N = fs
			if err := enc.Encode(chunk, emit); err != nil {
				b.Fatal(err)
			}
			samples += int64(chunk.N)
		}
	}
	b.StopTimer()
	seconds := float64(samples) / float64(src.Fmt.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func benchFormat() audio.Format {
	return audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
}

func BenchmarkEncodeSine(b *testing.B) {
	benchEncode(b, testutil.Sine(benchFormat(), 10*4096, 997, 0.8))
}

func BenchmarkEncodeNoise(b *testing.B) {
	benchEncode(b, testutil.Noise(benchFormat(), 10*4096, 42))
}
