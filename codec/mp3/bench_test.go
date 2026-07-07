package mp3_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The MP3 encode floor is 40x realtime per core (docs/quality-gates.md).
// Noise is the bit-hungry worst case (the quantizer's inner loop searches
// harder), the sine the sparse best case, so real music lands between.
func benchEncode(b *testing.B, src *audio.Buffer, bitrate int) {
	defer audio.Put(src)
	enc, err := mp3.NewEncoder(src.Fmt, &mp3.EncoderOptions{Bitrate: bitrate})
	if err != nil {
		b.Fatal(err)
	}
	fs := enc.FrameSize()
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

func BenchmarkEncodeSine128(b *testing.B) {
	benchEncode(b, testutil.Sine(benchFormat(), 10*4096, 997, 0.8), 128000)
}

func BenchmarkEncodeNoise128(b *testing.B) {
	benchEncode(b, testutil.Noise(benchFormat(), 10*4096, 42), 128000)
}
