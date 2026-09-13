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

// minMPEGFrame is the smallest compliant Layer III frame (8 kbit/s at
// 24 kHz), which bounds how many packets a frame-walked payload can yield.
const minMPEGFrame = 24

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
	// MP3, whose payload is not a flat array at all: the frames state their
	// own lengths, so a mutation here edits a frame header rather than a
	// geometry field and the walk has to resync from it.
	f.Add(fixtureBytes(f, "mp3.wav"))
	f.Add(wavHeader(tagMP3, 1, 22050, 576, 0, mp3Extra(1393), nil, 23616))
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
			// Unknown is the one negative length a track may state, and
			// only where nothing in the file counted the samples: a frame
			// run with no metadata frame and no fact chunk.
			units, byUnits := d.payload.(*blockReader)
			if track.Samples < 0 && (byUnits || track.Samples != -1) {
				t.Fatalf("accepted track with sample count %d", track.Samples)
			}
			// Progress guarantee: every packet carries at least one unit,
			// and the smallest unit either shape can deliver is bounded
			// below, so this loop is bounded by the input size.
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
					break // structured error is fine; spinning is not
				}
				if pkt.Dur <= 0 || len(pkt.Data) == 0 {
					t.Fatal("empty packet must be EOF or error")
				}
				got += pkt.Dur
			}
			if byUnits {
				// The walk delivers whole UNITS, which for a block codec is
				// more than the track's length: the file's declared count can
				// stop inside the last block, and format.Media trims there.
				// What is bounded is the payload, which the declared length
				// also cannot exceed.
				capacity := units.units * units.unitFrames
				if got > capacity {
					t.Fatalf("read %d samples from a payload holding %d", got, capacity)
				}
				if track.Samples > capacity {
					t.Fatalf("track declares %d samples from a payload holding %d", track.Samples, capacity)
				}
			} else if track.Samples > 0 {
				// A frame walk has no capacity to compare against without
				// running the walk itself, but the payload's byte length still
				// bounds it: nothing shorter than minMPEGFrame is a frame, so
				// a declared length above that many frames' worth is one the
				// file cannot be describing. This is the shape a fact chunk of
				// four billion samples takes on a four-kilobyte payload.
				ceiling := int64(len(data)) / minMPEGFrame * pkt.Dur
				if pkt.Dur > 0 && track.Samples > ceiling {
					t.Fatalf("track declares %d samples from %d bytes of frames (at most %d)",
						track.Samples, len(data), ceiling)
				}
			}

			// Seeking anywhere legal then reading must also terminate, and
			// land at or before the target: on a unit boundary where units
			// are what the payload holds, and on the frame the reservoir
			// backoff reaches where it holds frames.
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
				if byUnits && landed != target/units.unitFrames*units.unitFrames {
					t.Fatalf("SeekSample(%d) = %d, want the unit boundary at or before it", target, landed)
				}
				if err := d.ReadPacket(&pkt); err == nil && pkt.PTS != landed {
					t.Fatalf("post-seek PTS = %d, want %d", pkt.PTS, landed)
				}
			}
		}
	})
}
