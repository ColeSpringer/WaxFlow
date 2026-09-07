package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
)

// The AudioSampleEntry header carries channelcount, samplesize, and a 16.16
// samplerate beside the codec config box. Strict fMP4 consumers cross-check
// the two: Chromium's MP4 parser refuses an init segment whose FLAC entry
// disagrees with STREAMINFO on channels or depth, or whose Opus entry
// disagrees with dOps on channels or rate, and a header that lies is
// unplayable over Media Source Extensions in every Chromium-based browser.
// These tests pin the values written and hold every entry to the consumer's
// rules (mseAccepts).

// initEntry returns the first sample entry (type and payload) of an init
// segment or a muxed file.
func initEntry(t *testing.T, file []byte) (typ string, entry []byte) {
	t.Helper()
	descend(file, []string{"moov", "trak", "mdia", "minf", "stbl", "stsd"}, func(stsd []byte) {
		_ = walkBoxes(stsd[8:], func(bt string, payload []byte) error {
			if entry == nil {
				typ, entry = bt, payload
			}
			return nil
		})
	})
	if len(entry) < 28 {
		t.Fatal("no audio sample entry in the moov")
	}
	return typ, entry
}

// entryFields reads the AudioSampleEntry header: channelcount, samplesize,
// and the raw 16.16 samplerate.
func entryFields(entry []byte) (channels, depth int, rate16 uint32) {
	return int(be16(entry[16:18])), int(be16(entry[18:20])), be32(entry[24:28])
}

// mseAccepts applies the cross-checks Chromium's MP4 parser makes on an
// audio sample entry before MSE takes the init segment (media/formats/mp4/
// box_definitions.cc AudioSampleEntry::Parse, mp4_stream_parser.cc): a
// samplesize of 8, 16, 24, or 32; for fLaC, channelcount and samplesize equal
// to STREAMINFO's (the samplerate is overridden from STREAMINFO, never
// refused); for Opus, channelcount equal to dOps's and the samplerate equal
// to dOps's InputSampleRate, which the decoder is then created at.
func mseAccepts(typ string, entry []byte) error {
	channels, depth, rate16 := entryFields(entry)
	switch depth {
	case 8, 16, 24, 32:
	default:
		return fmt.Errorf("samplesize %d: MSE accepts 8, 16, 24, or 32", depth)
	}
	children := entry[28:]
	switch typ {
	case "fLaC":
		dfla := findChild(children, "dfLa")
		if dfla == nil {
			return fmt.Errorf("fLaC entry without a dfLa box")
		}
		_, _, rest, ok := fullBox(dfla)
		if !ok || len(rest) < 4+flac.StreamInfoLen {
			return fmt.Errorf("dfLa truncated")
		}
		si, err := flac.ParseStreamInfo(rest[4 : 4+flac.StreamInfoLen])
		if err != nil {
			return err
		}
		if channels != si.Channels {
			return fmt.Errorf("FLAC AudioSampleEntry channel count %d mismatches STREAMINFO %d", channels, si.Channels)
		}
		if depth != si.Bits {
			return fmt.Errorf("FLAC AudioSampleEntry sample size %d mismatches STREAMINFO %d", depth, si.Bits)
		}
	case "Opus":
		dops := findChild(children, "dOps")
		if len(dops) < 11 {
			return fmt.Errorf("Opus entry without a full dOps box")
		}
		if channels != int(dops[1]) {
			return fmt.Errorf("Opus AudioSampleEntry channel count %d mismatches dOps %d", channels, dops[1])
		}
		if want := be32(dops[4:8]); rate16>>16 != want {
			return fmt.Errorf("Opus AudioSampleEntry sample rate %d mismatches dOps InputSampleRate %d", rate16>>16, want)
		}
	}
	return nil
}

// flacTrackDepth builds a FLAC track (STREAMINFO only) at the given rate and depth.
func flacTrackDepth(t *testing.T, rate, depth int) container.Track {
	t.Helper()
	si := flac.StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: rate, Channels: 2, Bits: depth, Samples: 4096}
	cfg, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return container.Track{Codec: codec.FLAC, CodecConfig: cfg, Fmt: si.PCMFormat(), Samples: 4096}
}

// alacTrackAt builds an ALAC track from the encoder's magic cookie.
func alacTrackAt(t *testing.T, rate, depth int) container.Track {
	t.Helper()
	f := fmtFor(rate, 2, depth)
	enc, err := alac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	return container.Track{Codec: codec.ALAC, CodecConfig: enc.CodecConfig(), Fmt: f}
}

// opusTrackWithInputRate builds an Opus track whose OpusHead reports the
// given (informational) input sample rate, as a remuxed source does.
func opusTrackWithInputRate(inputRate uint32) container.Track {
	track, _ := opusTrackFor(312, 0, 0)
	binary.LittleEndian.PutUint32(track.CodecConfig[12:], inputRate)
	return track
}

func aacTrackAt(t *testing.T, rate int) container.Track {
	t.Helper()
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	enc, err := aac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	return container.Track{Codec: codec.AACLC, CodecConfig: enc.CodecConfig(), Fmt: f, Delay: int64(enc.Delay())}
}

// TestSampleEntryDepthFollowsTheCodecConfig pins samplesize to the depth the
// codec config declares for the lossless codecs (STREAMINFO bits, the ALAC
// cookie's bitDepth), which the FLAC-in-ISOBMFF spec requires and Chromium
// enforces, and to the conventional 16 for Opus (its spec says so) and AAC.
func TestSampleEntryDepthFollowsTheCodecConfig(t *testing.T) {
	cases := []struct {
		name  string
		track container.Track
		typ   string
		depth int
	}{
		{"flac-8", flacTrackDepth(t, 44100, 8), "fLaC", 8},
		{"flac-16", flacTrackDepth(t, 44100, 16), "fLaC", 16},
		{"flac-24", flacTrackDepth(t, 44100, 24), "fLaC", 24},
		{"flac-32", flacTrackDepth(t, 44100, 32), "fLaC", 32},
		{"alac-16", alacTrackAt(t, 44100, 16), "alac", 16},
		{"alac-20", alacTrackAt(t, 44100, 20), "alac", 20},
		{"alac-24", alacTrackAt(t, 44100, 24), "alac", 24},
		{"alac-32", alacTrackAt(t, 44100, 32), "alac", 32},
		{"opus", opusTrackWithInputRate(48000), "Opus", 16},
		{"aac", aacTrackAt(t, 44100), "mp4a", 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			init, err := InitSegment(tc.track)
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			typ, entry := initEntry(t, init)
			if typ != tc.typ {
				t.Fatalf("sample entry type %q, want %q", typ, tc.typ)
			}
			channels, depth, _ := entryFields(entry)
			if depth != tc.depth {
				t.Errorf("samplesize = %d, want %d", depth, tc.depth)
			}
			if channels != tc.track.Fmt.Channels {
				t.Errorf("channelcount = %d, want %d", channels, tc.track.Fmt.Channels)
			}
		})
	}
}

// TestProgressiveALACEntryDepth covers the progressive (flat moov) muxer's
// entry, which shares the segmenter's builder and must say what the cookie
// says: the depth, not a constant.
func TestProgressiveALACEntryDepth(t *testing.T) {
	track := alacTrackAt(t, 48000, 24)
	pkts := []codec.Packet{{Data: []byte{1, 2, 3, 4}, Dur: alac.FrameSize, Sync: true}}
	file := muxProgressive(t, track, pkts, codec.Trailer{Samples: alac.FrameSize})
	typ, entry := initEntry(t, file)
	channels, depth, rate16 := entryFields(entry)
	if typ != "alac" || depth != 24 || channels != 2 || rate16 != 48000<<16 {
		t.Errorf("entry %q = %d ch, %d bits, rate %#x; want alac, 2 ch, 24 bits, %#x", typ, channels, depth, rate16, 48000<<16)
	}
}

// TestFLACSampleEntryRateFollowsTheSpec pins the fLaC entry's 16.16
// samplerate to the FLAC-in-ISOBMFF rule: the rate itself when it fits,
// otherwise the greatest regular (whole-number) halving that does (48000.0
// for 96 and 192 kHz, 44100.0 for 88.2 and 176.4), and 65535.0 for a rate
// with no such halving. Never a wrapped low half.
func TestFLACSampleEntryRateFollowsTheSpec(t *testing.T) {
	cases := []struct {
		rate int
		want uint32
	}{
		{44100, 44100 << 16},
		{48000, 48000 << 16},
		{65535, 65535 << 16},
		{88200, 44100 << 16},
		{96000, 48000 << 16},
		{176400, 44100 << 16},
		{192000, 48000 << 16},
		{65537, 65535 << 16},  // halves to a fraction: the spec's fallback
		{655350, 65535 << 16}, // 327675 is whole, 163837.5 is not
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.rate), func(t *testing.T) {
			init, err := InitSegment(flacTrackDepth(t, tc.rate, 24))
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			_, entry := initEntry(t, init)
			if _, _, rate16 := entryFields(entry); rate16 != tc.want {
				t.Errorf("samplerate field = %#x (%d.%d), want %#x", rate16, rate16>>16, rate16&0xFFFF, tc.want)
			}
		})
	}
}

// TestSampleEntryRateClampsForCookieCodecs pins the ALAC and AAC entries'
// samplerate at 65535 for a rate the field cannot hold. Their config boxes
// carry the true rate and every decoder reads it there, but a metadata
// reader that takes the field verbatim exists (waxlabel does for ALAC), and
// an obviously saturated value is the honest one where no spec prescribes a
// substitute. Halving would hand such a reader a plausible wrong rate.
func TestSampleEntryRateClampsForCookieCodecs(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, rate int) container.Track
		rate  int
		want  uint32
	}{
		{"alac-44100", func(t *testing.T, rate int) container.Track { return alacTrackAt(t, rate, 24) }, 44100, 44100 << 16},
		{"alac-96000", func(t *testing.T, rate int) container.Track { return alacTrackAt(t, rate, 24) }, 96000, 65535 << 16},
		{"alac-192000", func(t *testing.T, rate int) container.Track { return alacTrackAt(t, rate, 24) }, 192000, 65535 << 16},
		{"aac-96000", func(t *testing.T, rate int) container.Track { return aacTrackAt(t, rate) }, 96000, 65535 << 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			init, err := InitSegment(tc.build(t, tc.rate))
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			_, entry := initEntry(t, init)
			if _, _, rate16 := entryFields(entry); rate16 != tc.want {
				t.Errorf("samplerate field = %#x, want %#x", rate16, tc.want)
			}
		})
	}
}

// TestOpusSampleEntryRefusesTheWrongTrackRate keeps the sibling codecs'
// cross-check on the Opus branch: the entry and dOps say 48000 by rule, so a
// track claiming any other rate would put a timescale in mdhd the entry
// contradicts, and is refused up front.
func TestOpusSampleEntryRefusesTheWrongTrackRate(t *testing.T) {
	track := opusTrackWithInputRate(48000)
	track.Fmt.Rate = 44100
	if _, err := InitSegment(track); err == nil {
		t.Fatal("InitSegment accepted an Opus track at 44100 Hz")
	}
	if _, err := NewSegmenter(track, &SegmenterOptions{SegmentSamples: 960}); err == nil {
		t.Fatal("NewSegmenter accepted an Opus track at 44100 Hz")
	}
}

// TestOpusSampleEntryNormalizesTheInputRate pins the Opus mapping: the entry's
// samplerate is 48000 (Opus-in-ISOBMFF) and dOps's InputSampleRate is written
// as 48000 too, whatever the OpusHead said. RFC 7845 makes that field
// informational, but Chromium refuses the init segment unless the two agree,
// so a remuxed source whose header says 44100 (every CD-sourced opusenc file)
// would otherwise be unplayable over MSE. The reader reports the normalized
// value; the original does not survive the round trip.
func TestOpusSampleEntryNormalizesTheInputRate(t *testing.T) {
	track, pkts := opusTrackFor(312, 12*960-312, 12)
	binary.LittleEndian.PutUint32(track.CodecConfig[12:], 44100)
	init, media := segmentize(t, track, pkts, 4*960)

	_, entry := initEntry(t, init)
	if _, _, rate16 := entryFields(entry); rate16 != 48000<<16 {
		t.Errorf("Opus entry samplerate = %d, want 48000", rate16>>16)
	}
	dops := findChild(entry[28:], "dOps")
	if len(dops) < 11 {
		t.Fatal("no dOps box")
	}
	if got := be32(dops[4:8]); got != 48000 {
		t.Errorf("dOps InputSampleRate = %d, want 48000", got)
	}

	d, err := NewDemuxer(container.BytesSource(append(init, media...)), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	head := d.Tracks()[0].CodecConfig
	if got := binary.LittleEndian.Uint32(head[12:]); got != 48000 {
		t.Errorf("read-back OpusHead input rate = %d, want 48000", got)
	}
}

// TestSampleEntriesSatisfyMSE holds the entries Chromium parses to its
// cross-checks: FLAC at the depths and rates a library reaches, Opus from
// an encoder header and from a remuxed one, and AAC (whose only check is the
// samplesize). ALAC is absent because Chromium never parses it.
func TestSampleEntriesSatisfyMSE(t *testing.T) {
	tracks := map[string]container.Track{
		"flac-16":       flacTrackDepth(t, 44100, 16),
		"flac-24":       flacTrackDepth(t, 44100, 24),
		"flac-24-96k":   flacTrackDepth(t, 96000, 24),
		"opus-in-48000": opusTrackWithInputRate(48000),
		"opus-in-44100": opusTrackWithInputRate(44100),
		"aac":           aacTrackAt(t, 44100),
	}
	for name, track := range tracks {
		t.Run(name, func(t *testing.T) {
			init, err := InitSegment(track)
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			typ, entry := initEntry(t, init)
			if err := mseAccepts(typ, entry); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestMSERuleNamesTheUnplayableFLACDepths pins the documented limit rather
// than hiding it: 12- and 20-bit FLAC entries are correct (samplesize is
// STREAMINFO's, as the spec requires) and still fail Chromium's parser,
// which takes only 8, 16, 24, and 32. The remedy is the caller's (`bits=`),
// and the hls-js caps profile says so.
func TestMSERuleNamesTheUnplayableFLACDepths(t *testing.T) {
	for _, depth := range []int{12, 20} {
		init, err := InitSegment(flacTrackDepth(t, 44100, depth))
		if err != nil {
			t.Fatal(err)
		}
		typ, entry := initEntry(t, init)
		if _, got, _ := entryFields(entry); got != depth {
			t.Errorf("%d-bit FLAC entry samplesize = %d", depth, got)
		}
		if err := mseAccepts(typ, entry); err == nil {
			t.Errorf("%d-bit FLAC passed the MSE rule; Chromium refuses that sample size", depth)
		}
	}
}

// TestMSERuleCatchesALyingEntry keeps mseAccepts honest: a FLAC entry whose
// samplesize disagrees with STREAMINFO must fail it.
func TestMSERuleCatchesALyingEntry(t *testing.T) {
	init, err := InitSegment(flacTrackDepth(t, 44100, 24))
	if err != nil {
		t.Fatal(err)
	}
	typ, entry := initEntry(t, init)
	lying := bytes.Clone(entry)
	binary.BigEndian.PutUint16(lying[18:20], 16)
	if err := mseAccepts(typ, lying); err == nil {
		t.Error("a 16-bit samplesize over a 24-bit STREAMINFO passed the MSE rule")
	}
}
