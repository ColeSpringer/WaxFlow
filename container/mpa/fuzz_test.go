package mpa_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpa"
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
		for _, strict := range []bool{false, true} {
			d, err := mpa.NewDemuxer(container.BytesSource(data), &mpa.DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			// The smallest Layer III frame is 24 bytes (8 kbit/s at
			// 24 kHz), so packet production is bounded by the input.
			maxPackets := int64(len(data))/24 + 4
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
				if err != nil {
					continue
				}
				if landed > target {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
			}
		}
	})
}
