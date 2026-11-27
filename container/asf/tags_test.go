package asf_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestDescriptorNamesReadAsTheLibraryKeysThem: a descriptor reads under the
// key the tag library gives it, since both readers speak for a .wma source and
// a field the two key differently arrives twice. Names compare trimmed and
// without case, and a name with no row is keyed by its text after the first
// "/". WM/Provider is the distributor, which the library keys PROVIDER, not
// ORGANIZATION, which it reads as LABEL.
func TestDescriptorNamesReadAsTheLibraryKeysThem(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav1.wma"))
	ecd := []descriptor{
		{"WM/Provider", 0, utf16("Prov")},
		{"WM/OriginalAlbumTitle", 0, utf16("OrigAlb")},
		{"WM/AuthorURL", 0, utf16("http://example.invalid")},
		{"wm/albumtitle", 0, utf16("Album")},
		{" WM/Genre ", 0, utf16("Genre")},
		{"WM/Writer", 0, utf16("Lyr")},
		{"WM/BeatsPerMinute", 3, u32(120)},
		{"MusicBrainz/Track Id", 0, utf16("mbid")},
		{"WM/IsCompilation", 2, u32(1)}, // BOOL, the 32 bits the specification gives it
		{"WM/Mood", 0, utf16("Calm")},
		{"lyrics", 0, utf16("Words")},
		{"WM/Album_Artist", 0, utf16("AA")}, // a shared alias behind the prefix
		{"Some/Deep/Name", 0, utf16("Deep")},
		{"WM/EncodingSettings", 0, utf16("Lavf62.3.100")},
		{"WM/ToolName", 0, utf16("Tool")},
		{"WM/MediaClassPrimaryID", 0, utf16("{D1607DBC-E323-4BE2-86A1-48A42A28441E}")},
		{"WM/Track", 3, u32(2)}, // zero-based, beside WM/TrackNumber
		{"WM/Picture", 0, utf16("not a picture")},
		{"WM/Künstler", 0, utf16("unread")},
		{"WM/Bad=Name", 0, utf16("unread")},
	}
	tags := open(t, withExtendedContentDescription(t, raw, ecd)).Tags()
	want := map[string][]string{
		"PROVIDER":            {"Prov"},
		"ORIGINALALBUMTITLE":  {"OrigAlb"},
		"AUTHORURL":           {"http://example.invalid"},
		"ALBUM":               {"Album"},
		"GENRE":               {"Genre"},
		"LYRICIST":            {"Lyr"},
		"BPM":                 {"120"},
		"MUSICBRAINZ_TRACKID": {"mbid"},
		"COMPILATION":         {"1"},
		"MOOD":                {"Calm"},
		"LYRICS":              {"Words"},
		"ALBUMARTIST":         {"AA"},
		"DEEP/NAME":           {"Deep"},
	}
	if !equalTags(tags, want) {
		t.Errorf("read %v, want %v", tags, want)
	}

	// A false flag is "0", and some writers spend 16 bits on it.
	for _, v := range [][]byte{u32(0), u16(0)} {
		tags := open(t, withExtendedContentDescription(t, raw, []descriptor{{"WM/IsCompilation", 2, v}})).Tags()
		if got := tags["COMPILATION"]; len(got) != 1 || got[0] != "0" {
			t.Errorf("a %d-byte false flag read as %q, want [0]", len(v), got)
		}
	}
	if tags := open(t, withExtendedContentDescription(t, raw, []descriptor{{"WM/IsCompilation", 2, []byte{1}}})).Tags(); len(tags) != 0 {
		t.Errorf("a one-byte flag read as %v, want nothing", tags)
	}
}

// TestNumberPairsSplitLikeTheLibrary: a track or disc number holding its total
// splits once every tag object is read, by the library's rule: "3/12" and
// "3 of 12", a single value only, and a blank number side drops the key.
func TestNumberPairsSplitLikeTheLibrary(t *testing.T) {
	raw := bytes.Clone(fixture(t, "sine-wmav1.wma"))
	for _, tc := range []struct {
		name string
		ecd  []descriptor
		want map[string][]string
	}{
		{"slash", []descriptor{{"WM/TrackNumber", 0, utf16("3/12")}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}},
		{"worded", []descriptor{{"WM/PartOfSet", 0, utf16("1 of 2")}},
			map[string][]string{"DISCNUMBER": {"1"}, "DISCTOTAL": {"2"}}},
		{"no number", []descriptor{{"WM/TrackNumber", 0, utf16("/12")}},
			map[string][]string{"TRACKTOTAL": {"12"}}},
		{"text stays", []descriptor{{"WM/TrackNumber", 0, utf16("Part 3 of 12")}},
			map[string][]string{"TRACKNUMBER": {"Part 3 of 12"}}},
		{"two values stay", []descriptor{{"WM/TrackNumber", 0, utf16("3/12")}, {"track", 0, utf16("4/12")}},
			map[string][]string{"TRACKNUMBER": {"3/12", "4/12"}}},
		// The library compares values as written, so a trailing space or a
		// blank value is a second value there and nothing splits.
		{"trailing space", []descriptor{{"WM/TrackNumber", 0, utf16("3/12")}, {"WM/TrackNumber", 0, utf16("3/12 ")}},
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
		{"blank beside", []descriptor{{"WM/TrackNumber", 0, utf16("  ")}, {"track", 0, utf16("3/12")}},
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
		{"empty beside", []descriptor{{"WM/TrackNumber", 0, utf16("")}, {"track", 0, utf16("3/12")}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}},
		{"same value twice", []descriptor{{"WM/TrackNumber", 0, utf16("3/12")}, {"track", 0, utf16("3/12")}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}},
		{"blank total held", []descriptor{{"WM/TrackNumber", 0, utf16("3/12")}, {"TotalTracks", 0, utf16(" ")}},
			map[string][]string{"TRACKNUMBER": {"3"}}},
		{"folded name beside", []descriptor{{"WM/TrackNumber", 0, utf16("3/12")}, {"WM/Trac\u212aNumber", 0, utf16("4")}},
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
	} {
		tags := open(t, withExtendedContentDescription(t, raw, tc.ecd)).Tags()
		if !equalTags(tags, tc.want) {
			t.Errorf("%s: read %v, want %v", tc.name, tags, tc.want)
		}
	}
}

// guidPadding is the Padding Object's GUID, spelled here so the test does not
// borrow it from the package.
var guidPadding = []byte{0x74, 0xD4, 0x06, 0x18, 0xDF, 0xCA, 0x09, 0x45, 0xA4, 0xBA, 0x9A, 0xAB, 0xCB, 0x96, 0xAA, 0xE8}

// tagObject is an Extended Content Description Object holding ds.
func tagObject(ds ...descriptor) []byte {
	body := u16(uint16(len(ds)))
	for _, d := range ds {
		name := utf16(d.name)
		body = append(body, u16(uint16(len(name)))...)
		body = append(body, name...)
		body = append(body, u16(d.valType)...)
		body = append(body, u16(uint16(len(d.value)))...)
		body = append(body, d.value...)
	}
	return object(guidExtendedContentDesc, body)
}

// threePackets is a builder holding three one-object packets.
func threePackets() *builder {
	b := newBuilder()
	b.blockAlign = 16
	b.playHNS = b.prerollMS*10_000 + 3*10_000_000
	for i := range 3 {
		b.packet(uint32(i*20), byte(i+1), 0, 4, uint32(b.prerollMS)+uint32(i*20), []byte{byte(i), 1, 2, 3})
	}
	return b
}

// TestAnOversizedHeaderStillOpens: cover art lives in the Header Object, so a
// header can run past any cap a whole-header read would need. The objects the
// demuxer parses are loaded on their own and everything else is stepped over
// unread, so a 17 MiB Padding Object costs a few bytes of reading, and the
// tags and audio behind it arrive whole, strict mode included.
func TestAnOversizedHeaderStillOpens(t *testing.T) {
	const big = 17 << 20
	b := threePackets()
	b.lead = [][]byte{object(guidPadding, make([]byte, big))}
	b.extra = [][]byte{
		object(guidPadding, make([]byte, big)),
		tagObject(descriptor{"WM/AlbumTitle", 0, utf16("Behind the padding")}),
	}
	raw := b.build()
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := asf.NewDemuxer(src, &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a %d-byte header was refused: %v", len(raw), err)
	}
	if src.Bytes > 1<<20 {
		t.Errorf("the open read %d bytes, want the padding stepped over unread", src.Bytes)
	}
	if got := d.Tags()["ALBUM"]; len(got) != 1 || got[0] != "Behind the padding" {
		t.Errorf("ALBUM = %q, want the tag behind the padding", got)
	}
	if got := collect(t, raw); len(got) != 3 {
		t.Errorf("%d objects, want the 3 the file holds", len(got))
	}
}

// TestAnOversizedTagObjectIsSkipped: a tag object past the per-object cap is
// not read, and says so with a note rather than costing the audio; strict mode
// opens the file too, since nothing in it is damaged.
func TestAnOversizedTagObjectIsSkipped(t *testing.T) {
	b := threePackets()
	ds := make([]descriptor, 0, 300)
	for range 300 {
		ds = append(ds, descriptor{"WM/Lyrics", 0, make([]byte, 60_000)})
	}
	b.extra = [][]byte{tagObject(ds...)}
	raw := b.build()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a file with an oversized tag object was refused: %v", err)
	}
	if len(d.Tags()) != 0 {
		t.Errorf("read %d tags out of an object past the cap", len(d.Tags()))
	}
	w := d.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].Msg, "Extended Content Description") || w[0].Kind == container.Damage {
		t.Errorf("warnings %v, want one note naming the skipped object", w)
	}
	if got := collect(t, raw); len(got) != 3 {
		t.Errorf("%d objects, want the 3 the file holds", len(got))
	}
}

// TestEncryptionIsFoundPastALargeExtension: the Header Extension is walked by
// its children's object headers, so a large child (a Metadata Library holding
// cover art) neither refuses the file nor hides the encryption object after it.
func TestEncryptionIsFoundPastALargeExtension(t *testing.T) {
	const big = 17 << 20
	children := object(guidPadding, make([]byte, big))
	children = append(children, object(guidAdvancedContentEncrypt, nil)...)
	ext := make([]byte, 22, 22+len(children))
	le.PutUint16(ext[16:], 6)
	le.PutUint32(ext[18:], uint32(len(children)))
	ext = append(ext, children...)
	b := threePackets()
	b.extra = [][]byte{object(guidHeaderExtension, ext)}
	_, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("err = %v, want the encryption refused", err)
	}
}

// headerWalkSeed is a small file whose header the walk steps through child by
// child: padding before and after the properties, a Header Extension holding
// padding and a Metadata Library Object, and tags behind them.
func headerWalkSeed() []byte {
	b := threePackets()
	b.lead = [][]byte{object(guidPadding, make([]byte, 64))}
	b.extra = [][]byte{
		headerExtension(
			object(guidPadding, make([]byte, 40)),
			metadataObject(true, record{"WM/Mood", 0, utf16("Calm")}, record{"WM/Picture", 1, make([]byte, 16)}),
		),
		tagObject(
			descriptor{"WM/TrackNumber", 0, utf16("3 of 12")},
			descriptor{"WM/IsCompilation", 2, u32(1)},
			descriptor{"wm/provider", 0, utf16("Prov")},
		),
	}
	return b.build()
}

// TestHeaderWalkSeedOpens keeps the fuzz seed above a file that opens, so the
// fuzzer starts from the walk's whole path.
func TestHeaderWalkSeedOpens(t *testing.T) {
	d, err := asf.NewDemuxer(container.BytesSource(headerWalkSeed()), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "COMPILATION": {"1"}, "PROVIDER": {"Prov"}, "MOOD": {"Calm"}}
	if !equalTags(d.Tags(), want) {
		t.Errorf("read %v, want %v", d.Tags(), want)
	}
}

// TestHeaderObjectCountIsBounded: a header of tiny objects is walked up to
// the object cap and no further.
func TestHeaderObjectCountIsBounded(t *testing.T) {
	b := threePackets()
	pad := object(guidPadding, nil)
	b.lead = [][]byte{bytes.Repeat(pad, 5000)}
	if _, err := asf.NewDemuxer(container.BytesSource(b.build()), &container.DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted a header past the object cap")
	}
}

func equalTags(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
	}
	return true
}
