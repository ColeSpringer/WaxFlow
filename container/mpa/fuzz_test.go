package mpa_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/internal/testutil"
)

func fixture(f *testing.F, name string) []byte {
	f.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		f.Fatal(err)
	}
	return raw
}

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production,
// and seeks that never land past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-untagged.mp3")
	f.Add(full)
	f.Add(full[:500])
	f.Add(fixture(f, "sine-vbr.mp3"))
	f.Add(fixture(f, "sine-8000-cbr16.mp3"))
	f.Add([]byte("ID3\x04\x00\x00\x00\x00\x00\x0a0123456789\xff\xfb\x90\x64"))

	f.Fuzz(func(t *testing.T, data []byte) {
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return mpa.NewDemuxer(src, &mpa.DemuxerOptions{Strict: strict})
			},
			// The smallest Layer III frame is 24 bytes (8 kbit/s at
			// 24 kHz), so packet production is bounded by the input.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/24 + 4 },
			Targets:    []int64{0, 1000, 1 << 40},
		}.Run(t, data)
	})
}
