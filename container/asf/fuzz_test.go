package asf_test

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production, and
// seeks that never come back past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-wmav2.wma")
	f.Add(full)
	f.Add(full[:1000])
	f.Add(full[:60])
	f.Add(fixture(f, "frag.wma"))
	f.Add(fixture(f, "tagged.wma"))
	f.Add(fixture(f, "mono-8k.wma"))
	f.Add(appendSimpleIndex(fixture(f, "mono-8k.wma"), 8, 1_000_0000))
	f.Add(guidHeader)

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := asf.NewDemuxer(container.BytesSource(data), &asf.DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			if track.SamplesExact {
				t.Fatal("ASF states milliseconds; no track from it is sample-exact")
			}
			// A media object needs at least a length byte and a byte of its
			// own, so production is bounded by the input either way.
			maxPackets := int64(len(data))/2 + 8
			var pkt container.Packet
			firstPTS := int64(-1)
			for i := int64(0); ; i++ {
				if i > maxPackets {
					t.Fatalf("more than %d packets from %d bytes", maxPackets, len(data))
				}
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) || err != nil {
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
				// Landing past the target is only legitimate when the stream
				// itself starts after it.
				if landed > max(target, firstPTS) {
					t.Fatalf("seek to %d landed at %d (stream starts at %d)", target, landed, firstPTS)
				}
			}
		}
	})
}
