package mpc_test

import (
	"testing"

	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/internal/testutil"
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
	// A block behind the stream that passes for a tag and is none.
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		f.Add(append(fixture(f, name), overlappingTag()...))
	}
	// What follows the audio: a Lyrics3 block between the tags, a tag behind
	// junk, and a footer that points into the first frame.
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		f.Add(lyricsStack(f, name))
		f.Add(tagBehindJunk(f, name))
		f.Add(footerIntoFirstFrame(f, name))
	}
	f.Add(buildSV8(sv8Packet("SH", shPayload(sv8Header{count: 3000, freq: 0, maxBand: 20, channels: 2, blockPwr: 0}))))
	f.Add(buildSV8(sv8Packet("SH", shPayload(sv8Header{count: 0, freq: 1, maxBand: 31, channels: 1, blockPwr: 14})), sv8Packet("AP", apBlock(30)), sv8Packet("SE", nil)))
	f.Add(buildSV7(sv7Header{frames: 2, maxBand: 20, gapless: true, last: 700}, []sv7Frame{randomFrame(1, 100), randomFrame(2, 50)}, 700, nil))

	// A snapshot needs 64 scanned states, more than a fuzz-sized input
	// holds; at two, every input whose seeks scan gives one to restore.
	f.Cleanup(mpc.SetIdxMinStatesForTest(2))

	f.Fuzz(func(t *testing.T, data []byte) {
		var cfg musepack.Config
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return mpc.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			},
			// A packet is at least a 20-bit length field (SV7) or a
			// three-byte header (SV8) of input, so production is bounded by
			// the input length.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/2 + 4 },
			Targets:    []int64{0, 1000, 1 << 40},
			SeekPTS:    true,
			// Landing past the target is only legitimate when the stream
			// itself starts after it.
			LateLanding: true,
			Restore:     true,
			Opened: func(t testing.TB, d container.Demuxer, _ bool) {
				track := d.Tracks()[0]
				if track.Samples < -1 {
					t.Fatalf("accepted a track declaring %d samples", track.Samples)
				}
				var err error
				if cfg, err = musepack.ParseConfig(track.CodecConfig); err != nil {
					t.Fatalf("accepted a track whose config does not parse: %v", err)
				}
			},
			Packet: func(t testing.TB, pkt *container.Packet) {
				if _, _, _, err := musepack.ParsePacketHeader(pkt.Data, cfg); err != nil {
					t.Fatalf("emitted a packet the decoder cannot parse: %v", err)
				}
			},
		}.Run(t, data)
	})
}
