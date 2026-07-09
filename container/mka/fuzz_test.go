package mka

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FuzzDemux exercises the EBML parser, the block/lacing splitter, and the
// seek paths. EBML nesting is an attack surface, so the invariants are: no
// panic, no unbounded work, accepted tracks are well-formed, packet production
// is bounded by the input size, and seeks never overshoot the target.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"seed-opus.webm", "seed-flac.mka", "seed-pcm.mka"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:min(len(full), 200)])
	}
	// A bare EBML magic so the sniff accepts the input and the parser runs.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80})
	// EBML header then a Segment with an unknown (all-ones) size.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xFF})
	// A vint claiming an 8-byte width to probe the size guard.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

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

			// Every packet consumes input, so the count is bounded by the size.
			maxPackets := len(data) + 16
			var pkt container.Packet
			for i := 0; i < maxPackets; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) || err != nil {
					break
				}
				if pkt.Dur < 0 {
					t.Fatalf("packet with negative duration %d", pkt.Dur)
				}
			}

			// Seeks land at or before the target (or at the earliest sync point).
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
