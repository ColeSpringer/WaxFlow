package ape_test

import (
	"os"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
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

// Encode costs the same filter taps decode does, run the other way, plus the
// mid/side pass and the frame's CRC over the source bytes. There is no search
// over candidates the way WavPack has one, so a level's encode and decode cost
// track each other closely and the shape of this curve is the cascade's.
func benchEncode(b *testing.B, name string, level int) {
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

	// One buffer of the whole fixture, handed to the encoder in frames below.
	f := cfg.Format()
	src := audio.Get(f, len(packets)*cfg.BlocksPerFrame)
	defer audio.Put(src)
	for _, p := range packets {
		if err := dec.Decode(p, func(buf *audio.Buffer) error {
			audio.CopyFrames(src, src.N, buf, 0, buf.N)
			src.N += buf.N
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}

	chunk := audio.Get(f, ape.BlocksPerFrame)
	defer audio.Put(chunk)
	drop := func(codec.Packet) error { return nil }
	b.ResetTimer()
	for b.Loop() {
		enc, err := ape.NewEncoder(f, &ape.EncoderOptions{Level: level})
		if err != nil {
			b.Fatal(err)
		}
		for off := 0; off < src.N; off += ape.BlocksPerFrame {
			chunk.N = min(ape.BlocksPerFrame, src.N-off)
			audio.CopyFrames(chunk, 0, src, off, chunk.N)
			if err := enc.Encode(chunk, drop); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := enc.Finish(drop); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	seconds := float64(src.N) * float64(b.N) / float64(f.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkEncodeSine(b *testing.B)  { benchEncode(b, "sine-s16.ape", ape.DefaultEncoderLevel) }
func BenchmarkEncodeNoise(b *testing.B) { benchEncode(b, "noise-s16.ape", ape.DefaultEncoderLevel) }
func BenchmarkEncodeNoiseHigh(b *testing.B) {
	benchEncode(b, "noise-s16.ape", ape.LevelHigh)
}
