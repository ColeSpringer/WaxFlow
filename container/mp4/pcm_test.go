package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// The PCM fixtures: one per sample-entry shape the MP4 family spells
// uncompressed audio with. Every one is the same 440 Hz sine, mono unless
// stated, and 0.1 s or shorter except the two in24 files, which are half a
// second so the interleaved one has enough audio to fill several chunks.
//
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.5" \
//	    -ac 1 -c:a pcm_s24be -f mov pcm-in24.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.5" \
//	    -f lavfi -i "anullsrc=sample_rate=8000:channel_layout=mono" -t 0.5 \
//	    -map 0:a -map 1:a -ac 1 -c:a pcm_s24be -f mov pcm-in24-chunks.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_s24le -f mov pcm-in24le.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 2 -c:a pcm_s16le -f mov pcm-sowt.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_s16be -f mov pcm-twos.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_s32be -f mov pcm-in32.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_u8 -f mov pcm-raw.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_f32le -f mov pcm-fl32.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_f64be -f mov pcm-fl64.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=96000:duration=0.05" \
//	    -ac 1 -c:a pcm_s24le -f mov pcm-lpcm.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.1" \
//	    -ac 1 -c:a pcm_s24le -f mp4 pcm-ipcm.mp4
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=96000:duration=0.1" \
//	    -ac 1 -c:a pcm_s24le -f mp4 pcm-ipcm-96k.mp4
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.1" \
//	    -ac 1 -c:a pcm_f32le -f mp4 pcm-fpcm.mp4
//
// (ffmpeg 8.0.1, Ubuntu.) What each one is for: pcm-in24.mov is a version 1
// QuickTime entry with the geometry in a wave/enda pair; pcm-in24le.mov is the
// same with enda 1; in32 is the 32-bit integer of the same family; sowt and
// twos are version 0 entries whose fourcc alone states the byte order; raw is
// unsigned 8-bit; fl32/fl64 are the float pair,
// one each way round; pcm-lpcm.mov is ffmpeg's version 2 lpcm, which it writes
// for any rate the 16.16 field cannot hold; the three .mp4 files are the ISO
// ipcm/fpcm form with a pcmC box, and the 96 kHz one writes a ZERO rate field
// and no srat, so its rate can only come from the media timescale.
type pcmFixture struct {
	name     string
	cfg      pcm.Config
	rate     int
	channels int
	frames   int64
	// srcDepth is the Track.SourceBitDepth expected, 0 for a track whose
	// Fmt.BitDepth already carries the source's depth.
	srcDepth int
}

var pcmFixtures = []pcmFixture{
	{"pcm-in24.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true}, 8000, 1, 4000, 0},
	{"pcm-in24-chunks.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true}, 8000, 1, 4000, 0},
	{"pcm-in24le.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 8000, 1, 800, 0},
	{"pcm-sowt.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 8000, 2, 800, 0},
	{"pcm-twos.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}, 8000, 1, 800, 0},
	{"pcm-in32.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 32, BigEndian: true}, 8000, 1, 800, 0},
	{"pcm-raw.mov", pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, 8000, 1, 800, 0},
	{"pcm-fl32.mov", pcm.Config{Encoding: pcm.Float, Bits: 32}, 8000, 1, 800, 0},
	// audio.Format carries floats as float32, so a 64-bit source decodes at
	// BitDepth 32 and its own depth rides in SourceBitDepth.
	{"pcm-fl64.mov", pcm.Config{Encoding: pcm.Float, Bits: 64, BigEndian: true}, 8000, 1, 800, 64},
	{"pcm-lpcm.mov", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 96000, 1, 4800, 0},
	{"pcm-ipcm.mp4", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 48000, 1, 4800, 0},
	{"pcm-ipcm-96k.mp4", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 96000, 1, 9600, 0},
	{"pcm-fpcm.mp4", pcm.Config{Encoding: pcm.Float, Bits: 32}, 48000, 1, 4800, 0},
}

// TestDemuxPCMFixtures pins the track every fixture opens as and walks its
// packets. The wire config is the whole of what a PCM track carries, so it is
// compared field by field: an entry read with the wrong width or the wrong
// byte order still yields a plausible track and audible audio.
func TestDemuxPCMFixtures(t *testing.T) {
	for _, f := range pcmFixtures {
		t.Run(f.name, func(t *testing.T) {
			d := open(t, fixture(t, f.name))
			tr := d.Tracks()[0]
			if tr.Codec != codec.PCM {
				t.Fatalf("codec = %q, want %q", tr.Codec, codec.PCM)
			}
			cfg, err := pcm.ParseConfig(tr.CodecConfig)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if cfg != f.cfg {
				t.Errorf("wire config = %+v, want %+v", cfg, f.cfg)
			}
			want := f.cfg.PCMFormat(f.rate, f.channels, audio.DefaultLayout(f.channels))
			if tr.Fmt != want {
				t.Errorf("format = %v, want %v", tr.Fmt, want)
			}
			if tr.Samples != f.frames {
				t.Errorf("samples = %d, want %d", tr.Samples, f.frames)
			}
			if tr.Delay != 0 || tr.Padding != 0 {
				t.Errorf("delay/padding = %d/%d, want 0/0: none of these files states a trim",
					tr.Delay, tr.Padding)
			}
			if tr.SourceBitDepth != f.srcDepth {
				t.Errorf("source bit depth = %d, want %d", tr.SourceBitDepth, f.srcDepth)
			}

			frameBytes := f.cfg.BytesPerFrame(f.channels)
			var total, count int64
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("ReadPacket %d: %v", count, err)
				}
				if !pkt.Sync {
					t.Fatalf("packet %d is not a sync point; every PCM frame is one", count)
				}
				if len(pkt.Data)%frameBytes != 0 {
					t.Fatalf("packet %d is %d bytes, not a whole number of %d-byte frames",
						count, len(pkt.Data), frameBytes)
				}
				if int64(len(pkt.Data)/frameBytes) != pkt.Dur {
					t.Fatalf("packet %d carries %d frames and declares %d",
						count, len(pkt.Data)/frameBytes, pkt.Dur)
				}
				if pkt.PTS != total {
					t.Fatalf("packet %d pts = %d, want %d", count, pkt.PTS, total)
				}
				// The chunking the reader chooses, not the file's: a
				// per-frame packet would be correct and unusable.
				if pkt.Dur > audio.StandardChunk {
					t.Fatalf("packet %d carries %d frames, more than the standard chunk %d",
						count, pkt.Dur, audio.StandardChunk)
				}
				total += pkt.Dur
				count++
			}
			if total != f.frames {
				t.Errorf("packets carry %d frames, the track declares %d", total, f.frames)
			}
			if count == 0 {
				t.Error("no packets")
			}

			// Strict must accept every one: these are well-formed files, and
			// the one limitation any of them reaches (a rate taken from the
			// media timescale) is a Note rather than damage.
			if _, err := NewDemuxer(container.BytesSource(fixture(t, f.name)), &DemuxerOptions{Strict: true}); err != nil {
				t.Errorf("strict mode refused a well-formed file: %v", err)
			}
			for _, w := range d.Warnings() {
				if w.Kind == container.Damage {
					t.Errorf("warning %q is damage; the file is well formed", w.Msg)
				}
			}
		})
	}
}

// TestPCMSeeksAreExact holds the seek to the exactness the uniform table
// makes possible. Every unit on that path is one frame, so a landing needs no
// pre-roll and no sync search: the answer is the target itself, and the packet
// that follows starts there.
func TestPCMSeeksAreExact(t *testing.T) {
	for _, f := range pcmFixtures {
		t.Run(f.name, func(t *testing.T) {
			d := open(t, fixture(t, f.name))
			for _, target := range []int64{0, 1, 7, 511, 512, 799, f.frames / 2, f.frames - 1} {
				if target >= f.frames {
					continue
				}
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek %d: %v", target, err)
				}
				if landed != target {
					t.Fatalf("seek %d landed at %d; a PCM frame needs no backoff", target, landed)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatalf("read after seek %d: %v", target, err)
				}
				if pkt.PTS != target {
					t.Fatalf("seek %d then read gave pts %d", target, pkt.PTS)
				}
			}
			// Past the end lands on the last frame rather than past it.
			landed, err := d.SeekSample(0, f.frames+1000)
			if err != nil {
				t.Fatal(err)
			}
			if landed != f.frames-1 {
				t.Errorf("past-end seek landed at %d, want the last frame %d", landed, f.frames-1)
			}
		})
	}
}

// TestPCMChunkIndexSpansSeveralChunks is what pcm-in24-chunks.mov is for. The
// mov muxer merges contiguous samples into one chunk of about a megabyte, so a
// single short track never yields more than one; a second interleaved track
// forces the audio into four, which is the shape the chunk index exists for.
//
// It also pins which track comes out. Both are 8 kHz mono in24, so nothing but
// the order distinguishes them, and the first one is the sine: a reader that
// took the second would deliver silence and pass every assertion above.
func TestPCMChunkIndexSpansSeveralChunks(t *testing.T) {
	d := open(t, fixture(t, "pcm-in24-chunks.mov"))
	st := &d.sel.st
	if !st.uniform {
		t.Fatal("the track did not take the uniform path")
	}
	if len(st.chunkOff) < 2 {
		t.Fatalf("chunk index holds %d chunks, want the file's several", len(st.chunkOff))
	}
	if int64(len(st.chunkFirst)) != int64(len(st.chunkOff))+1 {
		t.Fatalf("chunkFirst has %d entries for %d chunks; it needs the sentinel",
			len(st.chunkFirst), len(st.chunkOff))
	}
	if st.chunkFirst[len(st.chunkFirst)-1] != st.total {
		t.Errorf("chunkFirst sentinel = %d, want the total %d", st.chunkFirst[len(st.chunkFirst)-1], st.total)
	}

	// Packets must cross the chunk boundaries without a gap or a repeat, so
	// compare the bytes the walk yields against the same bytes addressed
	// chunk by chunk.
	var got []byte
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, pkt.Data...)
	}
	if int64(len(got)) != st.total*st.unitBytes {
		t.Fatalf("the walk yielded %d bytes for %d frames of %d", len(got), st.total, st.unitBytes)
	}
	// The first track is the sine, so the payload cannot be silent.
	nonzero := false
	for _, b := range got {
		if b != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Error("the selected track decoded to silence; the second (anullsrc) track was taken")
	}
}

// TestPCMRateFromTheMediaTimescale pins the fallback pcm-ipcm-96k.mp4 exists
// for. Above 65535 Hz the 16.16 rate field cannot hold the value, and ffmpeg's
// mp4 muxer writes a zero there and no srat box at all, so the media timescale
// is the only statement of the rate in the file. It says so with a Note, since
// the file is well formed and this is what the reader did about it.
func TestPCMRateFromTheMediaTimescale(t *testing.T) {
	d := open(t, fixture(t, "pcm-ipcm-96k.mp4"))
	if got := d.Tracks()[0].Fmt.Rate; got != 96000 {
		t.Errorf("rate = %d, want the media timescale's 96000", got)
	}
	var said bool
	for _, w := range d.Warnings() {
		if w.Kind == container.Note && strings.Contains(w.Msg, "timescale") {
			said = true
		}
	}
	if !said {
		t.Errorf("warnings = %v, want a note naming where the rate came from", d.Warnings())
	}
}

// TestPCMInAFragmentedMovieIsRefused covers a shape nothing writes: the
// fragmented path is per-sample by construction, and a PCM sample is one
// frame, so a fragment of them would be a trun with tens of thousands of
// entries per second.
//
// The second cell is the one that matters. Refusing is right for the track and
// wrong for the movie: selectAudio picks the first track it can decode, so a
// refusal raised after that pick fails a file whose other audio track is
// perfectly readable. The track has to leave the candidate set instead.
func TestPCMInAFragmentedMovieIsRefused(t *testing.T) {
	pcmTrack := func() *track {
		t.Helper()
		var tr track
		tr.handler = "soun"
		entry := soundEntry("sowt", 2, 16, 48000)
		if err := (&Demuxer{}).parseAudioSampleEntry(&tr, "sowt", entry[8:], 1); err != nil {
			t.Fatal(err)
		}
		return &tr
	}

	d := &Demuxer{fragmented: true}
	err := d.selectAudio([]*track{pcmTrack()})
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Fatalf("error = %v, want unsupported-format", err)
	}
	if !strings.Contains(err.Error(), "fragmented") {
		t.Errorf("error = %q, want it to name the fragmented movie", err)
	}

	// The same movie with an AAC track beside it opens on the AAC track.
	aac := &track{handler: "soun", codec: codec.AACLC, perAU: 1024,
		fmt: audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
			Type: audio.Float, BitDepth: 32}}
	d = &Demuxer{fragmented: true}
	if err := d.selectAudio([]*track{pcmTrack(), aac}); err != nil {
		t.Fatalf("a fragmented movie carrying PCM and AAC was refused whole: %v", err)
	}
	if got := d.Tracks()[0].Codec; got != codec.AACLC {
		t.Errorf("selected %q, want the AAC track the movie also carries", got)
	}
}

// TestChapterTrackWithAPCMEntryKeepsItsSamples is the crash this gate exists to
// stop. A chunk-indexed table leaves no per-sample offsets or sizes behind, and
// readTextChapters reads exactly those, so a text track whose sample
// description happens to carry a PCM fourcc used to open with a table whose two
// halves disagreed and panic on the chapter walk before a packet was read.
//
// Nothing writes such a file; a hostile one is two lines of edit away, which is
// why the gate is on the handler rather than on good intentions.
func TestChapterTrackWithAPCMEntryKeepsItsSamples(t *testing.T) {
	raw := withTextTrack(buildMovie(soundEntry("sowt", 2, 16, movieTimescale), 4, 64, 0))
	// The open itself is the assertion: this panicked in NewDemuxer, on the
	// chapter walk, before any packet was read.
	d := open(t, raw)
	if !d.sel.st.uniform {
		t.Error("the audio track did not take the uniform path")
	}
	_ = d.Chapters()

	// And the shape behind it: the demuxer surfaces only the track it chose,
	// so the tree is parsed again here to reach the text one.
	at := bytes.Index(raw, []byte("moov")) - 4
	size := int(binary.BigEndian.Uint32(raw[at:]))
	tracks, err := (&Demuxer{size: int64(len(raw))}).parseMoov(raw[at+8 : at+size])
	if err != nil {
		t.Fatal(err)
	}
	var text *track
	for _, tr := range tracks {
		if tr.handler == "text" {
			text = tr
		}
	}
	if text == nil {
		t.Fatal("no text track parsed")
	}
	if text.st.uniform {
		t.Error("the text track took the uniform path; the chapter walk reads per-sample arrays")
	}
	if int64(len(text.st.offsets)) != text.st.total || int64(len(text.st.sizes)) != text.st.total {
		t.Errorf("text track has %d offsets and %d sizes for %d samples",
			len(text.st.offsets), len(text.st.sizes), text.st.total)
	}
}

// withTextTrack clones a movie's single trak, turns the clone into a text
// track, and splices it back in: one audio track and one chapter track sharing
// a sample description, which is what a file crafted to reach the chapter walk
// through an audio entry looks like.
func withTextTrack(raw []byte) []byte {
	moovAt := bytes.Index(raw, []byte("moov")) - 4
	moovSize := int(binary.BigEndian.Uint32(raw[moovAt:]))
	trakAt := bytes.Index(raw, []byte("trak")) - 4
	trakSize := int(binary.BigEndian.Uint32(raw[trakAt:]))
	trak := bytes.Clone(raw[trakAt : trakAt+trakSize])
	copy(trak[bytes.Index(trak, []byte("soun")):], "text")
	out := make([]byte, 0, len(raw)+trakSize)
	out = append(out, raw[:moovAt+moovSize]...)
	out = append(out, trak...)
	out = append(out, raw[moovAt+moovSize:]...)
	binary.BigEndian.PutUint32(out[moovAt:], uint32(moovSize+trakSize))
	// mdat moved by the inserted trak, so every chunk offset in both traks does.
	for at := 0; ; {
		i := bytes.Index(out[at:], []byte("stco"))
		if i < 0 {
			break
		}
		at += i + 4
		for c, n := 0, int(binary.BigEndian.Uint32(out[at+4:])); c < n; c++ {
			off := at + 8 + 4*c
			binary.BigEndian.PutUint32(out[off:], binary.BigEndian.Uint32(out[off:])+uint32(trakSize))
		}
	}
	return out
}

// The hand-built cells below cover the entry and table shapes no fixture on
// this box can produce: the fourcc/flag combinations ffmpeg has no encoder
// for, the boxes a file can omit or misstate, and the sample tables a reader
// must decline to chunk-index. They ride on buildMovie, so what varies is the
// sample entry and the table, and nothing else.

// v1SoundEntry builds a version 1 AudioSampleEntry, the shape QuickTime writes
// for an uncompressed fourcc: four fields stating the packet geometry, then the
// children. bytesPerFrame is written as given, so a caller can lie in it.
func v1SoundEntry(format string, channels, bits, rate, bytesPerFrame int, children ...[]byte) []byte {
	head := [][]byte{
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(1), u16(0), u32(0), // version 1, revision, vendor
		u16(uint16(channels)), u16(uint16(bits)),
		u16(0), u16(0), // compressionID, packetSize
		u32(uint32(rate) << 16),
		u32(1), u32(uint32(bytesPerFrame)), // samplesPerPacket, bytesPerPacket
		u32(uint32(bytesPerFrame)), u32(2), // bytesPerFrame, bytesPerSample
	}
	return makeBox(format, append(head, children...)...)
}

// waveBox wraps codec extension boxes the way QuickTime does, terminator box
// and all: ffmpeg writes an 8-byte box of type NUL NUL NUL NUL to end the list,
// so a reader that mistakes it for a size is exercised here too.
func waveBox(children ...[]byte) []byte {
	parts := append([][]byte(nil), children...)
	return makeBox("wave", append(parts, makeBox("\x00\x00\x00\x00"))...)
}

// endaBox is QuickTime's byte-order box: 1 says the samples are little-endian.
func endaBox(little uint16) []byte { return makeBox("enda", u16(little)) }

// pcmCBox is ISO/IEC 23003-5's PCM configuration: bit 0 of the flags says
// little-endian, then the sample size in bits.
func pcmCBox(formatFlags, bits byte) []byte {
	return makeFullBox("pcmC", 0, 0, []byte{formatFlags, bits})
}

// sratBox states a sample rate the 16.16 field cannot hold.
func sratBox(hz uint32) []byte { return makeFullBox("srat", 0, 0, u32(hz)) }

// lpcmEntry builds a version 2 'lpcm' sound description with its LPCM fields
// filled: the formatSpecificFlags word, the bytes per packet, and the frames
// per packet. v2SoundEntry writes zeros in all three.
func lpcmEntry(rate float64, channels, depth, flags, packetBytes, framesPerPacket uint32) []byte {
	e := v2SoundEntry("lpcm", rate, channels, depth, nil)
	e = withU32(e, 8+52, flags)
	e = withU32(e, 8+56, packetBytes)
	e = withU32(e, 8+60, framesPerPacket)
	return e
}

// msWaveAtom is a WAVEFORMATEX struct stored as a box of the entry's own
// fourcc, which is where an "ms" entry's geometry lives when it has one. The
// struct is little-endian, unlike every box field around it.
func msWaveAtom(typ string, tag uint16, channels, rate, bits int) []byte {
	blockAlign := bits / 8 * channels
	return makeBox(typ,
		le16(tag), le16(uint16(channels)), le32(uint32(rate)),
		le32(uint32(rate*blockAlign)), le16(uint16(blockAlign)),
		le16(uint16(bits)), le16(0))
}

func le16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

func le32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// The lpcm flag combinations, spelled out so the cells below read as the
// storage they describe.
const (
	lpcmSigned16LE = lpcmIsSignedInteger | lpcmIsPacked
	lpcmFloat64BE  = lpcmIsFloat | lpcmIsBigEndian | lpcmIsPacked
	lpcmUnsigned8  = lpcmIsPacked
)

// TestPCMEntrySpellings reads one hand-built entry per storage the fixtures
// cannot reach, and pins the wire config and rate each comes out as.
func TestPCMEntrySpellings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entry     []byte
		unitBytes int
		channels  int
		cfg       pcm.Config
		rate      int
	}{{
		// 'NONE' names neither width nor byte order: the entry's samplesize
		// gives the first and an enda box the second.
		name:      "NONE-enda-le",
		entry:     v1SoundEntry("NONE", 1, 16, movieTimescale, 2, waveBox(endaBox(1))),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16},
		rate: movieTimescale,
	}, {
		// 'twos' is 16-bit by convention and carries other widths; at 8 bits
		// it is signed, where 'raw ' and 'NONE' are offset binary. BigEndian
		// is false and not true: the config keys the cache, and one byte has
		// no ends to put in an order, so the spellings of 8-bit storage must
		// not describe it two ways.
		name:      "twos-8-bit",
		entry:     soundEntry("twos", 1, 8, movieTimescale),
		unitBytes: 1, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 8},
		rate: movieTimescale,
	}, {
		// 'twos' names big-endian and an enda box overrides it, which is not
		// hypothetical: the reference reader answers pcm_s16le for this exact
		// entry (ffmpeg 8.0.1). A fourcc naming an order does not make the box
		// inapplicable, and reading it the other way round is full-scale noise.
		name:      "twos-enda-le",
		entry:     v1SoundEntry("twos", 1, 16, movieTimescale, 2, waveBox(endaBox(1))),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16},
		rate: movieTimescale,
	}, {
		// And the override runs one way only: enda 0 beside a little-endian
		// fourcc leaves it little-endian. Again the reference reader's answer
		// (pcm_s16le here, pcm_s16be for 'twos' with the same box).
		name:      "sowt-enda-be-is-ignored",
		entry:     v1SoundEntry("sowt", 1, 16, movieTimescale, 2, waveBox(endaBox(0))),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16},
		rate: movieTimescale,
	}, {
		// An empty box of the right type is still a box: it must not read as
		// absent and let the search look past it to the one beside it.
		name: "empty-enda-does-not-shadow",
		entry: v1SoundEntry("in24", 1, 16, movieTimescale, 3,
			waveBox(makeBox("enda")), endaBox(1)),
		unitBytes: 3, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true},
		rate: movieTimescale,
	}, {
		// Fourccs fold: a 'TWOS' entry reads as pcm_s16be in the reference
		// reader, so refusing it here would refuse a file every other reader
		// opens, and refuse it while naming the codec this package decodes.
		name:      "uppercase-fourcc",
		entry:     soundEntry("TWOS", 1, 16, movieTimescale),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true},
		rate: movieTimescale,
	}, {
		// 'raw ' names an encoding, not a width: the reference reader answers
		// pcm_u8 for it at 8 bits and pcm_s16be at 16, exactly as it does for
		// 'NONE'. Refusing the wider form as a fourcc contradicting its own
		// depth refused a file that plays elsewhere.
		name:      "raw-16-bit",
		entry:     soundEntry("raw ", 1, 16, movieTimescale),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true},
		rate: movieTimescale,
	}, {
		name:      "in32-be",
		entry:     v1SoundEntry("in32", 1, 16, movieTimescale, 4, waveBox(endaBox(0))),
		unitBytes: 4, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 32, BigEndian: true},
		rate: movieTimescale,
	}, {
		name:      "lpcm-float64-be",
		entry:     lpcmEntry(float64(movieTimescale), 1, 64, lpcmFloat64BE, 8, 1),
		unitBytes: 8, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.Float, Bits: 64, BigEndian: true},
		rate: movieTimescale,
	}, {
		// No IsSignedInteger and no IsFloat at 8 bits is offset binary, the
		// one unsigned width that exists.
		name:      "lpcm-unsigned-8",
		entry:     lpcmEntry(float64(movieTimescale), 2, 8, lpcmUnsigned8, 2, 1),
		unitBytes: 2, channels: 2,
		cfg:  pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8},
		rate: movieTimescale,
	}, {
		name:      "ipcm-16-be",
		entry:     soundEntryWith("ipcm", 2, 16, movieTimescale, pcmCBox(0, 16)),
		unitBytes: 4, channels: 2,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true},
		rate: movieTimescale,
	}, {
		name:      "ipcm-32-le",
		entry:     soundEntryWith("ipcm", 1, 16, movieTimescale, pcmCBox(1, 32)),
		unitBytes: 4, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 32},
		rate: movieTimescale,
	}, {
		name:      "fpcm-64-le",
		entry:     soundEntryWith("fpcm", 1, 16, movieTimescale, pcmCBox(1, 64)),
		unitBytes: 8, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.Float, Bits: 64},
		rate: movieTimescale,
	}, {
		// An "ms" entry follows WAVE's conventions, not QuickTime's: little
		// endian, and the width from the entry when there is nothing else.
		name:      "ms-pcm-from-the-entry",
		entry:     soundEntry("ms\x00\x01", 1, 16, movieTimescale),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16},
		rate: movieTimescale,
	}, {
		// And from the WAVEFORMATEX atom when it carries one, which is the
		// structure the tag belongs to: 24 bits there over 16 in the entry.
		name: "ms-pcm-from-the-waveformatex",
		entry: soundEntryWith("ms\x00\x01", 1, 16, movieTimescale,
			waveBox(msWaveAtom("ms\x00\x01", 1, 1, movieTimescale, 24))),
		unitBytes: 3, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 24},
		rate: movieTimescale,
	}, {
		// The channel count comes from the atom too, and for the same reason.
		// Taking the width from one field and the channels from the other is
		// how a frame size neither of them states gets built.
		name: "ms-pcm-channels-from-the-waveformatex",
		entry: soundEntryWith("ms\x00\x01", 2, 24, movieTimescale,
			waveBox(msWaveAtom("ms\x00\x01", 1, 2, movieTimescale, 24))),
		unitBytes: 6, channels: 2,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 24},
		rate: movieTimescale,
	}, {
		// The version 1 packet geometry is derived, never read, so an entry
		// that misstates it is read correctly rather than refused. ffmpeg
		// writes the truth here; a reader that believed it would still have
		// to agree with stsz, which is what the uniform table checks.
		name:      "v1-lying-bytes-per-frame",
		entry:     v1SoundEntry("in24", 1, 16, movieTimescale, 999, waveBox(endaBox(0))),
		unitBytes: 3, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true},
		rate: movieTimescale,
	}, {
		// srat states a rate the 16.16 field cannot hold, so the field is
		// zero and the box is the only statement of it.
		name: "srat-over-a-zero-rate-field",
		entry: soundEntryWith("ipcm", 1, 16, 0,
			pcmCBox(1, 16), sratBox(movieTimescale)),
		unitBytes: 2, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 16},
		rate: movieTimescale,
	}, {
		// Neither: the media timescale is what is left, and a byte-linear
		// track's timescale is its frame rate.
		name:      "rate-from-the-media-timescale",
		entry:     soundEntryWith("ipcm", 1, 16, 0, pcmCBox(1, 24)),
		unitBytes: 3, channels: 1,
		cfg:  pcm.Config{Encoding: pcm.SignedInt, Bits: 24},
		rate: movieTimescale,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildMovie(tc.entry, tc.unitBytes, 64, 0)
			d := open(t, raw)
			tr := d.Tracks()[0]
			if tr.Codec != codec.PCM {
				t.Fatalf("codec = %q, want %q", tr.Codec, codec.PCM)
			}
			cfg, err := pcm.ParseConfig(tr.CodecConfig)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if cfg != tc.cfg {
				t.Errorf("wire config = %+v, want %+v", cfg, tc.cfg)
			}
			want := tc.cfg.PCMFormat(tc.rate, tc.channels, audio.DefaultLayout(tc.channels))
			if tr.Fmt != want {
				t.Errorf("format = %v, want %v", tr.Fmt, want)
			}
			if tr.Samples != 64 {
				t.Errorf("samples = %d, want 64", tr.Samples)
			}
			if !d.sel.st.uniform {
				t.Errorf("the track did not take the uniform path")
			}
			// The whole payload, in order: buildMovie fills mdat with a
			// counting pattern, so a misread offset shows up as a mismatch
			// rather than as plausible audio.
			if got := readAllPacketData(t, d); !bytes.Equal(got, movieMdat(raw, tc.unitBytes*64)) {
				t.Errorf("the packet walk delivered %d bytes, not the mdat payload", len(got))
			}
		})
	}
}

// TestPCMEntryRefusals covers the entries that are well formed as boxes and
// state a storage this reader does not unpack, or state one impossibly. Each
// must be refused with a message naming what it found, since the alternative
// is audio decoded from a layout nobody agreed on.
//
// Which code each carries is the other half: unsupported says the file is
// fine and this build is not, malformed says the entry contradicts itself.
func TestPCMEntryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry []byte
		code  waxerr.Code
		want  string
	}{{
		// The fourcc predates the version 2 layout, and a version 0 or 1
		// 'lpcm' entry says nothing about which LPCM storage it holds.
		name:  "lpcm-not-version-2",
		entry: soundEntry("lpcm", 1, 16, movieTimescale),
		code:  waxerr.CodeUnsupportedFormat, want: "version 0 sample entry",
	}, {
		name:  "lpcm-aligned-high",
		entry: lpcmEntry(float64(movieTimescale), 1, 24, lpcmSigned16LE|lpcmIsAlignedHigh, 3, 1),
		code:  waxerr.CodeUnsupportedFormat, want: "high-aligned",
	}, {
		name:  "lpcm-non-interleaved",
		entry: lpcmEntry(float64(movieTimescale), 2, 16, lpcmSigned16LE|lpcmIsNonInterleaved, 4, 1),
		code:  waxerr.CodeUnsupportedFormat, want: "non-interleaved",
	}, {
		// 20 bits in 20 bits: the samples do not start on byte boundaries,
		// so there is no width pcm.Config can describe them at.
		name:  "lpcm-unpacked-20-bit",
		entry: lpcmEntry(float64(movieTimescale), 1, 20, lpcmIsSignedInteger, 3, 1),
		code:  waxerr.CodeUnsupportedFormat, want: "do not begin on byte boundaries",
	}, {
		// Byte-aligned, but the packet size contradicts the width and the
		// channel count, so the samples are padded in a way pcm.Config's
		// left-justified spare bits cannot describe.
		name:  "lpcm-packet-size-disagrees",
		entry: lpcmEntry(float64(movieTimescale), 2, 16, lpcmSigned16LE, 3, 1),
		code:  waxerr.CodeUnsupportedFormat, want: "3-byte packets for 2 channels of 16 bits",
	}, {
		// Unsigned wider than 8 bits does not exist; a file spelling it is
		// asking for a conversion nobody defines.
		name:  "lpcm-unsigned-16",
		entry: lpcmEntry(float64(movieTimescale), 1, 16, lpcmIsPacked, 2, 1),
		code:  waxerr.CodeUnsupportedFormat, want: "unsigned 16-bit",
	}, {
		// Mandatory in 23003-5, and there is nothing to fall back on: the
		// entry's samplesize reads 16 whatever the samples are.
		name:  "ipcm-without-pcmC",
		entry: soundEntry("ipcm", 1, 16, movieTimescale),
		code:  waxerr.CodeMalformedInput, want: "no pcmC box",
	}, {
		// A pcmC is exactly two bytes of content. A longer one is not the box
		// the spec defines, so the two fields are not where they would be
		// read from.
		name: "pcmC-too-long",
		entry: soundEntryWith("ipcm", 1, 16, movieTimescale,
			makeFullBox("pcmC", 0, 0, make([]byte, 12))),
		code: waxerr.CodeMalformedInput, want: "pcmC box of 16 bytes, want the 6",
	}, {
		name:  "ipcm-8-bit",
		entry: soundEntryWith("ipcm", 1, 16, movieTimescale, pcmCBox(1, 8)),
		code:  waxerr.CodeMalformedInput, want: "8-bit integers",
	}, {
		name:  "fpcm-16-bit",
		entry: soundEntryWith("fpcm", 1, 16, movieTimescale, pcmCBox(1, 16)),
		code:  waxerr.CodeMalformedInput, want: "16-bit floats",
	}, {
		name:  "twos-12-bit",
		entry: soundEntry("twos", 1, 12, movieTimescale),
		code:  waxerr.CodeUnsupportedFormat, want: "12-bit samples",
	}, {
		name:  "sowt-zero-channels",
		entry: soundEntry("sowt", 0, 16, movieTimescale),
		code:  waxerr.CodeMalformedInput, want: "0 channels",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildMovie(tc.entry, 4, 64, 0)
			_, err := NewDemuxer(container.BytesSource(raw), nil)
			if err == nil {
				t.Fatal("the entry was accepted")
			}
			if got := waxerr.CodeOf(err); got != tc.code {
				t.Errorf("code = %q, want %q (%v)", got, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestPCMEntryDamageIsToleratedAndNamed covers the entries that are wrong
// about themselves in a way the samples survive: the storage is still where
// the rest of the entry says, so an ordinary read carries on and --strict
// refuses.
func TestPCMEntryDamageIsToleratedAndNamed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entry     []byte
		unitBytes int
		timescale int
		rate      int
		want      string
	}{{
		// LPCM is one frame per packet by definition, so a field saying
		// otherwise contradicts the storage it is describing.
		name:      "lpcm-frames-per-packet",
		entry:     lpcmEntry(float64(movieTimescale), 1, 16, lpcmSigned16LE, 2, 4),
		unitBytes: 2, timescale: movieTimescale, rate: movieTimescale,
		want: "4 frames per packet",
	}, {
		// Two fields, one number. srat is the wider one and the spec's
		// order puts it first, so it wins and the disagreement is damage.
		name: "srat-disagrees-with-the-rate-field",
		entry: soundEntryWith("ipcm", 1, 16, movieTimescale,
			pcmCBox(1, 16), sratBox(44100)),
		unitBytes: 2, timescale: 44100, rate: 44100,
		want: "srat says 44100 Hz and the sample entry says 8000",
	}, {
		// The entry header and the WAVEFORMATEX beside it state one geometry
		// twice. Where they disagree the atom wins, and a reader that took
		// the width from one and the channels from the other would build a
		// frame size neither of them states.
		name: "ms-geometry-disagrees",
		entry: soundEntryWith("ms\x00\x01", 2, 16, movieTimescale,
			waveBox(msWaveAtom("ms\x00\x01", 1, 1, movieTimescale, 16))),
		unitBytes: 2, timescale: movieTimescale, rate: movieTimescale,
		want: "says 2 channels of 16 bits, its WAVEFORMATEX says 1 of 16",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := movie{entry: tc.entry, unitBytes: tc.unitBytes, frames: 64,
				timescale: tc.timescale}.build()
			d := open(t, raw)
			if got := d.Tracks()[0].Fmt.Rate; got != tc.rate {
				t.Errorf("rate = %d, want %d", got, tc.rate)
			}
			var said bool
			for _, w := range d.Warnings() {
				if w.Kind == container.Damage && strings.Contains(w.Msg, tc.want) {
					said = true
				}
			}
			if !said {
				t.Errorf("warnings = %v, want damage naming %q", d.Warnings(), tc.want)
			}
			if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
				t.Error("strict accepted an entry that contradicts itself")
			}
		})
	}
}

// TestPCMTableFallsBackToFlatten pins the two tables a byte-linear track can
// arrive with that the chunk index cannot describe, and that the fallback is
// the flat one rather than a refusal: both files are well formed and hold
// exactly the samples the uniform build does.
func TestPCMTableFallsBackToFlatten(t *testing.T) {
	entry := v1SoundEntry("in24", 1, 16, movieTimescale, 3, waveBox(endaBox(0)))
	const frames = 64
	base := movie{entry: entry, unitBytes: 3, frames: frames}
	wantData := readAllPacketData(t, open(t, base.build()))

	for _, tc := range []struct {
		name string
		m    movie
	}{{
		// A per-sample stsz for constant-size samples is legal and wasteful,
		// and it is the shape the chunk index has no constant size to read.
		name: "per-sample-stsz",
		m:    movie{entry: entry, unitBytes: 3, frames: frames, perSample: true},
	}, {
		// Two ticks per frame is a timeline the uniform table cannot express
		// (its unit is one frame of one tick), and the samples are unmoved.
		name: "two-ticks-per-frame",
		m:    movie{entry: entry, unitBytes: 3, frames: frames, sttsDelta: 2},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			d := open(t, tc.m.build())
			if d.sel.st.uniform {
				t.Fatal("the track took the uniform path; this table cannot be chunk-indexed")
			}
			if got := readAllPacketData(t, d); !bytes.Equal(got, wantData) {
				t.Errorf("the flat table delivered %d bytes, the uniform one %d", len(got), len(wantData))
			}
		})
	}
}

// TestByteLinearLengthDisagreementIsNamed pins the cross-check the flat
// fallback needs and the chunk-indexed path gets for free.
//
// A byte-linear track states its length twice, in the time-to-sample table and
// in the bytes its samples occupy, and the two are the same number. When they
// are not, believing the timeline turns a tenth of a second into a 27-hour
// track that probe reports and a segmenter plans against, so it is named
// instead: --strict refuses it, and an ordinary read says so.
func TestByteLinearLengthDisagreementIsNamed(t *testing.T) {
	raw := patchBoxField(fixture(t, "pcm-twos.mov"), "stts", 12, 999999) // the delta
	d := open(t, raw)
	if d.sel.st.uniform {
		t.Fatal("the track took the uniform path; this stts cannot be chunk-indexed")
	}
	var said bool
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage && strings.Contains(w.Msg, "its samples hold 800") {
			said = true
		}
	}
	if !said {
		t.Errorf("warnings = %v, want damage naming the two lengths", d.Warnings())
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict accepted a track whose two lengths disagree")
	}
}

// TestAbsurdMediaTimescaleIsRefusedTheSameWayEverywhere covers the one field
// that reaches a sample rate without passing through a bound. mdhd's timescale
// is a uint32, so a 32-bit build narrows it negative where a 64-bit one does
// not, and make test and make test-386 would answer differently about one
// file.
func TestAbsurdMediaTimescaleIsRefusedTheSameWayEverywhere(t *testing.T) {
	// An ipcm entry with no rate of its own, so the timescale is the rate.
	raw := buildMovie(soundEntryWith("ipcm", 1, 16, 0, pcmCBox(1, 16)), 2, 64, 0)
	raw = patchBoxField(raw, "mdhd", 12, 0xFFFFFFFF)
	_, err := NewDemuxer(container.BytesSource(raw), nil)
	if err == nil {
		t.Fatal("a 4294967295 Hz media timescale was accepted")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeMalformedInput {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeMalformedInput, err)
	}
	if !strings.Contains(err.Error(), "4294967295") {
		t.Errorf("error %q does not name the timescale it refused", err)
	}
}

// TestPCMChunkIndexCrossesManyChunks is the multi-chunk pin that depends on no
// muxer's interleaving: ten chunks of seven frames, walked end to end. The
// mdat payload is a counting pattern, so a packet that reads from the wrong
// chunk or the wrong offset inside one shows up as a byte mismatch.
func TestPCMChunkIndexCrossesManyChunks(t *testing.T) {
	const unitBytes, frames, chunkFrames = 3, 64, 7
	raw := buildMovie(v1SoundEntry("in24", 1, 16, movieTimescale, unitBytes,
		waveBox(endaBox(0))), unitBytes, frames, chunkFrames)
	d := open(t, raw)
	st := &d.sel.st
	if want := (frames + chunkFrames - 1) / chunkFrames; len(st.chunkOff) != want {
		t.Fatalf("chunk index holds %d chunks, want %d", len(st.chunkOff), want)
	}
	if st.total != frames {
		t.Fatalf("total = %d, want %d", st.total, frames)
	}
	if got, want := readAllPacketData(t, d), movieMdat(raw, unitBytes*frames); !bytes.Equal(got, want) {
		t.Fatalf("the walk delivered %d bytes, want the mdat payload's %d", len(got), len(want))
	}
	// And a seek into the middle of a chunk reads from inside it, not from
	// its start: the offset within the chunk is the part a chunk index can
	// get wrong.
	for _, target := range []int64{0, 6, 7, 8, 20, 63} {
		landed, err := d.SeekSample(0, target)
		if err != nil || landed != target {
			t.Fatalf("seek %d = %d, %v", target, landed, err)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek %d: %v", target, err)
		}
		want := movieMdat(raw, unitBytes*frames)[target*unitBytes:]
		if !bytes.HasPrefix(want, pkt.Data) {
			t.Errorf("seek %d read %x, want the payload at that frame %x",
				target, pkt.Data[:min(len(pkt.Data), 6)], want[:min(len(want), 6)])
		}
	}
}

// TestPCMTruncatedMdatKeepsWhatItHolds pins the bound on the chunk index: a
// chunk that runs past the end of the file keeps the whole frames that fit,
// says so as damage, and refuses under --strict. A truncated download is the
// common case, and playing what arrived beats refusing the file.
func TestPCMTruncatedMdatKeepsWhatItHolds(t *testing.T) {
	const unitBytes, frames = 4, 64
	full := buildMovie(soundEntry("sowt", 2, 16, movieTimescale), unitBytes, frames, 0)
	// Half the payload, and not on a frame boundary: the odd two bytes must
	// not become a thirty-second frame.
	cut := full[:len(full)-(frames/2*unitBytes)-2]
	d := open(t, cut)
	tr := d.Tracks()[0]
	if want := int64(frames/2 - 1); tr.Samples != want {
		t.Errorf("samples = %d, want the %d whole frames the file holds", tr.Samples, want)
	}
	var said bool
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage && strings.Contains(w.Msg, "runs past end of file") {
			said = true
		}
	}
	if !said {
		t.Errorf("warnings = %v, want damage naming the truncation", d.Warnings())
	}
	// Every packet the shortened table describes must still read.
	if got := int64(len(readAllPacketData(t, d))); got != tr.Samples*unitBytes {
		t.Errorf("the walk delivered %d bytes for %d frames", got, tr.Samples)
	}
	if _, err := NewDemuxer(container.BytesSource(cut), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict accepted a truncated mdat")
	}
}

// TestPCMSampleCountLargerThanTheFileIsClamped is the hostile-input side of
// dropping the sample cap for a constant-size table. The declared count is
// what a crafted file controls, and with no per-sample allocation behind it
// the bound that matters is the file's own size.
func TestPCMSampleCountLargerThanTheFileIsClamped(t *testing.T) {
	const unitBytes, frames = 4, 64
	raw := buildMovie(soundEntry("sowt", 2, 16, movieTimescale), unitBytes, frames, 0)
	// A billion frames in a table the file has room for sixty-four of, said
	// twice: the sample count, and the samples-per-chunk that would reach
	// them.
	huge := patchBoxField(raw, "stsz", 8, 1<<30)
	huge = patchBoxField(huge, "stsc", 12, 1<<30)
	huge = patchBoxField(huge, "stts", 8, 1<<30)
	d := open(t, huge)
	tr := d.Tracks()[0]
	// Exactly what the file holds: the chunk index keeps the whole frames
	// that fit between the chunk offset and the end of the file, which for
	// this movie is the payload buildMovie wrote.
	if tr.Samples != frames {
		t.Fatalf("samples = %d, want the %d frames the %d-byte file holds", tr.Samples, frames, len(huge))
	}
	if got := int64(len(readAllPacketData(t, d))); got != tr.Samples*unitBytes {
		t.Errorf("the walk delivered %d bytes for %d frames", got, tr.Samples)
	}
	// And the index it built is sized by what it yielded, not by what the
	// table declared: one chunk, not a billion.
	if n := len(d.sel.st.chunkOff); n != 1 {
		t.Errorf("chunk index holds %d chunks, want 1", n)
	}
}

// TestChunkIndexIsSizedByTheSamplesItYields is the memory bound's other half.
// A table can declare far more CHUNKS than it has samples, and reserving one
// index entry per declared chunk is the mirror of the per-sample table this
// representation exists to avoid.
func TestChunkIndexIsSizedByTheSamplesItYields(t *testing.T) {
	const unitBytes = 4
	// Ten thousand chunks of one frame each, and a sample table that then says
	// there is one sample.
	raw := buildMovie(soundEntry("sowt", 2, 16, movieTimescale), unitBytes, 10000, 1)
	raw = patchBoxField(raw, "stsz", 8, 1)
	raw = patchBoxField(raw, "stts", 8, 1)
	d := open(t, raw)
	st := &d.sel.st
	if st.total != 1 {
		t.Fatalf("total = %d, want 1", st.total)
	}
	if c := cap(st.chunkOff); c > 1 {
		t.Errorf("chunk index reserved %d entries for %d samples", c, st.total)
	}
}

// TestUniformTableNeedsNoPerSampleMemory is why the chunk index exists.
//
// The source claims a terabyte and serves only the movie header, and the table
// declares 134 million frames in one chunk: a reader that materialised an
// offset and a size per sample would need 1.6 GB before it could discover that
// none of the data is there. The assertion is structural rather than a memory
// measurement, which is the same thing said exactly: the table describes every
// one of those frames with no per-sample array at all, and the first read
// fails on the source instead.
func TestUniformTableNeedsNoPerSampleMemory(t *testing.T) {
	const unitBytes, declared = 4, int64(1) << 27
	raw := buildMovie(soundEntry("sowt", 2, 16, movieTimescale), unitBytes, 64, 0)
	raw = patchBoxField(raw, "stsz", 8, uint32(declared))
	raw = patchBoxField(raw, "stsc", 12, uint32(declared))
	raw = patchBoxField(raw, "stts", 8, uint32(declared))

	d, err := NewDemuxer(headOnlySource{head: raw, size: 1 << 40}, nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	st := &d.sel.st
	if st.total != declared {
		t.Fatalf("total = %d, want the declared %d", st.total, declared)
	}
	if len(st.offsets) != 0 || len(st.sizes) != 0 {
		t.Fatalf("the table holds %d offsets and %d sizes; a uniform one holds none",
			len(st.offsets), len(st.sizes))
	}
	if d.Tracks()[0].Samples != declared {
		t.Errorf("samples = %d, want %d", d.Tracks()[0].Samples, declared)
	}
	// The data is not there, and that is what the read reports.
	var pkt container.Packet
	for i := 0; i < 4; i++ {
		if err = d.ReadPacket(&pkt); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("ReadPacket delivered samples from a source that holds none")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeSourceUnreadable {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeSourceUnreadable, err)
	}
}

// headOnlySource serves the bytes it holds and claims a size no file has, so a
// table sized from a declared sample count allocates before any read can fail.
type headOnlySource struct {
	head []byte
	size int64
}

func (s headOnlySource) Size() int64 { return s.size }

func (s headOnlySource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s.head)) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, s.head[off:])
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// patchBoxField overwrites the 4-byte field at fieldOff bytes past a box's
// type, so a test can state a table the movie it was built around cannot hold.
func patchBoxField(raw []byte, typ string, fieldOff int, v uint32) []byte {
	at := bytes.Index(raw, []byte(typ))
	if at < 0 {
		panic("no " + typ + " box in the movie")
	}
	out := bytes.Clone(raw)
	binary.BigEndian.PutUint32(out[at+4+fieldOff:], v)
	return out
}

// movieMdat returns the last n bytes of a built movie, which is its mdat
// payload: buildMovie writes mdat last and fills it with a counting pattern.
func movieMdat(raw []byte, n int) []byte { return raw[len(raw)-n:] }

// readAllPacketData concatenates every packet's bytes, which for PCM is the
// decoded stream itself.
func readAllPacketData(t *testing.T, d *Demuxer) []byte {
	t.Helper()
	var out []byte
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, pkt.Data...)
	}
}

// TestOneTablePerTrackWhateverTheFileOffers pins what happens when a movie
// carries two sample tables for one track, which nothing orders or forbids.
//
// The two representations do not overwrite each other field for field, so the
// second pass used to install a flat table while leaving the first pass's
// uniform flag and chunk index standing. Read through a chunk index that no
// longer spans it, the walk emitted a zero-length packet of zero duration and
// never advanced: not a wrong answer but an unbounded loop, in NewDemuxer's
// caller rather than in NewDemuxer.
func TestOneTablePerTrackWhateverTheFileOffers(t *testing.T) {
	uniform := buildMovie(soundEntry("sowt", 2, 16, movieTimescale), 4, 64, 0)
	// More samples than the first table, so a stale chunk index cannot span it.
	flat := movie{entry: soundEntry("sowt", 2, 16, movieTimescale), unitBytes: 4,
		frames: 200, perSample: true}.build()
	raw := spliceSecondStbl(t, uniform, flat)

	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		return // refusing the file is a fine answer; looping is not
	}
	st := &d.sel.st
	if st.uniform && int64(len(st.offsets)) > 0 {
		t.Fatalf("the track carries both representations: uniform with %d per-sample offsets",
			len(st.offsets))
	}
	var pkt container.Packet
	for i := 0; i < 4*int(st.total)+8; i++ {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
		if pkt.Dur <= 0 {
			t.Fatalf("packet %d has duration %d and %d bytes: the walk cannot advance",
				i, pkt.Dur, len(pkt.Data))
		}
	}
	t.Fatal("the walk never reached the end of a table it had already delivered")
}

// spliceSecondStbl puts b's sample table into a as a second stbl inside the
// same minf, growing every box that contains it. Both movies must share a
// layout, which they do when both come from buildMovie.
func spliceSecondStbl(t *testing.T, a, b []byte) []byte {
	t.Helper()
	at := bytes.Index(a, []byte("stbl")) - 4
	size := int(binary.BigEndian.Uint32(a[at:]))
	bat := bytes.Index(b, []byte("stbl")) - 4
	bsize := int(binary.BigEndian.Uint32(b[bat:]))

	out := append(bytes.Clone(a[:at+size]), b[bat:bat+bsize]...)
	out = append(out, a[at+size:]...)
	for _, typ := range []string{"minf", "mdia", "trak", "moov"} {
		i := bytes.Index(out, []byte(typ)) - 4
		binary.BigEndian.PutUint32(out[i:], binary.BigEndian.Uint32(out[i:])+uint32(bsize))
	}
	return out
}

// TestSampleTableIsReadBeforeTheBoxesItNeeds pins the box order the sample
// table is built against. mdhd states the timescale it is timed in and hdlr
// says whether it is audio at all, and both are minf's SIBLINGS: nothing
// orders siblings, so a movie that writes minf first reached the table builder
// with no timescale to rescale against and no handler to decide by.
func TestSampleTableIsReadBeforeTheBoxesItNeeds(t *testing.T) {
	raw := minfFirst(t, buildMovie(soundEntry("sowt", 2, 16, movieTimescale), 4, 64, 0))
	d := open(t, raw)
	tr := d.Tracks()[0]
	if tr.Fmt.Rate != movieTimescale || tr.Samples != 64 {
		t.Errorf("track = %d Hz / %d samples, want %d / 64", tr.Fmt.Rate, tr.Samples, movieTimescale)
	}
	if !d.sel.st.uniform {
		t.Error("the track did not take the uniform path; the timescale was not read in time")
	}
	// And the rate fallback, which reads the timescale directly.
	raw = minfFirst(t, buildMovie(soundEntryWith("ipcm", 1, 16, 0, pcmCBox(1, 24)), 3, 64, 0))
	if got := open(t, raw).Tracks()[0].Fmt.Rate; got != movieTimescale {
		t.Errorf("rate = %d, want the media timescale's %d", got, movieTimescale)
	}
}

// minfFirst moves the minf box ahead of its mdhd and hdlr siblings, which the
// format permits and no muxer does.
func minfFirst(t *testing.T, raw []byte) []byte {
	t.Helper()
	mdia := bytes.Index(raw, []byte("mdia")) - 4
	body := mdia + 8
	minf := bytes.Index(raw[body:], []byte("minf")) - 4 + body
	size := int(binary.BigEndian.Uint32(raw[minf:]))
	out := append(bytes.Clone(raw[:body]), raw[minf:minf+size]...)
	out = append(out, raw[body:minf]...)
	return append(out, raw[minf+size:]...)
}

// TestByteLinearTrackOnARescaledTimeline covers a media timescale that is not
// the sample rate, which is legal and which the uniform gate used to exclude.
//
// Two things were wrong with excluding it. The track fell to the per-frame
// flat table, which past the sample cap refuses the file outright; and the
// length cross-check then compared an exact frame count against a total the
// rescale had floored, so --strict refused a well-formed file. The timescale
// does not change the geometry, only the clock the geometry is read on.
func TestByteLinearTrackOnARescaledTimeline(t *testing.T) {
	// 8000 Hz samples timed on a 16000-tick clock: two ticks per frame.
	raw := movie{entry: soundEntry("sowt", 2, 16, movieTimescale), unitBytes: 4,
		frames: 64, timescale: 2 * movieTimescale, sttsDelta: 2}.build()
	d := open(t, raw)
	if !d.sel.st.uniform {
		t.Error("a rescaled timeline dropped the track onto the per-frame table")
	}
	tr := d.Tracks()[0]
	if tr.Fmt.Rate != movieTimescale {
		t.Errorf("rate = %d, want %d", tr.Fmt.Rate, movieTimescale)
	}
	if tr.Samples != 64 {
		t.Errorf("samples = %d, want the 64 frames the file holds", tr.Samples)
	}
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			t.Errorf("damage %q on a well-formed file", w.Msg)
		}
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err != nil {
		t.Errorf("strict refused a well-formed file: %v", err)
	}
	// The packets still tile the track exactly.
	var total int64
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != total {
			t.Fatalf("pts = %d, want %d", pkt.PTS, total)
		}
		total += pkt.Dur
	}
	if total != 64 {
		t.Errorf("packets carry %d frames, want 64", total)
	}
}

// TestDamagedChildListIsRefused pins what an unreachable box costs. walkBoxes
// stops at the first box it cannot size, so a damaged box before an 'enda'
// makes the 'enda' unreachable, and unreachable is indistinguishable from
// absent for a box whose absence is a defined answer. The track would decode
// at the wrong byte order rather than fail.
func TestDamagedChildListIsRefused(t *testing.T) {
	// A box declaring a size larger than the list that holds it, then the enda
	// that would have been read.
	broken := append([]byte{0x00, 0x00, 0x7F, 0xFF}, []byte("junk")...)
	entry := v1SoundEntry("in24", 1, 16, movieTimescale, 3, waveBox(broken, endaBox(1)))
	_, err := NewDemuxer(container.BytesSource(buildMovie(entry, 3, 64, 0)), nil)
	if err == nil {
		t.Fatal("an entry whose child list does not parse was accepted")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeMalformedInput {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeMalformedInput, err)
	}
}
