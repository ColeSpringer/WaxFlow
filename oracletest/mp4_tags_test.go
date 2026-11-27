package oracletest

// The MP4 tag agreement cells. On a mapper-wired route both readers speak
// for a source's tags: the demuxer's are the floor and the tag library's
// win per key, so a store or a spelling only one of them reads surfaces as
// two tags, or as a tag the probe reports and a transcode drops.

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxlabel/tag"
)

// mp4TagsEqual requires both readers to give the same tags, and returns
// them.
func mp4TagsEqual(t *testing.T, raw []byte) map[string][]string {
	t.Helper()
	native, mapped := mp4Readers(t, raw)
	if !maps.EqualFunc(native.Tags, mapped.Tags, slices.Equal[[]string]) {
		t.Errorf("the container read %q, the tag library %q", native.Tags, mapped.Tags)
	}
	return native.Tags
}

// TestMP4TagStoresAgreeAcrossReaders covers where a movie keeps tags and
// what it repeats: duplicate stores, a meta directly under moov, a keys
// store, classic QuickTime text atoms, and the item shapes a strict reader
// drops.
func TestMP4TagStoresAgreeAcrossReaders(t *testing.T) {
	const itunes = "com.apple.iTunes"
	first := mp4Meta(mp4Item("\xa9nam", "Song"), mp4Item("\xa9alb", "Album"))
	second := mp4Meta(mp4Item("\xa9nam", "SecondTitle"), mp4Item("\xa9alb", "Album"), mp4Item("\xa9ART", "Only here"))
	keys := mp4KeysMeta("title", "KeysTitle", "com.apple.quicktime.artist", "KeysArtist", "album_artist", "AA",
		"track", "3/12", "disc", "1", "sort_name", "Sorted", "gapless_playback", "1", "DATE", "2020", "custom.thing", "Custom")
	keysWant := map[string][]string{"TITLE": {"KeysTitle"}, "ARTIST": {"KeysArtist"}, "ALBUMARTIST": {"AA"},
		"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"}, "TITLESORT": {"Sorted"},
		"ITUNESGAPLESS": {"1"}, "RECORDINGDATE": {"2020"}, "CUSTOM.THING": {"Custom"}}
	qtText := func(atom, text string) []byte {
		return mp4Box(atom, mp4U16(uint16(len(text))), mp4U16(0x55C4), []byte(text))
	}
	for _, tc := range []struct {
		name  string
		movie mp4Movie
		want  map[string][]string
	}{
		{"two udta", mp4Movie{udta: [][]byte{first, second}}, map[string][]string{"TITLE": {"Song"}, "ALBUM": {"Album"}}},
		{"two meta", mp4Movie{udta: [][]byte{mp4Join(first, second)}}, map[string][]string{"TITLE": {"Song"}, "ALBUM": {"Album"}}},
		{"two ilst", mp4Movie{udta: [][]byte{mp4FullBox("meta",
			mp4FullBox("hdlr", mp4U32(0), []byte("mdir"), []byte("appl"), mp4U32(0), mp4U32(0), []byte{0}),
			mp4Box("ilst", mp4Item("\xa9nam", "Song")), mp4Box("ilst", mp4Item("\xa9nam", "SecondTitle")))}},
			map[string][]string{"TITLE": {"Song"}}},
		{"two items of one atom", mp4Movie{udta: [][]byte{mp4Meta(mp4Item("\xa9nam", "Song"), mp4Item("\xa9nam", "SecondTitle"))}},
			map[string][]string{"TITLE": {"Song", "SecondTitle"}}},
		{"two data atoms in one item", mp4Movie{udta: [][]byte{mp4Meta(mp4Box("\xa9ART", mp4Data(1, []byte("One")), mp4Data(1, []byte("Two"))))}},
			map[string][]string{"ARTIST": {"One", "Two"}}},
		{"a meta under moov", mp4Movie{meta: first}, map[string][]string{"TITLE": {"Song"}, "ALBUM": {"Album"}}},
		{"a meta under moov beside one under udta", mp4Movie{meta: second, udta: [][]byte{mp4Meta(mp4Item("\xa9nam", "Song"))}},
			map[string][]string{"TITLE": {"Song"}, "ALBUM": {"Album"}, "ARTIST": {"Only here"}}},
		{"a number with no total, and implicit text", mp4Movie{udta: [][]byte{mp4Meta(
			mp4Box("\xa9ART", mp4Data(0, []byte("ImplicitArtist"))),
			mp4Box("trkn", mp4Data(0, []byte{0, 0, 0, 7})),
			mp4Box("disk", mp4Data(0, []byte{0, 0, 0, 2, 0, 5})))}},
			map[string][]string{"ARTIST": {"ImplicitArtist"}, "TRACKNUMBER": {"7"}, "DISCNUMBER": {"2"}, "DISCTOTAL": {"5"}}},
		{"a keys store under udta", mp4Movie{udta: [][]byte{keys}}, keysWant},
		{"a keys store under moov", mp4Movie{meta: keys}, keysWant},
		{"classic udta text atoms", mp4Movie{udta: [][]byte{mp4Join(qtText("\xa9nam", "MovTitle"), qtText("\xa9ART", "MovArtist"),
			qtText("\xa9swr", "A Tool"), qtText("\xa9nam", "A second title atom"))}},
			map[string][]string{"TITLE": {"MovTitle"}, "ARTIST": {"MovArtist"}, "ENCODER": {"A Tool"}}},
		{"udta text atoms beside an item list", mp4Movie{udta: [][]byte{mp4Join(qtText("\xa9nam", "MovTitle"), qtText("\xa9ART", "MovArtist"),
			mp4Meta(mp4Item("\xa9nam", "Song")))}}, map[string][]string{"TITLE": {"Song"}, "ARTIST": {"MovArtist"}}},
		{"freeforms", mp4Movie{udta: [][]byte{mp4Meta(
			mp4Freeform(itunes, "replaygain_track_gain", "-3.00 dB"),
			mp4Freeform(itunes, "REPLAYGAIN_TRACK_PEAK", "0.900000"),
			mp4Freeform("org.example", "REPLAYGAIN_ALBUM_GAIN", "-9.99 dB"),
			mp4Box("----", mp4FullBox("mean", []byte(itunes)), mp4FullBox("name", []byte("REPLAYGAIN_ALBUM_PEAK")), mp4Data(21, []byte{0, 1})),
			mp4Freeform(itunes, "MusicBrainz Track Id", "mbid"),
			mp4Freeform(itunes, "CUSTOMFOO", "bar"),
			mp4Freeform(itunes, "TRACKNUMBER", "5/9"),
			mp4Freeform(itunes, "iTunNORM", " 0000 0000"))}},
			map[string][]string{"REPLAYGAIN_TRACK_GAIN": {"-3.00 dB"}, "REPLAYGAIN_TRACK_PEAK": {"0.900000"},
				"MUSICBRAINZ_TRACKID": {"mbid"}, "CUSTOMFOO": {"bar"}, "TRACKNUMBER": {"5"}, "TRACKTOTAL": {"9"}}},
		{"two atoms of one key", mp4Movie{udta: [][]byte{mp4Meta(mp4Item("\xa9day", "2020-05-06"), mp4Freeform(itunes, "DATE", "1999-01-01"))}},
			map[string][]string{"RECORDINGDATE": {"2020-05-06"}}},
		{"a year ahead of its date", mp4Movie{udta: [][]byte{mp4Meta(mp4Freeform(itunes, "originalyear", "1999"), mp4Freeform(itunes, "originaldate", "1999-03-04"))}},
			map[string][]string{"ORIGINALDATE": {"1999-03-04"}}},
		{"integer and genre atoms", mp4Movie{udta: [][]byte{mp4Meta(
			mp4Box("cpil", mp4Data(21, []byte{1})), mp4Box("pgap", mp4Data(0, []byte{0})), mp4Box("stik", mp4Data(21, []byte{2})),
			mp4Box("tmpo", mp4Data(21, []byte{0, 120})), mp4Box("tvsn", mp4Data(21, []byte{0, 0, 0, 2})),
			mp4Box("gnre", mp4Data(0, []byte{0, 18})), mp4Box("shwm", mp4Data(1, []byte("0"))))}},
			map[string][]string{"COMPILATION": {"1"}, "ITUNESGAPLESS": {"0"}, "MEDIATYPE": {"2"}, "BPM": {"120"},
				"SEASONNUMBER": {"2"}, "GENRE": {"Rock"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mp4TagsEqual(t, tc.movie.build())
			if !maps.EqualFunc(got, tc.want, slices.Equal[[]string]) {
				t.Errorf("tags = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMP4TerminatedValuesAddNoKey: the demuxer sheds the NUL some writers
// end a value with, which the tag library keeps. That may change a value
// and must never change which keys there are: a number with a NUL behind
// its total is no pair to the library, so it is none here, and a year with
// one behind it echoes no date.
func TestMP4TerminatedValuesAddNoKey(t *testing.T) {
	const itunes = "com.apple.iTunes"
	for _, tc := range []struct {
		name  string
		movie mp4Movie
		want  map[string][]string
	}{
		{"a freeform number", mp4Movie{udta: [][]byte{mp4Meta(mp4Freeform(itunes, "TRACKNUMBER", "3/12\x00"),
			mp4Freeform(itunes, "DISCNUMBER", "1 of 2\x00"))}}, map[string][]string{"TRACKNUMBER": {"3/12"}, "DISCNUMBER": {"1 of 2"}}},
		{"a keys-store number", mp4Movie{meta: mp4KeysMeta("track", "3/12\x00")}, map[string][]string{"TRACKNUMBER": {"3/12"}}},
		{"a year ahead of a date", mp4Movie{udta: [][]byte{mp4Meta(mp4Item("\xa9day", "2020\x00"), mp4Freeform(itunes, "DATE", "2020-05-01"))}},
			map[string][]string{"RECORDINGDATE": {"2020"}}},
		{"a terminated title", mp4Movie{udta: [][]byte{mp4Meta(mp4Item("\xa9nam", "Song\x00"))}}, map[string][]string{"TITLE": {"Song"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native, mapped := mp4Readers(t, tc.movie.build())
			if !maps.EqualFunc(native.Tags, tc.want, slices.Equal[[]string]) {
				t.Errorf("the container read %q, want %q", native.Tags, tc.want)
			}
			if got, want := slices.Sorted(maps.Keys(native.Tags)), slices.Sorted(maps.Keys(mapped.Tags)); !slices.Equal(got, want) {
				t.Errorf("the container read the keys %q, the tag library %q", got, want)
			}
			for key, vs := range mapped.Tags {
				for i, v := range vs {
					if i < len(native.Tags[key]) && strings.TrimRight(v, "\x00") != native.Tags[key][i] {
						t.Errorf("%s: the container read %q, the tag library %q", key, native.Tags[key], vs)
					}
				}
			}
		})
	}
}

// TestMP4TagNamesFoldAsTheTagLibraryFolds holds the demuxer's name tables
// to the library's, key by key: every canonical key and every alias
// spelling of one, in either case, as a freeform name and as a keys-store
// name. A spelling the library learns and the demuxer does not fails here.
func TestMP4TagNamesFoldAsTheTagLibraryFolds(t *testing.T) {
	const itunes = "com.apple.iTunes"
	value := func(key tag.Key) string {
		if key == tag.TrackNumber || key == tag.DiscNumber {
			return "3"
		}
		return "v"
	}
	for _, key := range tag.KnownKeys() {
		names := append([]string{string(key)}, tag.KeyAliases(key)...)
		for _, name := range names {
			for _, spelled := range []string{name, strings.ToLower(name)} {
				t.Run(string(key)+"/"+spelled, func(t *testing.T) {
					got := mp4TagsEqual(t, mp4Movie{udta: [][]byte{mp4Meta(mp4Freeform(itunes, spelled, value(key)))}}.build())
					// A canonical key in lowercase is no key and names none:
					// only an alias, or the key as written, has to be read.
					if spelled == name || name != string(key) {
						if !slices.Equal(got[string(key)], []string{value(key)}) {
							t.Errorf("a freeform named %q read %q, want it under %s", spelled, got, key)
						}
					}
					got = mp4TagsEqual(t, mp4Movie{meta: mp4KeysMeta(spelled, value(key))}.build())
					if len(got) != 1 {
						t.Errorf("a keys entry named %q read %q, want one key", spelled, got)
					}
				})
			}
		}
	}
}

// TestMP4NamesMP2AsTheTagLibraryDoes: an mp4a object type says MPEG audio
// and no layer. Layer II is named off the first frame by both readers, and
// refused here, where there is no decoder for it, before a transcode starts.
func TestMP4NamesMP2AsTheTagLibraryDoes(t *testing.T) {
	testutil.FFmpegEncoder(t, "mp2")
	path := filepath.Join(t.TempDir(), "mp2.mp4")
	testutil.FFmpegGenerate(t, path, 44100, 1, "mp2", "-f", "mp4")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name := waxlabelTrack(t, raw).Codec
	if name != "MP2" {
		t.Fatalf("waxlabel names the codec %q, want MP2", name)
	}
	_, err = waxflow.New().Probe(context.Background(), container.BytesSource(raw), "", nil)
	if err == nil {
		t.Fatal("waxflow opened MP2 in MP4; this build has no Layer II decoder")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("waxflow refuses with %v, which does not carry waxlabel's name %q", err, name)
	}
}

// TestMP4NamesMP2InAFragmentedMovie: the same refusal where the first frame
// sits in a fragment, or in the table a fragmented movie's moov still holds.
func TestMP4NamesMP2InAFragmentedMovie(t *testing.T) {
	testutil.FFmpegEncoder(t, "mp2")
	for _, flags := range []string{"frag_keyframe", "frag_keyframe+empty_moov"} {
		t.Run(flags, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mp2.mp4")
			testutil.FFmpegGenerateDuration(t, path, 2, 44100, 1, "mp2", "-movflags", flags, "-f", "mp4")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = waxflow.New().Probe(context.Background(), container.BytesSource(raw), "", nil)
			if err == nil || !strings.Contains(err.Error(), "MP2") {
				t.Errorf("waxflow answers %v, want a refusal naming MP2", err)
			}
			// The library names the layer off the moov's table, which an
			// empty_moov movie does not have.
			if name := waxlabelTrack(t, raw).Codec; name != "MP2" && flags == "frag_keyframe" {
				t.Errorf("waxlabel names the codec %q, want MP2", name)
			}
		})
	}
}

// TestMP4NamesMP2BehindAnotherTracksMoof: a movie that gives each track its
// own moofs opens with the video's, so the sound track's first frame is in
// a later one. The layer is still read there, and the refusal still comes
// at open.
func TestMP4NamesMP2BehindAnotherTracksMoof(t *testing.T) {
	testutil.FFmpegEncoder(t, "mp2")
	testutil.FFmpegEncoder(t, "mpeg4")
	for _, flags := range []string{"frag_keyframe+empty_moov+separate_moof", "frag_keyframe+empty_moov+separate_moof+default_base_moof"} {
		t.Run(flags, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "av.mp4")
			cmd := exec.Command(testutil.FFmpeg(t), "-v", "error", "-y",
				"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=2",
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=2",
				"-c:v", "mpeg4", "-c:a", "mp2", "-movflags", flags, "-f", "mp4", path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ffmpeg: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = waxflow.New().Probe(context.Background(), container.BytesSource(raw), "", nil)
			if err == nil || !strings.Contains(err.Error(), "MP2") {
				t.Errorf("waxflow answers %v, want a refusal naming MP2", err)
			}
		})
	}
}

// TestMP4BehindLeadingPaddingIsRecognized: an ftyp behind a free box is
// sniffed as MP4 with no extension to go by, as the tag library sniffs it.
func TestMP4BehindLeadingPaddingIsRecognized(t *testing.T) {
	raw := mp4Movie{lead: mp4Box("free", make([]byte, 24)), udta: [][]byte{mp4Meta(mp4Item("\xa9nam", "Song"))}}.build()
	if got := mp4TagsEqual(t, raw); !slices.Equal(got["TITLE"], []string{"Song"}) {
		t.Errorf("tags = %q, want the title", got)
	}
}
