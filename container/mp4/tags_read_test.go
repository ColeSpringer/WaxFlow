package mp4

import (
	"maps"
	"slices"
	"testing"
)

// fxText1 is a text ilst item holding one UTF-8 value.
func fxText1(atom, value string) []byte {
	return makeBox(atom, dataAtom(1, []byte(value)))
}

// fxFreeform is a freeform item with one data atom of the given type.
func fxFreeform(mean, name string, typ uint32, value string) []byte {
	return makeBox("----",
		makeFullBox("mean", 0, 0, []byte(mean)),
		makeFullBox("name", 0, 0, []byte(name)),
		dataAtom(typ, []byte(value)))
}

// fxQTText is a classic QuickTime text atom: a 16-bit length, a language
// code, then the text.
func fxQTText(atom, text string) []byte {
	return makeBox(atom, u16(uint16(len(text))), u16(0x55C4), []byte(text))
}

func wantTagMap(t *testing.T, got, want map[string][]string) {
	t.Helper()
	if !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("tags = %v, want %v", got, want)
	}
}

// TestTagsInAMetaUnderMoov: a meta directly under moov, as Apple's recorders
// write it, is a tag store. Beside one under moov.udta it fills the keys
// that one lacks.
func TestTagsInAMetaUnderMoov(t *testing.T) {
	moovMeta := fxMeta(fxText1("\xa9nam", "MoovTitle"), fxText1("\xa9alb", "MoovAlbum"))
	t.Run("alone", func(t *testing.T) {
		wantTagMap(t, openFx(t, fxMovie{meta: moovMeta}.build()).Tags(),
			map[string][]string{"TITLE": {"MoovTitle"}, "ALBUM": {"MoovAlbum"}})
	})
	t.Run("beside moov.udta.meta", func(t *testing.T) {
		raw := fxMovie{meta: moovMeta, udta: [][]byte{fxMeta(fxText1("\xa9nam", "Song"))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"TITLE": {"Song"}, "ALBUM": {"MoovAlbum"}})
	})
}

// TestDuplicateStoresAreReadOnce: of two udta boxes, two meta boxes or two
// item lists the first is the one read, so a value both hold is not doubled.
// Two items of one atom in one list are still one multi-valued field.
func TestDuplicateStoresAreReadOnce(t *testing.T) {
	first := []byte(joinBytes(fxText1("\xa9nam", "Song"), fxText1("\xa9alb", "Album")))
	second := []byte(joinBytes(fxText1("\xa9nam", "SecondTitle"), fxText1("\xa9alb", "Album"), fxText1("\xa9ART", "Only here")))
	want := map[string][]string{"TITLE": {"Song"}, "ALBUM": {"Album"}}
	t.Run("two udta", func(t *testing.T) {
		wantTagMap(t, openFx(t, fxMovie{udta: [][]byte{fxMeta(first), fxMeta(second)}}.build()).Tags(), want)
	})
	t.Run("two meta", func(t *testing.T) {
		wantTagMap(t, openFx(t, fxMovie{udta: [][]byte{joinBytes(fxMeta(first), fxMeta(second))}}.build()).Tags(), want)
	})
	t.Run("two ilst", func(t *testing.T) {
		meta := makeFullBox("meta", 0, 0,
			makeFullBox("hdlr", 0, 0, u32(0), []byte("mdir"), []byte("appl"), u32(0), u32(0), []byte{0}),
			makeBox("ilst", first), makeBox("ilst", second))
		wantTagMap(t, openFx(t, fxMovie{udta: [][]byte{meta}}.build()).Tags(), want)
	})
	t.Run("two items of one atom", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{fxMeta(fxText1("\xa9nam", "Song"), fxText1("\xa9nam", "SecondTitle"))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"TITLE": {"Song", "SecondTitle"}})
	})
	// Two atoms that map to one key are a disagreement, and the first decides.
	t.Run("two atoms of one key", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{fxMeta(fxText1("\xa9day", "2020-05-06"),
			fxFreeform("com.apple.iTunes", "DATE", 1, "1999-01-01"))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"RECORDINGDATE": {"2020-05-06"}})
	})
	// A date is the exception: an item holding only its year gives way to a
	// later one holding the date.
	t.Run("a year ahead of its date", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{fxMeta(fxFreeform("com.apple.iTunes", "originalyear", 1, "1999"),
			fxFreeform("com.apple.iTunes", "originaldate", 1, "1999-03-04"))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"ORIGINALDATE": {"1999-03-04"}})
	})
}

// TestNumberPairWithNoTotalAndImplicitText: a trkn or disk atom may stop
// after the number, and a text item may carry the implicit data type.
func TestNumberPairWithNoTotalAndImplicitText(t *testing.T) {
	raw := fxMovie{udta: [][]byte{fxMeta(
		makeBox("\xa9ART", dataAtom(0, []byte("ImplicitArtist"))),
		makeBox("trkn", dataAtom(0, []byte{0, 0, 0, 7})),
		makeBox("disk", dataAtom(21, []byte{0, 0, 0, 2, 0, 5})),
	)}}.build()
	wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{
		"ARTIST": {"ImplicitArtist"}, "TRACKNUMBER": {"7"}, "DISCNUMBER": {"2"}, "DISCTOTAL": {"5"}})
}

// TestKeysStoreTags: a meta whose handler is mdta keys its items through a
// keys box. The names are Apple's, or the ones ffmpeg gives the iTunes
// atoms; any alias spelling is folded, and a track or disc entry holds
// "n/total".
func TestKeysStoreTags(t *testing.T) {
	meta := fxKeysMeta(
		[2]string{"title", "KeysTitle"},
		[2]string{"com.apple.quicktime.artist", "KeysArtist"},
		[2]string{"album_artist", "AA"},
		[2]string{"track", "3/12"},
		[2]string{"disc", "1"},
		[2]string{"sort_name", "Sorted"},
		[2]string{"synopsis", "Long"},
		[2]string{"gapless_playback", "1"},
		[2]string{"DATE", "2020"},
		[2]string{"custom.thing", "Custom"},
		[2]string{"com.apple.quicktime.artwork", "not text to us"},
	)
	want := map[string][]string{
		"TITLE": {"KeysTitle"}, "ARTIST": {"KeysArtist"}, "ALBUMARTIST": {"AA"},
		"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"},
		"TITLESORT": {"Sorted"}, "LONGDESCRIPTION": {"Long"}, "ITUNESGAPLESS": {"1"},
		"RECORDINGDATE": {"2020"}, "CUSTOM.THING": {"Custom"},
	}
	t.Run("under moov.udta", func(t *testing.T) {
		wantTagMap(t, openFx(t, fxMovie{udta: [][]byte{meta}}.build()).Tags(), want)
	})
	t.Run("under moov", func(t *testing.T) {
		wantTagMap(t, openFx(t, fxMovie{meta: meta}.build()).Tags(), want)
	})
}

// TestQuickTimeUdtaTextAtoms: a plain .mov keeps its tags as text atoms
// directly under udta. An item list beside them wins per key.
func TestQuickTimeUdtaTextAtoms(t *testing.T) {
	texts := joinBytes(fxQTText("\xa9nam", "MovTitle"), fxQTText("\xa9ART", "MovArtist"), fxQTText("\xa9swr", "A Tool"),
		fxQTText("\xa9nam", "A second title atom"))
	t.Run("alone", func(t *testing.T) {
		wantTagMap(t, openFx(t, fxMovie{udta: [][]byte{texts}}.build()).Tags(),
			map[string][]string{"TITLE": {"MovTitle"}, "ARTIST": {"MovArtist"}, "ENCODER": {"A Tool"}})
	})
	t.Run("beside an item list", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{joinBytes(texts, fxMeta(fxText1("\xa9nam", "Song")))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(),
			map[string][]string{"TITLE": {"Song"}, "ARTIST": {"MovArtist"}, "ENCODER": {"A Tool"}})
	})
}

// TestFreeformTagsAreTheITunesTextOnes: a freeform item is a tag only in the
// com.apple.iTunes namespace and only as text, and its name folds onto its
// key in any case, which is how a tagger's lowercase ReplayGain reads.
func TestFreeformTagsAreTheITunesTextOnes(t *testing.T) {
	const itunes = "com.apple.iTunes"
	raw := fxMovie{udta: [][]byte{fxMeta(
		fxFreeform(itunes, "replaygain_track_gain", 1, "-3.00 dB"),
		fxFreeform(itunes, "REPLAYGAIN_TRACK_PEAK", 0, "0.900000"),
		fxFreeform("org.example", "REPLAYGAIN_ALBUM_GAIN", 1, "-9.99 dB"),
		fxFreeform(itunes, "REPLAYGAIN_ALBUM_PEAK", 21, "\x00\x01"),
		fxFreeform(itunes, "MusicBrainz Track Id", 1, "mbid"),
		fxFreeform(itunes, "LABEL", 1, "A Label"),
		fxFreeform(itunes, "CUSTOMFOO", 1, "bar"),
		fxFreeform(itunes, "iTunNORM", 1, " 0000 0000"),
	)}}.build()
	wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{
		"REPLAYGAIN_TRACK_GAIN": {"-3.00 dB"}, "REPLAYGAIN_TRACK_PEAK": {"0.900000"},
		"MUSICBRAINZ_TRACKID": {"mbid"}, "LABEL": {"A Label"}, "CUSTOMFOO": {"bar"}})
}

// TestTagValuesShedTheirTerminator: a value some writers end with a NUL
// reads without it, and a value of nothing is no value.
func TestTagValuesShedTheirTerminator(t *testing.T) {
	raw := fxMovie{udta: [][]byte{fxMeta(fxText1("\xa9nam", "Song\x00"), fxText1("\xa9alb", ""),
		fxText1("\xa9ART", "A"), fxText1("\xa9ART", "\x00"), fxText1("\xa9ART", "B"),
		fxFreeform("com.apple.iTunes", "LABEL", 1, "L\x00\x00"))}}.build()
	wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"TITLE": {"Song"}, "ARTIST": {"A", "B"}, "LABEL": {"L"}})
}

// TestTagRulesSeeTheValueAsWritten: the terminator is shed from what is
// stored, after every rule that splits or compares values has run on the
// value the file holds. A number with a NUL behind its total is no pair, and
// a year with one behind it echoes no date.
func TestTagRulesSeeTheValueAsWritten(t *testing.T) {
	const itunes = "com.apple.iTunes"
	t.Run("a freeform number", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{fxMeta(fxFreeform(itunes, "TRACKNUMBER", 1, "3/12\x00"),
			fxFreeform(itunes, "DISCNUMBER", 1, "1 of 2\x00"))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"TRACKNUMBER": {"3/12"}, "DISCNUMBER": {"1 of 2"}})
	})
	t.Run("a keys-store number", func(t *testing.T) {
		raw := fxMovie{meta: fxKeysMeta([2]string{"track", "3/12\x00"}, [2]string{"disc", "1/2"})}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"TRACKNUMBER": {"3/12"}, "DISCNUMBER": {"1"}, "DISCTOTAL": {"2"}})
	})
	t.Run("a year ahead of a date", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{fxMeta(fxText1("\xa9day", "2020\x00"), fxFreeform(itunes, "DATE", 1, "2020-05-01"))}}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"RECORDINGDATE": {"2020"}})
	})
	// A udta text atom of nothing but a terminator holds a value, so the
	// meta under moov does not fill its key.
	t.Run("a udta text atom", func(t *testing.T) {
		raw := fxMovie{udta: [][]byte{joinBytes(fxQTText("\xa9wrk", "\x00"), fxMeta(fxText1("\xa9nam", "T")))},
			meta: fxMeta(fxText1("\xa9wrk", "MoovWork"))}.build()
		wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{"TITLE": {"T"}})
	})
}

// TestGaplessDescriptorReadFromAnyNamespace: iTunSMPB is gapless state, not
// a tag, and is read whatever namespace and data type a writer gave it.
func TestGaplessDescriptorReadFromAnyNamespace(t *testing.T) {
	smpb := smpbPayload(1024, 14000)
	for _, tc := range []struct {
		name, mean string
		typ        uint32
	}{
		{"the iTunes namespace", "com.apple.iTunes", 1},
		{"a foreign namespace", "org.example", 1},
		{"a binary data type", "com.apple.iTunes", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openFx(t, fxMovie{udta: [][]byte{fxMeta(fxFreeform(tc.mean, "iTunSMPB", tc.typ, smpb))}}.build())
			if !d.smpbOK || d.smpbDelay != 1024 || d.smpbTotal != 14000 {
				t.Errorf("gapless state = %v, delay %d, length %d; want 1024 and 14000", d.smpbOK, d.smpbDelay, d.smpbTotal)
			}
			if tr := d.Tracks()[0]; tr.Delay != 1024 || tr.Samples != 14000 {
				t.Errorf("track trims = delay %d, %d samples; want 1024 and 14000", tr.Delay, tr.Samples)
			}
			if len(d.Tags()) != 0 {
				t.Errorf("iTunSMPB read as a tag: %v", d.Tags())
			}
		})
	}
}

// TestIntegerAndGenreAtoms covers the atoms that are not text: booleans,
// integers of one to four bytes, and the legacy numeric genre.
func TestIntegerAndGenreAtoms(t *testing.T) {
	raw := fxMovie{udta: [][]byte{fxMeta(
		makeBox("cpil", dataAtom(21, []byte{1})),
		makeBox("pgap", dataAtom(0, []byte{0})),
		makeBox("stik", dataAtom(21, []byte{2})),
		makeBox("tmpo", dataAtom(21, []byte{0, 120})),
		makeBox("tvsn", dataAtom(21, []byte{0, 0, 0, 2})),
		makeBox("gnre", dataAtom(0, []byte{0, 18})),
		// A text-typed boolean is not read: "0" is a nonzero byte.
		makeBox("shwm", dataAtom(1, []byte("0"))),
	)}}.build()
	wantTagMap(t, openFx(t, raw).Tags(), map[string][]string{
		"COMPILATION": {"1"}, "ITUNESGAPLESS": {"0"}, "MEDIATYPE": {"2"}, "BPM": {"120"},
		"SEASONNUMBER": {"2"}, "GENRE": {"Rock"}})
}
