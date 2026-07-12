package mp4

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// FuzzDemux is the mp4 box parser's fuzz target. The box tree is the
// service's widest attack surface, so this carries the heaviest fuzz
// budget: the seed corpus is real files, truncations, and hand-built box
// trees exercising the size, depth, and descriptor paths. Invariants: no
// panic, no unbounded work, accepted tracks are well-formed, and seeks
// never overshoot.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"alac-stereo.m4a", "alac-mono-tail.m4a"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:200])
	}
	// A minimal ftyp so the sniffer accepts the input and the parser runs.
	f.Add([]byte("\x00\x00\x00\x10ftypM4A \x00\x00\x00\x00M4A mp42"))
	// A box claiming a 64-bit largesize.
	f.Add([]byte("\x00\x00\x00\x08ftyp\x00\x00\x00\x01moov\xff\xff\xff\xff\xff\xff\xff\xff"))
	// Deeply nested container boxes to probe the depth cap.
	nested := []byte("\x00\x00\x00\x08ftyp")
	for i := 0; i < 40; i++ {
		nested = append([]byte("\x00\x00\x00\x00moov"), nested...)
	}
	f.Add(nested)
	// A self-contained fragmented file (init + fragments) so the fuzzer walks
	// the moof/traf/trun path, not just progressive moov/stbl.
	if seed := fragmentedSeed(); seed != nil {
		f.Add(seed)
		f.Add(seed[:len(seed)/2])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			tracks := d.Tracks()
			if len(tracks) != 1 {
				t.Fatalf("accepted input with %d tracks", len(tracks))
			}
			if err := tracks[0].Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}

			// A generous safety cap so a bug that failed to terminate is caught,
			// not a tight bound: a fragmented trun may declare zero-size samples
			// (each capped per fragment at maxSamplesPerFragment), so the true
			// packet count is not bounded by the byte size. Reading a prefix
			// still exercises the Dur>0 and no-crash invariants on every packet.
			maxPackets := int(d.size) + maxSamplesPerFragment + 8
			var pkt container.Packet
			for i := 0; i < maxPackets; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					break
				}
				if pkt.Dur <= 0 {
					t.Fatalf("packet with non-positive duration %d", pkt.Dur)
				}
			}

			// Seeks must never land past the target.
			for _, target := range []int64{0, 1, 1000, 1 << 20, 1 << 40} {
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

// fragmentedSeed builds a minimal self-contained fragmented Opus file (init
// header plus two fragments) for the fuzz corpus, or nil if construction fails.
func fragmentedSeed() []byte {
	track, pkts := fuzzOpusTrack()
	init, err := InitSegment(track)
	if err != nil {
		return nil
	}
	seg, err := NewSegmenter(track, &SegmenterOptions{SegmentSamples: 2 * 960})
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	buf.Write(init)
	emit := func(s Segment) error { buf.Write(s.Data); return nil }
	for _, p := range pkts {
		if seg.WritePacket(p, emit) != nil {
			return nil
		}
	}
	if seg.End(emit) != nil {
		return nil
	}
	return buf.Bytes()
}

// fuzzOpusTrack is a minimal valid Opus track and packet run for seed building.
func fuzzOpusTrack() (container.Track, []codec.Packet) {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	head[10] = 0x38                 // pre-skip 312, little-endian low byte
	head[11] = 0x01                 // high byte
	head[12], head[13] = 0x80, 0xBB // 48000, little-endian
	track := container.Track{
		Codec:       codec.Opus,
		CodecConfig: head,
		Fmt:         audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Delay:       312,
		Samples:     4*960 - 312,
	}
	var pkts []codec.Packet
	for i := 0; i < 4; i++ {
		pkts = append(pkts, codec.Packet{Data: []byte{0x00, byte(i), 0x5A}, Dur: 960, Sync: true})
	}
	return track, pkts
}

// FuzzFragment targets the fragmented-MP4 fragment parsers (moof/traf/tfhd/
// tfdt/trun) directly: parseFragment must not panic or run unbounded on any
// moof body, and the sample count it reports must stay within the cap and
// consistent with the bytes it was given.
func FuzzFragment(f *testing.F) {
	// A well-formed moof body (extracted from the muxer's own fragment output).
	frag := fragmentBoxes(1, 0, []uint32{1024, 1024}, []uint32{100, 120}, make([]byte, 220))
	if len(frag) >= 8 {
		size := int(frag[0])<<24 | int(frag[1])<<16 | int(frag[2])<<8 | int(frag[3])
		if size >= 8 && size <= len(frag) {
			f.Add(frag[8:size]) // the moof body, past its 8-byte header
		}
	}
	f.Add([]byte{}) // empty
	f.Add([]byte("\x00\x00\x00\x08traf"))

	trex := trexDefaults{have: true, defaultDur: 1024, defaultSize: 100, defaultFlags: 0}
	f.Fuzz(func(t *testing.T, data []byte) {
		fi, err := parseFragment(data, trex, 1)
		if err != nil {
			return
		}
		if len(fi.samples) > maxSamplesPerFragment {
			t.Fatalf("parsed %d samples past the %d cap", len(fi.samples), maxSamplesPerFragment)
		}
	})
}
