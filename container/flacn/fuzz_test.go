package flacn_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production,
// positions that never go backwards across a resync, and seeks that land
// where the next packet starts and never past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-s16.flac")
	f.Add(full)
	f.Add(full[:200])
	f.Add(full[:4+4+34])
	f.Add([]byte("fLaC\x80\x00\x00\x22"))
	noise := fixture(f, "noise-s16.flac")
	f.Add(noise)
	// A stream-copy cut's shape: the first frame numbered past 0.
	f.Add(testutil.RenumberFLACFrames(f, full, 1000))
	// Frames whose bytes do not check, mid-stream: the resync.
	f.Add(damageFrames(f, full, frameSpans(f, full), 1))
	f.Add(damageFrames(f, noise, frameSpans(f, noise), 0, 2))

	f.Fuzz(func(t *testing.T, data []byte) {
		var packets, lastPTS int64
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return flacn.NewDemuxer(src, &flacn.DemuxerOptions{Strict: strict})
			},
			// A frame is at least 9 bytes (header, one subframe byte, and
			// the CRC-16), so packet production is bounded by the input.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/6 + 4 },
			Targets:    []int64{0, 1000, 5000, 1 << 40},
			// Landing past the target is only legitimate when the stream
			// itself starts after it.
			LateLanding: true,
			SeekPTS:     true,
			Opened:      func(testing.TB, container.Demuxer, bool) { packets, lastPTS = 0, -1 },
			Packet: func(t testing.TB, pkt *container.Packet) {
				if pkt.PTS < 0 {
					t.Fatalf("packet %d at %d: positions start at 0", packets, pkt.PTS)
				}
				if pkt.PTS <= lastPTS {
					t.Fatalf("packet %d at %d after one at %d: positions went backwards", packets, pkt.PTS, lastPTS)
				}
				lastPTS = pkt.PTS
				packets++
			},
			Landed: func(t testing.TB, _ container.Demuxer, target, landed int64) {
				if landed < 0 {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
			},
		}.Run(t, data)
	})
}
