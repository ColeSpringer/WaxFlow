package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/server"
)

// writeTaggedFragmentedMP4 builds the source these tests need: a fragmented
// (CMAF) MP4 carrying a full ilst. It is the shape the tag library refuses to
// parse, so every tag that reaches an output from it came off the container,
// which is what B1 added and what these tests pin. The engine writes it
// directly because the tag set has to be chosen here, which no flag does.
func writeTaggedFragmentedMP4(t *testing.T, in, path string, tags []container.Tag, art *container.Picture) {
	t.Helper()
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	_, err = waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", out,
		waxflow.TranscodeOptions{Format: "aac", Tags: tags, Art: art})
	if err != nil {
		t.Fatal(err)
	}
}

// hasMovieFragments reports whether an MP4 carries movie fragments, which is
// what tells the CMAF form from the flat one.
func hasMovieFragments(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(raw, []byte("moof"))
}

// probeJSON runs `probe --json` and decodes the wire shape.
func probeJSON(t *testing.T, path string) server.ProbeInfo {
	t.Helper()
	code, out, errOut := run(t, "probe", "--json", path)
	if code != 0 {
		t.Fatalf("probe --json %s exit = %d; stderr: %s", path, code, errOut)
	}
	var doc server.ProbeInfo
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	return doc
}

// sourceTags is the tag set the container-fallback tests carry, one of each
// atom shape the mp4 muxer writes.
var sourceTags = []container.Tag{
	{Key: "TITLE", Value: "A Title"},
	{Key: "ARTIST", Value: "An Artist"},
	{Key: "ALBUM", Value: "An Album"},
	{Key: "TRACKNUMBER", Value: "4"},
}

// TestContainerTagsReachTheOutput is B1 end to end: a source the tag library
// cannot read still carries its tags onto a transcode, because the demuxer
// parsed them off the header and the transfer folds them in.
func TestContainerTagsReachTheOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, sourceTags, nil)

	// The premise: the container sees the tags, and probe reports them with
	// no mapper in the picture.
	if got := probeJSON(t, frag).Tags; len(got["TITLE"]) != 1 || got["TITLE"][0] != "A Title" {
		t.Fatalf("probe read no container tags off the fragmented source: %v", got)
	}

	out := filepath.Join(dir, "out.flac")
	if code, _, errOut := run(t, "transcode", frag, out); code != 0 {
		t.Fatalf("transcode exit = %d; stderr: %s", code, errOut)
	}
	tags := probeJSON(t, out).Tags
	for _, want := range sourceTags {
		if vs := tags[want.Key]; len(vs) != 1 || vs[0] != want.Value {
			t.Errorf("%s = %v, want [%q]", want.Key, vs, want.Value)
		}
	}
}

// TestNoTagsSuppressesTheContainerFallback pins the flag against the new
// source of tags: --no-tags means no tags, not "no tags the mapper found".
func TestNoTagsSuppressesTheContainerFallback(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, sourceTags, nil)

	out := filepath.Join(dir, "out.flac")
	if code, _, errOut := run(t, "transcode", "--no-tags", frag, out); code != 0 {
		t.Fatalf("transcode exit = %d; stderr: %s", code, errOut)
	}
	if tags := probeJSON(t, out).Tags; len(tags) != 0 {
		t.Errorf("--no-tags wrote %v", tags)
	}
}

// TestGainedTranscodeDropsRecoveredReplayGain is the fold-order case, and the
// one a naive implementation gets wrong: the container carries the four
// REPLAYGAIN_* keys too, so folding them in after the ReplayGain drop would
// put the source's stale numbers onto an output whose gain just changed.
func TestGainedTranscodeDropsRecoveredReplayGain(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, append([]container.Tag{
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: "-07.77 dB"},
		{Key: "REPLAYGAIN_TRACK_PEAK", Value: "0.777000"},
	}, sourceTags...), nil)

	// The premise: those values really are on the source, via the container.
	if got := probeJSON(t, frag).Tags["REPLAYGAIN_TRACK_GAIN"]; len(got) != 1 || got[0] != "-07.77 dB" {
		t.Fatalf("the fixture carries no recoverable ReplayGain: %v", got)
	}

	out := filepath.Join(dir, "out.flac")
	if code, _, errOut := run(t, "transcode", "--gain", "3", frag, out); code != 0 {
		t.Fatalf("transcode exit = %d; stderr: %s", code, errOut)
	}
	tags := probeJSON(t, out).Tags
	for key := range tags {
		if strings.HasPrefix(key, "REPLAYGAIN_") {
			t.Errorf("a gained output carries the source's stale %s = %v", key, tags[key])
		}
	}
	// The descriptive tags still come across; only the loudness ones go.
	if vs := tags["TITLE"]; len(vs) != 1 || vs[0] != "A Title" {
		t.Errorf("TITLE = %v, want [%q]", vs, "A Title")
	}
}

// TestMeasuredReplayGainBeatsTheRecoveredOne is the same fold order under
// --loudness analyze, where the output must carry what was measured rather
// than what the source said.
func TestMeasuredReplayGainBeatsTheRecoveredOne(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeSineWAV(t, in, 44100, 2, 16, 44100)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, []container.Tag{
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: "-07.77 dB"},
		{Key: "REPLAYGAIN_TRACK_PEAK", Value: "0.777000"},
	}, nil)

	out := filepath.Join(dir, "out.flac")
	if code, _, errOut := run(t, "transcode", "--loudness", "analyze", frag, out); code != 0 {
		t.Fatalf("transcode exit = %d; stderr: %s", code, errOut)
	}
	tags := probeJSON(t, out).Tags
	if vs := tags["REPLAYGAIN_TRACK_GAIN"]; len(vs) != 1 || vs[0] == "-07.77 dB" {
		t.Errorf("REPLAYGAIN_TRACK_GAIN = %v; the source's stale value survived a measurement", vs)
	}
	if vs := tags["REPLAYGAIN_TRACK_PEAK"]; len(vs) != 1 || vs[0] == "0.777000" {
		t.Errorf("REPLAYGAIN_TRACK_PEAK = %v; the source's stale value survived a measurement", vs)
	}
}

// TestContainerArtIsNotAdvertised pins the one surface the fallback
// deliberately does not cover. hasArt reports what /art can serve, and /art
// serves only what a mapper hands it, so a covr atom the container saw must
// not flip it: hasArt true beside a 404 is worse than the omission.
func TestContainerArtIsNotAdvertised(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, sourceTags,
		&container.Picture{MIME: "image/jpeg", Data: []byte("\xff\xd8\xff\xe0not really a jpeg")})

	doc := probeJSON(t, frag)
	if len(doc.Tags) == 0 {
		t.Fatal("the fixture lost its tags, so this test is not covering the fallback")
	}
	if doc.HasArt {
		t.Error("hasArt went true off a covr atom the container saw; /art has nothing to serve")
	}
}

// TestFragmentedContainerIsReachable pins the escape hatch C2 needs: an
// mp4-family file output is flat by default, so the delivery form has to be
// nameable or the CLI could not produce one at all. --container fragmented is
// that name, and the C2 rewrite must not touch an explicit override.
func TestFragmentedContainerIsReachable(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)

	flat := filepath.Join(dir, "flat.m4a")
	if code, _, errOut := run(t, "transcode", in, flat); code != 0 {
		t.Fatalf("default exit = %d; stderr: %s", code, errOut)
	}
	frag := filepath.Join(dir, "frag.m4a")
	if code, _, errOut := run(t, "transcode", "--container", "fragmented", in, frag); code != 0 {
		t.Fatalf("--container fragmented exit = %d; stderr: %s", code, errOut)
	}
	// Movie fragments are what makes a movie fragmented, so that is what is
	// asserted. The declared length is not a discriminator here: our
	// fragmented output carries an edit list, so it reports a length too.
	if hasMovieFragments(t, flat) {
		t.Error("the default output carries moof boxes; it is not the flat form")
	}
	if !hasMovieFragments(t, frag) {
		t.Error("--container fragmented carries no moof boxes; it is not the fragmented form")
	}
	// alac takes the same pair.
	al := filepath.Join(dir, "alac.m4a")
	if code, _, errOut := run(t, "transcode", "--format", "alac", "--container", "fragmented", in, al); code != 0 {
		t.Fatalf("alac --container fragmented exit = %d; stderr: %s", code, errOut)
	}
	// A name neither row has is still refused rather than falling back.
	if code, _, _ := run(t, "transcode", "--container", "bogus", in, filepath.Join(dir, "x.m4a")); code != 2 {
		t.Errorf("an unknown container exit = %d, want 2 (invalid)", code)
	}
}

// TestFileOutputIsFlatMP4 is C2: a file output takes the flat MP4 form, which
// is the one that reads back. The old fragmented default probed to samples -1
// through the tag library and carried nothing.
func TestFileOutputIsFlatMP4(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)

	for _, name := range []string{"out.m4a", "out.m4b"} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(dir, name)
			if code, _, errOut := run(t, "transcode", in, out); code != 0 {
				t.Fatalf("transcode exit = %d; stderr: %s", code, errOut)
			}
			doc := probeJSON(t, out)
			if len(doc.Tracks) != 1 {
				t.Fatalf("probe read %d tracks", len(doc.Tracks))
			}
			if doc.Tracks[0].Codec != "aac-lc" {
				t.Errorf("codec = %q, want aac-lc", doc.Tracks[0].Codec)
			}
			if hasMovieFragments(t, out) {
				t.Error("the output carries moof boxes; a file output is the flat form")
			}
			// And it reads back as a real length rather than the -1 the
			// report measured, which is the user-visible half of C2.
			if doc.Tracks[0].Samples <= 0 || doc.Tracks[0].DurationSeconds <= 0 {
				t.Errorf("samples = %d, durationSeconds = %v",
					doc.Tracks[0].Samples, doc.Tracks[0].DurationSeconds)
			}
		})
	}
}

// TestFileOutputCarriesTagsThroughMP4 is the other half of C2's point: with
// the flat form, an mp4-family output round trips its metadata.
func TestFileOutputCarriesTagsThroughMP4(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, sourceTags, nil)

	out := filepath.Join(dir, "out.m4a")
	if code, _, errOut := run(t, "transcode", frag, out); code != 0 {
		t.Fatalf("transcode exit = %d; stderr: %s", code, errOut)
	}
	tags := probeJSON(t, out).Tags
	for _, want := range sourceTags {
		if vs := tags[want.Key]; len(vs) != 1 || vs[0] != want.Value {
			t.Errorf("%s = %v, want [%q]", want.Key, vs, want.Value)
		}
	}
}

// TestProbeReportsSourceBitDepth is U3: audio.Format carries floats as
// float32, so a 64-bit float source used to probe as 32 and describe the
// decoder rather than the file. The human line and --json must agree.
func TestProbeReportsSourceBitDepth(t *testing.T) {
	cases := []struct {
		fixture string
		want    int
	}{
		{"sine-f64.wav", 64},
		{"sine-f32.wav", 32},
		{"sine-f32.aiff", 32},
		{"sine-s24.wav", 24}, // an integer source is unchanged
		{"sine-s16.wav", 16},
	}
	for _, tt := range cases {
		t.Run(tt.fixture, func(t *testing.T) {
			path := filepath.Join("..", "testdata", tt.fixture)
			doc := probeJSON(t, path)
			if got := doc.Tracks[0].BitDepth; got != tt.want {
				t.Errorf("--json bitDepth = %d, want %d", got, tt.want)
			}
			code, out, errOut := run(t, "probe", path)
			if code != 0 {
				t.Fatalf("probe exit = %d; stderr: %s", code, errOut)
			}
			// The human line renders the same depth, so the two spellings of
			// one probe cannot disagree about one file.
			want := fmt.Sprintf("%s%d", doc.Tracks[0].SampleType, tt.want)
			if !strings.Contains(out, want) {
				t.Errorf("the human line does not say %s:\n%s", want, out)
			}
		})
	}
}

// TestLoudnessMeasuresTheResolvedWidth pins the CLI half of C1's trap. Both
// the measurement basis and the peak clamp used to key on --channels, which
// sees an explicit downmix and not an implicit one. A lossy row folds a
// too-wide source on its own now, so a flag-keyed pass meters channels the
// encode never encodes and publishes a peak the limiter had already clamped.
func TestLoudnessMeasuresTheResolvedWidth(t *testing.T) {
	dir := t.TempDir()
	// Two seconds: the gated integrated loudness needs whole blocks, and a
	// fixture shorter than the 400 ms gate reports -Inf.
	in := filepath.Join(dir, "surround.wav")
	writeSineWAV(t, in, 48000, 6, 16, 2*48000)

	// The same fold, spelled implicitly and explicitly. Opus holds no 5.1, so
	// the first request folds without being asked.
	implicit := filepath.Join(dir, "implicit.opus")
	_, _, implicitLog := run(t, "transcode", "--loudness", "analyze", in, implicit)
	explicit := filepath.Join(dir, "explicit.opus")
	_, _, explicitLog := run(t, "transcode", "--loudness", "analyze", "--channels", "2", in, explicit)

	source := func(log string) string {
		t.Helper()
		for _, line := range strings.Split(log, "\n") {
			if strings.HasPrefix(line, "loudness: source ") {
				return line
			}
		}
		t.Fatalf("no source loudness line:\n%s", log)
		return ""
	}
	if got, want := source(implicitLog), source(explicitLog); got != want {
		t.Errorf("the implicit fold measured %q and the explicit one %q; the two are the same encode", got, want)
	}
	// The Ogg muxer embeds ReplayGain at Begin, so the published peak is on
	// the output itself. A missed downmix leaves it unclamped.
	ip := probeJSON(t, implicit).Tags["REPLAYGAIN_TRACK_PEAK"]
	ep := probeJSON(t, explicit).Tags["REPLAYGAIN_TRACK_PEAK"]
	if len(ip) != 1 || len(ep) != 1 {
		t.Fatalf("peaks = %v and %v, want one value each", ip, ep)
	}
	if ip[0] != ep[0] {
		t.Errorf("implicit fold published peak %s, explicit %s; the limiter ran either way", ip[0], ep[0])
	}
}

// TestSplitCarriesContainerTags covers the fourth fold site. split reads its
// metadata once for the whole rip and stamps every piece with it, so a source
// the tag library cannot parse used to leave every piece untagged: the same
// B1 gap as transcode's, through a different path.
func TestSplitCarriesContainerTags(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 48000)
	frag := filepath.Join(dir, "album.m4a")
	writeTaggedFragmentedMP4(t, in, frag, sourceTags, nil)

	out := filepath.Join(dir, "pieces")
	if code, _, errOut := run(t, "split", "--at", "16000,32000", frag, out); code != 0 {
		t.Fatalf("split exit = %d; stderr: %s", code, errOut)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d pieces, want 3", len(entries))
	}
	for _, e := range entries {
		tags := probeJSON(t, filepath.Join(out, e.Name())).Tags
		// The album-level tags ride onto every piece; TRACKNUMBER is the
		// per-piece one the cut list overwrites, so it is not compared here.
		for _, want := range sourceTags {
			if want.Key == "TRACKNUMBER" {
				continue
			}
			if vs := tags[want.Key]; len(vs) != 1 || vs[0] != want.Value {
				t.Errorf("%s: %s = %v, want [%q]", e.Name(), want.Key, vs, want.Value)
			}
		}
	}

	// --no-tags suppresses the container fallback here too. It is scoped to
	// the source's metadata, which is what the flag says: a piece still
	// carries the TRACKNUMBER/TRACKTOTAL the cut list gives it, since those
	// describe the split's own product and were never read off the source.
	bare := filepath.Join(dir, "bare")
	if code, _, errOut := run(t, "split", "--no-tags", "--at", "16000", frag, bare); code != 0 {
		t.Fatalf("split --no-tags exit = %d; stderr: %s", code, errOut)
	}
	entries, err = os.ReadDir(bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		tags := probeJSON(t, filepath.Join(bare, e.Name())).Tags
		for _, key := range []string{"TITLE", "ARTIST", "ALBUM"} {
			if vs := tags[key]; len(vs) != 0 {
				t.Errorf("%s: --no-tags carried the source's %s = %v", e.Name(), key, vs)
			}
		}
	}
}

// TestProbeTextAndJSONAgreeOnTags pins the two spellings of one probe against
// each other on the tag half, as TestProbeReportsSourceBitDepth does on the
// depth half. printProbe read tags only from the mapper, so a source the tag
// library cannot parse printed no title while --json reported one.
func TestProbeTextAndJSONAgreeOnTags(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 8192)
	frag := filepath.Join(dir, "frag.m4a")
	writeTaggedFragmentedMP4(t, in, frag, sourceTags, nil)

	doc := probeJSON(t, frag)
	if len(doc.Tags["TITLE"]) == 0 {
		t.Fatal("--json read no container tags, so this test proves nothing")
	}
	code, out, errOut := run(t, "probe", frag)
	if code != 0 {
		t.Fatalf("probe exit = %d; stderr: %s", code, errOut)
	}
	for _, want := range []string{"title:", "A Title", "artist:", "An Artist", "album:", "An Album"} {
		if !strings.Contains(out, want) {
			t.Errorf("the human line omits %q while --json reports it:\n%s", want, out)
		}
	}
}
