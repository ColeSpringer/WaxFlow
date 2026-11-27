package mp4

import (
	"bytes"
	"maps"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/container"
)

// muxTagged writes tags through both muxers and returns the two files, the
// fragmented one first.
func muxTagged(t *testing.T, tags []container.Tag) [2][]byte {
	t.Helper()
	src := metaTone(alac.FrameSize)
	defer audio.Put(src)
	var frag bytes.Buffer
	muxALACMeta(t, &frag, src, &MuxerOptions{Tags: tags})

	track, pkts := opusTrackFor(0, 960*4, 4)
	sb := &seekBuf{}
	m := NewProgressiveMuxer(sb, &MuxerOptions{Tags: tags})
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, p := range pkts {
		if err := m.WritePacket(container.Packet{Track: 0, Packet: p}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(codec.Trailer{Samples: 960 * 4}); err != nil {
		t.Fatalf("End: %v", err)
	}
	return [2][]byte{frag.Bytes(), sb.Buf}
}

// tagVocabulary is one value, or several, for every atom shape the muxer
// writes: each text atom, the two number pairs, the boolean and integer
// atoms, the freeform spellings taggers use, and keys with no atom at all.
var tagVocabulary = map[string][]string{
	"TITLE": {"A Title"}, "ARTIST": {"Artist One", "Artist Two"}, "ALBUM": {"An Album"},
	"ALBUMARTIST": {"An Album Artist"}, "COMPOSER": {"A Composer"}, "GENRE": {"Rock", "Jazz"},
	"RECORDINGDATE": {"2020-05-06"}, "COMMENT": {"A comment"}, "LYRICS": {"A lyric\nsheet"},
	"COPYRIGHT": {"(c) Someone"}, "GROUPING": {"A Group"}, "DESCRIPTION": {"Short"},
	"LONGDESCRIPTION": {"Long"}, "TITLESORT": {"Title, A"}, "ARTISTSORT": {"One, Artist"},
	"ALBUMSORT": {"Album, An"}, "ALBUMARTISTSORT": {"Artist, An Album"}, "COMPOSERSORT": {"Composer, A"},
	"WORK": {"A Work"}, "MOVEMENTNAME": {"Allegro"}, "SHOW": {"A Show"}, "EPISODEID": {"S02E05"},
	"NETWORK": {"A Network"}, "SHOWSORT": {"Show, A"},
	"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"}, "DISCTOTAL": {"2"},
	"COMPILATION": {"1"}, "ITUNESGAPLESS": {"1"}, "SHOWMOVEMENT": {"0"}, "MEDIATYPE": {"2"},
	"ITUNESADVISORY": {"1"}, "MOVEMENT": {"2"}, "MOVEMENTTOTAL": {"4"}, "BPM": {"120"},
	"EPISODENUMBER": {"5"}, "SEASONNUMBER": {"2"},
	"REPLAYGAIN_TRACK_GAIN": {"-3.00 dB"}, "REPLAYGAIN_TRACK_PEAK": {"0.900000"},
	"REPLAYGAIN_ALBUM_GAIN": {"-4.00 dB"}, "REPLAYGAIN_ALBUM_PEAK": {"0.950000"},
	"MUSICBRAINZ_TRACKID":  {"22222222-2222-2222-2222-222222222222"},
	"MUSICBRAINZ_ARTISTID": {"33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444"},
	"ACOUSTID_ID":          {"aid"}, "ORIGINALDATE": {"1999"}, "RELEASEDATE": {"2020-07-08"},
	"RELEASECOUNTRY": {"US"}, "RELEASESTATUS": {"official"}, "RELEASETYPE": {"album"},
	"LYRICIST": {"A Lyricist"}, "PRODUCER": {"A Producer"}, "DJMIXER": {"A DJ"}, "ISRC": {"USABC0000001"},
	"BARCODE": {"0123456789012"}, "CATALOGNUMBER": {"CAT-1"}, "LABEL": {"A Label"}, "MEDIA": {"CD"},
	"NARRATOR": {"A Narrator"}, "CONDUCTOR": {"A Conductor"}, "REMIXER": {"A Remixer"}, "MOOD": {"Happy"},
	"PERFORMER": {"A Performer", "Another"}, "CUSTOMFOO": {"bar"}, "CUSTOM KEY.2": {"baz"},
}

func tagList(m map[string][]string) []container.Tag {
	var out []container.Tag
	for _, k := range slices.Sorted(maps.Keys(m)) {
		for _, v := range m[k] {
			out = append(out, container.Tag{Key: k, Value: v})
		}
	}
	return out
}

// TestMuxedTagsReadBack: every key the muxer is handed comes back from its
// output under the same key with the same values, from both muxers.
func TestMuxedTagsReadBack(t *testing.T) {
	for i, raw := range muxTagged(t, tagList(tagVocabulary)) {
		form := [2]string{"fragmented", "progressive"}[i]
		got := openFx(t, raw).Tags()
		for key, want := range tagVocabulary {
			if !slices.Equal(got[key], want) {
				t.Errorf("%s: %s = %q, want %q", form, key, got[key], want)
			}
		}
		for key := range got {
			if _, ok := tagVocabulary[key]; !ok {
				t.Errorf("%s: read a key nothing wrote: %s = %q", form, key, got[key])
			}
		}
	}
}

// TestMuxedTagAtoms pins the bytes of each atom shape: where a key goes and
// how its value is spelled there.
func TestMuxedTagAtoms(t *testing.T) {
	text := func(vals ...string) []byte {
		var out []byte
		for _, v := range vals {
			out = append(out, dataAtom(1, []byte(v))...)
		}
		return out
	}
	freeform := func(name string, vals ...string) []byte {
		return makeBox("----", makeFullBox("mean", 0, 0, []byte("com.apple.iTunes")),
			makeFullBox("name", 0, 0, []byte(name)), text(vals...))
	}
	for _, tc := range []struct {
		name string
		tags []container.Tag
		want []byte // an atom the output holds
		not  string // bytes it must not hold
	}{
		{"one data atom per value", []container.Tag{{Key: "ARTIST", Value: "One"}, {Key: "ARTIST", Value: "Two"}},
			makeBox("\xa9ART", text("One", "Two")), ""},
		{"a number holding its total", []container.Tag{{Key: "TRACKNUMBER", Value: "3/12"}},
			makeBox("trkn", dataAtom(0, []byte{0, 0, 0, 3, 0, 12, 0, 0})), ""},
		{"a number of a total, spelled out", []container.Tag{{Key: "DISCNUMBER", Value: "1 of 2"}},
			makeBox("disk", dataAtom(0, []byte{0, 0, 0, 1, 0, 2})), ""},
		{"a total on its own key wins", []container.Tag{{Key: "TRACKNUMBER", Value: "3/12"}, {Key: "TRACKTOTAL", Value: "14"}},
			makeBox("trkn", dataAtom(0, []byte{0, 0, 0, 3, 0, 14, 0, 0})), ""},
		{"a boolean in any spelling", []container.Tag{{Key: "COMPILATION", Value: " Yes "}},
			makeBox("cpil", dataAtom(21, []byte{1})), ""},
		{"a boolean that is false", []container.Tag{{Key: "ITUNESGAPLESS", Value: "No"}},
			makeBox("pgap", dataAtom(21, []byte{0})), ""},
		{"one byte", []container.Tag{{Key: "MEDIATYPE", Value: "2"}}, makeBox("stik", dataAtom(21, []byte{2})), ""},
		{"two bytes", []container.Tag{{Key: "MOVEMENTTOTAL", Value: "300"}}, makeBox("\xa9mvc", dataAtom(21, []byte{1, 44})), ""},
		{"a whole tempo, however spelled", []container.Tag{{Key: "BPM", Value: "0120.0"}},
			makeBox("tmpo", dataAtom(21, []byte{0, 120})), ""},
		{"a tempo with a fraction rounds into the atom", []container.Tag{{Key: "BPM", Value: "120.5"}},
			makeBox("tmpo", dataAtom(21, []byte{0, 121})), "----"},
		{"four bytes", []container.Tag{{Key: "EPISODENUMBER", Value: "70000"}},
			makeBox("tves", dataAtom(21, []byte{0, 1, 0x11, 0x70})), ""},
		{"an episode that is no plain decimal stays text", []container.Tag{{Key: "EPISODENUMBER", Value: "05"}},
			freeform("EPISODENUMBER", "05"), "tves"},
		{"a tagger's freeform spelling", []container.Tag{{Key: "MUSICBRAINZ_TRACKID", Value: "id"}},
			freeform("MusicBrainz Track Id", "id"), ""},
		{"ReplayGain in lowercase", []container.Tag{{Key: "REPLAYGAIN_TRACK_GAIN", Value: "-3.00 dB"}},
			freeform("replaygain_track_gain", "-3.00 dB"), ""},
		{"a key with no atom", []container.Tag{{Key: "CONDUCTOR", Value: "A"}, {Key: "CONDUCTOR", Value: "B"}},
			freeform("CONDUCTOR", "A", "B"), ""},
		{"an alias spelling goes to its key's atom", []container.Tag{{Key: "date", Value: "2020"}},
			makeBox("\xa9day", text("2020")), "----"},
		{"an encoder stamp is not the muxer's to copy", []container.Tag{{Key: "TITLE", Value: "T"},
			{Key: "ENCODER", Value: "SomeTool 1.0"}, {Key: "ENCODEDBY", Value: "Someone"}},
			makeBox("\xa9nam", text("T")), "Some"},
		{"a key that is not legal is skipped", []container.Tag{{Key: "TITLE", Value: "T"}, {Key: "BAD=KEY", Value: "nope"}},
			makeBox("\xa9nam", text("T")), "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, raw := range muxTagged(t, tc.tags) {
				form := [2]string{"fragmented", "progressive"}[i]
				if !bytes.Contains(raw, tc.want) {
					t.Errorf("%s: output lacks the atom % x", form, tc.want)
				}
				if tc.not != "" && bytes.Contains(raw, []byte(tc.not)) {
					t.Errorf("%s: output holds %q", form, tc.not)
				}
			}
		})
	}
}

// TestMuxedTagsIgnoreTheOrderHandedOver: the atoms are written in a fixed
// order, so the same tags in another order are the same bytes.
func TestMuxedTagsIgnoreTheOrderHandedOver(t *testing.T) {
	tags := tagList(tagVocabulary)
	want := muxTagged(t, tags)
	// Keys reversed, each key's values kept in order.
	var back []container.Tag
	for i := len(tags) - 1; i >= 0; {
		j := i
		for j > 0 && tags[j-1].Key == tags[i].Key {
			j--
		}
		back = append(back, tags[j:i+1]...)
		i = j - 1
	}
	got := muxTagged(t, back)
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("output %d differs when the tags are handed over in another order", i)
		}
	}
}

// TestMuxedValuesAnAtomCannotHoldStayText: an integer, boolean or pair atom
// holds one number of its width. Any other value of its key (text, a number
// past the width, a zero in a field where zero reads as absent, a fraction
// the atom would round, several values) is written as the freeform of the
// key's own name instead, which reads back as the same key. Every value
// handed over comes back, and none lands in an atom that would change it.
func TestMuxedValuesAnAtomCannotHoldStayText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tags  map[string][]string
		want  map[string][]string // nil: the tags as handed over
		atoms []string            // item atoms the output holds, "----" aside
	}{
		{"a tempo in words", map[string][]string{"BPM": {"fast"}}, nil, nil},
		{"a tempo past sixteen bits", map[string][]string{"BPM": {"70000"}}, nil, nil},
		{"two tempos", map[string][]string{"BPM": {"120", "121"}}, nil, nil},
		{"a media type by name", map[string][]string{"MEDIATYPE": {"Audiobook"}}, nil, nil},
		{"a media type past a byte", map[string][]string{"MEDIATYPE": {"300"}}, nil, nil},
		{"two media types", map[string][]string{"MEDIATYPE": {"1", "2"}}, nil, nil},
		{"an advisory by name", map[string][]string{"ITUNESADVISORY": {"explicit"}}, nil, nil},
		{"a movement past sixteen bits", map[string][]string{"MOVEMENT": {"70000"}}, nil, nil},
		{"a movement total in words", map[string][]string{"MOVEMENTTOTAL": {"four"}}, nil, nil},
		{"a compilation flag that is no boolean", map[string][]string{"COMPILATION": {"maybe"}}, nil, nil},
		{"a gapless flag that is no boolean", map[string][]string{"ITUNESGAPLESS": {"maybe"}}, nil, nil},
		{"a movement flag that is a number", map[string][]string{"SHOWMOVEMENT": {"2"}}, nil, nil},
		{"two compilation flags", map[string][]string{"COMPILATION": {"1", "0"}}, nil, nil},
		{"an episode with a leading zero", map[string][]string{"EPISODENUMBER": {"05"}}, nil, nil},
		{"a season in words", map[string][]string{"SEASONNUMBER": {"two"}}, nil, nil},
		{"a track number in text", map[string][]string{"TRACKNUMBER": {"A1"}}, nil, nil},
		{"a track number past sixteen bits", map[string][]string{"TRACKNUMBER": {"70000"}}, nil, nil},
		{"a track number of zero", map[string][]string{"TRACKNUMBER": {"0"}}, nil, nil},
		{"two track numbers", map[string][]string{"TRACKNUMBER": {"3", "4"}}, nil, nil},
		{"a track total in words", map[string][]string{"TRACKTOTAL": {"twelve"}}, nil, nil},
		{"two track totals", map[string][]string{"TRACKTOTAL": {"12", "13"}}, nil, nil},
		{"a disc number in text", map[string][]string{"DISCNUMBER": {"A"}}, nil, nil},
		{"a disc total past sixteen bits", map[string][]string{"DISCTOTAL": {"70000"}}, nil, nil},
		// A value some tagger repeated is one value, compared as the atom
		// stores it, and keeps its atom. (Values that differ, above, do not:
		// they go whole to the freeform.)
		{"a track number given twice", map[string][]string{"TRACKNUMBER": {"3", "3"}},
			map[string][]string{"TRACKNUMBER": {"3"}}, []string{"trkn"}},
		{"a track number in two spellings", map[string][]string{"TRACKNUMBER": {"3", "03"}, "TRACKTOTAL": {"12", " 12"}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}, []string{"trkn"}},
		{"a disc pair given twice", map[string][]string{"DISCNUMBER": {"1/2", "1/2"}},
			map[string][]string{"DISCNUMBER": {"1"}, "DISCTOTAL": {"2"}}, []string{"disk"}},
		{"a compilation flag given twice", map[string][]string{"COMPILATION": {"1", "true"}},
			map[string][]string{"COMPILATION": {"1"}}, []string{"cpil"}},
		{"a tempo given twice", map[string][]string{"BPM": {"120", "120.0"}},
			map[string][]string{"BPM": {"120"}}, []string{"tmpo"}},
		{"a media type given twice", map[string][]string{"MEDIATYPE": {"2", "2"}},
			map[string][]string{"MEDIATYPE": {"2"}}, []string{"stik"}},
		{"an episode given twice", map[string][]string{"EPISODENUMBER": {"5", "5"}},
			map[string][]string{"EPISODENUMBER": {"5"}}, []string{"tves"}},
		{"text given twice", map[string][]string{"BPM": {"fast", "fast"}}, map[string][]string{"BPM": {"fast"}}, nil},
		// A number with and without its total is one value too, the one
		// that states the total, where no total on its own key disagrees.
		{"a number beside itself with a total", map[string][]string{"TRACKNUMBER": {"3", "3/12"}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}, []string{"trkn"}},
		{"the same the other way round", map[string][]string{"DISCNUMBER": {"1/2", "01", "1 of 2"}},
			map[string][]string{"DISCNUMBER": {"1"}, "DISCTOTAL": {"2"}}, []string{"disk"}},
		{"beside a total that agrees", map[string][]string{"TRACKNUMBER": {"3", "3/12"}, "TRACKTOTAL": {"12"}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}}, []string{"trkn"}},
		// Numbers or totals that really differ stay whole in the freeform,
		// which both readers split as they split any pair.
		{"beside a total that disagrees", map[string][]string{"TRACKNUMBER": {"3", "3/12"}, "TRACKTOTAL": {"14"}},
			map[string][]string{"TRACKNUMBER": {"3", "3"}, "TRACKTOTAL": {"14"}}, []string{"trkn"}},
		{"totals that differ", map[string][]string{"TRACKNUMBER": {"3/12", "3/13"}},
			map[string][]string{"TRACKNUMBER": {"3", "3"}, "TRACKTOTAL": {"12", "13"}}, nil},
		{"a total of zero is not a blank one", map[string][]string{"TRACKNUMBER": {"3", "3/0"}},
			map[string][]string{"TRACKNUMBER": {"3", "3"}, "TRACKTOTAL": {"0"}}, nil},
		// The half a pair atom can hold stays in it.
		{"a text number beside a total", map[string][]string{"TRACKNUMBER": {"A1"}, "TRACKTOTAL": {"12"}}, nil, []string{"trkn"}},
		{"a number beside a text total", map[string][]string{"DISCNUMBER": {"1"}, "DISCTOTAL": {"two"}}, nil, []string{"disk"}},
		{"a number holding a total past sixteen bits", map[string][]string{"TRACKNUMBER": {"3/70000"}},
			map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"70000"}}, []string{"trkn"}},
		{"a number past sixteen bits holding its total", map[string][]string{"TRACKNUMBER": {"70000/12"}},
			map[string][]string{"TRACKNUMBER": {"70000"}, "TRACKTOTAL": {"12"}}, []string{"trkn"}},
		{"several of them at once", map[string][]string{"BPM": {"fast"}, "MEDIATYPE": {"Audiobook"}, "TRACKNUMBER": {"A1"},
			"TITLE": {"T"}, "MOVEMENT": {"2"}}, nil, []string{"\xa9nam", "\xa9mvi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == nil {
				want = tc.tags
			}
			tags := tagList(tc.tags)
			if dropped := DroppedTags(tags); len(dropped) != 0 {
				t.Errorf("DroppedTags = %q, want none", dropped)
			}
			for i, raw := range muxTagged(t, tags) {
				form := [2]string{"fragmented", "progressive"}[i]
				d := openFx(t, raw)
				if got := d.Tags(); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
					t.Errorf("%s: read back %q, want %q", form, got, want)
				}
				if got := ilstAtoms(t, raw); !slices.Equal(got, tc.atoms) {
					t.Errorf("%s: the values went to the atoms %q, want %q beside the freeforms", form, got, tc.atoms)
				}
			}
		})
	}
}

// ilstAtoms lists the items of a muxed movie's ilst by atom name, freeforms
// left out.
func ilstAtoms(t *testing.T, raw []byte) []string {
	t.Helper()
	udta := findBoxForTest(t, findBoxForTest(t, raw, "moov"), "udta")
	meta := findBoxForTest(t, udta, "meta")
	var out []string
	_ = walkBoxes(findBoxForTest(t, meta[4:], "ilst"), func(typ string, _ []byte) error {
		if typ != "----" {
			out = append(out, typ)
		}
		return nil
	})
	return out
}

// TestMuxedAliasSpellingsFoldOnce: a key handed over in its canonical
// spelling takes that spelling's values, and an alias or another case of it
// beside it is ignored. With no canonical spelling the others fold in the
// order of their names, each distinct value once. Either way the bytes do
// not depend on the order the tags were handed over in.
func TestMuxedAliasSpellingsFoldOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []container.Tag
		want map[string][]string
	}{
		{"an alias beside its key, same value", []container.Tag{{Key: "DATE", Value: "2020"}, {Key: "RECORDINGDATE", Value: "2020"}},
			map[string][]string{"RECORDINGDATE": {"2020"}}},
		{"an alias beside its key, another value", []container.Tag{{Key: "YEAR", Value: "2020"}, {Key: "RECORDINGDATE", Value: "2020-05-06"}},
			map[string][]string{"RECORDINGDATE": {"2020-05-06"}}},
		{"another case beside the key", []container.Tag{{Key: "Title", Value: "y"}, {Key: "TITLE", Value: "x"}},
			map[string][]string{"TITLE": {"x"}}},
		{"an alias of a number beside it", []container.Tag{{Key: "TRACK", Value: "5"}, {Key: "TRACKNUMBER", Value: "3"}},
			map[string][]string{"TRACKNUMBER": {"3"}}},
		{"two aliases that agree", []container.Tag{{Key: "YEAR", Value: "2020"}, {Key: "DATE", Value: "2020"}},
			map[string][]string{"RECORDINGDATE": {"2020"}}},
		{"two aliases that differ", []container.Tag{{Key: "YEAR", Value: "2020"}, {Key: "DATE", Value: "2020-05-06"}},
			map[string][]string{"RECORDINGDATE": {"2020-05-06", "2020"}}},
		{"a key repeated is still several values", []container.Tag{{Key: "ARTIST", Value: "x"}, {Key: "ARTIST", Value: "x"}},
			map[string][]string{"ARTIST": {"x", "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back := slices.Clone(tc.tags)
			slices.Reverse(back)
			first, second := muxTagged(t, tc.tags), muxTagged(t, back)
			for i := range first {
				if got := openFx(t, first[i]).Tags(); !maps.EqualFunc(got, tc.want, slices.Equal[[]string]) {
					t.Errorf("output %d reads back %q, want %q", i, got, tc.want)
				}
				if !bytes.Equal(first[i], second[i]) {
					t.Errorf("output %d differs when the tags are handed over the other way round", i)
				}
			}
		})
	}
}

// TestDroppedTagsNamesWhatIsNotWritten: the keys the muxer leaves out are
// the ones DroppedTags names, and nothing of them reaches the file.
func TestDroppedTagsNamesWhatIsNotWritten(t *testing.T) {
	tags := []container.Tag{
		{Key: "TITLE", Value: "T"},
		{Key: "BAD=KEY", Value: "left out 1"},
		{Key: "ENCODER", Value: "left out 2"},
		{Key: "encoded_by", Value: "left out 3"},
		// A custom key spelled as a tagger's freeform name in another case
		// would read back as that name's key.
		{Key: "MUSICBRAINZ TRACK ID", Value: "left out 4"},
		{Key: "BAD=KEY", Value: "left out 5"},
		{Key: "CUSTOMFOO", Value: "kept"},
	}
	if got, want := DroppedTags(tags), []string{"BAD=KEY", "ENCODER", "encoded_by", "MUSICBRAINZ TRACK ID"}; !slices.Equal(got, want) {
		t.Errorf("DroppedTags = %q, want %q", got, want)
	}
	for i, raw := range muxTagged(t, tags) {
		if bytes.Contains(raw, []byte("left out")) {
			t.Errorf("output %d holds a value of a key it drops", i)
		}
		want := map[string][]string{"TITLE": {"T"}, "CUSTOMFOO": {"kept"}}
		if got := openFx(t, raw).Tags(); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("output %d reads back %q, want %q", i, got, want)
		}
	}
	if got := DroppedTags(tagList(tagVocabulary)); len(got) != 0 {
		t.Errorf("DroppedTags names %q of a vocabulary that is all written", got)
	}
}
