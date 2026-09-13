package aiff

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
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
		return ws.Buf
	}
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}, 2, 64))
	f.Add(seed(pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}, 1, 40))
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true}, 6, 16))
	f.Add([]byte("FORM\xff\xff\xff\xffAIFF"))
	f.Add([]byte("FORM\x00\x00\x00\x2aAIFCCOMM"))

	// The compressed compression types, which the muxer cannot write: ima4
	// carries the packet-counting COMM field and the only unit that is not a
	// frame, and the G.711 pair carry the width field that is not the storage
	// width.
	f.Add(buildAIFCn(compALaw, 1, 8, make([]byte, 64), 64))
	f.Add(buildAIFCn(compULaw, 2, 16, make([]byte, 64), 32))
	f.Add(buildAIFCn(compIMA4, 1, 4, make([]byte, adpcm.QuickTimeBlockBytes*3), 3))
	f.Add(buildAIFCn(compIMA4, 2, 4, make([]byte, adpcm.QuickTimeBlockBytes*2*3), 3))
	f.Add(buildAIFCn(compIn24, 1, 16, make([]byte, 48), 16))

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
			// The walk delivers whole UNITS, and for ima4 a unit is 64
			// samples, so what is bounded is the payload rather than the
			// declared length; COMM counts units, so the two agree here, and
			// the check is written against the payload anyway because that is
			// the property (no unbounded production from bounded input).
			capacity := d.payload.units * d.payload.unitFrames
			if got > capacity {
				t.Fatalf("read %d samples from a payload holding %d", got, capacity)
			}
			if track.Samples > capacity {
				t.Fatalf("track declares %d samples from a payload holding %d", track.Samples, capacity)
			}
			// Seeking anywhere legal must land on a unit boundary at or before
			// the target, or at zero for a decoder whose state crosses units.
			if track.Samples > 0 {
				target := track.Samples / 2
				want := target / d.payload.unitFrames * d.payload.unitFrames
				if d.carryState {
					want = 0
				}
				landed, err := d.SeekSample(0, target)
				if err != nil || landed != want {
					t.Fatalf("SeekSample(%d) = %d, %v; want %d", target, landed, err, want)
				}
			}
		}
	})
}
