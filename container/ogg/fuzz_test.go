package ogg

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production,
// and sane seek landings.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-s16.oga")
	f.Add(full)
	f.Add(full[:100])
	f.Add([]byte("OggS\x00\x02" + "\x00\x00\x00\x00\x00\x00\x00\x00" + "\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x33"))
	f.Add(fixture(f, "noise-s24.oga"))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			// Every packet consumes at least one lacing byte, so packet
			// production is bounded by the input length.
			maxPackets := int64(len(data)) + 8
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
					continue
				}
				if landed > max(target, firstPTS) {
					t.Fatalf("seek to %d landed at %d (stream starts at %d)", target, landed, firstPTS)
				}
			}
		}
	})
}
