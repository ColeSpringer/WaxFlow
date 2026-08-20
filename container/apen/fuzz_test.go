package apen_test

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production, and
// seeks that never come back past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "seek.ape")
	f.Add(full)
	f.Add(full[:200])
	f.Add(full[:80])
	f.Add(fixture(f, "sine-s16.ape"))
	f.Add(fixture(f, "tagged.ape"))
	f.Add([]byte("MAC \x8e\x0f\x62\x33\x34\x00\x00\x00"))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := apen.NewDemuxer(container.BytesSource(data), &apen.DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			if track.Samples < 0 {
				t.Fatalf("accepted a track declaring %d samples", track.Samples)
			}
			// A frame's extent comes from the seek table, whose entries are
			// four bytes each, so packet production is bounded by the input.
			maxPackets := int64(len(data))/4 + 4
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
				if pkt.Dur <= 0 || len(pkt.Data) < ape.FrameHeaderLen {
					t.Fatal("empty packet must be EOF or error")
				}
				if _, _, _, err := ape.ParseFrameHeader(pkt.Data); err != nil {
					t.Fatalf("emitted a packet the decoder cannot parse: %v", err)
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
				// Landing past the target is only legitimate when the stream
				// itself starts after it.
				if landed > max(target, firstPTS) {
					t.Fatalf("seek to %d landed at %d (stream starts at %d)", target, landed, firstPTS)
				}
			}
		}
	})
}
