package mpc_test

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes, in both
// modes: no panics, errors instead of garbage tracks, bounded packet
// production, packets the decoder can parse, and seeks that never come back
// past their target.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc", "tagged.mpc", "gapless-sv7.mpc", "chapters.mpc"} {
		raw := fixture(f, name)
		f.Add(raw)
		f.Add(raw[:len(raw)/2])
		f.Add(raw[:40])
	}
	f.Add([]byte("MP+\x07"))
	f.Add(buildSV8(sv8Packet("SH", shPayload(sv8Header{count: 3000, freq: 0, maxBand: 20, channels: 2, blockPwr: 0}))))
	f.Add(buildSV8(sv8Packet("SH", shPayload(sv8Header{count: 0, freq: 1, maxBand: 31, channels: 1, blockPwr: 14})), sv8Packet("AP", apBlock(30)), sv8Packet("SE", nil)))
	f.Add(buildSV7(sv7Header{frames: 2, maxBand: 20, gapless: true, last: 700}, []sv7Frame{randomFrame(1, 100), randomFrame(2, 50)}, 700, nil))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := mpc.NewDemuxer(container.BytesSource(data), &mpc.DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			if track.Samples < -1 {
				t.Fatalf("accepted a track declaring %d samples", track.Samples)
			}
			cfg, err := musepack.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("accepted a track whose config does not parse: %v", err)
			}
			// A packet is at least a 20-bit length field (SV7) or a
			// three-byte header (SV8) of input, so production is bounded by
			// the input length.
			maxPackets := int64(len(data))/2 + 4
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
				if pkt.Dur <= 0 {
					t.Fatal("empty packet must be EOF or error")
				}
				if _, _, _, err := musepack.ParsePacketHeader(pkt.Data, cfg); err != nil {
					t.Fatalf("emitted a packet the decoder cannot parse: %v", err)
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
