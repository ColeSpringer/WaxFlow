package oracletest

// The MP4 tag round trip. Every MP4 output form embeds its own tags and no
// post-pass follows, so the muxer's vocabulary is the whole of what an aac,
// he-aac or alac file keeps of its source. The tag library is the reader
// that says whether a key came through under the name it went in by.

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

// plausibleTagValues is one value for a canonical key, or two for a
// multi-valued one, of the shape the key holds: the integer and boolean
// atoms store only numbers, and a tempo only a whole one unrounded.
func plausibleTagValues(key tag.Key) []string {
	switch key {
	case tag.TrackNumber, tag.EpisodeNumber:
		return []string{"3"}
	case tag.TrackTotal:
		return []string{"12"}
	case tag.DiscNumber, tag.ITunesAdvisory, tag.Compilation, tag.ITunesGapless, tag.ShowMovement:
		return []string{"1"}
	case tag.DiscTotal, tag.MediaType, tag.Movement, tag.SeasonNumber:
		return []string{"2"}
	case tag.MovementTotal:
		return []string{"4"}
	case tag.BPM:
		return []string{"120"}
	case tag.RecordingDate:
		return []string{"2020-05-06"}
	case tag.ReleaseDate:
		return []string{"2020-07-08"}
	case tag.OriginalDate:
		return []string{"1999-03-04"}
	case tag.ReplayGainTrackGain, tag.ReplayGainAlbumGain:
		return []string{"-3.00 dB"}
	case tag.ReplayGainTrackPeak, tag.ReplayGainAlbumPeak:
		return []string{"0.900000"}
	}
	if key.Multivalued() {
		return []string{"first " + string(key), "second " + string(key)}
	}
	return []string{"value of " + string(key)}
}

// TestMP4OutputKeepsEveryKnownKey muxes every key of the tag library's
// vocabulary, and a custom one, into each MP4 output form through the
// engine. The library must read back exactly what went in, and the
// container's own reader the same.
//
// The two encoder stamps are the exception: an output is stamped by what
// encoded it, so the muxer leaves a handed-over ENCODER and ENCODEDBY out.
func TestMP4OutputKeepsEveryKnownKey(t *testing.T) {
	want := map[string][]string{"CUSTOMFOO": {"bar"}}
	for _, key := range tag.KnownKeys() {
		want[string(key)] = plausibleTagValues(key)
	}
	var tags []container.Tag
	for _, key := range slices.Sorted(maps.Keys(want)) {
		for _, v := range want[key] {
			tags = append(tags, container.Tag{Key: key, Value: v})
		}
	}
	delete(want, string(tag.Encoder))
	delete(want, string(tag.EncodedBy))

	wav := rampWAV(t, 44100, 2, 8192)
	for _, form := range []struct{ format, container string }{
		{"aac", "progressive"}, {"aac", "fragmented"},
		{"alac", "progressive"}, {"alac", "fragmented"},
		{"he-aac", "progressive"},
	} {
		t.Run(form.format+"/"+form.container, func(t *testing.T) {
			raw := mp4Transcode(t, wav, form.format, form.container, waxflow.WithTags(tags))
			doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
			if err != nil {
				t.Fatalf("waxlabel.Parse: %v", err)
			}
			got := map[string][]string{}
			for k, vs := range doc.Tags().All() {
				got[string(k)] = vs
			}
			for _, key := range slices.Sorted(maps.Keys(want)) {
				if !slices.Equal(got[key], want[key]) {
					t.Errorf("%s: the tag library reads %q, want %q", key, got[key], want[key])
				}
			}
			for _, key := range slices.Sorted(maps.Keys(got)) {
				if _, ok := want[key]; !ok {
					t.Errorf("the tag library reads %s = %q, which nothing wrote", key, got[key])
				}
			}
			if native := mp4TagsEqual(t, raw); len(native) != len(want) {
				t.Errorf("the container reads %d keys of the %d written", len(native), len(want))
			}
		})
	}
}

// TestMP4OutputSplitsANumberHoldingItsTotal: a track number handed over as
// "3/12", as an embedder with no mapper hands a Vorbis comment's, fills
// both fields of the trkn atom.
func TestMP4OutputSplitsANumberHoldingItsTotal(t *testing.T) {
	raw := mp4Transcode(t, rampWAV(t, 44100, 2, 8192), "alac", "progressive", waxflow.WithTags([]container.Tag{
		{Key: "TRACKNUMBER", Value: "3/12"}, {Key: "DISCNUMBER", Value: "1 of 2"},
	}))
	want := map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"}, "DISCTOTAL": {"2"}}
	if got := mp4TagsEqual(t, raw); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

// TestMP4OutputKeepsValuesAnAtomCannotHold: an integer, boolean or pair atom
// holds one number of its width, and MP4 has no post-pass to report what a
// muxer left out. A value such an atom cannot hold is written as the
// freeform of its key's own name, and both readers must give it back under
// that key unchanged.
func TestMP4OutputKeepsValuesAnAtomCannotHold(t *testing.T) {
	wav := rampWAV(t, 44100, 2, 8192)
	for key, values := range map[string][][]string{
		"BPM":            {{"fast"}, {"70000"}, {"120", "121"}},
		"MEDIATYPE":      {{"Audiobook"}, {"300"}, {"1", "2"}},
		"ITUNESADVISORY": {{"explicit"}, {"300"}},
		"COMPILATION":    {{"maybe"}, {"2"}},
		"ITUNESGAPLESS":  {{"maybe"}},
		"SHOWMOVEMENT":   {{"maybe"}},
		"MOVEMENT":       {{"70000"}, {"second"}},
		"MOVEMENTTOTAL":  {{"70000"}, {"four"}},
		"EPISODENUMBER":  {{"05"}, {"five"}},
		"SEASONNUMBER":   {{"two"}},
		"TRACKNUMBER":    {{"A1"}, {"70000"}, {"0"}, {"3", "4"}},
		"TRACKTOTAL":     {{"twelve"}, {"70000"}, {"0"}},
		"DISCNUMBER":     {{"A"}, {"70000"}, {"0"}},
		"DISCTOTAL":      {{"two"}, {"70000"}},
	} {
		for _, vs := range values {
			t.Run(key+"/"+strings.Join(vs, ","), func(t *testing.T) {
				var tags []container.Tag
				for _, v := range vs {
					tags = append(tags, container.Tag{Key: key, Value: v})
				}
				for _, form := range []string{"progressive", "fragmented"} {
					raw := mp4Transcode(t, wav, "alac", form, waxflow.WithTags(tags))
					want := map[string][]string{key: vs}
					if got := mp4TagsEqual(t, raw); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
						t.Errorf("%s: both readers give %q, want %q", form, got, want)
					}
				}
			})
		}
	}
	// The half of a pair its atom can hold stays there, and the other half
	// is still its own key.
	t.Run("pairs", func(t *testing.T) {
		raw := mp4Transcode(t, wav, "alac", "progressive", waxflow.WithTags([]container.Tag{
			{Key: "TRACKNUMBER", Value: "A1"}, {Key: "TRACKTOTAL", Value: "12"},
			{Key: "DISCNUMBER", Value: "1/70000"},
		}))
		want := map[string][]string{"TRACKNUMBER": {"A1"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"}, "DISCTOTAL": {"70000"}}
		if got := mp4TagsEqual(t, raw); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("both readers give %q, want %q", got, want)
		}
	})
}

// TestMP4OutputKeepsTheAtomOfARepeatedValue: a source two taggers wrote
// holds a number or a flag twice. Given twice it is one value, however each
// was spelled and whether or not each states the total, and it stays in the
// atom players read: ffprobe, which knows
// the atoms and none of the freeforms, still sees the track, the disc and
// the compilation flag. A tempo with a fraction is rounded into its atom.
func TestMP4OutputKeepsTheAtomOfARepeatedValue(t *testing.T) {
	testutil.FFprobe(t)
	tags := []container.Tag{
		{Key: "TRACKNUMBER", Value: "3"}, {Key: "TRACKNUMBER", Value: "03"}, {Key: "TRACKNUMBER", Value: "3/12"},
		{Key: "DISCNUMBER", Value: "1/2"}, {Key: "DISCNUMBER", Value: "1/2"},
		{Key: "COMPILATION", Value: "1"}, {Key: "COMPILATION", Value: "true"},
		{Key: "BPM", Value: "120.5"}, {Key: "BPM", Value: "120.5"},
	}
	want := map[string][]string{"TRACKNUMBER": {"3"}, "TRACKTOTAL": {"12"}, "DISCNUMBER": {"1"}, "DISCTOTAL": {"2"},
		"COMPILATION": {"1"}, "BPM": {"121"}}
	raw := mp4Transcode(t, rampWAV(t, 44100, 2, 8192), "alac", "progressive", waxflow.WithTags(tags))
	if got := mp4TagsEqual(t, raw); !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("both readers give %q, want %q", got, want)
	}
	path := filepath.Join(t.TempDir(), "out.m4a")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(testutil.FFprobe(t), "-v", "error", "-show_entries", "format_tags", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var doc struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("ffprobe output: %v", err)
	}
	// ffprobe lists no tempo, so tmpo is held to our two readers above.
	for key, value := range map[string]string{"track": "3/12", "disc": "1/2", "compilation": "1"} {
		if got := doc.Format.Tags[key]; got != value {
			t.Errorf("ffprobe reads %s = %q, want %q (it reads %q)", key, got, value, doc.Format.Tags)
		}
	}
}

// mp4Transcode runs wav through the engine to an MP4 output form and
// returns the file.
func mp4Transcode(t *testing.T, wav []byte, format, form string, run ...waxflow.RunOption) []byte {
	t.Helper()
	opts := waxflow.TranscodeOptions{Format: format, Container: form}
	src := container.BytesSource(wav)
	if form == "fragmented" {
		var out bytes.Buffer
		if _, err := waxflow.New().Transcode(context.Background(), src, "wav", &out, opts, run...); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	path := filepath.Join(t.TempDir(), "out.m4a")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waxflow.New().Transcode(context.Background(), src, "wav", f, opts, run...); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
