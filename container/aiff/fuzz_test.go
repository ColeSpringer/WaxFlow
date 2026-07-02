package aiff

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
// panics, errors instead of garbage tracks, and bounded packet reading.
func FuzzDemux(f *testing.F) {
	seed := func(cfg pcm.Config, channels, frames int) []byte {
		fm := cfg.PCMFormat(44100, channels, audio.DefaultLayout(channels))
		cfgBytes, err := cfg.MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		ws := &memWS{}
		m := NewMuxer(ws)
		track := container.Track{Codec: codec.PCM, CodecConfig: cfgBytes, Fmt: fm, Samples: int64(frames)}
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
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}, 2, 64))
	f.Add(seed(pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}, 1, 40))
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true}, 6, 16))
	f.Add([]byte("FORM\xff\xff\xff\xffAIFF"))
	f.Add([]byte("FORM\x00\x00\x00\x2aAIFCCOMM"))

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
			if track.Samples < 0 {
				t.Fatalf("accepted track with negative sample count %d", track.Samples)
			}
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
					break
				}
				if pkt.Dur <= 0 || len(pkt.Data) == 0 {
					t.Fatal("empty packet must be EOF or error")
				}
				got += pkt.Dur
			}
			if got > track.Samples {
				t.Fatalf("read %d samples from a track declaring %d", got, track.Samples)
			}
		}
	})
}
