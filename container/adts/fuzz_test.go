package adts

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// FuzzDemux fuzzes the ADTS framing parser. Invariants: no panic, accepted
// tracks are well-formed, packet duration agrees with the stamped codec,
// packet production is bounded, and seeks never overshoot.
//
// Corpus entry cd1b2108ef2b5857 is a fuzzer find from the SBR-detection
// landing: a probe-upgraded stream whose 2048-sample durations tripped
// this harness's own stale 1024-only invariant (a test bug, not a code
// bug); it stays as a seed that exercises the DetectSBR path.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"stereo.aac", "mono.aac"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:20])
	}
	f.Add([]byte("\xff\xf1\x50\x80\x21\x1f\xfc")) // a lone header
	f.Add([]byte("ID3\x04\x00\x00\x00\x00\x00\x0a\xff\xf1\x50\x80\x21\x1f\xfc"))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			if err := d.Tracks()[0].Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			// The core frame is 1024 samples; a stream the first-frame probe
			// upgraded to HE-AAC delivers 2048 at the doubled rate.
			want := int64(1024)
			if d.Tracks()[0].Codec == codec.HEAAC {
				want = 2048
			}
			maxPackets := len(data)/7 + 4
			var pkt container.Packet
			for i := 0; i < maxPackets; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) || err != nil {
					break
				}
				if pkt.Dur != want {
					t.Fatalf("frame duration %d, want %d", pkt.Dur, want)
				}
			}
			for _, target := range []int64{0, 1024, 1 << 20, 1 << 40} {
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
