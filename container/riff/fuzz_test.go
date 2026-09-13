package riff

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
		return ws.Buf
	}
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 64, 64, nil))
	f.Add(seed(pcm.Config{Encoding: pcm.Float, Bits: 32}, 1, 40, 40, nil))
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24}, 6, 16, 16, nil))
	f.Add(seed(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 300, 300, &MuxerOptions{SizeLimit: 128}))
	// The compressed tags, which the muxer cannot write: their block geometry
	// lives in the fmt chunk's extra bytes and their length in a fact chunk,
	// so a mutation of one of these edits a block size, a samples-per-block
	// count, a predictor table or a declared length, none of which a PCM seed
	// carries at all.
	f.Add(wavHeader(tagALaw, 1, 8000, 1, 8, nil, make([]byte, 64), 64))
	f.Add(wavHeader(tagMuLaw, 2, 8000, 2, 8, nil, make([]byte, 64), -1))
	f.Add(wavHeader(tagIMAADPCM, 1, 8000, 20, 4, imaExtra(33), make([]byte, 80), 100))
	f.Add(wavHeader(tagIMAADPCM, 2, 44100, 24, 4, imaExtra(17), make([]byte, 96), -1))
	f.Add(wavHeader(tagMSADPCM, 1, 8000, 20, 4, msExtra(28, adpcm.DefaultCoefs), make([]byte, 80), 100))
	f.Add(wavHeader(tagMSADPCM, 2, 44100, 32, 4, msExtra(20, adpcm.DefaultCoefs), make([]byte, 128), -1))
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
			// The walk delivers whole UNITS, which for a block codec is more
			// than the track's length: the file's declared count can stop
			// inside the last block, and format.Media trims there. What is
			// bounded is the payload, which the declared length also cannot
			// exceed.
			capacity := d.payload.units * d.payload.unitFrames
			if got > capacity {
				t.Fatalf("read %d samples from a payload holding %d", got, capacity)
			}
			if track.Samples > capacity {
				t.Fatalf("track declares %d samples from a payload holding %d", track.Samples, capacity)
			}

			// Seeking anywhere legal then reading must also terminate, and
			// land on a unit boundary at or before the target.
			if track.Samples > 0 {
				target := track.Samples / 2
				want := target / d.payload.unitFrames * d.payload.unitFrames
				landed, err := d.SeekSample(0, target)
				if err != nil || landed != want {
					t.Fatalf("SeekSample(%d) = %d, %v; want %d", target, landed, err, want)
				}
				if err := d.ReadPacket(&pkt); err == nil && pkt.PTS != want {
					t.Fatalf("post-seek PTS = %d, want %d", pkt.PTS, want)
				}
			}
		}
	})
}
