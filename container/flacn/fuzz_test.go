package flacn_test

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production,
// and seeks that never come back past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-s16.flac")
	f.Add(full)
	f.Add(full[:200])
	f.Add(full[:4+4+34])
	f.Add([]byte("fLaC\x80\x00\x00\x22"))
	f.Add(fixture(f, "noise-s16.flac"))

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
			firstPTS := int64(-1)
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
				if firstPTS < 0 {
					firstPTS = pkt.PTS
				}
			}
			for _, target := range []int64{0, 1000, 1 << 40} {
				landed, err := d.SeekSample(0, target)
				if err != nil || firstPTS < 0 {
					// Errors are fine; so is any landing when no packet was
					// ever readable (reads after the seek just hit EOF).
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
