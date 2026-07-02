package riff

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, no unbounded allocations, errors instead of garbage tracks, and
// the strict progress guarantee (packet reading terminates in a bounded
// number of iterations).
func FuzzDemux(f *testing.F) {
	// Seed with real muxer output in the shapes the parser branches on.
	seed := func(cfg pcm.Config, channels, frames int, announce int64, opts *MuxerOptions) []byte {
		fm := cfg.PCMFormat(44100, channels, audio.DefaultLayout(channels))
		cfgBytes, err := cfg.MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		ws := &memWS{}
		m := NewMuxer(ws, opts)
		track := container.Track{Codec: codec.PCM, CodecConfig: cfgBytes, Fmt: fm, Samples: announce}
		if err := m.Begin([]container.Track{track}); err != nil {
			f.Fatal(err)
		}
		wire := make([]byte, cfg.BytesPerFrame(channels)*frames)
		err = m.WritePacket(container.Packet{Packet: codec.Packet{Data: wire, Dur: int64(frames), Sync: true}})
		if err != nil {
			f.Fatal(err)
		}
		if err := m.End(codec.Trailer{Samples: int64(frames)}); err != nil {
			f.Fatal(err)
		}
		return ws.b
	}
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 64, 64, nil))
	f.Add(seed(pcm.Config{Encoding: pcm.Float, Bits: 32}, 1, 40, 40, nil))
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24}, 6, 16, 16, nil))
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 300, 300, &MuxerOptions{SizeLimit: 128}))
	f.Add([]byte("RIFF\xff\xff\xff\xffWAVE"))
	f.Add([]byte("RF64\xff\xff\xff\xffWAVEds64"))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			src := container.BytesSource(data)
			d, err := NewDemuxer(src, &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			track := d.Tracks()[0]
			if err := track.Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			if track.Samples < 0 {
				t.Fatalf("accepted track with negative sample count %d", track.Samples)
			}
			// Progress guarantee: every packet carries at least one frame
			// of at least one byte, so this loop is bounded by the input
			// size, packet by packet.
			maxPackets := int64(len(data))/int64(audio.StandardChunk) + 2
			var pkt container.Packet
			var got int64
			for i := int64(0); ; i++ {
				if i > maxPackets {
					t.Fatalf("demuxer produced more than %d packets from %d bytes", maxPackets, len(data))
				}
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break // structured error is fine; spinning is not
				}
				if pkt.Dur <= 0 || len(pkt.Data) == 0 {
					t.Fatal("empty packet must be EOF or error")
				}
				got += pkt.Dur
			}
			if got > track.Samples {
				t.Fatalf("read %d samples from a track declaring %d", got, track.Samples)
			}

			// Seeking anywhere legal then reading must also terminate.
			if track.Samples > 0 {
				target := track.Samples / 2
				landed, err := d.SeekSample(0, target)
				if err != nil || landed != target {
					t.Fatalf("SeekSample(%d) = %d, %v", target, landed, err)
				}
				if err := d.ReadPacket(&pkt); err == nil && pkt.PTS != target {
					t.Fatalf("post-seek PTS = %d, want %d", pkt.PTS, target)
				}
			}
		}
	})
}
