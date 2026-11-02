package flacn_test

import (
	"errors"
	"io"
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
		for _, strict := range []bool{false, true} {
			d, err := flacn.NewDemuxer(container.BytesSource(data), &flacn.DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			// A frame is at least 9 bytes (header, one subframe byte, and
			// the CRC-16), so packet production is bounded by the input.
			maxPackets := int64(len(data))/6 + 4
			var pkt container.Packet
			firstPTS, lastPTS := int64(-1), int64(-1)
			for i := int64(0); ; i++ {
				if i > maxPackets {
					t.Fatalf("more than %d packets from %d bytes", maxPackets, len(data))
				}
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
				if pkt.Dur <= 0 || len(pkt.Data) == 0 {
					t.Fatal("empty packet must be EOF or error")
				}
				if pkt.PTS < 0 {
					t.Fatalf("packet %d at %d: positions start at 0", i, pkt.PTS)
				}
				if pkt.PTS <= lastPTS {
					t.Fatalf("packet %d at %d after one at %d: positions went backwards", i, pkt.PTS, lastPTS)
				}
				lastPTS = pkt.PTS
				if firstPTS < 0 {
					firstPTS = pkt.PTS
				}
			}
			for _, target := range []int64{0, 1000, 5000, 1 << 40} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					continue // errors are fine
				}
				if landed < 0 {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
				// A landing in a hole is owed what the hole took.
				if err := d.ReadPacket(&pkt); err == nil && pkt.PTS-pkt.Lost != landed {
					t.Fatalf("seek to %d landed at %d, then read a packet at %d owing %d", target, landed, pkt.PTS, pkt.Lost)
				}
				if firstPTS < 0 {
					// Any landing is fine when no packet was ever readable
					// (reads after the seek just hit EOF).
					continue
				}
				// Landing past the target is only legitimate when the
				// stream itself starts after it.
				if landed > max(target, firstPTS) {
					t.Fatalf("seek to %d landed at %d (stream starts at %d)", target, landed, firstPTS)
				}
			}
		}
	})
}
