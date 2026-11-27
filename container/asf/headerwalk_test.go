package asf_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The Metadata and Metadata Library Objects' GUIDs, spelled here so the tests
// do not borrow them from the package.
var (
	guidMetadataObj        = []byte{0xEA, 0xCB, 0xF8, 0xC5, 0xAF, 0x5B, 0x77, 0x48, 0x84, 0x67, 0xAA, 0x8C, 0x44, 0xFA, 0x4C, 0xCA}
	guidMetadataLibraryObj = []byte{0x94, 0x1C, 0x23, 0x44, 0x98, 0x94, 0xD1, 0x49, 0xA1, 0x41, 0x1D, 0x13, 0x4E, 0x45, 0x70, 0x54}
)

// record is one Metadata or Metadata Library record.
type record struct {
	name    string
	valType uint16
	value   []byte
}

// metadataObject is a Metadata (or, with library set, Metadata Library)
// Object holding rs.
func metadataObject(library bool, rs ...record) []byte {
	body := u16(uint16(len(rs)))
	for _, r := range rs {
		name := utf16(r.name)
		body = append(body, 0, 0, 0, 0) // language or reserved, stream
		body = append(body, u16(uint16(len(name)))...)
		body = append(body, u16(r.valType)...)
		body = append(body, u32(uint32(len(r.value)))...)
		body = append(body, name...)
		body = append(body, r.value...)
	}
	id := guidMetadataObj
	if library {
		id = guidMetadataLibraryObj
	}
	return object(id, body)
}

// headerExtension is a Header Extension Object holding children.
func headerExtension(children ...[]byte) []byte {
	data := bytes.Join(children, nil)
	ext := make([]byte, 22, 22+len(data))
	le.PutUint16(ext[16:], 6)
	le.PutUint32(ext[18:], uint32(len(data)))
	return object(guidHeaderExtension, append(ext, data...))
}

// TestMetadataObjectsAreRead: the Metadata and Metadata Library Objects carry
// tags the tag library reads, so the demuxer reads them too, as one set with
// the Extended Content Description's. A number beside a different one there
// is a second value, and a record too large to be a tag is stepped over
// unread: a 17 MiB cover costs a few bytes of reading.
func TestMetadataObjectsAreRead(t *testing.T) {
	b := threePackets()
	b.extra = [][]byte{
		tagObject(descriptor{"WM/TrackNumber", 0, utf16("3/12")}),
		headerExtension(
			metadataObject(false, record{"WM/Mood", 0, utf16("Calm")}),
			metadataObject(true,
				record{"WM/Picture", 1, make([]byte, 17<<20)},
				record{"WM/TrackNumber", 3, u32(4)},
				record{"WM/IsCompilation", 5, u16(1)}, // WORD
				record{"WM/BeatsPerMinute", 4, u64(120)},
			),
		),
	}
	raw := b.build()
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := asf.NewDemuxer(src, &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if src.Bytes > 1<<20 {
		t.Errorf("the open read %d bytes, want the cover stepped over", src.Bytes)
	}
	want := map[string][]string{
		"MOOD":        {"Calm"},
		"TRACKNUMBER": {"3/12", "4"},
		"COMPILATION": {"1"},
		"BPM":         {"120"},
	}
	if !equalTags(d.Tags(), want) {
		t.Errorf("read %v, want %v", d.Tags(), want)
	}
}

// TestNumberPairsCountWhatTheLibrarySees: a number splits only where the
// library's read holds that one value, which takes in the Metadata Library
// and the records the demuxer cannot read.
func TestNumberPairsCountWhatTheLibrarySees(t *testing.T) {
	for _, tc := range []struct {
		name string
		ext  []byte
		want map[string][]string
	}{
		{"the same value again", headerExtension(metadataObject(true, record{"WM/TrackNumber", 0, utf16("3/12")})),
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}},
		{"a total in the library", headerExtension(metadataObject(true, record{"TotalTracks", 0, utf16("10")})),
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"10"}}},
		{"a number too large to read", headerExtension(metadataObject(true, record{"WM/TrackNumber", 0, make([]byte, 70_000)})),
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
	} {
		b := threePackets()
		b.extra = [][]byte{tagObject(descriptor{"WM/TrackNumber", 0, utf16("3/12")}), tc.ext}
		d, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !equalTags(d.Tags(), tc.want) {
			t.Errorf("%s: read %v, want %v", tc.name, d.Tags(), tc.want)
		}
	}
}

// TestHeaderObjectBudgetIsShared: the Header Object and every Header
// Extension walk one object budget, so a header of many extensions each
// holding thousands of empty children costs a bounded number of reads, and a
// header past the budget is refused: an encryption object behind the cut
// would go unseen. Padding up front puts every child past the prefix read,
// so each costs a read of its own.
func TestHeaderObjectBudgetIsShared(t *testing.T) {
	empty := bytes.Repeat(object(guidPadding, nil), 4096)
	b := threePackets()
	b.lead = [][]byte{object(guidPadding, make([]byte, 256<<10))}
	for range 256 {
		b.extra = append(b.extra, headerExtension(empty))
	}
	raw := b.build()
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	_, err := asf.NewDemuxer(src, nil)
	if err == nil || !strings.Contains(err.Error(), "objects") {
		t.Errorf("err = %v, want the header refused at the object budget", err)
	}
	if src.Reads > 4096+16 {
		t.Errorf("the open took %d reads, want at most the budget's", src.Reads)
	}

	// The budget runs out inside one extension before its encryption object.
	b = threePackets()
	b.extra = [][]byte{headerExtension(empty, object(guidAdvancedContentEncrypt, nil))}
	if _, err := asf.NewDemuxer(container.BytesSource(b.build()), nil); err == nil {
		t.Error("opened a file whose encryption object lies past the budget")
	}
}

// TestABrokenExtensionChildIsDamage: a Header Extension child whose size does
// not fit hides whatever follows it, an encryption object included, so it is
// damage: strict mode refuses, and a tolerant read opens with a warning.
func TestABrokenExtensionChildIsDamage(t *testing.T) {
	bad := object(guidPadding, nil)
	le.PutUint64(bad[16:], 1<<40)
	b := threePackets()
	b.extra = [][]byte{headerExtension(bad, object(guidAdvancedContentEncrypt, nil))}
	raw := b.build()
	if _, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode opened a file with a broken Header Extension child")
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := d.Warnings(); len(w) != 1 || w[0].Kind != container.Damage || !strings.Contains(w[0].Msg, "Header Extension") {
		t.Errorf("warnings %v, want one naming the Header Extension child", w)
	}
}

// TestTagsPastTheHeaderCountAreRead: every header child is read for tags and
// chapters whatever the Header Object's count states, with no warning, since
// a miscount is ordinary (ffmpeg's chapters.wma states 6 children and holds
// 7). The tag library reads only the children the count covers, so a value
// past it does not count toward the number split: a number found only there
// does not split, and a counted number is not blinded by an uncounted one.
func TestTagsPastTheHeaderCountAreRead(t *testing.T) {
	num := func(v string) []byte { return tagObject(descriptor{"WM/TrackNumber", 0, utf16(v)}) }
	// counted builds a file of the two properties and first, with rest laid
	// after them and left out of the Header Object's count.
	counted := func(first []byte, rest ...[]byte) []byte {
		b := markedBuilder()
		b.extra = append([][]byte{first}, rest...)
		raw := b.build()
		le.PutUint32(raw[24:], 3)
		return raw
	}
	open := func(raw []byte) *asf.Demuxer {
		t.Helper()
		d, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
		if err != nil {
			t.Fatal(err)
		}
		if w := d.Warnings(); len(w) != 0 {
			t.Errorf("warnings %v, want none", w)
		}
		return d
	}

	d := open(counted(tagObject(descriptor{"WM/AlbumTitle", 0, utf16("Counted")}),
		headerExtension(metadataObject(true, record{"WM/Mood", 0, utf16("Past")})),
		tagObject(descriptor{"WM/Genre", 0, utf16("Past")}),
		markerObject("Chapters", synthMarker{at: markerPreroll, desc: "Past"})))
	if want := map[string][]string{"ALBUM": {"Counted"}, "MOOD": {"Past"}, "GENRE": {"Past"}}; !equalTags(d.Tags(), want) {
		t.Errorf("read %v, want %v", d.Tags(), want)
	}
	if chs := d.Chapters(); len(chs) != 1 || chs[0].Title != "Past" {
		t.Errorf("chapters %+v, want the one past the count", chs)
	}

	for _, tc := range []struct {
		name string
		raw  []byte
		want map[string][]string
	}{
		{"a number only past the count", counted(tagObject(), num("3/12")),
			map[string][]string{"TRACKNUMBER": {"3/12"}}},
		{"the same number past the count", counted(num("3/12"), num("3/12")),
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}},
		{"another number past the count", counted(num("3/12"), num("4")),
			map[string][]string{"TRACKNUMBER": {"3/12", "4"}}},
		{"a folded number past the count", counted(num("3/12"), tagObject(descriptor{"WM/Trac\u212aNumber", 0, utf16("4")})),
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}},
		{"a total past the count", counted(num("3/12"), tagObject(descriptor{"TotalTracks", 0, utf16("10")})),
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"10"}}},
	} {
		if got := open(tc.raw).Tags(); !equalTags(got, tc.want) {
			t.Errorf("%s: read %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestABlankNumberUnderAnUnknownCountGoes: where the demuxer cannot take the
// library's count (a name the library folds onto the key through Unicode
// case mapping, a record too large to read), a number with a blank number
// side still goes, since the library either drops it too or holds more
// values, and no total is derived.
func TestABlankNumberUnderAnUnknownCountGoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra [][]byte
	}{
		{"a folded name", [][]byte{tagObject(descriptor{"WM/TrackNumber", 0, utf16("/12")}, descriptor{"WM/Trac\u212aNumber", 0, utf16("/12")})}},
		{"an oversized record", [][]byte{
			tagObject(descriptor{"WM/TrackNumber", 0, utf16("/12")}),
			headerExtension(metadataObject(true, record{"WM/TrackNumber", 0, append(utf16("/12"), make([]byte, 70_000)...)})),
		}},
	} {
		b := threePackets()
		b.extra = tc.extra
		d, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Tags()) != 0 {
			t.Errorf("%s: read %v, want nothing", tc.name, d.Tags())
		}
	}
}

// TestTagGrowthIsBounded: one key's values, and the text of all of them, are
// capped, and the dedupe is a set lookup. Four Extended Content Description
// Objects of 65535 distinct values under one name, and lyrics of 60 KB
// enough to pass the text cap, open quickly, keep their bounds and say so
// once.
func TestTagGrowthIsBounded(t *testing.T) {
	b := threePackets()
	for o := range 4 {
		ds := make([]descriptor, 0, 65535)
		for i := range 65535 {
			ds = append(ds, descriptor{"A", 3, u32(uint32(o<<16 | i))})
		}
		b.extra = append(b.extra, tagObject(ds...))
	}
	lyric := bytes.Repeat([]byte{'x', 0}, 30_000)
	for i := range 200 {
		lyric[0] = byte('a' + i%26)
		lyric[2] = byte('a' + i/26)
		b.extra = append(b.extra, tagObject(descriptor{"WM/Lyrics", 0, bytes.Clone(lyric)}))
	}
	raw := b.build()
	start := time.Now()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a %d-byte header took %v to open", len(raw), took)
	}
	tags := d.Tags()
	if n := len(tags["A"]); n != 1<<10 {
		t.Errorf("A holds %d values, want the cap of 1024", n)
	}
	text := 0
	for _, vs := range tags {
		for _, v := range vs {
			text += len(v)
		}
	}
	if text > 4<<20 {
		t.Errorf("the tags hold %d bytes of text, want at most 4 MiB", text)
	}
	if len(tags["LYRICS"]) == 200 {
		t.Error("every lyric was kept, past the text cap")
	}
	if w := d.Warnings(); len(w) != 1 || w[0].Kind != container.Note {
		t.Errorf("warnings %v, want one note", w)
	}
}

// TestTheRecordCapIsSaidOnce: past the metadata record cap, every later
// Metadata Library Object is stepped over under the one note.
func TestTheRecordCapIsSaidOnce(t *testing.T) {
	rs := make([]record, 4096)
	for i := range rs {
		rs[i] = record{"WM/Mood", 0, utf16("Calm")}
	}
	children := [][]byte{metadataObject(true, rs...)}
	for range 200 {
		children = append(children, metadataObject(true, record{"WM/Mood", 0, utf16("Later")}))
	}
	b := threePackets()
	b.extra = [][]byte{headerExtension(children...)}
	d, err := asf.NewDemuxer(container.BytesSource(b.build()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := d.Warnings(); len(w) != 1 || !strings.Contains(w[0].Msg, "metadata records") {
		t.Errorf("warnings %v, want the one note", w)
	}
}
