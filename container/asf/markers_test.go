package asf_test

// The Marker Object, hand-built. ffmpeg's muxer writes one shape (entries in
// order, every description NUL-terminated, an honest entry length), and the
// committed chapters.wma covers it; the forms below are the ones a reader
// meets from other writers and the ones a crafted file can turn against it.

import (
	"bytes"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
)

// guidMarker is spelled here too, so the synthetic files do not borrow it from
// the package they exercise.
var guidMarker = []byte{0x01, 0xCD, 0x87, 0xF4, 0x51, 0xA9, 0xCF, 0x11, 0x8E, 0xE6, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}

// markerPreroll is the pre-roll every marked file below declares, so a marker
// meant for playback time t is stored at markerPreroll+t, as ASF stores it.
// The files play for markedPlay after it.
const (
	markerPreroll = 3 * time.Second
	markedPlay    = 5 * time.Second
)

// synthMarker is one entry: its presentation time on the file's own timeline
// (the pre-roll included) and its description. The optional fields override
// the entry's own bookkeeping to build the malformed shapes a reader must
// survive: a raw presentation time, raw description bytes in place of the
// encoded text, a wrong entry length, and a wrong description length in
// WCHARs (zero means the true one).
type synthMarker struct {
	at        time.Duration
	hns       uint64
	desc      string
	descBytes []byte
	entryLen  uint16
	descLen   uint32
}

// markerObject lays a Marker Object: the reserved GUID, the count, a reserved
// word, the object's name, then one entry per marker with an 8-byte offset,
// the presentation time in 100 ns units, the entry length, a send time, flags,
// and the description behind its length in WCHARs.
func markerObject(name string, marks ...synthMarker) []byte {
	body := make([]byte, 16)
	body = appendU32(body, uint32(len(marks)))
	body = append(body, 0, 0)
	n := utf16(name)
	body = append(body, u16(uint16(len(n)))...)
	body = append(body, n...)
	for _, m := range marks {
		desc := m.descBytes
		if desc == nil {
			desc = utf16(m.desc)
		}
		entryLen, descLen := uint16(12+len(desc)), uint32(len(desc)/2)
		if m.entryLen != 0 {
			entryLen = m.entryLen
		}
		if m.descLen != 0 {
			descLen = m.descLen
		}
		hns := uint64(m.at / (100 * time.Nanosecond))
		if m.hns != 0 {
			hns = m.hns
		}
		body = append(body, make([]byte, 8)...)
		body = append(body, u64(hns)...)
		body = append(body, u16(entryLen)...)
		body = appendU32(body, 0)
		body = appendU32(body, 0)
		body = appendU32(body, descLen)
		body = append(body, desc...)
	}
	return object(guidMarker, body)
}

// markerCount is the offset of the count field inside a built Marker Object:
// behind the object header and the reserved GUID.
const markerCount = 24 + 16

// markedBuilder is a minimal file with the shared pre-roll and play length and
// one audio packet, for header objects to be added to.
func markedBuilder() *builder {
	b := newBuilder()
	b.blockAlign = 4
	b.prerollMS = uint64(markerPreroll / time.Millisecond)
	b.playHNS = uint64((markerPreroll + markedPlay) / (100 * time.Nanosecond))
	b.packet(0, 1, 0, 4, uint32(b.prerollMS), []byte{1, 2, 3, 4})
	return b
}

// markedFile wraps header objects in a minimal file, laid after the two
// objects the builder always writes.
func markedFile(objs ...[]byte) []byte {
	b := markedBuilder()
	b.extra = objs
	return b.build()
}

// marked is markedFile around one Marker Object.
func marked(marks ...synthMarker) []byte {
	return markedFile(markerObject("Chapters", marks...))
}

func chapters(t *testing.T, raw []byte, strict bool) ([]container.Chapter, []container.Warning) {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: strict})
	if err != nil {
		t.Fatal(err)
	}
	return d.Chapters(), d.Warnings()
}

// oneWarning asserts the list holds exactly one warning and that it says want.
func oneWarning(t *testing.T, warnings []container.Warning, want string) {
	t.Helper()
	if len(warnings) != 1 || !strings.Contains(warnings[0].Msg, want) {
		t.Errorf("warnings %v, want one saying %q", warnings, want)
	}
}

// refused asserts strict mode refuses each file.
func refused(t *testing.T, files map[string][]byte) {
	t.Helper()
	for name, raw := range files {
		if _, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true}); err == nil {
			t.Errorf("strict mode accepted %s", name)
		}
	}
}

// TestMarkersReadAsChapters: entries stored out of order with the pre-roll in
// their times come back in start order on the playback timeline, start-only,
// and a clean object draws no warning in strict mode. The object is read the
// same on either side of the File Properties Object that holds the pre-roll.
func TestMarkersReadAsChapters(t *testing.T) {
	marks := []synthMarker{
		{at: markerPreroll + 4*time.Second, desc: "Three"},
		{at: markerPreroll, desc: "One"},
		{at: markerPreroll + 2*time.Second, desc: "Two"},
	}
	want := []container.Chapter{
		{Start: 0, Title: "One"},
		{Start: 2 * time.Second, Title: "Two"},
		{Start: 4 * time.Second, Title: "Three"},
	}
	b := markedBuilder()
	b.lead = [][]byte{markerObject("Chapters", marks...)}
	for name, raw := range map[string][]byte{"after File Properties": marked(marks...), "before File Properties": b.build()} {
		t.Run(name, func(t *testing.T) {
			got, warnings := chapters(t, raw, true)
			if !slices.Equal(got, want) {
				t.Errorf("chapters = %+v, want %+v", got, want)
			}
			if len(warnings) != 0 {
				t.Errorf("warnings %v", warnings)
			}
		})
	}
}

// TestMarkerForms covers the entry shapes a reader meets: no description, a
// time inside the pre-roll (clamped to the start, with a note), two markers
// sharing an instant (file order kept), an entry length field that lies
// (ignored, as ffprobe ignores it), a NUL inside the description (ends the
// title, since a title carrying one could be copied nowhere), padding around
// a title (kept, since the title is copied as it stands and ffprobe and the
// tag library keep it), and a description past the tag cap (cut, with a
// note).
func TestMarkerForms(t *testing.T) {
	long := bytes.Repeat([]byte{'A', 0}, 40_000)
	for _, tc := range []struct {
		name  string
		marks []synthMarker
		want  []container.Chapter
		note  string // a warning the file draws, or none
	}{
		{"untitled", []synthMarker{{at: markerPreroll + time.Second}},
			[]container.Chapter{{Start: time.Second}}, ""},
		{"inside the pre-roll", []synthMarker{{at: time.Second, desc: "early"}},
			[]container.Chapter{{Start: 0, Title: "early"}}, "inside the pre-roll"},
		{"shared instant", []synthMarker{{at: markerPreroll + time.Second, desc: "b"}, {at: markerPreroll, desc: "first"}, {at: markerPreroll + time.Second, desc: "a"}},
			[]container.Chapter{{Start: 0, Title: "first"}, {Start: time.Second, Title: "b"}, {Start: time.Second, Title: "a"}}, ""},
		{"entry length lies", []synthMarker{{at: markerPreroll, desc: "One", entryLen: 0xFFFF}, {at: markerPreroll + time.Second, desc: "Two"}},
			[]container.Chapter{{Start: 0, Title: "One"}, {Start: time.Second, Title: "Two"}}, ""},
		{"embedded NUL ends the title", []synthMarker{{at: markerPreroll, descBytes: append(utf16("One"), utf16("junk")...)}},
			[]container.Chapter{{Start: 0, Title: "One"}}, ""},
		{"padding is part of the title", []synthMarker{{at: markerPreroll, desc: "  Padded  "}},
			[]container.Chapter{{Start: 0, Title: "  Padded  "}}, ""},
		{"a description past the tag cap is cut", []synthMarker{{at: markerPreroll, descBytes: append(long, 0, 0)}},
			[]container.Chapter{{Start: 0, Title: strings.Repeat("A", 32_768)}}, "cut at 65536 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := chapters(t, marked(tc.marks...), false)
			if !slices.Equal(got, tc.want) {
				t.Errorf("chapters = %+v, want %+v", got, tc.want)
			}
			if tc.note == "" {
				if len(warnings) != 0 {
					t.Errorf("warnings %v", warnings)
				}
			} else {
				oneWarning(t, warnings, tc.note)
			}
		})
	}
}

// TestMarkerDamage covers what a damaged object costs and reports. Each fault
// is reported once for the object, whatever the entry count, since the
// warning list has a budget the whole file shares; the entries it does not
// touch are kept; and strict mode refuses every one, as it refuses damage
// elsewhere in the header.
func TestMarkerDamage(t *testing.T) {
	short := markerObject("", synthMarker{at: markerPreroll + time.Second, desc: "only"})
	le.PutUint32(short[markerCount:], 4)
	// A count no object of this size could back is a lie whatever the walk
	// finds, so the cap cannot hide it.
	pastTheCap := markerObject("", synthMarker{at: markerPreroll + time.Second, desc: "only"})
	le.PutUint32(pastTheCap[markerCount:], 1<<20)
	misnamed := markerObject("Chapters", synthMarker{at: markerPreroll, desc: "lost"})
	le.PutUint16(misnamed[24+22:], 0xFFFF)
	// The description of the last entry runs past the object. ffprobe drops
	// that entry; it is kept here with the description the object holds, its
	// time being intact, and the walk ends there.
	overrun := marked(synthMarker{at: markerPreroll, desc: "One", descLen: 1 << 31}, synthMarker{at: markerPreroll + time.Second, desc: "Two"})
	files := map[string]struct {
		raw   []byte
		want  []container.Chapter
		warns string
	}{
		"a short list":            {markedFile(short), []container.Chapter{{Start: time.Second, Title: "only"}}, "declares 4 markers and holds 1"},
		"a count past the cap":    {markedFile(pastTheCap), []container.Chapter{{Start: time.Second, Title: "only"}}, "declares 1048576 markers and holds 1"},
		"a name past the object":  {markedFile(misnamed), nil, "name of 65535 bytes runs past the object"},
		"a description overrun":   {overrun, []container.Chapter{{Start: 0, Title: "One"}}, "marker 1's description runs past the Marker Object"},
		"a second Marker Object":  {markedFile(markerObject("", synthMarker{at: markerPreroll, desc: "first"}), markerObject("", synthMarker{at: markerPreroll + time.Second, desc: "second"})), []container.Chapter{{Start: 0, Title: "first"}}, "a second Marker Object"},
		"markers past the end":    {marked(synthMarker{at: markerPreroll + markedPlay + time.Second, desc: "late"}, synthMarker{at: markerPreroll, desc: "in"}), []container.Chapter{{Start: 0, Title: "in"}, {Start: markedPlay + time.Second, Title: "late"}}, "1 markers start past the end of the stream"},
		"times past any duration": {marked(synthMarker{hns: math.MaxUint64, desc: "beyond"}, synthMarker{at: markerPreroll, desc: "kept"}, synthMarker{hns: math.MaxUint64 - 1, desc: "beyond too"}), []container.Chapter{{Start: 0, Title: "kept"}}, "marker 1 is at a presentation time past any duration; skipped (and 1 more)"},
	}
	strict := map[string][]byte{}
	for name, tc := range files {
		t.Run(name, func(t *testing.T) {
			got, warnings := chapters(t, tc.raw, false)
			if !slices.Equal(got, tc.want) {
				t.Errorf("chapters = %+v, want %+v", got, tc.want)
			}
			oneWarning(t, warnings, tc.warns)
		})
		strict[name] = tc.raw
	}
	refused(t, strict)
}

// TestMarkerCap: the cap counts every entry walked, kept or not, so a run of
// unplaceable entries is bounded by it as a run of good ones is. A list past
// it is cut with a note, which is not damage, so strict mode still opens the
// file; a count the object's own size rules out is damage even past the cap.
func TestMarkerCap(t *testing.T) {
	const limit = 1 << 16 // maxMarkers in asf.go
	kept := make([]synthMarker, limit+1)
	skipped := make([]synthMarker, limit+1)
	for i := range kept {
		kept[i] = synthMarker{at: markerPreroll + time.Duration(i)*50*time.Microsecond}
		skipped[i] = synthMarker{hns: math.MaxUint64}
	}
	got, warnings := chapters(t, marked(kept...), true)
	if len(got) != limit {
		t.Errorf("%d chapters, want the cap of %d", len(got), limit)
	}
	oneWarning(t, warnings, "more than 65536 markers")

	got, warnings = chapters(t, marked(skipped...), false)
	if len(got) != 0 {
		t.Errorf("%d chapters from unplaceable entries", len(got))
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0].Msg, "more than 65536 markers") ||
		!strings.Contains(warnings[1].Msg, "past any duration; skipped (and 65535 more)") {
		t.Errorf("warnings %v, want the cap and one report for the skipped entries", warnings)
	}

	lying := markerObject("Chapters", kept...)
	le.PutUint32(lying[markerCount:], 1<<20)
	got, warnings = chapters(t, markedFile(lying), false)
	if len(got) != limit {
		t.Errorf("%d chapters, want the cap of %d", len(got), limit)
	}
	// The room is an upper bound, counted in the entries' fixed part alone.
	oneWarning(t, warnings, "declares 1048576 markers and has room for at most")
	refused(t, map[string][]byte{"a count past the cap the object cannot hold": markedFile(lying)})
}
