package label_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// writeFragmentedMP4 builds the shape both cells need: our own fragmented
// (CMAF) MP4, which is the aac row's default container. The tag library reads
// it and refuses to rewrite it, so it is the one source that exercises the
// note and the refusal at once. Built through the engine because a CLI file
// output is the flat form.
func writeFragmentedMP4(t *testing.T, path string, tags []container.Tag) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", out,
		waxflow.TranscodeOptions{Format: "aac"}, waxflow.WithTags(tags)); err != nil {
		t.Fatal(err)
	}
}

// TestFragmentedIsANoteNotAWarning pins the sourceLint classification at its
// source, which is the switch behind both surfaces that show a mapper warning
// to a user: logMetaNote (transcode and split) and a job's client-visible
// Warnings. Probe's human line and the /probe envelope are not among them,
// though they look like they should be: both iterate format.Info.Warnings off
// the probe itself, and ProbeMetadata carries no warnings at all.
//
// A fragmented source reads its tags exactly, so it must not spend a warning
// saying so.
func TestFragmentedIsANoteNotAWarning(t *testing.T) {
	frag := filepath.Join(t.TempDir(), "frag.m4a")
	writeFragmentedMP4(t, frag, []container.Tag{{Key: "TITLE", Value: "A Title"}})

	raw, err := os.ReadFile(frag)
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "m4a", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The premise: the mapper really did read this file, so the note is
	// describing a source it parsed rather than one it gave up on.
	if vs := info.Tags["TITLE"]; len(vs) != 1 || vs[0] != "A Title" {
		t.Fatalf("the mapper read TITLE = %v off a fragmented source, want [%q]", vs, "A Title")
	}
	if !slices.ContainsFunc(info.Notes, func(n string) bool { return strings.Contains(n, "fragmented") }) {
		t.Errorf("no fragmented note: %v", info.Notes)
	}
	// The muxer's own iTunSMPB freeform draws the tag library's
	// invalid-tag-key code, and it is machine state WaxFlow rewrites itself,
	// so it must land beside the fragmented note rather than spend a warning
	// on every MP4 source. The fixture carries the atom because it is an AAC
	// encode onto a seekable *os.File; the fragmented muxer writes no
	// iTunSMPB on a pure stream, so this cell would go vacuous if the helper
	// ever switched to a pipe.
	if !slices.ContainsFunc(info.Notes, func(n string) bool { return strings.Contains(n, "iTunSMPB") }) {
		t.Errorf("no iTunSMPB machine-key note: %v", info.Notes)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("a readable fragmented source warned: %v", info.Warnings)
	}
}

// TestFragmentedApplyIsUnsupportedNotInternal pins the code on the write
// refusal. The tag library moved that refusal from parse to plan time, which
// routed it through the blanket Prepare mapping and relabelled a file it
// simply cannot rewrite an internal error (exit 1) instead of an unsupported
// one (exit 5).
//
// The edit has to be real. The refusal sits behind the unchanged-file fast
// path, so applying a tag set the file already carries returns a no-op plan
// and no error, and a cell built from a tagged fixture would pass on the
// unfixed mapping. Hence the untagged fixture.
//
// The production Apply sites route MP4 outputs to the muxer instead
// (internal/tofile/tofile.go, cli/split.go), so this pins the seam's contract
// rather than a live route.
func TestFragmentedApplyIsUnsupportedNotInternal(t *testing.T) {
	frag := filepath.Join(t.TempDir(), "frag.m4a")
	writeFragmentedMP4(t, frag, nil)

	err := label.New().Apply(t.Context(), frag,
		&meta.Info{Tags: map[string][]string{"TITLE": {"A Title"}}}, nil)
	if err == nil {
		t.Fatal("Apply accepted a fragmented MP4; the fixture is not exercising the refusal")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeUnsupportedFormat, err)
	}
}

// TestCanceledApplyIsCanceled keeps the format mapping from swallowing the one
// thing it must not: an abandoned operation is not a statement about the
// output, which would otherwise report exit 5 "unsupported" for a file nothing
// is wrong with.
//
// Scope, precisely: ParseFile checks the context first thing, so a ctx canceled
// up front always stops there and this covers that branch alone. The matching
// check on plan.Execute is for a cancel that lands mid-write, which has no
// deterministic seam to drive from a test; it is symmetric code, not code this
// pins. No fixture is built for the same reason: ParseFile returns before it
// opens anything, so any path answers identically.
func TestCanceledApplyIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := label.New().Apply(ctx, filepath.Join("..", "..", "testdata", "sine-s16.flac"),
		&meta.Info{Tags: map[string][]string{"TITLE": {"A Title"}}}, nil)
	if err == nil {
		t.Fatal("a canceled Apply succeeded, so this cell covers nothing")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeCanceled {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeCanceled, err)
	}
}

// TestUnwritableTextIsRepairedNotFailed pins the per-item half of the
// best-effort contract. Prepare refuses a whole edit over one value holding a
// NUL or invalid UTF-8, and such a value comes straight off the source (a
// Vorbis comment stores either), so the refusal cost a finished transcode all
// of its metadata: tags, art and chapters alike. The library's own copy drops
// the one item and writes the rest; Apply repairs it the way a reader shows
// it and says so in the log.
func TestUnwritableTextIsRepairedNotFailed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.flac")
	writeFLAC(t, raw, path)

	var buf bytes.Buffer
	m := label.NewLogged(slog.New(slog.NewTextHandler(&buf, nil)))
	if err := m.Apply(t.Context(), path, &meta.Info{
		Tags: map[string][]string{
			"TITLE":   {"survives"},
			"ARTIST":  {"cut\x00here", "whole"},
			"COMMENT": {"bad\xff\xfevalue"},
			"ALBUM":   {"\x00nothing left"},
		},
		Chapters: []container.Chapter{{Start: 0, Title: "one\x00two"}},
	}, []container.Tag{{Key: "GENRE", Value: "\x00"}}); err != nil {
		t.Fatalf("Apply failed over text it can repair: %v", err)
	}
	info := readBack(t, path, "flac")
	for key, want := range map[string][]string{
		"TITLE":   {"survives"},
		"ARTIST":  {"cut", "whole"},
		"COMMENT": {"bad\uFFFDvalue"},
		"ALBUM":   nil, // a value the cut leaves empty is no value
		"GENRE":   nil,
	} {
		if got := info.Tags[key]; !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if len(info.Chapters) != 1 || info.Chapters[0].Title != "one" {
		t.Errorf("chapters = %+v, want the one chapter titled %q", info.Chapters, "one")
	}
	if got := buf.String(); !strings.Contains(got, "metadata text repaired") || !strings.Contains(got, "field=ARTIST") {
		t.Errorf("the repair was not logged; got %q", got)
	}
}

// TestOversizedTagBlockIsARefusalNotBadMetadata pins the bucket for tag
// content past a format's structural limit, which waxlabel v1.9.0 gave its
// own sentinel (content-too-large). FLAC caps a metadata block at 16 MiB, so
// a comment block past that is the picture cell's shape: well-formed
// metadata this output cannot take, not metadata wrong in itself.
func TestOversizedTagBlockIsARefusalNotBadMetadata(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.flac")
	writeFLAC(t, raw, path)

	err = label.New().Apply(t.Context(), path,
		&meta.Info{Tags: map[string][]string{"COMMENT": {strings.Repeat("x", 17<<20)}}}, nil)
	if err == nil {
		t.Fatal("Apply wrote a comment block past FLAC's limit; this cell covers nothing")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeUnsupportedFormat, err)
	}
}

// TestOversizedPictureIsARefusalNotBadMetadata pins the bucket for a picture
// the destination format cannot store. FLAC caps a metadata block at 16 MiB,
// so a larger cover is the same shape as a chapter count past the format's
// limit: well-formed metadata this output cannot take (unsupported), not
// metadata that is wrong in itself (invalid request). The tag library agrees
// since v1.4.1, which classifies ErrPictureTooLarge with the write refusals.
func TestOversizedPictureIsARefusalNotBadMetadata(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.flac")
	writeFLAC(t, raw, path)

	err = label.New().Apply(t.Context(), path,
		&meta.Info{Pictures: []meta.Picture{{MIME: "image/png", Front: true, Data: oversizedPNG()}}}, nil)
	if err == nil {
		t.Fatal("Apply embedded a picture past FLAC's block limit; this cell covers nothing")
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %q, want %q (%v)", got, waxerr.CodeUnsupportedFormat, err)
	}
}

// oversizedPNG is a real PNG signature and IHDR on a body past FLAC's 16 MiB
// block limit: the picture sniff accepts it, only the FLAC write refuses it.
func oversizedPNG() []byte {
	data := make([]byte, 17<<20)
	copy(data, "\x89PNG\r\n\x1a\n")        // signature
	copy(data[8:], "\x00\x00\x00\x0dIHDR") // IHDR length + type
	copy(data[16:], "\x00\x00\x00\x10"+    // width 16
		"\x00\x00\x00\x10"+ // height 16
		"\x08\x02\x00\x00\x00") // 8-bit RGB
	return data
}

// TestUnreadableMetadataIsAWarningNotAnError pins the fallback Read promises:
// a source whose metadata cannot be parsed yields an empty Info carrying a
// warning, never an error, because audio must still flow. The tag library
// reads every format WaxFlow decodes, so only damage or junk reaches this
// branch, which is why the fixture is plain garbage rather than any real
// container.
func TestUnreadableMetadataIsAWarningNotAnError(t *testing.T) {
	info, err := label.New().Read(t.Context(),
		container.BytesSource([]byte("no signature any codec claims")), "bin", meta.ReadOptions{})
	if err != nil {
		t.Fatalf("an unreadable source errored: %v", err)
	}
	if len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "metadata unread") {
		t.Errorf("warnings = %v, want one saying metadata unread", info.Warnings)
	}
	if len(info.Tags) != 0 {
		t.Errorf("tags = %v off an unreadable source, want none", info.Tags)
	}
}

// writeFLAC builds a taggable output: an ordinary flat file the mapper
// rewrites without complaint, so a refusal is about the metadata.
func writeFLAC(t *testing.T, wav []byte, path string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(wav), "wav", out,
		waxflow.TranscodeOptions{Format: "flac"}); err != nil {
		t.Fatal(err)
	}
}

// writeWavPack builds an output whose metadata store is APEv2: no chapter or
// synced-lyrics store at all, and a Cover Art convention of one item name per
// role. It is the destination that exercises every unstorable-field branch.
func writeWavPack(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", out,
		waxflow.TranscodeOptions{Format: "wavpack"}); err != nil {
		t.Fatal(err)
	}
}

// smallPNG is a real signature and IHDR on a body every format can store, the
// complement of oversizedPNG: here the picture is fine and only the
// destination's slot count is at issue.
func smallPNG() []byte {
	data := make([]byte, 64)
	copy(data, "\x89PNG\r\n\x1a\n")
	copy(data[8:], "\x00\x00\x00\x0dIHDR")
	copy(data[16:], "\x00\x00\x00\x10"+
		"\x00\x00\x00\x10"+
		"\x08\x02\x00\x00\x00")
	return data
}

// TestUnstorableFieldIsDroppedNotFailed pins Apply's best-effort contract
// against the fields an output has no store for. A source carrying chapters,
// synced lyrics, or two covers of one role, where APE has one Cover Art item
// name a role, must not fail a finished transcode: the storable part applies
// and the rest is a plan warning, which is the answer a cross-format copy
// gives.
//
// waxlabel v1.6.1 made the cover with no name left a hard refusal (before it,
// the APE writer's backstop dropped it), so without the drop option such a
// source silently became a failed metadata pass on every .wv/.ape output. The
// chapter and lyric branches were refusals before that release too. Since
// v1.9.0 every role has a name of its own, so it takes a second picture of
// one role to run out.
func TestUnstorableFieldIsDroppedNotFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *meta.Info
	}{
		{"chapters", &meta.Info{Chapters: []container.Chapter{{Start: 0, End: time.Second, Title: "one"}}}},
		{"synced lyrics", &meta.Info{Synced: []meta.SyncedLyrics{{
			Language: "eng", Lines: []meta.SyncedLine{{Time: 0, Text: "la"}},
		}}}},
		{"a cover past the slots", &meta.Info{Pictures: []meta.Picture{
			{MIME: "image/png", Front: true, Data: smallPNG()},
			{MIME: "image/png", Data: smallPNG()},
			{MIME: "image/png", Data: smallPNG()},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.wv")
			writeWavPack(t, path)
			tc.info.Tags = map[string][]string{"TITLE": {"survives"}}
			if err := label.New().Apply(t.Context(), path, tc.info, nil); err != nil {
				t.Fatalf("Apply failed on a field WavPack cannot store: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wv",
				meta.ReadOptions{Pictures: true})
			if err != nil {
				t.Fatal(err)
			}
			// The storable part of the same edit has to land, or "dropped the
			// unstorable field" would be indistinguishable from writing nothing.
			if got := info.Tags["TITLE"]; !slices.Equal(got, []string{"survives"}) {
				t.Errorf("TITLE = %v, want [survives]", got)
			}
			if tc.name == "a cover past the slots" && len(info.Pictures) != 2 {
				t.Errorf("pictures = %d, want 2 (the front cover and one Other)", len(info.Pictures))
			}
		})
	}
}

// TestUnstorableDropIsLogged pins the drop's visibility. Apply returns only an
// error, so a field it drops is invisible to the caller; the log is the one
// channel left, and NewLogged is documented to carry exactly these notes.
func TestUnstorableDropIsLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.wv")
	writeWavPack(t, path)

	var buf bytes.Buffer
	m := label.NewLogged(slog.New(slog.NewTextHandler(&buf, nil)))
	if err := m.Apply(t.Context(), path, &meta.Info{
		Chapters: []container.Chapter{{Start: 0, End: time.Second, Title: "one"}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "metadata dropped") ||
		!strings.Contains(got, "chapters") {
		t.Errorf("dropped chapters were not logged; got %q", got)
	}
}

// TestUnrecognizedCoverIsCarriedNotRefused pins the carry contract for a
// cover whose bytes no image sniffer names. waxlabel v1.7.0 validates every
// added picture at Prepare and refuses one it cannot recognize, which is the
// right default for a tagger authoring an edit: the caller almost certainly
// handed it the wrong file. Apply is not authoring, it is carrying what the
// source already held, so it opts out the way waxlabel's own transfer engine
// does. Without the opt-out a source with an exotic or mislabeled cover fails
// the metadata pass of a transcode that otherwise succeeded.
func TestUnrecognizedCoverIsCarriedNotRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.flac")
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	writeFLAC(t, raw, path)

	junk := []byte("this is not an image, and no sniffer will say it is")
	if err := label.New().Apply(t.Context(), path, &meta.Info{
		Tags:     map[string][]string{"TITLE": {"survives"}},
		Pictures: []meta.Picture{{MIME: "image/jpeg", Front: true, Data: junk}},
	}, nil); err != nil {
		t.Fatalf("Apply refused a cover the source already carried: %v", err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(out), "flac",
		meta.ReadOptions{Pictures: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Tags["TITLE"]; !slices.Equal(got, []string{"survives"}) {
		t.Errorf("TITLE = %v, want [survives]", got)
	}
	if len(info.Pictures) != 1 || !bytes.Equal(info.Pictures[0].Data, junk) {
		t.Fatalf("pictures = %d, want the one carried cover back", len(info.Pictures))
	}
	// The declared MIME does not survive: the bytes are the authority, and
	// they say nothing, so the cover is stored as raw binary.
	if got := info.Pictures[0].MIME; got != "application/octet-stream" {
		t.Errorf("cover MIME = %q, want application/octet-stream", got)
	}
}

// readBack is the mapper's own reading of a file Apply wrote.
func readBack(t *testing.T, path, hint string) *meta.Info {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), hint, meta.ReadOptions{Pictures: true})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// encode is the engine's output in a format, untagged, as bytes.
func encode(t *testing.T, format string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: format}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// writeEncoded writes the engine's untagged output in a format to a file.
func writeEncoded(t *testing.T, format, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, encode(t, format), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// id3v1 is a 128-byte ID3v1 tag.
func id3v1(title, artist string) []byte {
	tag := make([]byte, 128)
	copy(tag, "TAG")
	copy(tag[3:33], title)
	copy(tag[33:63], artist)
	tag[127] = 255 // no genre
	return tag
}

// apeItem is one APEv2 item; binary marks it as binary data.
type apeItem struct {
	key, value string
	binary     bool
}

// apev2 is an APEv2 tag with a header and a footer.
func apev2(items ...apeItem) []byte {
	var body []byte
	for _, it := range items {
		var flags uint32
		if it.binary {
			flags = 1 << 1
		}
		body = binary.LittleEndian.AppendUint32(body, uint32(len(it.value)))
		body = binary.LittleEndian.AppendUint32(body, flags)
		body = append(append(append(body, it.key...), 0), it.value...)
	}
	frame := func(flags uint32) []byte {
		b := []byte("APETAGEX")
		b = binary.LittleEndian.AppendUint32(b, 2000)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(body)+32))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(items)))
		b = binary.LittleEndian.AppendUint32(b, flags)
		return append(b, make([]byte, 8)...)
	}
	return slices.Concat(frame(0xA0000000), body, frame(0x80000000))
}

// TestLegacyOnlyValuesAreCarried pins the floor under the canonical tags. The
// tag library keeps a value only a legacy block holds out of its canonical
// view, because a tagger editing in place keeps the block. A transcode keeps
// nothing, so an MP3 whose one tag is ID3v1 came out with no tags at all and
// only a debug note calling the tag "preserved". waxlabel v1.9.0 reads such
// blocks after ADTS, FLAC and Ogg streams too, where it used to warn of
// trailing bytes, so the same silence spread to them.
func TestLegacyOnlyValuesAreCarried(t *testing.T) {
	t.Run("an MP3 with ID3v1 alone", func(t *testing.T) {
		src := append(encode(t, "mp3"), id3v1("Old Title", "Old Artist")...)
		info, err := label.New().Read(t.Context(), container.BytesSource(src), "mp3", meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Tags["TITLE"]; !slices.Equal(got, []string{"Old Title"}) {
			t.Errorf("TITLE = %q, want the ID3v1 title", got)
		}
		if got := info.Tags["ARTIST"]; !slices.Equal(got, []string{"Old Artist"}) {
			t.Errorf("ARTIST = %q, want the ID3v1 artist", got)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("a tag whose values were all carried warned: %v", info.Warnings)
		}
	})
	t.Run("an APEv2 tag behind a FLAC stream", func(t *testing.T) {
		path := writeEncoded(t, "flac", "native.flac")
		if err := label.New().Apply(t.Context(), path,
			&meta.Info{Tags: map[string][]string{"TITLE": {"Native"}}}, nil); err != nil {
			t.Fatal(err)
		}
		flac, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := append(flac, apev2(apeItem{key: "Title", value: "Legacy"}, apeItem{key: "Composer", value: "Only Here"})...)
		info, err := label.New().Read(t.Context(), container.BytesSource(src), "flac", meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Tags["COMPOSER"]; !slices.Equal(got, []string{"Only Here"}) {
			t.Errorf("COMPOSER = %q, want the value only the APEv2 tag holds", got)
		}
		// The floor is a floor: the stream's own tags win the key both hold.
		if got := info.Tags["TITLE"]; !slices.Equal(got, []string{"Native"}) {
			t.Errorf("TITLE = %q, want the Vorbis comment's", got)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("a tag whose values were all carried warned: %v", info.Warnings)
		}
	})
	// The library lists a legacy block's values one entry each, so taking a
	// key's first entry would keep the first of two composers.
	t.Run("every value of a multi-valued key", func(t *testing.T) {
		src := append(encode(t, "flac"), apev2(
			apeItem{key: "Composer", value: "C1\x00C2"},
			apeItem{key: "Artist", value: "A1"}, apeItem{key: "Artist", value: "A2"}, apeItem{key: "Artist", value: "A1"})...)
		info, err := label.New().Read(t.Context(), container.BytesSource(src), "flac", meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Tags["COMPOSER"]; !slices.Equal(got, []string{"C1", "C2"}) {
			t.Errorf("COMPOSER = %q, want [C1 C2]", got)
		}
		if got := info.Tags["ARTIST"]; !slices.Equal(got, []string{"A1", "A2"}) {
			t.Errorf("ARTIST = %q, want [A1 A2], a repeat once", got)
		}
	})
	// A total counts the numbering of the block that states it.
	t.Run("a total rides only with its number", func(t *testing.T) {
		tagged := func(number string) []byte {
			path := writeEncoded(t, "flac", "numbered.flac")
			if err := label.New().Apply(t.Context(), path,
				&meta.Info{Tags: map[string][]string{"TRACKNUMBER": {number}}}, nil); err != nil {
				t.Fatal(err)
			}
			flac, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return append(flac, apev2(apeItem{key: "Track", value: "5/12"})...)
		}
		for number, want := range map[string][]string{"05": {"12"}, "3": nil} {
			info, err := label.New().Read(t.Context(), container.BytesSource(tagged(number)), "flac", meta.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Tags["TRACKNUMBER"]; !slices.Equal(got, []string{number}) {
				t.Errorf("TRACKNUMBER = %q, want the Vorbis comment's %q", got, number)
			}
			if got := info.Tags["TRACKTOTAL"]; !slices.Equal(got, want) {
				t.Errorf("beside track %s the legacy 5/12 gave TRACKTOTAL = %q, want %q", number, got, want)
			}
		}
	})
	// Two legacy blocks that number the track differently: the pair carried
	// is one of theirs, never one block's number beside the other's total.
	t.Run("a total and its number come from one block", func(t *testing.T) {
		v1 := id3v1("Old Title", "Old Artist")
		v1[125], v1[126] = 0, 5            // ID3v1.1: track 5
		trck := func(text string) []byte { // a stray ID3v2.3 tag of one TRCK frame
			frame := append([]byte("TRCK"), 0, 0, 0, byte(1+len(text)), 0, 0, 0)
			frame = append(frame, text...)
			return append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, byte(len(frame))}, frame...)
		}
		for name, tc := range map[string]struct {
			src   []byte
			hint  string
			pairs [][2]string // number and total; "" for none
		}{
			"an MP3 with APEv2 3/12 and ID3v1 track 5": {
				append(append(encode(t, "mp3"), apev2(apeItem{key: "Track", value: "3/12"})...), v1...),
				"mp3", [][2]string{{"3", "12"}, {"5", ""}}},
			"a FLAC with ID3v2 5/9 ahead and APEv2 track 3 behind": {
				append(append(trck("5/9"), encode(t, "flac")...), apev2(apeItem{key: "Track", value: "3"})...),
				"flac", [][2]string{{"3", ""}, {"5", "9"}}},
			// A block that states a total and no number counts no numbering
			// another block's number is known to be in.
			"an MP3 with an APEv2 total alone and ID3v1 track 5": {
				append(append(encode(t, "mp3"), apev2(apeItem{key: "TRACKTOTAL", value: "12"})...), v1...),
				"mp3", [][2]string{{"5", ""}}},
		} {
			info, err := label.New().Read(t.Context(), container.BytesSource(tc.src), tc.hint, meta.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := [2]string{strings.Join(info.Tags["TRACKNUMBER"], "+"), strings.Join(info.Tags["TRACKTOTAL"], "+")}
			if !slices.Contains(tc.pairs, got) {
				t.Errorf("%s: number %q with total %q, want one block's pair of %q", name, got[0], got[1], tc.pairs)
			}
		}
	})
}

// TestFillerLanguageIsNotAQualifier: an ID3 frame holds "eng" for a comment
// given no language, so every MP3 with a comment reads one back. Carried on
// as a qualifier it made a WAV output need an ID3 chunk for the language's
// sake, and the tag library then writes no LIST/INFO chunk beside it, the
// one place most WAV readers look.
func TestFillerLanguageIsNotAQualifier(t *testing.T) {
	mp3 := writeEncoded(t, "mp3", "src.mp3")
	if err := label.New().Apply(t.Context(), mp3,
		&meta.Info{Tags: map[string][]string{"COMMENT": {"a plain comment"}}}, nil); err != nil {
		t.Fatal(err)
	}
	src := readBack(t, mp3, "mp3")
	if !slices.Equal(src.Tags["COMMENT"], []string{"a plain comment"}) {
		t.Fatalf("COMMENT = %q, want the comment back", src.Tags["COMMENT"])
	}
	if src.CommentItems != nil {
		t.Errorf("a comment with no description and the filler language reads qualified: %+v", src.CommentItems)
	}
	wav := writeEncoded(t, "wav", "out.wav")
	if err := label.New().Apply(t.Context(), wav, src, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("LIST")) || !bytes.Contains(raw, []byte("INFO")) {
		t.Error("the WAV output holds no LIST/INFO chunk")
	}
}

// TestOpaqueLegacyContentWarns is the other side of the legacy notes: a
// legacy block holding more than text has content the floor cannot carry, and
// a note saying the block is "preserved" would be the only word on it.
func TestOpaqueLegacyContentWarns(t *testing.T) {
	src := append(encode(t, "flac"), apev2(apeItem{key: "Cover Art (Front)", value: "c.png\x00" + string(smallPNG()), binary: true})...)
	info, err := label.New().Read(t.Context(), container.BytesSource(src), "flac", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(info.Warnings, func(w string) bool { return strings.Contains(w, "legacy tag block holds more") }) {
		t.Errorf("a cover only a legacy tag holds drew no warning: warnings %v, notes %v", info.Warnings, info.Notes)
	}
}

// TestUnwritableTimesAndCountsDoNotFailTheWrite pins the refusals Prepare
// keeps even under the drop option, each of which cost the whole edit: a
// chapter time no writer stores (refused since waxlabel v1.9.0, adjusted
// silently before), a chapter list past a one-byte count, and more synced
// lyrics sets than the output's one.
func TestUnwritableTimesAndCountsDoNotFailTheWrite(t *testing.T) {
	title := map[string][]string{"TITLE": {"survives"}}
	t.Run("chapter times", func(t *testing.T) {
		path := writeEncoded(t, "flac", "out.flac")
		if err := label.New().Apply(t.Context(), path, &meta.Info{Tags: title, Chapters: []container.Chapter{
			{Start: -time.Second, End: time.Second, Title: "before zero"},
			{Start: 2 * time.Second, End: time.Second, Title: "ends early"},
		}}, nil); err != nil {
			t.Fatalf("Apply failed over a chapter time: %v", err)
		}
		info := readBack(t, path, "flac")
		if len(info.Chapters) != 2 || info.Chapters[0].Start != 0 || info.Chapters[1].Start != 2*time.Second {
			t.Errorf("chapters = %+v, want starts at 0 and 2s", info.Chapters)
		}
		if got := info.Tags["TITLE"]; !slices.Equal(got, title["TITLE"]) {
			t.Errorf("TITLE = %q, want %q", got, title["TITLE"])
		}
	})
	t.Run("a chapter list past the limit", func(t *testing.T) {
		path := writeEncoded(t, "mp3", "out.mp3")
		chs := make([]container.Chapter, 256) // one past ID3's CTOC count
		for i := range chs {
			chs[i] = container.Chapter{Start: time.Duration(i) * time.Millisecond, Title: "c"}
		}
		var buf bytes.Buffer
		m := label.NewLogged(slog.New(slog.NewTextHandler(&buf, nil)))
		if err := m.Apply(t.Context(), path, &meta.Info{Tags: title, Chapters: chs}, nil); err != nil {
			t.Fatalf("Apply failed over a chapter list the output cannot hold: %v", err)
		}
		info := readBack(t, path, "mp3")
		if got := info.Tags["TITLE"]; !slices.Equal(got, title["TITLE"]) || len(info.Chapters) != 0 {
			t.Errorf("TITLE = %q with %d chapters, want the title and no chapters", got, len(info.Chapters))
		}
		if got := buf.String(); !strings.Contains(got, "metadata dropped") || !strings.Contains(got, "chapters=256") {
			t.Errorf("the dropped list was not logged; got %q", got)
		}
	})
	t.Run("synced lyrics sets past the limit", func(t *testing.T) {
		path := writeEncoded(t, "flac", "out.flac")
		set := func(lang string) meta.SyncedLyrics {
			return meta.SyncedLyrics{Language: lang, Lines: []meta.SyncedLine{{Time: 0, Text: "la"}}}
		}
		if err := label.New().Apply(t.Context(), path,
			&meta.Info{Tags: title, Synced: []meta.SyncedLyrics{set("eng"), set("deu")}}, nil); err != nil {
			t.Fatalf("Apply failed over a second synced lyrics set: %v", err)
		}
		if got := readBack(t, path, "flac").Tags["TITLE"]; !slices.Equal(got, title["TITLE"]) {
			t.Errorf("TITLE = %q, want %q", got, title["TITLE"])
		}
	})
}

// TestPictureRolesSurvive pins the role each picture is written under. Every
// picture but the front cover used to go in as Other, which lost a back
// cover's role everywhere and, on APEv2, whose item names are one per role,
// lost every picture past the first.
func TestPictureRolesSurvive(t *testing.T) {
	pic := func(typ uint8, body string) meta.Picture {
		return meta.Picture{MIME: "image/png", Type: typ, Front: typ == meta.PictureFrontCover,
			Data: append(smallPNG(), body...)}
	}
	for _, tc := range []struct {
		format, name string
		in           []meta.Picture
		want         []uint8
	}{
		// Two file icons: the role holds one, so the second is Other.
		{"flac", "out.flac", []meta.Picture{pic(3, "a"), pic(4, "b"), pic(8, "c"), pic(1, "d"), pic(1, "e"), pic(200, "f")},
			[]uint8{3, 4, 8, 1, 0, 0}},
		{"wavpack", "out.wv", []meta.Picture{pic(3, "a"), pic(4, "b"), pic(8, "c")}, []uint8{3, 4, 8}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			path := writeEncoded(t, tc.format, tc.name)
			if err := label.New().Apply(t.Context(), path, &meta.Info{Pictures: tc.in}, nil); err != nil {
				t.Fatal(err)
			}
			var got []uint8
			for _, p := range readBack(t, path, filepath.Ext(tc.name)[1:]).Pictures {
				got = append(got, p.Type)
				if p.Front != (p.Type == meta.PictureFrontCover) {
					t.Errorf("a picture of type %d reads Front = %v", p.Type, p.Front)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("picture types = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTextQualifiersSurvive pins the description and language of a comment
// and of unsynced lyrics, which waxlabel v1.9.0 models and ID3 stores. Set
// as bare values, two lyrics told apart only by language are one frame's
// worth: the writer keeps the first and drops the second.
func TestTextQualifiersSurvive(t *testing.T) {
	path := writeEncoded(t, "mp3", "out.mp3")
	comments := []meta.TextItem{{Text: "a note", Description: "review", Language: "deu"}}
	lyrics := []meta.TextItem{{Text: "words", Language: "eng"}, {Text: "Worte", Language: "deu"}}
	if err := label.New().Apply(t.Context(), path, &meta.Info{
		Tags:         map[string][]string{"COMMENT": {"a note"}, "LYRICS": {"words", "Worte"}},
		CommentItems: comments,
		LyricsItems:  lyrics,
	}, nil); err != nil {
		t.Fatal(err)
	}
	info := readBack(t, path, "mp3")
	if !slices.Equal(info.CommentItems, comments) {
		t.Errorf("comments = %+v, want %+v", info.CommentItems, comments)
	}
	// "eng" is what an ID3 frame holds for a text given no language, so it
	// reads back as none; the frame still tells the two lyrics apart.
	want := []meta.TextItem{{Text: "words"}, {Text: "Worte", Language: "deu"}}
	if !slices.Equal(info.LyricsItems, want) {
		t.Errorf("lyrics = %+v, want %+v", info.LyricsItems, want)
	}
	if got := info.Tags["LYRICS"]; !slices.Equal(got, []string{"words", "Worte"}) {
		t.Errorf("LYRICS = %q, want both values", got)
	}
}

// TestStaleTextQualifiersDescribeNothing pins the authority of Tags: a caller
// that replaced or removed the comment leaves items naming a text that is no
// longer a value, and they must neither bring the old text back nor land on
// the new one's frame as its description.
func TestStaleTextQualifiersDescribeNothing(t *testing.T) {
	stale := []meta.TextItem{{Text: "the old comment", Description: "review", Language: "deu"}}
	t.Run("removed", func(t *testing.T) {
		path := writeEncoded(t, "mp3", "out.mp3")
		if err := label.New().Apply(t.Context(), path, &meta.Info{
			Tags: map[string][]string{"TITLE": {"t"}}, CommentItems: stale,
		}, nil); err != nil {
			t.Fatal(err)
		}
		if got := readBack(t, path, "mp3").Tags["COMMENT"]; len(got) != 0 {
			t.Errorf("COMMENT = %q, want none: the items brought a removed comment back", got)
		}
	})
	t.Run("replaced", func(t *testing.T) {
		path := writeEncoded(t, "mp3", "out.mp3")
		if err := label.New().Apply(t.Context(), path, &meta.Info{
			Tags: map[string][]string{"COMMENT": {"the old comment"}}, CommentItems: stale,
		}, []container.Tag{{Key: "COMMENT", Value: "a new comment"}}); err != nil {
			t.Fatal(err)
		}
		info := readBack(t, path, "mp3")
		if got := info.Tags["COMMENT"]; !slices.Equal(got, []string{"a new comment"}) {
			t.Fatalf("COMMENT = %q, want the replacement alone", got)
		}
		for _, it := range info.CommentItems {
			if it.Description != "" || it.Language == "deu" {
				t.Errorf("the replacement inherited the old comment's qualifiers: %+v", it)
			}
		}
	})
}

// TestRepeatedExtraKeyIsMultiValued pins the extras against container.Tag's
// own rule, a multi-valued field repeating its key. A split hands the album's
// tags over this way, so setting each in turn left a piece only the last of
// an album's artists.
func TestRepeatedExtraKeyIsMultiValued(t *testing.T) {
	path := writeEncoded(t, "flac", "out.flac")
	if err := label.New().Apply(t.Context(), path,
		&meta.Info{Tags: map[string][]string{"ARTIST": {"replaced"}, "TITLE": {"kept"}}},
		[]container.Tag{{Key: "ARTIST", Value: "one"}, {Key: "GENRE", Value: "g"}, {Key: "ARTIST", Value: "two"}}); err != nil {
		t.Fatal(err)
	}
	info := readBack(t, path, "flac")
	if got := info.Tags["ARTIST"]; !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("ARTIST = %q, want [one two]", got)
	}
	if got := info.Tags["TITLE"]; !slices.Equal(got, []string{"kept"}) {
		t.Errorf("TITLE = %q, want [kept]", got)
	}
}

// oggPage frames one Ogg page holding a single packet of under 255 bytes.
func oggPage(headerType byte, serial, seq uint32, packet []byte) []byte {
	p := []byte("OggS\x00")
	p = append(p, headerType)
	p = binary.LittleEndian.AppendUint64(p, 0)
	p = binary.LittleEndian.AppendUint32(p, serial)
	p = binary.LittleEndian.AppendUint32(p, seq)
	p = append(p, 0, 0, 0, 0, 1, byte(len(packet)))
	p = append(p, packet...)
	var crc uint32
	for _, b := range p {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	binary.LittleEndian.PutUint32(p[22:], crc)
	return p
}

// TestMultiplexedStreamIsANoteNotAWarning: an Ogg file whose audio shares its
// pages with a second logical stream (a Skeleton index, here) reads its tags
// exactly, so the shape must not spend a warning.
func TestMultiplexedStreamIsANoteNotAWarning(t *testing.T) {
	path := writeEncoded(t, "vorbis", "tagged.ogg")
	if err := label.New().Apply(t.Context(), path,
		&meta.Info{Tags: map[string][]string{"TITLE": {"A Title"}}}, nil); err != nil {
		t.Fatal(err)
	}
	ogg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second stream that begins beside the first: its beginning-of-stream
	// page goes right behind the audio's, which is one page of 27 bytes, a
	// one-entry segment table and its packet.
	first := 27 + 1 + int(ogg[27])
	skeleton := append([]byte("fishead\x00"), make([]byte, 56)...)
	const serial = 0x5eed
	src := slices.Concat(ogg[:first],
		oggPage(0x02, serial, 0, skeleton), ogg[first:], oggPage(0x04, serial, 1, nil))

	info, err := label.New().Read(t.Context(), container.BytesSource(src), "ogg", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Tags["TITLE"]; !slices.Equal(got, []string{"A Title"}) {
		t.Fatalf("TITLE = %q off a multiplexed source, want [A Title]", got)
	}
	if !slices.ContainsFunc(info.Notes, func(n string) bool { return strings.Contains(n, "multiplexed-stream") }) {
		t.Errorf("no multiplexed-stream note; this cell covers nothing: notes %v, warnings %v", info.Notes, info.Warnings)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("a readable multiplexed source warned: %v", info.Warnings)
	}
}

// TestChaptersComeBackInStartOrder: a Nero chapter list may be stored out of
// order, and the tag library returns it as stored. A demuxer's chapters come
// sorted (container.Chapterer), and the mapper's replace them, so a caller
// taking chapter n's end from chapter n+1's start needs the same order here.
func TestChaptersComeBackInStartOrder(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: "aac"}, waxflow.WithChapters([]container.Chapter{
			{Start: 0, Title: "first"}, {Start: 20 * time.Millisecond, Title: "later"},
		})); err != nil {
		t.Fatal(err)
	}
	// Swap the two entries' start times in the chpl: each is an 8-byte start
	// in 100 ns units, a length byte and a five-letter title.
	src := out.Bytes()
	a, b := bytes.Index(src, []byte("\x05first")), bytes.Index(src, []byte("\x05later"))
	if a < 8 || b < 8 {
		t.Fatal("the fixture holds no chpl entries to swap")
	}
	var start [8]byte
	copy(start[:], src[a-8:a])
	copy(src[a-8:a], src[b-8:b])
	copy(src[b-8:b], start[:])

	info, err := label.New().Read(t.Context(), container.BytesSource(src), "m4a", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Chapters) != 2 || info.Chapters[0].Title != "later" || info.Chapters[1].Title != "first" ||
		info.Chapters[0].Start != 0 || info.Chapters[1].Start != 20*time.Millisecond {
		t.Errorf("chapters = %+v, want later at 0 then first at 20ms", info.Chapters)
	}
}
