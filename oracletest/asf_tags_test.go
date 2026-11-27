package oracletest

// The WMA tag agreement cell. container/asf reads a .wma source's
// descriptors for the probe and any embedder with no mapper, and the tag
// library behind cli/label reads them on the mapper-wired route, where its
// keys win per key and the demuxer's fill in the rest. A descriptor the two
// key differently arrives twice, and under a key that is an alias (as
// ORGANIZATION is of LABEL) it lands in another field too. So every native
// key has to be one the library reads, with the same value.
// The native reader leaves out only the encoder stamp the library reads as
// ENCODER, which ffmpeg writes on every file.

import (
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxlabel/tag"
)

func TestASFTagsAgreeAcrossReaders(t *testing.T) {
	testutil.FFmpegEncoder(t, "wmav2")
	path := filepath.Join(t.TempDir(), "tagged.wma")
	var meta_ []string
	for _, kv := range []string{
		"title=T", "artist=A", "album=Al", "album_artist=AA", "composer=C",
		"genre=G", "date=2026", "track=3/12", "disc=1 of 2", "copyright=Cr",
		"comment=Cm", "publisher=Pub", "lyrics=Ly",
		"WM/Provider=Prov", "WM/OriginalAlbumTitle=OrigAlb", "WM/AuthorURL=http://example.invalid",
		"WM/Writer=Lyr", "WM/Conductor=Cond", "WM/BeatsPerMinute=120", "WM/ISRC=USXXX0000001",
		"WM/SetSubtitle=Sub", "WM/CatalogNo=CAT1", "WM/Barcode=0123", "WM/ArtistSortOrder=Sort",
		"WM/ContentGroupDescription=Grp", "WM/EncodedBy=Enc", "WM/Mood=Calm",
		"MusicBrainz/Track Id=mbid", "REPLAYGAIN_TRACK_GAIN=-1.00 dB",
	} {
		meta_ = append(meta_, "-metadata", kv)
	}
	testutil.FFmpegGenerate(t, path, 8000, 1, "wmav2", meta_...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	native := d.Tags()
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wma", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(native) < 20 {
		t.Fatalf("the demuxer read %d keys, want the cell to cover most of what ffmpeg wrote: %v", len(native), native)
	}
	for _, k := range slices.Sorted(maps.Keys(native)) {
		if got, ok := info.Tags[k]; !ok || !slices.Equal(got, native[k]) {
			t.Errorf("%s: native %q, library %q", k, native[k], got)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(info.Tags)) {
		if _, ok := native[k]; !ok && k != "ENCODER" {
			t.Errorf("%s: the library read %q, the demuxer nothing", k, info.Tags[k])
		}
	}
}

// asfRowNames are the names the library's ASF table lists
// (internal/mapping/asf.go asfNames), which no exported API does. The sweep
// also reads the table itself out of the library's source (libraryTable).
var asfRowNames = []string{
	"Title", "Author", "Copyright", "Description", "Rating",
	"WM/AlbumTitle", "WM/AlbumArtist", "WM/Composer", "WM/Writer", "WM/Conductor",
	"WM/Producer", "WM/Engineer", "WM/Mixer", "WM/ModifiedBy", "WM/Genre", "WM/Year",
	"WM/OriginalReleaseYear", "WM/OriginalReleaseTime", "WM/TrackNumber", "WM/PartOfSet",
	"WM/SetSubtitle", "WM/Publisher", "WM/ISRC", "WM/Barcode", "WM/CatalogNo", "WM/Media",
	"WM/Lyrics", "WM/ContentGroupDescription", "WM/EncodedBy", "WM/ToolName",
	"WM/EncodingSettings", "WM/BeatsPerMinute",
	"WM/TitleSortOrder", "WM/ArtistSortOrder", "WM/AlbumSortOrder",
	"WM/AlbumArtistSortOrder", "WM/ComposerSortOrder", "WM/IsCompilation",
	"WM/MediaClassPrimaryID", "WM/MediaClassSecondaryID", "WM/WMCollectionGroupID",
	"WM/WMCollectionID", "WM/WMContentID", "WM/UniqueFileIdentifier", "WM/Provider Style",
	"WM/EncodingTime", "WM/MCDI", "WM/Track", "AspectRatioX", "AspectRatioY", "IsVBR",
	"WMFSDKVersion", "WMFSDKNeeded", "DeviceConformanceTemplate", "Buffer Average", "VBR Peak",
	"MusicBrainz/Album Id", "MusicBrainz/Release Group Id", "MusicBrainz/Track Id",
	"MusicBrainz/Release Track Id", "MusicBrainz/Work Id", "MusicBrainz/Disc Id",
	"MusicBrainz/Artist Id", "MusicBrainz/Album Artist Id", "MusicBrainz/Album Status",
	"MusicBrainz/Album Type", "MusicBrainz/Album Release Country", "Acoustid/Id",
	"Acoustid/Fingerprint", "REPLAYGAIN_TRACK_GAIN", "REPLAYGAIN_TRACK_PEAK",
	"REPLAYGAIN_ALBUM_GAIN", "REPLAYGAIN_ALBUM_PEAK",
	// Names the library keys by their text after the first "/".
	"WM/Provider", "WM/OriginalAlbumTitle", "WM/AuthorURL", "WM/Mood", "WM/InitialKey",
	"WM/MediaFoundationVersion", "WM/WMADRCPeakReference", "Some/Deep/Name",
	" WM/Genre ", "WM/ Genre", "WM/", "/", "WM/Bad=Name", "WM/tilde~",
}

// asfNonASCIIFolds are names the library folds through Unicode case mapping
// (a Kelvin sign lowercases to k) where its key parser refuses any byte past
// ASCII; the demuxer refuses them. Each entry is the key the library reads;
// an entry it no longer reads that way is stale and fails.
var asfNonASCIIFolds = map[string]string{
	"WM/Trac\u212aNumber": "TRACKNUMBER",
	"WM/\u212aey":         "KEY",
}

// asfEncoderNames are the library rows the demuxer leaves out on purpose.
var asfEncoderNames = map[string]bool{"wm/toolname": true, "wm/encodingsettings": true}

// withDescriptor appends to a .wma's Header Object an Extended Content
// Description Object holding one descriptor of type valType.
func withDescriptor(raw []byte, name string, valType uint16, v []byte) []byte {
	return withChild(raw, ecdObject(asfDesc{name, valType, v}))
}

// asfDesc is one descriptor or metadata record.
type asfDesc struct {
	name    string
	valType uint16
	value   []byte
}

// ecdObject is an Extended Content Description Object holding ds.
func ecdObject(ds ...asfDesc) []byte {
	le := binary.LittleEndian
	body := le.AppendUint16(nil, uint16(len(ds)))
	for _, d := range ds {
		n := utf16Value(d.name)
		body = le.AppendUint16(body, uint16(len(n)))
		body = append(body, n...)
		body = le.AppendUint16(body, d.valType)
		body = le.AppendUint16(body, uint16(len(d.value)))
		body = append(body, d.value...)
	}
	return asfObject([]byte{0x40, 0xA4, 0xD0, 0xD2, 0x07, 0xE3, 0xD2, 0x11, 0x97, 0xF0, 0x00, 0xA0, 0xC9, 0x5E, 0xA8, 0x50}, body)
}

// libraryExtension is a Header Extension Object holding one Metadata Library
// Object of rs.
func libraryExtension(rs ...asfDesc) []byte {
	le := binary.LittleEndian
	body := le.AppendUint16(nil, uint16(len(rs)))
	for _, r := range rs {
		n := utf16Value(r.name)
		body = append(body, 0, 0, 0, 0)
		body = le.AppendUint16(body, uint16(len(n)))
		body = le.AppendUint16(body, r.valType)
		body = le.AppendUint32(body, uint32(len(r.value)))
		body = append(body, n...)
		body = append(body, r.value...)
	}
	lib := asfObject([]byte{0x94, 0x1C, 0x23, 0x44, 0x98, 0x94, 0xD1, 0x49, 0xA1, 0x41, 0x1D, 0x13, 0x4E, 0x45, 0x70, 0x54}, body)
	ext := make([]byte, 22, 22+len(lib))
	le.PutUint16(ext[16:], 6)
	le.PutUint32(ext[18:], uint32(len(lib)))
	return asfObject([]byte{0xB5, 0x03, 0xBF, 0x5F, 0x2E, 0xA9, 0xCF, 0x11, 0x8E, 0xE3, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}, append(ext, lib...))
}

func asfObject(id, body []byte) []byte {
	obj := binary.LittleEndian.AppendUint64(slices.Clone(id), uint64(24+len(body)))
	return append(obj, body...)
}

// withChild appends obj to a .wma's Header Object, counted.
func withChild(raw, obj []byte) []byte {
	hdr := binary.LittleEndian.Uint64(raw[16:])
	out := slices.Concat(raw[:hdr], obj, raw[hdr:])
	binary.LittleEndian.PutUint64(out[16:], hdr+uint64(len(obj)))
	binary.LittleEndian.PutUint32(out[24:], binary.LittleEndian.Uint32(out[24:])+1)
	return out
}

// TestASFNumberPairsAgreeAcrossReaders: a track number holding its total
// splits through both readers alike, the Metadata Library's records and the
// values only the library compares apart (a trailing space, a blank one)
// included. The library keeps a blank value the demuxer drops.
func TestASFNumberPairsAgreeAcrossReaders(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "container", "asf", "testdata", "ms-v2-44100-2ch-128k.wma"))
	if err != nil {
		t.Fatal(err)
	}
	num := func(v string) asfDesc { return asfDesc{"WM/TrackNumber", 0, utf16Value(v)} }
	for i, children := range [][][]byte{
		{ecdObject(num("3/12"))},
		{ecdObject(num("3/12"), num("3/12 "))},
		{ecdObject(num("  "), num("3/12"))},
		{ecdObject(num("3/12"), asfDesc{"TotalTracks", 0, utf16Value(" ")})},
		{ecdObject(num("3/12")), libraryExtension(asfDesc{"WM/TrackNumber", 3, binary.LittleEndian.AppendUint32(nil, 4)})},
		{ecdObject(num("3/12")), libraryExtension(num("3/12"))},
		{ecdObject(num("3/12")), libraryExtension(asfDesc{"TotalTracks", 0, utf16Value("10")})},
		{ecdObject(num("3/12")), libraryExtension(asfDesc{"WM/Mood", 0, utf16Value("Calm")})},
		// Counts the demuxer cannot take: a name the library folds onto the
		// key, a record too large to read. A blank number side goes, and no
		// total is derived, so the demuxer reads nothing the library lacks.
		{ecdObject(num("/12"), asfDesc{"WM/Trac\u212aNumber", 0, utf16Value("/12")})},
		{ecdObject(num("/12")), libraryExtension(asfDesc{"WM/TrackNumber", 0, append(utf16Value("/12"), make([]byte, 70_000)...)})},
	} {
		raw := base
		for _, c := range children {
			raw = withChild(raw, c)
		}
		d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wma", meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lib := map[string][]string{}
		for k, vs := range info.Tags {
			for _, v := range vs {
				if v = strings.TrimSpace(v); v != "" && !slices.Contains(lib[k], v) {
					lib[k] = append(lib[k], v)
				}
			}
		}
		if i >= 8 {
			// The library reads what the demuxer could not, so native is
			// a subset with equal values.
			for k, vs := range d.Tags() {
				if !slices.Equal(vs, lib[k]) {
					t.Errorf("case %d: native %v, library %v", i, d.Tags(), info.Tags)
				}
			}
			continue
		}
		if !maps.EqualFunc(d.Tags(), lib, slices.Equal) {
			t.Errorf("case %d: native %v, library %v", i, d.Tags(), info.Tags)
		}
	}
}

// TestASFTagsPastTheHeaderCount states a disagreement as it is: the tag
// library reads only the Header Object children its child count covers
// (internal/asf/object.go walkObjects), and the demuxer reads every child, by
// decision, since a miscount is ordinary (ffmpeg undercounts by one when it
// writes markers). An object past the count is then read by the demuxer
// alone, so the native keys are a superset there. The day the library reads
// past the count this cell says so.
func TestASFTagsPastTheHeaderCount(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "container", "asf", "testdata", "ms-v2-44100-2ch-128k.wma"))
	if err != nil {
		t.Fatal(err)
	}
	raw := withChild(base, ecdObject(asfDesc{"WM/Mood", 0, utf16Value("Past")}))
	binary.LittleEndian.PutUint32(raw[24:], binary.LittleEndian.Uint32(raw[24:])-1)
	d, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wma", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Tags()["MOOD"], []string{"Past"}) {
		t.Errorf("the demuxer read %v, want MOOD from the object past the count", d.Tags())
	}
	if got, ok := info.Tags["MOOD"]; ok {
		t.Errorf("the library now reads past the Header Object's child count (MOOD %q): the demuxer and this cell can agree with it", got)
	}
}

// utf16Value is s as a NUL-terminated UTF-16LE descriptor value.
func utf16Value(s string) []byte {
	var b []byte
	for _, r := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, r)
	}
	return binary.LittleEndian.AppendUint16(b, 0)
}

// TestASFDescriptorKeysAgreeAcrossReaders sweeps the names: every one the
// library's table lists, every known key and shared alias spelling bare and
// behind "WM/", and the odd ones, each in a file of its own.
func TestASFDescriptorKeysAgreeAcrossReaders(t *testing.T) {
	// Media Foundation's file, which carries no encoder stamp to fold into a
	// swept ENCODER.
	base, err := os.ReadFile(filepath.Join("..", "container", "asf", "testdata", "ms-v2-44100-2ch-128k.wma"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range append(libraryTable(t, "internal/mapping/asf.go", "asfNames"), asfRowNames...) {
		names[n] = true
		names[strings.ToUpper(n)] = true
	}
	for _, k := range tag.KnownKeys() {
		for _, s := range append(tag.KeyAliases(k), string(k)) {
			names[s] = true
			names[strings.ToLower(s)] = true
			names["WM/"+s] = true
		}
	}
	le := binary.LittleEndian
	values := []struct {
		typ uint16
		v   []byte
	}{
		{0, utf16Value("v")},
		{1, []byte{1, 2, 3}},             // byte array
		{2, le.AppendUint32(nil, 1)},     // BOOL
		{2, le.AppendUint16(nil, 0)},     // BOOL in 16 bits
		{3, le.AppendUint32(nil, 7)},     // DWORD
		{4, le.AppendUint64(nil, 1<<40)}, // QWORD
		{5, le.AppendUint16(nil, 9)},     // WORD
	}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		for _, val := range values {
			raw := withDescriptor(base, n, val.typ, val.v)
			d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wma", meta.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			lib := info.Tags
			if asfEncoderNames[strings.ToLower(n)] {
				if _, ok := d.Tags()["ENCODER"]; ok || (val.typ != 1 && len(lib["ENCODER"]) != 1) {
					t.Errorf("descriptor %q type %d: native %v, library %v; want ENCODER from the library alone", n, val.typ, d.Tags(), lib)
				}
				continue
			}
			if !maps.EqualFunc(d.Tags(), lib, slices.Equal) {
				t.Errorf("descriptor %q type %d: native %v, library %v", n, val.typ, d.Tags(), lib)
			}
		}
	}
	for n, want := range asfNonASCIIFolds {
		raw := withDescriptor(base, n, 0, utf16Value("v"))
		d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wma", meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lib := maps.Clone(info.Tags)
		delete(lib, "MEDIAFOUNDATIONVERSION") // the fixture's own
		if len(d.Tags()) != 1 || !maps.EqualFunc(lib, map[string][]string{want: {"v"}}, slices.Equal) {
			t.Errorf("descriptor %q: native %v, library %v; the recorded divergence is stale", n, d.Tags(), lib)
		}
	}
}
