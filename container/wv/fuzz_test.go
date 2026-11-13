package wv_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production, and
// seeks that never come back past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "seek-mono.wv")
	f.Add(full)
	f.Add(full[:200])
	f.Add(full[:32])
	f.Add(fixture(f, "sine-s16.wv"))
	f.Add(fixture(f, "tagged.wv"))
	f.Add([]byte("wvpk\x18\x00\x00\x00\x10\x04\x00\x00"))

	f.Fuzz(func(t *testing.T, data []byte) {
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return wv.NewDemuxer(src, &wv.DemuxerOptions{Strict: strict})
			},
			// A block is at least its 32-byte header, so packet production is
			// bounded by the input.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/32 + 4 },
			Targets:    []int64{0, 1000, 1 << 40},
			// Landing past the target is only legitimate when the stream
			// itself starts after it.
			LateLanding: true,
		}.Run(t, data)
	})
}
