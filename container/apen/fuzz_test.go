package apen_test

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/internal/testutil"
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
				if _, _, _, _, err := ape.ParseFrameHeader(pkt.Data); err != nil {
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

// FuzzMuxDemuxedFrames drives the path the transmux rung takes: whatever the
// demuxer makes of arbitrary bytes goes straight back into the muxer. The
// muxer's word packer reads a frame's bytes at offsets up to three past their
// own index, so a packet whose last word is not all there is exactly the shape
// that reaches off the end, and a hostile file is where one comes from.
//
// The invariant is that a mux either refuses or produces a file whose own
// header describes what it was handed.
//
// The header is what is checked rather than a re-demux, and the fuzzer is why:
// it found inputs whose *audio* spells an APEv2 footer at exactly the offset
// our output's tail lands on, and the trailer peel then eats into the frames
// or the header itself. That ambiguity belongs to the format -- an APEv2 tag
// is identified by a footer at the end of the file, so a file whose last
// thirty-two bytes spell one is two files -- and every reader has it, the
// reference included. Reaching it takes eight specific bytes landing in the
// last frame's tail, which a crafted input can arrange and audio cannot.
func FuzzMuxDemuxedFrames(f *testing.F) {
	f.Add(fixture(f, "seek.ape"))
	f.Add(fixture(f, "sine-s16.ape"))
	f.Add(fixture(f, "tagged.ape"))

	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := apen.NewDemuxer(container.BytesSource(data), nil)
		if err != nil {
			return
		}
		track := d.Tracks()[0]
		var pkts []container.Packet
		var samples int64
		var pkt container.Packet
		for {
			if err := d.ReadPacket(&pkt); err != nil {
				break
			}
			pkts = append(pkts, container.Packet{Packet: codec.Packet{
				Data: append([]byte(nil), pkt.Data...), PTS: pkt.PTS, Dur: pkt.Dur, Sync: pkt.Sync,
			}})
			samples += pkt.Dur
		}
		if len(pkts) == 0 {
			return
		}
		dst := &testutil.MemWriteSeeker{}
		m := apen.NewMuxer(dst, nil)
		if err := m.Begin([]container.Track{track}); err != nil {
			return
		}
		for _, p := range pkts {
			if err := m.WritePacket(p); err != nil {
				return
			}
		}
		if err := m.End(codec.Trailer{Samples: samples}); err != nil {
			return
		}
		h, err := ape.ParseHeader(dst.Buf)
		if err != nil {
			t.Fatalf("a file this muxer wrote has an unparsable header: %v", err)
		}
		if h.TotalFrames != len(pkts) {
			t.Fatalf("wrote %d frames, the header says %d", len(pkts), h.TotalFrames)
		}
		if h.Samples() != samples {
			t.Fatalf("wrote %d samples, the header says %d", samples, h.Samples())
		}
		if int64(len(dst.Buf)) != h.FrameDataOffset+h.FrameDataBytes {
			t.Fatalf("the file is %d bytes for a %d-byte frame region at %d",
				len(dst.Buf), h.FrameDataBytes, h.FrameDataOffset)
		}
	})
}
