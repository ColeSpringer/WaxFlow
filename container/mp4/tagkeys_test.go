package mp4

import (
	"slices"
	"testing"
)

// TestVocabularyTablesAgree holds the write direction to the read one:
// every text atom but the two encoder stamps has a place in the write
// order, and no key or freeform name is spelled twice.
func TestVocabularyTablesAgree(t *testing.T) {
	for atom, key := range ilstTextKeys {
		if textAtomOf[key] != atom {
			t.Errorf("%s maps to two text atoms", key)
		}
		if got, want := slices.Contains(ilstTextOrder, key), !unwrittenKeys[key]; got != want {
			t.Errorf("%s: in the write order %v, want %v", key, got, want)
		}
	}
	if len(ilstTextOrder) != len(ilstTextKeys)-len(unwrittenKeys) {
		t.Errorf("the write order lists %d text keys of %d", len(ilstTextOrder), len(ilstTextKeys)-len(unwrittenKeys))
	}
	if len(freeformNameOf) != len(freeformKeys) {
		t.Errorf("%d freeform names for %d keys: a key is spelled twice", len(freeformKeys), len(freeformNameOf))
	}
	for name, key := range freeformKeys {
		if got, ok := freeformKey(name); !ok || got != key {
			t.Errorf("freeform %q reads back as %q, want %q", name, got, key)
		}
		// A key the muxer writes as a freeform must not also have an atom.
		if _, text := textAtomOf[key]; text {
			t.Errorf("%s has a text atom and the freeform name %q", key, name)
		}
	}
	for alias, key := range keyAliases {
		if !canonicalKey(key) {
			t.Errorf("alias %q names %q, which is no canonical key", alias, key)
		}
	}
}

func TestFreeformKey(t *testing.T) {
	for name, want := range map[string]string{
		"MusicBrainz Track Id":  "MUSICBRAINZ_TRACKID",
		"MUSICBRAINZ TRACK ID":  "MUSICBRAINZ_TRACKID",
		"replaygain_track_gain": "REPLAYGAIN_TRACK_GAIN",
		"REPLAYGAIN_TRACK_GAIN": "REPLAYGAIN_TRACK_GAIN",
		" OriginalDate ":        "ORIGINALDATE",
		"DATE":                  "RECORDINGDATE",
		"totaltracks":           "TRACKTOTAL",
		"Album Artist":          "ALBUMARTIST",
		"bpm":                   "BPM",
		"work":                  "WORK",
		"CUSTOMFOO":             "CUSTOMFOO",
		"CUSTOM KEY.2":          "CUSTOM KEY.2",
	} {
		if got, ok := freeformKey(name); !ok || got != want {
			t.Errorf("freeformKey(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	// A name that is no key as written and folds onto none is not a tag: the
	// iTunes-internal mixed-case names, and a key with an edge space.
	for _, name := range []string{"iTunNORM", "iTunSMPB", "customfoo", " EDGE", "A=B", "", "café"} {
		if got, ok := freeformKey(name); ok {
			t.Errorf("freeformKey(%q) = %q, want no key", name, got)
		}
	}
}

func TestMdtaKey(t *testing.T) {
	for name, want := range map[string]string{
		"title":                     "TITLE",
		"com.apple.quicktime.title": "TITLE",
		"com.apple.quicktime.album": "ALBUM",
		"albumartist":               "ALBUMARTIST",
		"album_artist":              "ALBUMARTIST",
		"track":                     "TRACKNUMBER",
		"disc":                      "DISCNUMBER",
		"rating":                    "ITUNESADVISORY",
		"episode_sort":              "EPISODENUMBER",
		"TOTALTRACKS":               "TRACKTOTAL",
		// The table is matched as written, so a capitalized custom key is not
		// Apple's lowercase name.
		"SOFTWARE":          "SOFTWARE",
		"software":          "ENCODER",
		"org.example.thing": "ORG.EXAMPLE.THING",
		"naïve=key~":        "NA_VE_KEY_",
		" padded ":          "PADDED",
	} {
		if got, ok := mdtaKey(name); !ok || got != want {
			t.Errorf("mdtaKey(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "   ", "com.apple.quicktime."} {
		if got, ok := mdtaKey(name); ok {
			t.Errorf("mdtaKey(%q) = %q, want no key", name, got)
		}
	}
}

func TestMuxKey(t *testing.T) {
	for key, want := range map[string]string{
		"TITLE": "TITLE", " title ": "TITLE", "date": "RECORDINGDATE", "Year": "RECORDINGDATE",
		"TOTALDISCS": "DISCTOTAL", "customfoo": "CUSTOMFOO", "CUSTOM KEY.2": "CUSTOM KEY.2",
	} {
		if got, ok := muxKey(key); !ok || got != want {
			t.Errorf("muxKey(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"", "A=B", "café", "ENCODER", "encoder", "ENCODEDBY", "ENCODED_BY", "TILDE~"} {
		if got, ok := muxKey(key); ok {
			t.Errorf("muxKey(%q) = %q, want it left out", key, got)
		}
	}
}

// TestGenreTable pins the ends of the numeric genre table gnre indexes.
func TestGenreTable(t *testing.T) {
	if len(id3v1Genres) != 192 || id3v1Genres[0] != "Blues" || id3v1Genres[17] != "Rock" || id3v1Genres[191] != "Psybient" {
		t.Errorf("genre table has %d entries, %q first and %q last", len(id3v1Genres), id3v1Genres[0], id3v1Genres[len(id3v1Genres)-1])
	}
}

// TestTagItemCaps: an item list is walked to a cap, and an item of more
// data atoms than any tag holds is not read at all.
func TestTagItemCaps(t *testing.T) {
	var items [][]byte
	for range maxTagItems + 10 {
		items = append(items, fxText1("\xa9nam", "T"))
	}
	if got := openFx(t, fxMovie{frames: 64, udta: [][]byte{fxMeta(items...)}}.build()).Tags()["TITLE"]; len(got) != maxTagItems {
		t.Errorf("read %d items of a list capped at %d", len(got), maxTagItems)
	}
	var atoms [][]byte
	for range maxItemValues {
		atoms = append(atoms, dataAtom(dataUTF8, []byte("v")))
	}
	full := fxMeta(makeBox("\xa9ART", atoms...))
	if got := openFx(t, fxMovie{frames: 64, udta: [][]byte{full}}.build()).Tags()["ARTIST"]; len(got) != maxItemValues {
		t.Errorf("read %d values of an item at the cap of %d", len(got), maxItemValues)
	}
	over := fxMeta(makeBox("\xa9ART", append(atoms, dataAtom(dataUTF8, []byte("v")))...))
	if got := openFx(t, fxMovie{frames: 64, udta: [][]byte{over}}.build()).Tags(); len(got) != 0 {
		t.Errorf("an item past the value cap was read: %d values", len(got["ARTIST"]))
	}
}

// TestDateEchoes: a bare year echoes a date it opens, and nothing else does.
func TestDateEchoes(t *testing.T) {
	for _, tc := range []struct {
		years, dates []string
		want         bool
	}{
		{[]string{"1999"}, []string{"1999-03-04"}, true},
		{[]string{"1999", "2001"}, []string{"2001-01", "1999-03-04"}, true},
		{[]string{"1999"}, []string{"1999"}, false},
		{[]string{"1999"}, []string{"19990304"}, false},
		{[]string{"1999", "2000"}, []string{"1999-03-04"}, false},
		{[]string{"1999-03"}, []string{"1999-03-04"}, false},
		{nil, []string{"1999-03-04"}, true},
	} {
		if got := echoesOf(tc.years, tc.dates); got != tc.want {
			t.Errorf("echoesOf(%q, %q) = %v, want %v", tc.years, tc.dates, got, tc.want)
		}
	}
}
