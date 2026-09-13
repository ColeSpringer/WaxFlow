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

// minMPEGFrame is the smallest compliant Layer III frame (8 kbit/s at
// 24 kHz), which bounds how many packets a frame-walked payload can yield.
const minMPEGFrame = 24

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
	// MP3, whose payload is not a flat array at all: the frames state their
	// own lengths, so a mutation here edits a frame header rather than a
	// geometry field and the walk has to resync from it.
	f.Add(buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, mp3Payload(), mp3FrameCount))
	f.Add(buildAIFCrate("ms\x00\x55", mp3Channels, 16, mp3Rate, mp3Payload(), mp3FrameCount))

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
			// Unknown is the one negative length a track may state, and only
			// where nothing in the file counted the samples: a frame run with
			// no metadata frame at the head of it.
			units, byUnits := d.payload.(*blockReader)
			if track.Samples < 0 && (byUnits || track.Samples != -1) {
				t.Fatalf("accepted track with sample count %d", track.Samples)
			}
			maxPackets := int64(len(data))/int64(audio.StandardChunk) + 2
			if !byUnits {
				maxPackets = int64(len(data))/minMPEGFrame + 2 // one frame per packet
			}
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
			if byUnits {
				// The walk delivers whole UNITS, and for ima4 a unit is 64
				// samples, so what is bounded is the payload rather than the
				// declared length; COMM counts units, so the two agree here,
				// and the check is written against the payload anyway because
				// that is the property (no unbounded production from bounded
				// input).
				capacity := units.units * units.unitFrames
				if got > capacity {
					t.Fatalf("read %d samples from a payload holding %d", got, capacity)
				}
				if track.Samples > capacity {
					t.Fatalf("track declares %d samples from a payload holding %d", track.Samples, capacity)
				}
			}
			// Seeking anywhere legal must land at or before the target: on a
			// unit boundary where units are what the payload holds, at zero
			// for a decoder whose state crosses units, and on the frame the
			// reservoir backoff reaches for a frame run.
			if track.Samples > 0 {
				target := track.Samples / 2
				landed, err := d.SeekSample(0, target)
				if err != nil {
					// A frame walk builds its index on demand, so a seek is
					// the call that reaches damage further down the payload,
					// and under Strict it fails there rather than at open.
					// That is the only way a seek may fail: a unit walk
					// computes its landing, and a tolerant frame walk records
					// the damage and carries on.
					if byUnits || !strict {
						t.Fatalf("SeekSample(%d): %v", target, err)
					}
					continue
				}
				if landed > target {
					t.Fatalf("SeekSample(%d) landed at %d", target, landed)
				}
				switch {
				case d.carryState && landed != 0:
					t.Fatalf("SeekSample(%d) = %d, want 0 for a carry-state decoder", target, landed)
				case byUnits && !d.carryState && landed != target/units.unitFrames*units.unitFrames:
					t.Fatalf("SeekSample(%d) = %d, want the unit boundary at or before it", target, landed)
				}
			}
		}
	})
}
