package mp4

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/container"
)

// FuzzDemux is the mp4 box parser's fuzz target. The box tree is the
// service's widest attack surface, so this carries the heaviest fuzz
// budget: the seed corpus is real files, truncations, and hand-built box
// trees exercising the size, depth, and descriptor paths. Invariants: no
// panic, no unbounded work, accepted tracks are well-formed, and seeks
// never overshoot.
func FuzzDemux(f *testing.F) {
	// mp3.mov is in the list for its own sample entry: a '.mp3' fourcc in a
	// version 1 entry, which reaches the offsets the esds path never does.
	//
	// The three PCM files are in it for the paths a compressed track never
	// takes. pcm-in24.mov reaches the version 1 packet-geometry fields, the
	// wave/enda unwrap (and its NUL-typed terminator box), and the chunk index
	// that replaces the flattened table; pcm-lpcm.mov reaches the version 2
	// struct's flags word and its packing and interleave checks;
	// pcm-ipcm-96k.mp4 reaches the pcmC parse and the zero rate field, so a
	// mutation there lands on the timescale fallback. All three also reach
	// parseStsz's constant-size arm, which no longer caps the declared count.
	//
	// ima4.mov is in it for the block-coded paths nothing else reaches: a
	// sample table whose units are blocks rather than frames, the short final
	// stts run that makes the timeline end inside the last one, and the
	// version 1 geometry fields ffmpeg writes as three zeros. ima4-frag.mov
	// is the same codec with no sample table at all, which is the only seed
	// that reaches the fragmented walk for a unit longer than a frame.
	for _, name := range []string{"alac-stereo.m4a", "alac-mono-tail.m4a", "mp3.mp4", "mp3.mov",
		"pcm-in24.mov", "pcm-lpcm.mov", "pcm-ipcm-96k.mp4", "ima4.mov", "ima4-frag.mov"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:200])
	}
	// Hand-built movies, which the fixtures above cannot stand in for: ffmpeg
	// writes mdat FIRST in a .mov, so every truncation of one of those files
	// loses the moov and dies at "no moov box" before reaching a sample entry
	// at all. These are a few hundred bytes each with the moov in front, so a
	// mutation anywhere in one still reaches the table builders, and they are
	// what a mutator can usefully edit: the sample entry, the chunk offsets and
	// the sample counts are all within a byte or two of each other.
	for _, seed := range [][]byte{
		buildMovie(soundEntry("sowt", 2, 16, movieTimescale), 4, 64, 0),
		buildMovie(soundEntry("sowt", 2, 16, movieTimescale), 4, 64, 7),
		buildMovie(v1SoundEntry("in24", 1, 16, movieTimescale, 3, waveBox(endaBox(1))), 3, 64, 0),
		buildMovie(lpcmEntry(float64(movieTimescale), 1, 24, lpcmSigned16LE, 3, 1), 3, 64, 0),
		buildMovie(soundEntryWith("ipcm", 1, 16, 0, pcmCBox(1, 24), sratBox(movieTimescale)), 3, 64, 0),
		// A chapter track whose sample description is an audio entry: the
		// shape that reaches the chapter walk through the PCM setter.
		withTextTrack(buildMovie(soundEntry("sowt", 2, 16, movieTimescale), 4, 64, 0)),
		// The two "ms" block codecs, whose whole geometry is a WAVEFORMATEX
		// inside the wave wrapper: a mutation of one of these edits a block
		// size, a samples-per-block count or a predictor table, none of which
		// any other seed here carries.
		movie{entry: soundEntryWith("ms\x00\x11", 1, 4, movieTimescale,
			waveBox(msADPCMAtom("ms\x00\x11", 0x0011, 1, movieTimescale, 20, 33, adpcm.DefaultCoefs))),
			unitBytes: 20, frames: 8, sttsDelta: 33}.build(),
		movie{entry: soundEntryWith("ms\x00\x02", 1, 4, movieTimescale,
			waveBox(msADPCMAtom("ms\x00\x02", 0x0002, 1, movieTimescale, 20, 28, adpcm.DefaultCoefs))),
			unitBytes: 20, frames: 8, sttsDelta: 28}.build(),
		// The G.711 spelling that is a format tag rather than a fourcc.
		movie{entry: soundEntryWith("ms\x00\x07", 2, 8, movieTimescale,
			waveBox(msWaveAtom("ms\x00\x07", 0x0007, 2, movieTimescale, 8))),
			unitBytes: 2, frames: 64}.build(),
		// The layout boxes: a chan bitmap, chan descriptions, chnl explicit
		// positions and a chnl configuration with an omitted-channels map,
		// each on a multichannel entry so a mutation reaches the fold.
		buildMovie(soundEntryWith("sowt", 6, 16, movieTimescale, chanBox(1<<16, 0x3F)), 12, 64, 0),
		buildMovie(soundEntryWith("sowt", 6, 16, movieTimescale, chanBox(0, 0, 1, 2, 5, 6, 3, 4)), 12, 64, 0),
		buildMovie(soundEntryWith("ipcm", 6, 16, movieTimescale, pcmCBox(1, 16), chnlPositions(0, 1, 8, 9, 2, 3)), 12, 64, 0),
		buildMovie(soundEntryWith("ipcm", 5, 16, movieTimescale, pcmCBox(1, 16), chnlDefined(6, 1<<5)), 10, 64, 0),
	} {
		f.Add(seed)
		f.Add(seed[:len(seed)/2])
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
	// The same shape with a segment index ahead of the fragments, so the length
	// resolver's scan and coverage rule are on the fuzzed path too.
	if seed := sidxFileSeed(); seed != nil {
		f.Add(seed)
		f.Add(seed[:len(seed)/2])
	}
	// A hybrid movie: a populated sample table in the moov and fragments
	// behind it, so the read path's transition between the two is fuzzed.
	if seed := hybridSeed(); seed != nil {
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

			// The walk reads the moof headers and settles the length, so it is
			// on the fuzzed path with the read. Its error is a refusal, not a
			// crash, and a settled track still has to describe itself.
			if err := d.Walk(); err == nil {
				tr := d.Tracks()[0]
				if tr.SamplesExact && tr.SamplesAdvisory {
					t.Fatal("a settled track is both exact and advisory")
				}
				if tr.Samples < -1 {
					t.Fatalf("a settled track reports %d samples", tr.Samples)
				}
			}

			// A generous safety cap so a bug that failed to terminate is caught,
			// not a tight bound: a fragmented trun may declare zero-size samples
			// (each capped per fragment at maxSamplesPerFragment), so the true
			// packet count is not bounded by the byte size. Reading a prefix
			// still exercises the Dur>0 and no-crash invariants on every packet.
			maxPackets := int(d.size) + maxSamplesPerFragment + 8
			var pkt container.Packet
			firstPTS := int64(-1)
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
				if firstPTS < 0 {
					firstPTS = pkt.PTS
				}
			}

			// A landing past the target is legitimate only when the stream
			// itself starts after it, which container.Seeker allows and a
			// crafted tfdt produces: the fragments can begin anywhere.
			for _, target := range []int64{0, 1, 1000, 1 << 20, 1 << 40} {
				landed, err := d.SeekSample(0, target)
				if err != nil || firstPTS < 0 {
					continue
				}
				if landed > max(target, firstPTS) {
					t.Fatalf("seek to %d landed at %d (the stream starts at %d)", target, landed, firstPTS)
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

// sidxFileSeed is fragmentedSeed with a complete segment index between the init
// header and the fragments, or nil if construction fails.
func sidxFileSeed() []byte {
	track, pkts := fuzzOpusTrack()
	track.Samples = -1 // no edit-list length, so the index is what states one
	init, err := InitSegment(track)
	if err != nil {
		return nil
	}
	seg, err := NewSegmenter(track, &SegmenterOptions{SegmentSamples: 2 * 960})
	if err != nil {
		return nil
	}
	var media bytes.Buffer
	emit := func(s Segment) error { media.Write(s.Data); return nil }
	for _, p := range pkts {
		if seg.WritePacket(p, emit) != nil {
			return nil
		}
	}
	if seg.End(emit) != nil {
		return nil
	}
	idx := sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(media.Len()), dur: 4 * 960}})
	out := append(append([]byte(nil), init...), idx...)
	return append(out, media.Bytes()...)
}

// hybridSeed is a movie carrying both a sample table and fragments, or nil if
// construction fails.
func hybridSeed() []byte {
	track, _ := fuzzOpusTrack()
	entry, err := sampleEntryFor(track)
	if err != nil {
		return nil
	}
	return hybridMovie(movie{entry: entry, unitBytes: 40, frames: 6, chunkFrames: 3,
		timescale: 48000, sttsDelta: 960}, 2, 3, true)
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
		var fi fragInfo
		err := parseFragment(&fi, data, trex, 1)
		if err != nil {
			return
		}
		if len(fi.samples) > maxSamplesPerFragment {
			t.Fatalf("parsed %d samples past the %d cap", len(fi.samples), maxSamplesPerFragment)
		}
	})
}

// FuzzSidx targets the segment-index parser directly: parseSidx must not panic
// or run unbounded on any payload, and its sums must stay inside the cap that
// keeps the coverage arithmetic from overflowing. The reference_count is a
// uint16 and referenced_size a uint31, so a real index cannot approach either
// bound; the point is that a crafted one cannot wrap past them into a small
// plausible number.
func FuzzSidx(f *testing.F) {
	f.Add(sidxSeed())
	f.Add([]byte{})
	f.Add([]byte("\x00\x00\x00\x00"))
	// A count the payload cannot hold: the length check must catch it before
	// the loop reads anything.
	f.Add(append([]byte("\x01\x00\x00\x00"), make([]byte, 28)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := parseSidx(data)
		if err != nil {
			return
		}
		if s.sumSize < 0 || s.sumSize > sidxSumCap {
			t.Fatalf("sumSize %d outside [0, %d]", s.sumSize, sidxSumCap)
		}
		if s.sumDuration < 0 || s.sumDuration > sidxSumCap {
			t.Fatalf("sumDuration %d outside [0, %d]", s.sumDuration, sidxSumCap)
		}
		if s.firstOffset < 0 || s.firstOffset > sidxSumCap {
			t.Fatalf("firstOffset %d outside [0, %d]", s.firstOffset, sidxSumCap)
		}
		if 12*s.refs > len(data) {
			t.Fatalf("accepted %d references from %d bytes", s.refs, len(data))
		}
		// coverageEnd is the sum the caps exist for: it must stay a number.
		if e := s.coverageEnd(); e < 0 {
			t.Fatalf("coverageEnd %d overflowed", e)
		}
	})
}

// sidxSeed is a well-formed sidx payload (past its 8-byte header) for the
// corpus: two references over a 48 kHz time base.
func sidxSeed() []byte {
	box := sidxBox(1, 48000, 0, 0, []sidxRef{{size: 1000, dur: 1920}, {size: 1200, dur: 1920}})
	return box[8:]
}
