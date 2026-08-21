package asf_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/waxerr"
)

var le = binary.LittleEndian

// The GUIDs the tests need, written out here rather than borrowed from the
// package: a test that reads its expectations out of the code under test
// cannot catch a wrong one.
var (
	guidHeader           = []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	guidData             = []byte{0x36, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	guidSimpleIndex      = []byte{0x90, 0x08, 0x00, 0x33, 0xB1, 0xE5, 0xCF, 0x11, 0x89, 0xF4, 0x00, 0xA0, 0xC9, 0x03, 0x49, 0xCB}
	guidFileProperties   = []byte{0xA1, 0xDC, 0xAB, 0x8C, 0x47, 0xA9, 0xCF, 0x11, 0x8E, 0xE4, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}
	guidStreamProperties = []byte{0x91, 0x07, 0xDC, 0xB7, 0xB7, 0xA9, 0xCF, 0x11, 0x8E, 0xE6, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}
	guidContentEncrypt   = []byte{0xFB, 0xB3, 0x11, 0x22, 0x23, 0xBD, 0xD2, 0x11, 0xB4, 0xB7, 0x00, 0xA0, 0xC9, 0x55, 0xFC, 0x6E}
	guidVideoMedia       = []byte{0xC0, 0xEF, 0x19, 0xBC, 0x4D, 0x5B, 0xCF, 0x11, 0xA8, 0xFD, 0x00, 0x80, 0x5F, 0x5C, 0x44, 0x2B}
	guidCodecList        = []byte{0x40, 0x52, 0xD1, 0x86, 0x1D, 0x31, 0xD0, 0x11, 0xA3, 0xA4, 0x00, 0xA0, 0xC9, 0x03, 0x48, 0xF6}
	guidHeaderExtension  = []byte{0xB5, 0x03, 0xBF, 0x5F, 0x2E, 0xA9, 0xCF, 0x11, 0x8E, 0xE3, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}

	guidExtendedContentDesc = []byte{0x40, 0xA4, 0xD0, 0xD2, 0x07, 0xE3, 0xD2, 0x11, 0x97, 0xF0, 0x00, 0xA0, 0xC9, 0x5E, 0xA8, 0x50}

	guidAdvancedContentEncrypt = []byte{0x33, 0x85, 0x05, 0x43, 0x81, 0x69, 0xE6, 0x49, 0x9B, 0x74, 0xAD, 0x12, 0xCB, 0x86, 0xD5, 0x8C}
)

// fixture loads a committed testdata file.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return raw
}

func open(t *testing.T, raw []byte) *asf.Demuxer {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// walk reads every packet, asserting the stream is a monotone run of sync
// points whose durations tile the timeline without gaps.
func walk(t *testing.T, d *asf.Demuxer) (objects int, first, end int64, bytes int) {
	t.Helper()
	var pkt container.Packet
	first, end = -1, -1
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return objects, first, end, bytes
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(pkt.Data) == 0 || pkt.Dur <= 0 || !pkt.Sync || pkt.Track != 0 {
			t.Fatalf("object %d: len=%d dur=%d sync=%v track=%d", objects, len(pkt.Data), pkt.Dur, pkt.Sync, pkt.Track)
		}
		if first < 0 {
			first = pkt.PTS
		} else if pkt.PTS != end {
			t.Fatalf("object %d starts at %d, previous ended at %d", objects, pkt.PTS, end)
		}
		end = pkt.PTS + pkt.Dur
		bytes += len(pkt.Data)
		objects++
	}
}

// TestDemuxTrack pins the track every committed fixture resolves to. WMA
// decodes in the float domain, the declared length comes from the play
// duration less the pre-roll, and it is never exact: ASF states milliseconds.
func TestDemuxTrack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rate       int
		channels   int
		tag        uint16
		samples    int64
		blockAlign int
	}{
		{"sine-wmav2.wma", 44100, 2, 0x0161, 90052, 743},
		{"sine-wmav1.wma", 44100, 2, 0x0160, 22491, 743},
		{"mono-8k.wma", 8000, 1, 0x0161, 4096, 256},
		{"frag.wma", 44100, 2, 0x0161, 22491, 1114},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := open(t, fixture(t, tc.name)).Tracks()[0]
			if tr.Codec != codec.WMA {
				t.Errorf("codec = %q, want %q", tr.Codec, codec.WMA)
			}
			want := audio.Format{Rate: tc.rate, Channels: tc.channels, Type: audio.Float, BitDepth: 32}
			if tr.Fmt != want {
				t.Errorf("format = %v, want %v", tr.Fmt, want)
			}
			if tr.Samples != tc.samples {
				t.Errorf("samples = %d, want %d", tr.Samples, tc.samples)
			}
			if tr.SamplesExact {
				t.Error("SamplesExact is set; ASF states milliseconds, not sample counts")
			}
			if !tr.Default || tr.Delay != 0 || tr.Padding != 0 {
				t.Errorf("track flags: default=%v delay=%d padding=%d", tr.Default, tr.Delay, tr.Padding)
			}
			// The config is the WAVEFORMATEX verbatim: the version the decoder
			// discriminates on rides in its first field.
			if len(tr.CodecConfig) < 18 {
				t.Fatalf("codec config is %d bytes", len(tr.CodecConfig))
			}
			if got := le.Uint16(tr.CodecConfig); got != tc.tag {
				t.Errorf("config format tag = %#04x, want %#04x", got, tc.tag)
			}
			if got := int(le.Uint16(tr.CodecConfig[12:])); got != tc.blockAlign {
				t.Errorf("config block align = %d, want %d", got, tc.blockAlign)
			}
			if got := int(le.Uint16(tr.CodecConfig[16:])); got != len(tr.CodecConfig)-18 {
				t.Errorf("config cbSize %d does not describe its %d trailing bytes", got, len(tr.CodecConfig)-18)
			}
		})
	}
}

// TestDemuxPackets walks each fixture. The uniform-size fixtures must emit
// whole media objects of exactly the declared block align, which is the check
// that the payload walk did not hand a fragment out as an object.
func TestDemuxPackets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		objects    int
		blockAlign int
	}{
		{"sine-wmav2.wma", 44, 743},
		{"sine-wmav1.wma", 11, 743},
		{"mono-8k.wma", 8, 256},
		{"frag.wma", 11, 1114},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := open(t, fixture(t, tc.name))
			objects, first, end, size := walk(t, d)
			if objects != tc.objects {
				t.Errorf("objects = %d, want %d", objects, tc.objects)
			}
			if first != 0 {
				t.Errorf("first object at %d, want 0", first)
			}
			if size != tc.objects*tc.blockAlign {
				t.Errorf("objects carry %d bytes, want %d", size, tc.objects*tc.blockAlign)
			}
			// The durations tile the timeline gaplessly (walk checks that) and
			// land on the declared total within one object: every position
			// here is a rounded millisecond, and the tail object has no
			// successor to measure against and takes its predecessor's length.
			tr := d.Tracks()[0]
			if off := end - tr.Samples; off > int64(tc.objects) || off < -int64(tc.objects) {
				t.Errorf("objects cover %d samples, track declares %d", end, tr.Samples)
			}
			if len(d.Warnings()) != 0 {
				t.Errorf("warnings on a clean file: %v", d.Warnings())
			}
		})
	}
}

// TestFragmentedObjectsAreReassembled proves the reassembly path ran rather
// than being incidentally satisfied: frag.wma is written with 512-byte packets
// and 1114-byte media objects, so no object fits in one packet and every one
// of them arrives in three pieces.
func TestFragmentedObjectsAreReassembled(t *testing.T) {
	raw := fixture(t, "frag.wma")
	packetLen := int64(le.Uint32(headerBody(t, raw, guidFileProperties)[68:]))
	if packetLen != 512 {
		t.Fatalf("fixture packet size is %d; the fragmentation it exists to test is gone", packetLen)
	}
	d := open(t, raw)
	blockAlign := int64(le.Uint16(d.Tracks()[0].CodecConfig[12:]))
	if blockAlign <= packetLen {
		t.Fatalf("media objects are %d bytes in %d-byte packets; nothing fragments", blockAlign, packetLen)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if int64(len(pkt.Data)) != blockAlign {
		t.Fatalf("first object is %d bytes, want the declared %d", len(pkt.Data), blockAlign)
	}
}

// TestDemuxTags reads both tag objects. ffmpeg writes the title, author, and
// copyright into each of them, so the dedupe is what keeps a single-valued
// field from arriving twice.
func TestDemuxTags(t *testing.T) {
	d := open(t, fixture(t, "tagged.wma"))
	want := map[string][]string{
		"TITLE":         {"Tïtle"},
		"ARTIST":        {"Artist"},
		"ALBUM":         {"Album"},
		"ALBUMARTIST":   {"AlbumArtist"},
		"COMPOSER":      {"Composer"},
		"GENRE":         {"Genre"},
		"COMMENT":       {"Comment"},
		"COPYRIGHT":     {"Copyright"},
		"RECORDINGDATE": {"2026"},
		"TRACKNUMBER":   {"3"},
		"TRACKTOTAL":    {"12"},
		"DISCNUMBER":    {"1"},
		"DISCTOTAL":     {"2"},
	}
	got := d.Tags()
	for key, vals := range want {
		if len(got[key]) != len(vals) || (len(vals) > 0 && got[key][0] != vals[0]) {
			t.Errorf("tag %s = %q, want %q", key, got[key], vals)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected tag %s = %q", key, got[key])
		}
	}
}

// TestUntaggedFileHasNoTags keeps the tag map empty rather than populated with
// the encoder's own settings string, which is the one descriptor ffmpeg writes
// unasked.
func TestUntaggedFileHasNoTags(t *testing.T) {
	if tags := open(t, fixture(t, "sine-wmav1.wma")).Tags(); len(tags) != 0 {
		t.Errorf("untagged file carries %v", tags)
	}
}

// TestSeekLandsAtOrBefore is the seek invariant: every landing is at or before
// the target, reads resume exactly there, and the run-up is real -- a landing
// equal to the target would mean the decoder got no bit-reservoir history.
func TestSeekLandsAtOrBefore(t *testing.T) {
	for _, name := range []string{"sine-wmav2.wma", "frag.wma", "mono-8k.wma"} {
		t.Run(name, func(t *testing.T) {
			d := open(t, fixture(t, name))
			total := d.Tracks()[0].Samples
			for _, frac := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.99, 1.5} {
				target := int64(float64(total) * frac)
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				if landed > target {
					t.Errorf("seek to %d landed at %d, past the target", target, landed)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err != nil {
					if err == io.EOF {
						continue
					}
					t.Fatalf("reading after a seek to %d: %v", target, err)
				}
				if pkt.PTS != landed {
					t.Errorf("seek to %d reported %d but the next packet is at %d", target, landed, pkt.PTS)
				}
			}
		})
	}
}

// TestSeekGivesRunUp pins the pre-roll the landing owes the decoder: a target
// inside the stream must land at least one media object earlier, since a WMA
// super-frame carries reservoir state out of the one before it.
func TestSeekGivesRunUp(t *testing.T) {
	for _, name := range []string{"sine-wmav2.wma", "frag.wma"} {
		t.Run(name, func(t *testing.T) {
			d := open(t, fixture(t, name))
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
			objectDur := pkt.Dur
			target := d.Tracks()[0].Samples / 2
			landed, err := d.SeekSample(0, target)
			if err != nil {
				t.Fatal(err)
			}
			if target-landed < objectDur {
				t.Errorf("seek to %d landed at %d, %d samples of run-up for a %d-sample object",
					target, landed, target-landed, objectDur)
			}
		})
	}
}

// TestSeekRestartsTheStream checks that a seek discards the lookahead and any
// half-built object: reading straight through from a mid-file landing must
// give the same packets as reading from the top and skipping ahead.
func TestSeekRestartsTheStream(t *testing.T) {
	raw := fixture(t, "frag.wma")
	linear := map[int64]int{}
	d := open(t, raw)
	var pkt container.Packet
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		linear[pkt.PTS] = len(pkt.Data)
	}
	d = open(t, raw)
	landed, err := d.SeekSample(0, d.Tracks()[0].Samples/2)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		want, ok := linear[pkt.PTS]
		if !ok {
			t.Fatalf("packet at %d after a seek to %d does not appear in a linear read", pkt.PTS, landed)
		}
		if want != len(pkt.Data) {
			t.Fatalf("packet at %d is %d bytes after a seek, %d bytes linearly", pkt.PTS, len(pkt.Data), want)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no packets after a mid-file seek")
	}
}

// TestSeekUsesTheSimpleIndex proves the index path runs rather than being
// shadowed by the bisection fallback. ffmpeg writes no Simple Index Object for
// an audio-only file, so the test appends one, and a deliberately blunt one:
// every entry names packet 0, which bisection would never produce for a
// mid-file target.
func TestSeekUsesTheSimpleIndex(t *testing.T) {
	raw := fixture(t, "sine-wmav2.wma")
	plain := open(t, raw)
	mid := plain.Tracks()[0].Samples / 2
	without, err := plain.SeekSample(0, mid)
	if err != nil {
		t.Fatal(err)
	}
	if without == 0 {
		t.Fatal("the fallback already lands at zero; the index test would prove nothing")
	}

	indexed := open(t, appendSimpleIndex(raw, 32, 1_000_0000))
	with, err := indexed.SeekSample(0, mid)
	if err != nil {
		t.Fatal(err)
	}
	if with != 0 {
		t.Errorf("seek to %d landed at %d with an index naming packet 0; the index was not read", mid, with)
	}
}

// TestSimpleIndexIsReadInAbsoluteTime pins the time domain, which the blunt
// index above cannot see: an entry names an ABSOLUTE presentation time, which
// counts from the start of the pre-roll, while a seek target is an output
// position. Looking one up without adding the pre-roll back reads an entry
// preroll/interval too early, and for a file shorter than its own pre-roll
// that is entry zero every time, so the index would be strictly worse than
// the bisection it exists to save.
//
// The index here is zeros except for the one entry a correct lookup reaches:
// at a 100 ms interval and the fixture's 3100 ms pre-roll, output time 900 ms
// is absolute 4000 ms, which is entry 40. A lookup in the wrong domain reads
// entry 9, finds the zero there, and lands at the start of the file.
func TestSimpleIndexIsReadInAbsoluteTime(t *testing.T) {
	const intervalMS, prerollMS, outMS = 100, 3100, 900
	raw := fixture(t, "sine-wmav2.wma")
	rate := int64(open(t, raw).Tracks()[0].Fmt.Rate)

	entries := make([]uint32, 64)
	entries[(outMS+prerollMS)/intervalMS] = 3 // a packet well before that time
	d := open(t, appendSimpleIndexEntries(raw, entries, intervalMS*1_0000))

	target := outMS * rate / 1000
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatal(err)
	}
	if landed > target {
		t.Fatalf("seek to %d landed at %d, past the target", target, landed)
	}
	if landed == 0 {
		t.Errorf("seek to %d (%d ms) rewound to the start: the entry it read was the one %d ms too early",
			target, outMS, prerollMS)
	}
}

// TestSeekIgnoresAnIndexPointingPastTheTarget is the hostile half of the index
// contract, and a fuzz finding. SeekSample's landing check would catch a
// forward-pointing entry anyway, by restarting at the top, so what this pins
// is the rejection in packetFor: a file with a lying index must seek exactly
// as well as the same file with no index at all.
func TestSeekIgnoresAnIndexPointingPastTheTarget(t *testing.T) {
	raw := fixture(t, "sine-wmav2.wma")
	plain := open(t, raw)
	// Every entry names a packet well into the file, so any target near the
	// front is one the index sends the seek past.
	lying := open(t, appendSimpleIndexAt(raw, 32, 1_000_0000, 8))
	for _, target := range []int64{4000, 20000, 44100} {
		want, err := plain.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		got, err := lying.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		if got > target {
			t.Errorf("seek to %d landed at %d; the index was believed", target, got)
		}
		if got != want {
			t.Errorf("seek to %d landed at %d with a lying index, %d without: the fallback is not bisection", target, got, want)
		}
	}
}

// TestEncryptionIsRefusedAheadOfTheCodec pins the ordering: a DRM-protected
// file is unreadable whatever is inside it, so naming a codec that is not the
// problem would send a reader looking for a build with wider codec support.
func TestEncryptionIsRefusedAheadOfTheCodec(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav2.wma"))
	copy(headerObject(t, raw, guidCodecList), guidContentEncrypt)
	le.PutUint16(headerBody(t, raw, guidStreamProperties)[54:], 0x0162) // WMA Pro
	_, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err == nil {
		t.Fatal("accepted an encrypted file")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q names the codec, not the encryption", err)
	}
}

// TestRefusalsNameWhatTheyFound is why the codec and stream-type tables exist:
// a file this build cannot read must say which format it is, not "unsupported".
func TestRefusalsNameWhatTheyFound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(raw []byte)
		want  string
	}{
		{"wma-pro", setFormatTag(0x0162), "Windows Media Audio Pro"},
		{"wma-pro-spdif", setFormatTag(0x0164), "Windows Media Audio Pro"},
		{"wma-lossless", setFormatTag(0x0163), "Windows Media Audio Lossless"},
		{"wma-voice", setFormatTag(0x000A), "Windows Media Audio Voice"},
		{"wma-voice-9", setFormatTag(0x000B), "Windows Media Audio Voice"},
		{"unknown-codec", setFormatTag(0x0055), "audio format 0x0055"},
		{"video-only", func(raw []byte) {
			copy(headerBody(nil, raw, guidStreamProperties), guidVideoMedia)
		}, "no audio stream (the file carries video)"},
		{"encrypted", func(raw []byte) {
			// Retype the Codec List Object in place: only the GUID changes,
			// so every other size in the header still adds up.
			copy(headerObject(nil, raw, guidCodecList), guidContentEncrypt)
		}, "encrypted"},
		{"variable-packets", func(raw []byte) {
			le.PutUint32(headerBody(nil, raw, guidFileProperties)[68:], 1024)
		}, "variable-size data packets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.Clone(fixture(t, "sine-wmav2.wma"))
			tc.patch(raw)
			_, err := asf.NewDemuxer(container.BytesSource(raw), nil)
			if err == nil {
				t.Fatal("accepted a file this build cannot read")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("error code = %q, want %q", code, waxerr.CodeUnsupportedFormat)
			}
		})
	}
}

// setFormatTag patches the selected stream's wFormatTag, the first field of
// the WAVEFORMATEX in its Stream Properties Object.
func setFormatTag(tag uint16) func(raw []byte) {
	return func(raw []byte) {
		le.PutUint16(headerBody(nil, raw, guidStreamProperties)[54:], tag)
	}
}

// TestAdvancedContentEncryptionIsFound covers the encryption object that does
// not sit at the top level. It is a Header Extension child by spec, and the
// extension is an opaque leaf here otherwise, so a protected file would open
// and hand a decoder ciphertext as super-frames.
func TestAdvancedContentEncryptionIsFound(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav2.wma"))
	extAt := bytes.Index(raw, guidHeaderExtension)
	if extAt < 0 {
		t.Fatal("the fixture carries no Header Extension Object")
	}
	child := make([]byte, 24)
	copy(child, guidAdvancedContentEncrypt)
	le.PutUint64(child[16:], 24)

	// Splice the child in at the end of the extension, then grow the three
	// lengths that cover it: the header object, the extension object, and the
	// extension's own child-data length.
	extLen := int(le.Uint64(raw[extAt+16:]))
	at := extAt + extLen
	grown := make([]byte, 0, len(raw)+len(child))
	grown = append(grown, raw[:at]...)
	grown = append(grown, child...)
	grown = append(grown, raw[at:]...)
	le.PutUint64(grown[16:], le.Uint64(grown[16:])+uint64(len(child)))
	le.PutUint64(grown[extAt+16:], uint64(extLen+len(child)))
	le.PutUint32(grown[extAt+24+18:], le.Uint32(grown[extAt+24+18:])+uint32(len(child)))

	_, err := asf.NewDemuxer(container.BytesSource(grown), nil)
	if err == nil {
		t.Fatal("accepted a file whose Header Extension carries an encryption object")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q does not name the encryption", err)
	}
}

// TestErrorsCarryThePublicContainerName pins the prefix, which is user-facing
// text: the driver row this package lands under is named wma, not asf.
func TestErrorsCarryThePublicContainerName(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"junk", bytes.Repeat([]byte{'z'}, 4096)},
		{"empty", nil},
		{"header-only", fixture(t, "sine-wmav2.wma")[:24]},
		{"no-data-object", fixture(t, "sine-wmav2.wma")[:492]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := asf.NewDemuxer(container.BytesSource(tc.raw), nil)
			if err == nil {
				t.Fatal("accepted a file that is not one")
			}
			if !strings.HasPrefix(err.Error(), "wma: ") {
				t.Errorf("error %q does not carry the public container name", err)
			}
		})
	}
}

// TestSeekRejectsBadArguments covers the two arguments a caller can get wrong.
func TestSeekRejectsBadArguments(t *testing.T) {
	d := open(t, fixture(t, "sine-wmav2.wma"))
	for _, tc := range []struct {
		name   string
		track  int
		sample int64
	}{
		{"no such track", 1, 0},
		{"negative target", 0, -1},
	} {
		if _, err := d.SeekSample(tc.track, tc.sample); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
			t.Errorf("%s: err = %v, want an invalid-request code", tc.name, err)
		}
	}
}

// TestSeekTargetsPastEveryPlausibleLengthAreSafe covers the ceiling the engine
// probes an unknown length with. It reaches the demuxer verbatim, and the
// conversions on the way to a packet number multiply it twice.
func TestSeekTargetsPastEveryPlausibleLengthAreSafe(t *testing.T) {
	for _, name := range []string{"sine-wmav2.wma", "frag.wma", "mono-8k.wma"} {
		for _, withIndex := range []bool{false, true} {
			raw := fixture(t, name)
			if withIndex {
				raw = appendSimpleIndex(raw, 32, 1_000_0000)
			}
			d := open(t, raw)
			for _, target := range []int64{1 << 40, 1 << 58, 1<<61 - 1, math.MaxInt64} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("%s (index %v): seek to %d: %v", name, withIndex, target, err)
				}
				if landed > target || landed < 0 {
					t.Errorf("%s (index %v): seek to %d landed at %d", name, withIndex, target, landed)
				}
			}
		}
	}
}

// TestTruncatedFileIsToleratedThenRefused is the strict-mode contract: a file
// cut short of its declared packet count reads as far as it goes with a
// warning, and strict turns that warning into a refusal. The Data Object's own
// packet count is the cross-check that makes the damage visible at all.
func TestTruncatedFileIsToleratedThenRefused(t *testing.T) {
	full := fixture(t, "sine-wmav2.wma")
	cut := full[:len(full)/2]

	d := open(t, cut)
	objects, _, _, _ := walk(t, d)
	if objects == 0 {
		t.Fatal("no packets from a half file")
	}
	if len(d.Warnings()) == 0 {
		t.Error("a truncated file probed clean; the packet-count cross-check is not wired")
	}
	found := false
	for _, w := range d.Warnings() {
		if strings.Contains(w.Msg, "packets") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings do not mention the short packet run: %v", d.Warnings())
	}

	if _, err := asf.NewDemuxer(container.BytesSource(cut), &asf.DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted a truncated file")
	}
}

// TestStrictAcceptsACleanFile is the other side of it: strict exists to reject
// real-world mess, so a well-formed file must sail through unchanged.
func TestStrictAcceptsACleanFile(t *testing.T) {
	for _, name := range []string{"sine-wmav2.wma", "sine-wmav1.wma", "mono-8k.wma", "frag.wma", "tagged.wma"} {
		d, err := asf.NewDemuxer(container.BytesSource(fixture(t, name)), &asf.DemuxerOptions{Strict: true})
		if err != nil {
			t.Fatalf("%s: strict mode refused a clean file: %v", name, err)
		}
		var pkt container.Packet
		for {
			if err := d.ReadPacket(&pkt); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Errorf("%s: strict read: %v", name, err)
				}
				break
			}
		}
	}
}

// TestTrailingBytesAreNotDamage covers what taggers bolt onto the end of a
// .wma. None of it is audio, a file with no index wants nothing from back
// there, and strict mode exists to reject mess in the stream, not baggage
// behind it.
func TestTrailingBytesAreNotDamage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tail       []byte
		recognized bool
	}{
		{"id3v1", append([]byte("TAG"), make([]byte, 125)...), true},
		{"nul padding", make([]byte, 64), false},
		{"garbage", bytes.Repeat([]byte{0xAB}, 100), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := append(bytes.Clone(fixture(t, "sine-wmav2.wma")), tc.tail...)
			d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatalf("strict mode refused a file with %s behind the audio: %v", tc.name, err)
			}
			if objects, _, _, _ := walk(t, d); objects != 44 {
				t.Errorf("%d objects, want the file's 44", objects)
			}
			// A tag is recognized and peeled, so it leaves nothing to say.
			// Bytes nobody can name end the index scan with a note.
			if tc.recognized && len(d.Warnings()) != 0 {
				t.Errorf("an ordinary %s tag was reported as an oddity: %v", tc.name, d.Warnings())
			}
		})
	}
}

// TestHeaderSizeLies covers the sizes a hostile file gets to choose. None of
// them may panic, hang, or produce a track; a refusal is the whole contract.
func TestHeaderSizeLies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(raw []byte)
	}{
		{"header runs past the file", func(raw []byte) { le.PutUint64(raw[16:], 1<<40) }},
		{"header is zero bytes", func(raw []byte) { le.PutUint64(raw[16:], 0) }},
		{"header is one byte", func(raw []byte) { le.PutUint64(raw[16:], 1) }},
		{"child object claims the world", func(raw []byte) { le.PutUint64(raw[46:], 1<<62) }},
		{"child object is zero bytes", func(raw []byte) { le.PutUint64(raw[46:], 0) }},
		{"packet size is zero", func(raw []byte) {
			le.PutUint32(headerBody(nil, raw, guidFileProperties)[68:], 0)
			le.PutUint32(headerBody(nil, raw, guidFileProperties)[72:], 0)
		}},
		{"packet size is enormous", func(raw []byte) {
			le.PutUint32(headerBody(nil, raw, guidFileProperties)[68:], 1<<31)
			le.PutUint32(headerBody(nil, raw, guidFileProperties)[72:], 1<<31)
		}},
		{"play duration is enormous", func(raw []byte) {
			le.PutUint64(headerBody(nil, raw, guidFileProperties)[40:], 1<<63-1)
		}},
		{"pre-roll is enormous", func(raw []byte) {
			le.PutUint64(headerBody(nil, raw, guidFileProperties)[56:], 1<<63-1)
		}},
		{"sample rate is enormous", func(raw []byte) {
			le.PutUint32(headerBody(nil, raw, guidStreamProperties)[54+4:], 1<<30)
			le.PutUint64(headerBody(nil, raw, guidFileProperties)[40:], 1<<62)
		}},
		{"type-specific data claims the world", func(raw []byte) {
			le.PutUint32(headerBody(nil, raw, guidStreamProperties)[40:], 1<<31)
		}},
		{"data object claims the world", func(raw []byte) { le.PutUint64(raw[492+16:], 1<<62) }},
		{"data object is zero bytes", func(raw []byte) { le.PutUint64(raw[492+16:], 0) }},
		{"data object declares a billion packets", func(raw []byte) { le.PutUint64(raw[492+40:], 1<<40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.Clone(fixture(t, "sine-wmav2.wma"))
			tc.patch(raw)
			d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				return
			}
			if err := d.Tracks()[0].Fmt.Valid(); err != nil {
				t.Fatalf("accepted a track with an invalid format: %v", err)
			}
			// A declared length is advisory here, but it is read as a count:
			// only -1 means unknown, so a wrapped total is a lie no consumer
			// can spot.
			if s := d.Tracks()[0].Samples; s < -1 {
				t.Fatalf("accepted a track declaring %d samples", s)
			}
			var pkt container.Packet
			for i := 0; i < len(raw); i++ {
				if err := d.ReadPacket(&pkt); err != nil {
					return
				}
			}
			t.Fatalf("more packets than the file has bytes")
		})
	}
}

// headerObject returns the object with the given GUID inside the Header
// Object, as a slice of raw so a patch writes through. t may be nil, for use
// inside a patch closure.
func headerObject(t *testing.T, raw []byte, want []byte) []byte {
	for off := int64(30); off+24 <= int64(len(raw)); {
		size := int64(le.Uint64(raw[off+16:]))
		if size < 24 || off+size > int64(len(raw)) {
			break
		}
		if bytes.Equal(raw[off:off+16], want) {
			return raw[off : off+size]
		}
		off += size
	}
	if t != nil {
		t.Fatalf("no object %x in the header", want)
	}
	panic("no such header object")
}

// headerBody is headerObject past its 24-byte GUID and size.
func headerBody(t *testing.T, raw []byte, want []byte) []byte {
	return headerObject(t, raw, want)[24:]
}

// appendSimpleIndex appends an index of count entries, every one naming packet
// 0. ffmpeg writes none for an audio-only file, so the index path needs one
// built by hand.
func appendSimpleIndex(raw []byte, count int, intervalHNS uint64) []byte {
	return appendSimpleIndexAt(raw, count, intervalHNS, 0)
}

// appendSimpleIndexAt is appendSimpleIndex with the packet every entry names
// spelled out, for the index that points somewhere it must not be believed.
func appendSimpleIndexAt(raw []byte, count int, intervalHNS uint64, target uint32) []byte {
	entries := make([]uint32, count)
	for i := range entries {
		entries[i] = target
	}
	return appendSimpleIndexEntries(raw, entries, intervalHNS)
}

// appendSimpleIndexEntries appends an index whose entries are spelled out, for
// the truthful table the time-domain check needs.
func appendSimpleIndexEntries(raw []byte, entries []uint32, intervalHNS uint64) []byte {
	body := make([]byte, 32+len(entries)*6)
	le.PutUint64(body[16:], intervalHNS)
	le.PutUint32(body[24:], 1)
	le.PutUint32(body[28:], uint32(len(entries)))
	for i, p := range entries {
		le.PutUint32(body[32+i*6:], p)
		le.PutUint16(body[32+i*6+4:], 1)
	}
	obj := make([]byte, 24, 24+len(body))
	copy(obj, guidSimpleIndex)
	le.PutUint64(obj[16:], uint64(24+len(body)))
	return append(bytes.Clone(raw), append(obj, body...)...)
}

// TestDataObjectGUIDIsRequired keeps the Data Object from being found by
// position alone: the header size names where it starts, and what sits there
// has to be the object it claims.
func TestDataObjectGUIDIsRequired(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav2.wma"))
	if !bytes.Equal(raw[492:492+16], guidData) {
		t.Fatalf("the fixture's Data Object is not at 492; the offsets these tests patch have moved")
	}
	raw[492] ^= 0xFF
	if _, err := asf.NewDemuxer(container.BytesSource(raw), nil); err == nil {
		t.Error("accepted a file with no Data Object where the header says one is")
	}
}

// TestSeekIntoAFragmentedObjectIsNotDamage is the other half of landing on a
// packet boundary: a seek into a file whose media objects span packets starts
// mid-object, and the fragments before the first fresh start are stepped over
// rather than reported. Without that, every seek in such a file logs a warning
// and strict mode fails one outright.
func TestSeekIntoAFragmentedObjectIsNotDamage(t *testing.T) {
	d, err := asf.NewDemuxer(container.BytesSource(fixture(t, "frag.wma")), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	total := d.Tracks()[0].Samples
	for _, frac := range []float64{0.15, 0.35, 0.55, 0.8} {
		target := int64(float64(total) * frac)
		if _, err := d.SeekSample(0, target); err != nil {
			t.Fatalf("strict seek to %d: %v", target, err)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("strict read after a seek to %d: %v", target, err)
		}
	}
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("seeking a clean file recorded %v", w)
	}
}

// TestNumericDescriptorsAreRead covers the value types Windows Media writes
// where ffmpeg writes strings: WM/TrackNumber and WM/Year come back as a
// DWORD from Windows Media Player, and the whole descriptor is dropped if the
// reader only understands the Unicode form.
func TestNumericDescriptorsAreRead(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav1.wma"))
	ecd := []descriptor{
		{"WM/TrackNumber", 3, u32(7)},                // DWORD
		{"WM/Year", 3, u32(1998)},                    // DWORD
		{"WM/PartOfSet", 5, u16(2)},                  // WORD
		{"WM/AlbumTitle", 0, utf16("Numeric Album")}, // the ordinary form
		{"WM/Genre", 4, u64(42)},                     // QWORD
		{"WM/Picture", 1, []byte{0xFF, 0xD8, 0xFF}},  // BYTE array: not a tag
		{"WM/IsVBR", 2, []byte{1, 0, 0, 0}},          // BOOL: not a tag
	}
	tags := open(t, withExtendedContentDescription(t, raw, ecd)).Tags()
	for key, want := range map[string]string{
		"TRACKNUMBER":   "7",
		"RECORDINGDATE": "1998",
		"DISCNUMBER":    "2",
		"ALBUM":         "Numeric Album",
		"GENRE":         "42",
	} {
		if got := tags[key]; len(got) != 1 || got[0] != want {
			t.Errorf("tag %s = %q, want [%q]", key, got, want)
		}
	}
	// A picture is not what Tagger serves, and a flag is not a tag at all.
	for _, key := range []string{"PICTURE", "ISVBR", "WM/PICTURE"} {
		if got := tags[key]; len(got) != 0 {
			t.Errorf("tag %s = %q, want nothing", key, got)
		}
	}
}

// descriptor is one Extended Content Description entry to write.
type descriptor struct {
	name    string
	valType uint16
	value   []byte
}

func u16(v uint16) []byte { b := make([]byte, 2); le.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); le.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); le.PutUint64(b, v); return b }

func utf16(s string) []byte {
	b := make([]byte, 0, len(s)*2+2)
	for _, r := range s {
		b = append(b, byte(r), byte(r>>8))
	}
	return append(b, 0, 0)
}

// withExtendedContentDescription replaces the file's Codec List Object with an
// Extended Content Description Object holding the given descriptors, so every
// other size in the header still adds up around it.
func withExtendedContentDescription(t *testing.T, raw []byte, ds []descriptor) []byte {
	t.Helper()
	body := make([]byte, 2)
	le.PutUint16(body, uint16(len(ds)))
	for _, d := range ds {
		name := utf16(d.name)
		body = append(body, u16(uint16(len(name)))...)
		body = append(body, name...)
		body = append(body, u16(d.valType)...)
		body = append(body, u16(uint16(len(d.value)))...)
		body = append(body, d.value...)
	}
	obj := make([]byte, 24, 24+len(body))
	copy(obj, guidExtendedContentDesc)
	le.PutUint64(obj[16:], uint64(24+len(body)))
	obj = append(obj, body...)

	old := headerObject(t, raw, guidCodecList)
	at := bytes.Index(raw, guidCodecList)
	out := make([]byte, 0, len(raw)-len(old)+len(obj))
	out = append(out, raw[:at]...)
	out = append(out, obj...)
	out = append(out, raw[at+len(old):]...)
	le.PutUint64(out[16:], le.Uint64(out[16:])-uint64(len(old))+uint64(len(obj)))
	return out
}

// TestAnUnreadableDurationIsUnknownNotEmpty separates the two answers a
// declared length can carry. Consumers read anything at or above zero as
// authoritative and only -1 as unknown, so a play duration that cannot be
// converted has to come back as unknown: reporting zero would be a claim that
// a file full of packets holds no audio.
func TestAnUnreadableDurationIsUnknownNotEmpty(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav2.wma"))
	// A rate and a duration whose product runs past int64.
	le.PutUint32(headerBody(t, raw, guidStreamProperties)[54+4:], 1<<30)
	le.PutUint64(headerBody(t, raw, guidFileProperties)[40:], 1<<62)

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Skipf("the patched file no longer opens: %v", err)
	}
	if got := d.Tracks()[0].Samples; got != -1 {
		t.Errorf("Samples = %d, want -1: a length that cannot be computed is unknown", got)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("the file still holds packets, but reading one gave %v", err)
	}
}
