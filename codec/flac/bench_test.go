package flac_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The performance floor is 100x realtime per core for FLAC decode
// (docs/quality-gates.md); `make bench` reports the factor as the
// "x-realtime" metric. The committed pink-noise fixture is the harder
// case (dense Rice residuals); the IETF vector, when fetched, adds a
// full-length real-encoder stream.

func benchDecode(b *testing.B, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	demux, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		b.Fatal(err)
	}
	track := demux.Tracks()[0]
	si, err := flac.ParseStreamInfo(track.CodecConfig)
	if err != nil {
		b.Fatal(err)
	}
	var packets [][]byte
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			b.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), pkt.Data...))
	}
	dec, err := flac.NewDecoder(si, track.Fmt)
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
	seconds := float64(samples) / float64(track.Fmt.Rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkDecodeNoise16(b *testing.B) {
	_, file, _, _ := runtime.Caller(0)
	benchDecode(b, filepath.Join(filepath.Dir(file), "..", "..", "testdata", "noise-s16.flac"))
}

func BenchmarkDecodeVector01(b *testing.B) {
	path := filepath.Join(testutil.VectorsDir(), "flac", "subset", "01 - blocksize 4096.flac")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		b.Skip("vector not fetched (run `make verify-vectors`)")
	}
	benchDecode(b, path)
}
