package mp4

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// FuzzDemux is the mp4 box parser's fuzz target. The box tree is the
// service's widest attack surface, so this carries the heaviest fuzz
// budget: the seed corpus is real files, truncations, and hand-built box
// trees exercising the size, depth, and descriptor paths. Invariants: no
// panic, no unbounded work, accepted tracks are well-formed, and seeks
// never overshoot.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"alac-stereo.m4a", "alac-mono-tail.m4a"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:200])
	}
	// A minimal ftyp so the sniffer accepts the input and the parser runs.
	f.Add([]byte("\x00\x00\x00\x10ftypM4A \x00\x00\x00\x00M4A mp42"))
	// A box claiming a 64-bit largesize.
	f.Add([]byte("\x00\x00\x00\x08ftyp\x00\x00\x00\x01moov\xff\xff\xff\xff\xff\xff\xff\xff"))
	// Deeply nested container boxes to probe the depth cap.
	nested := []byte("\x00\x00\x00\x08ftyp")
	for i := 0; i < 40; i++ {
		nested = append([]byte("\x00\x00\x00\x00moov"), nested...)
	}
	f.Add(nested)

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			tracks := d.Tracks()
			if len(tracks) != 1 {
				t.Fatalf("accepted input with %d tracks", len(tracks))
			}
			if err := tracks[0].Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}

			// Packet production is bounded by the sample count.
			maxPackets := int(d.sel.st.total) + 4
			var pkt container.Packet
			for i := 0; i < maxPackets; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
				if pkt.Dur <= 0 {
					t.Fatalf("packet with non-positive duration %d", pkt.Dur)
				}
			}

			// Seeks must never land past the target.
			for _, target := range []int64{0, 1, 1000, 1 << 20, 1 << 40} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					continue
				}
				if landed > target {
					t.Fatalf("seek to %d overshot to %d", target, landed)
				}
			}
		}
	})
}
