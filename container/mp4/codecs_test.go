package mp4

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// The companded and block-coded fixtures:
//
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_mulaw -f mov g711-ulaw.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.1" \
//	    -ac 1 -c:a pcm_alaw -f mov g711-alaw.mov
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=1" \
//	    -ac 2 -c:a adpcm_ima_qt -f mov ima4.mov
//
// (ffmpeg 8.0.1, Ubuntu.) The G.711 pair are version 0 entries whose whole
// configuration is the fourcc; ima4.mov is a version 1 entry whose four
// geometry fields ffmpeg writes as 64, 0, 0, 2, and whose final stts run is
// four ticks rather than sixty-four, so the track times 44100 samples while
// its 690 blocks hold 44160.

// TestCodecFixtureTracks pins what each fixture's boxes come out as: the
// codec, the format, the length, and the source depth probe reports beside
// the decoded one.
func TestCodecFixtureTracks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		codec    codec.ID
		rate     int
		channels int
		samples  int64
		srcDepth int
		exact    bool
	}{
		{"g711-ulaw.mov", codec.MuLaw, 8000, 1, 800, 8, false},
		{"g711-alaw.mov", codec.ALaw, 8000, 1, 800, 8, false},
		{"ima4.mov", codec.IMAADPCM, 44100, 2, 44100, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := open(t, fixture(t, tc.name))
			if w := d.Warnings(); len(w) != 0 {
				t.Errorf("warnings on a clean fixture: %v", w)
			}
			tr := d.Tracks()[0]
			if tr.Codec != tc.codec {
				t.Fatalf("codec = %q, want %q", tr.Codec, tc.codec)
			}
			want := audio.Format{Rate: tc.rate, Channels: tc.channels,
				Layout: audio.DefaultLayout(tc.channels), Type: audio.Int, BitDepth: 16}
			if tr.Fmt != want {
				t.Errorf("format = %v, want %v", tr.Fmt, want)
			}
			if tr.Samples != tc.samples {
				t.Errorf("samples = %d, want %d", tr.Samples, tc.samples)
			}
			if tr.SourceBitDepth != tc.srcDepth {
				t.Errorf("source bit depth = %d, want %d", tr.SourceBitDepth, tc.srcDepth)
			}
			// A block codec's last block decodes past the table's total, so
			// the total is a truncation instruction rather than an
			// observation; a byte-linear one's is neither.
			if tr.SamplesExact != tc.exact {
				t.Errorf("SamplesExact = %v, want %v", tr.SamplesExact, tc.exact)
			}
		})
	}
}

// TestIMA4SeeksToTheStart pins the landing rule Apple's layout forces. Its
// decoder carries a predictor across blocks and a block header restates only
// the top nine bits of it, so a decode begun at a block sits up to 127 LSB
// from the linear decode and never rejoins. Landing at zero for every target
// is therefore the only exact answer, and format.Media's pre-roll is what
// makes it a seek rather than a rewind.
//
// The G.711 pair is checked alongside so the assertion is not "the demuxer
// always returns zero": those land exactly.
func TestIMA4SeeksToTheStart(t *testing.T) {
	d := open(t, fixture(t, "ima4.mov"))
	for _, target := range []int64{0, 64, 1000, 40000, 1 << 20} {
		got, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("SeekSample(%d): %v", target, err)
		}
		if got != 0 {
			t.Errorf("SeekSample(%d) landed at %d, want 0", target, got)
		}
	}
	g := open(t, fixture(t, "g711-ulaw.mov"))
	if got, err := g.SeekSample(0, 500); err != nil || got != 500 {
		t.Errorf("G.711 SeekSample(500) = %d, %v; want an exact landing", got, err)
	}
}

// msADPCMAtom builds the WAVEFORMATEX atom an "ms" ADPCM entry carries in its
// 'wave' wrapper: the fixed header, then the extra bytes the tag defines.
func msADPCMAtom(typ string, tag uint16, channels, rate, blockAlign, samplesPerBlock int, coefs [7][2]int16) []byte {
	parts := [][]byte{
		le16(tag), le16(uint16(channels)), le32(uint32(rate)),
		le32(uint32(rate * blockAlign)), le16(uint16(blockAlign)), le16(4),
	}
	extra := [][]byte{le16(uint16(samplesPerBlock))}
	if tag == 0x0002 {
		extra = append(extra, le16(7))
		for _, pair := range coefs {
			extra = append(extra, le16(uint16(pair[0])), le16(uint16(pair[1])))
		}
	}
	n := 0
	for _, e := range extra {
		n += len(e)
	}
	parts = append(parts, le16(uint16(n)))
	return makeBox(typ, append(parts, extra...)...)
}

// TestMSEntryBlockCodecs reads the two WAVE-tagged block codecs out of an
// "ms" entry, which is the only spelling that carries them in this container
// and the only one whose geometry lives outside the sample entry entirely.
func TestMSEntryBlockCodecs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		format     string
		tag        uint16
		blockAlign int
		want       adpcm.Config
		codecID    codec.ID
	}{{
		name: "ima-adpcm", format: "ms\x00\x11", tag: 0x0011, blockAlign: 20,
		want:    adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 20, SamplesPerBlock: 33},
		codecID: codec.IMAADPCM,
	}, {
		name: "ms-adpcm", format: "ms\x00\x02", tag: 0x0002, blockAlign: 20,
		want: adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 20,
			SamplesPerBlock: 28, Coefs: adpcm.DefaultCoefs},
		codecID: codec.MSADPCM,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			atom := msADPCMAtom(tc.format, tc.tag, 1, movieTimescale, tc.blockAlign,
				tc.want.SamplesPerBlock, adpcm.DefaultCoefs)
			entry := soundEntryWith(tc.format, 1, 4, movieTimescale, waveBox(atom))
			raw := movie{entry: entry, unitBytes: tc.blockAlign, frames: 4,
				sttsDelta: tc.want.SamplesPerBlock}.build()
			d := open(t, raw)
			if w := d.Warnings(); len(w) != 0 {
				t.Errorf("warnings on a well-formed entry: %v", w)
			}
			tr := d.Tracks()[0]
			if tr.Codec != tc.codecID {
				t.Fatalf("codec = %q, want %q", tr.Codec, tc.codecID)
			}
			cfg, err := adpcm.ParseConfig(tr.CodecConfig)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if cfg != tc.want {
				t.Errorf("config = %+v, want %+v", cfg, tc.want)
			}
			if want := int64(4 * tc.want.SamplesPerBlock); tr.Samples != want {
				t.Errorf("samples = %d, want %d", tr.Samples, want)
			}
			if tr.SourceBitDepth != adpcm.SourceBitDepth {
				t.Errorf("source bit depth = %d, want %d", tr.SourceBitDepth, adpcm.SourceBitDepth)
			}
			// The whole payload, in block order: buildMovie fills mdat with a
			// counting pattern, so a misread block offset shows up as a
			// mismatch rather than as plausible audio.
			if got := readAllPacketData(t, d); !bytes.Equal(got, movieMdat(raw, tc.blockAlign*4)) {
				t.Errorf("the packet walk delivered %d bytes, not the mdat payload", len(got))
			}
		})
	}
}

// TestMSEntryG711 reads the G.711 pair out of an "ms" entry, where the law is
// a format tag rather than a fourcc and the channel count comes from the
// WAVEFORMATEX.
func TestMSEntryG711(t *testing.T) {
	for _, tc := range []struct {
		format string
		tag    uint16
		want   codec.ID
	}{
		{"ms\x00\x06", 0x0006, codec.ALaw},
		{"ms\x00\x07", 0x0007, codec.MuLaw},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			atom := msWaveAtom(tc.format, tc.tag, 2, movieTimescale, 8)
			entry := soundEntryWith(tc.format, 2, 8, movieTimescale, waveBox(atom))
			d := open(t, movie{entry: entry, unitBytes: 2, frames: 64}.build())
			tr := d.Tracks()[0]
			if tr.Codec != tc.want {
				t.Fatalf("codec = %q, want %q", tr.Codec, tc.want)
			}
			if tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 || tr.Samples != 64 {
				t.Errorf("track = %v over %d samples, want 2 channels of int16 over 64", tr.Fmt, tr.Samples)
			}
			if tr.SourceBitDepth != 8 {
				t.Errorf("source bit depth = %d, want 8", tr.SourceBitDepth)
			}
		})
	}
}

// TestIMA4NotesADisagreeingGeometry pins what a version 1 entry's four
// geometry fields are worth. They restate what the fourcc already fixes, and
// ffmpeg writes three of them as zero, so a nonzero one that disagrees is a
// Note rather than a refusal: the samples are still 34 bytes and 64 frames
// apiece whatever the field says.
func TestIMA4NotesADisagreeingGeometry(t *testing.T) {
	// samplesPerPacket 32 where the format's own geometry is 64.
	entry := v1SoundEntry("ima4", 1, 4, movieTimescale, 34)
	body := entry[8:]
	copy(body[28:32], u32(32))
	d := open(t, movie{entry: entry, unitBytes: 34, frames: 4, sttsDelta: 64}.build())
	if tr := d.Tracks()[0]; tr.Codec != codec.IMAADPCM {
		t.Fatalf("codec = %q, want %q", tr.Codec, codec.IMAADPCM)
	}
	var notes []string
	for _, w := range d.Warnings() {
		if w.Kind != container.Note {
			t.Errorf("a disagreeing geometry field is damage: %v", w)
			continue
		}
		notes = append(notes, w.Msg)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "32 samples per packet") {
		t.Errorf("notes = %v, want one naming the declared geometry", notes)
	}
}

// TestCodecEntryRefusals covers the entries that are well formed as boxes and
// state something this reader will not decode. Each must name what it found.
func TestCodecEntryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry []byte
		code  waxerr.Code
		want  string
	}{{
		// The block geometry lives in the WAVEFORMATEX and nowhere else, so
		// an entry without one states no block size at all.
		name:  "ms-adpcm-without-its-waveformatex",
		entry: soundEntry("ms\x00\x02", 1, 4, movieTimescale),
		code:  waxerr.CodeMalformedInput, want: "no WAVEFORMATEX atom",
	}, {
		name:  "ima-adpcm-without-its-waveformatex",
		entry: soundEntry("ms\x00\x11", 1, 4, movieTimescale),
		code:  waxerr.CodeMalformedInput, want: "no WAVEFORMATEX atom",
	}, {
		// A wrapper holding some other tag's struct is not this entry's
		// geometry, and reading it as one would invent a block size.
		name: "ms-adpcm-with-the-wrong-struct",
		entry: soundEntryWith("ms\x00\x02", 1, 4, movieTimescale,
			waveBox(msWaveAtom("ms\x00\x02", 0x0001, 1, movieTimescale, 16))),
		code: waxerr.CodeMalformedInput, want: "format tag 0x0001",
	}, {
		// A count the format does not define: the predictor table has seven
		// pairs and a stream with another number needs a shape this build
		// does not have.
		name: "ms-adpcm-with-four-coefficient-pairs",
		entry: soundEntryWith("ms\x00\x02", 1, 4, movieTimescale,
			waveBox(makeBox("ms\x00\x02",
				le16(0x0002), le16(1), le32(movieTimescale), le32(movieTimescale*20),
				le16(20), le16(4), le16(20), le16(28), le16(4), make([]byte, 16)))),
		code: waxerr.CodeUnsupportedFormat, want: "4 predictor coefficient pairs",
	}, {
		// A block that cannot hold its own header.
		name: "ima-adpcm-block-shorter-than-its-header",
		entry: soundEntryWith("ms\x00\x11", 2, 4, movieTimescale,
			waveBox(msADPCMAtom("ms\x00\x11", 0x0011, 2, movieTimescale, 4, 1, adpcm.DefaultCoefs))),
		code: waxerr.CodeMalformedInput, want: "too small for the 8 bytes",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDemuxer(container.BytesSource(buildMovie(tc.entry, 20, 4, 0)), nil)
			if got := waxerr.CodeOf(err); got != tc.code {
				t.Fatalf("error = %v (code %v), want %v", err, got, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v does not name %q", err, tc.want)
			}
		})
	}
}

// TestBlockTrackUniformGate pins the conversion the chunk index's gate turns
// on. A sample is a BLOCK for these codecs, so its time-to-sample run says 64
// ticks where a PCM one says 1, and a gate that compared against the
// per-FRAME number accepted a movie whose stts said 1: it then read the unit's
// duration back off that run, reported a track a sixty-fourth of its length,
// marked it exact and warned about nothing.
func TestBlockTrackUniformGate(t *testing.T) {
	entry := v1SoundEntry("ima4", 1, 4, movieTimescale, 34)

	// A per-frame stts on a block codec is a timeline the chunk index cannot
	// express, so the flat table has to take it and the length stays honest.
	d := open(t, movie{entry: entry, unitBytes: 34, frames: 10, sttsDelta: 1}.build())
	if d.sel.st.uniform {
		t.Error("a per-frame stts on a 64-frame unit passed the chunk-index gate")
	}
	if got := d.Tracks()[0].Samples; got != 10 {
		// The flat path times the table as written: 10 samples of one tick.
		t.Errorf("samples = %d, want the 10 the table times", got)
	}

	// The real shape: one run of 64-tick samples, which the chunk index does
	// express, and the unit's duration comes back off it.
	d = open(t, movie{entry: entry, unitBytes: 34, frames: 10, sttsDelta: 64}.build())
	if !d.sel.st.uniform {
		t.Fatal("a 64-tick stts on a 64-frame unit did not reach the chunk index")
	}
	if d.sel.st.unitDur != 64 {
		t.Errorf("unit duration = %d, want 64", d.sel.st.unitDur)
	}
	if got := d.Tracks()[0].Samples; got != 640 {
		t.Errorf("samples = %d, want 640", got)
	}
	if len(d.sel.st.sizes) != 0 {
		t.Errorf("the chunk-indexed track kept %d per-sample sizes", len(d.sel.st.sizes))
	}
}
