package tofile

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// fakeMapper answers Read with info or readErr and Apply with applyErr,
// counting both, and runs onApply inside Apply.
type fakeMapper struct {
	info            *meta.Info
	readErr         error
	applyErr        error
	onRead, onApply func()
	reads, applies  int
	applied         *meta.Info // what the last Apply was handed
}

func (m *fakeMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	m.reads++
	if m.onRead != nil {
		m.onRead()
	}
	if m.readErr != nil {
		return nil, m.readErr
	}
	return m.info, nil
}

func (m *fakeMapper) Apply(_ context.Context, _ string, info *meta.Info, _ []container.Tag) error {
	m.applies++
	m.applied = info
	if m.onApply != nil {
		m.onApply()
	}
	return m.applyErr
}

// taggedSource writes a flat m4a carrying a TITLE and two chapters, so the
// container itself holds metadata to fold.
func taggedSource(t *testing.T) string {
	t.Helper()
	return taggedWith(t, container.Tag{Key: "TITLE", Value: "from the container"})
}

// taggedWith is taggedSource holding tags.
func taggedWith(t *testing.T, tags ...container.Tag) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tagged.m4a")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src := openSource(t, testutil.PkgFile("..", "..", "testdata", "sine-s16.wav"))
	_, err = waxflow.New().Transcode(context.Background(), src, "wav", f,
		waxflow.TranscodeOptions{Format: "alac", Container: waxflow.ContainerProgressive},
		waxflow.WithTags(tags),
		waxflow.WithChapters([]container.Chapter{
			{Start: 0, End: 50 * time.Millisecond, Title: "one"},
			{Start: 50 * time.Millisecond, End: 100 * time.Millisecond, Title: "two"},
		}))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func openSource(t *testing.T, path string) container.Source {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	src, err := container.FileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// request plans opts against the source at path and opens an output for it.
func request(t *testing.T, path string, opts waxflow.TranscodeOptions) Request {
	t.Helper()
	e := waxflow.New()
	src := openSource(t, path)
	hint := filepath.Ext(path)
	probe, err := e.Probe(context.Background(), src, hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanTranscode(probe.Default(), opts)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out."+waxflow.OutputExt(opts.Format, opts.Container))
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return Request{Engine: e, Source: src, Hint: hint, Probe: probe, Options: opts, Plan: plan, Out: f, Path: out}
}

// probeOutput reads back what Run wrote.
func probeOutput(t *testing.T, path string) *format.Info {
	t.Helper()
	info, err := waxflow.New().Probe(context.Background(), openSource(t, path), filepath.Ext(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// closed reports whether f was closed.
func closed(f *os.File) bool {
	_, err := f.Seek(0, io.SeekCurrent)
	return errors.Is(err, os.ErrClosed)
}

var flatALAC = waxflow.TranscodeOptions{Format: "alac", Container: waxflow.ContainerProgressive}

func TestRunClosesOutOnEveryPathAndRemovesNothing(t *testing.T) {
	failing := errors.New("no gain")
	for _, tc := range []struct {
		name string
		// set shapes the request and may cancel the run.
		set         func(req *Request, cancel context.CancelFunc)
		wantErr     bool
		wantEncoded bool
	}{
		{"a success", func(*Request, context.CancelFunc) {}, false, true},
		{"a failure before the encode", func(req *Request, _ context.CancelFunc) {
			req.Gain = func(*meta.Info) (float64, error) { return 0, failing }
		}, true, false},
		{"a failed encode", func(_ *Request, cancel context.CancelFunc) { cancel() }, true, false},
		{"a failure after the encode", func(req *Request, cancel context.CancelFunc) {
			req.Loudness = true
			req.Progress = func(phase string, _, _ int64) {
				if phase == "finalize" {
					cancel()
				}
			}
		}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := request(t, longWAV(t), waxflow.TranscodeOptions{Format: "flac"})
			tc.set(&req, cancel)
			res, err := Run(ctx, req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run = %v, want an error %v", err, tc.wantErr)
			}
			if encoded := res != nil && res.Transcode != nil; encoded != tc.wantEncoded {
				t.Errorf("Run reported an encode %v, want %v", encoded, tc.wantEncoded)
			}
			if !closed(req.Out) {
				t.Error("Run left Out open")
			}
			if _, err := os.Stat(req.Path); err != nil {
				t.Errorf("Run removed the output: %v", err)
			}
		})
	}
}

func TestWriteMediaSyncsBeforeItCloses(t *testing.T) {
	// Linux refuses fsync on a pipe (EINVAL); elsewhere the premise is
	// probed on a scratch pipe first.
	if runtime.GOOS != "linux" {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		serr := pw.Sync()
		pr.Close()
		pw.Close()
		if serr == nil {
			t.Skipf("a pipe takes Sync on %s, so its refusal cannot mark the order", runtime.GOOS)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	want := &waxflow.TranscodeResult{Samples: 1}
	got, err := WriteMedia(w, func(io.Writer) (*waxflow.TranscodeResult, error) { return want, nil }, nil)
	// Had the close come first, the sync would have found the file closed.
	if err == nil || errors.Is(err, os.ErrClosed) || waxerr.CodeOf(err) != waxerr.CodeOutputUnwritable {
		t.Fatalf("WriteMedia = %v, want the sync's own refusal", err)
	}
	if got != want {
		t.Error("WriteMedia dropped the encode's result")
	}
	if !closed(w) {
		t.Error("WriteMedia left the file open")
	}
}

func TestWriteMediaReturnsTheEncodesErrorOverACloses(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	encodeErr := errors.New("encode failed")
	patched := false
	_, err = WriteMedia(f, func(io.Writer) (*waxflow.TranscodeResult, error) {
		f.Close() // so WriteMedia's own close fails too
		return nil, encodeErr
	}, func(*os.File, *waxflow.TranscodeResult) error {
		patched = true
		return nil
	})
	if err != encodeErr {
		t.Errorf("WriteMedia = %v, want the encode's error", err)
	}
	if patched {
		t.Error("WriteMedia patched a failed encode")
	}
}

func TestACanceledMetadataReadAbortsRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &fakeMapper{onRead: cancel, readErr: context.Canceled}
	req := request(t, testutil.PkgFile("..", "..", "testdata", "sine-s16.wav"), waxflow.TranscodeOptions{Format: "flac"})
	req.Mapper = m
	if _, err := Run(ctx, req); err == nil {
		t.Fatal("Run went on past a canceled metadata read")
	}
	if fi, err := os.Stat(req.Path); err != nil || fi.Size() != 0 {
		t.Errorf("the output is %v (%v), want it untouched", fi, err)
	}
}

func TestAFailedMetadataReadIsANoteAndTheContainerTagsFold(t *testing.T) {
	req := request(t, taggedSource(t), flatALAC)
	req.Mapper = &fakeMapper{readErr: waxerr.New(waxerr.CodeMalformedInput, "unparsable frame")}
	var notes []Note
	req.Note = func(n Note) { notes = append(notes, n) }
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(notes, func(n Note) bool {
		return n.Kind == NoteMetadataUnread && n.Text == "unparsable frame" && n.Err != nil
	}) {
		t.Errorf("notes = %+v, want the failed read", notes)
	}
	if got := probeOutput(t, req.Path).Tags["TITLE"]; !slices.Equal(got, []string{"from the container"}) {
		t.Errorf("TITLE = %q, want the container's", got)
	}
}

func TestAFailedPostPassIsANote(t *testing.T) {
	req := request(t, testutil.PkgFile("..", "..", "testdata", "sine-s16.wav"), waxflow.TranscodeOptions{Format: "flac"})
	m := &fakeMapper{info: &meta.Info{Tags: map[string][]string{"TITLE": {"t"}}}, applyErr: errors.New("not taggable")}
	req.Mapper = m
	var notes []Note
	req.Note = func(n Note) { notes = append(notes, n) }
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatalf("Run = %v, want the failed post-pass as a note", err)
	}
	if m.applies != 1 || !slices.ContainsFunc(notes, func(n Note) bool {
		return n.Kind == NotePostPassFailed && n.Text == "not taggable"
	}) {
		t.Errorf("%d post-passes, notes %+v; want one, failed, noted", m.applies, notes)
	}
}

// TestTheSourcesEncodingTagsStayBehind: every file output is a fresh encode,
// so the source's encoder stamp and mp3gain's record of its frames would be
// false on it. Neither door may carry them: the tags the muxer embeds, nor
// the set the post-pass writes. What still describes the audio is carried:
// who encoded the source, its coding history and its fingerprint.
func TestTheSourcesEncodingTagsStayBehind(t *testing.T) {
	kept := []string{"TITLE", "ENCODEDBY", "ENCODING_HISTORY", "ACOUSTID_FINGERPRINT"}
	left := []string{"ENCODER", "MP3GAIN_UNDO", "MP3GAIN_MINMAX", "MP3GAIN_ALBUM_MINMAX"}
	stamped := func() *meta.Info {
		tags := map[string][]string{}
		for _, k := range slices.Concat(kept, left) {
			tags[k] = []string{"v"}
		}
		return &meta.Info{Tags: tags}
	}
	wav := testutil.PkgFile("..", "..", "testdata", "sine-s16.wav")
	check := func(t *testing.T, where string, tags map[string][]string) {
		t.Helper()
		for _, k := range kept {
			if !slices.Equal(tags[k], []string{"v"}) {
				t.Errorf("%s: %s = %q, want it carried", where, k, tags[k])
			}
		}
		for _, k := range left {
			if v, ok := tags[k]; ok {
				t.Errorf("%s: %s = %q, want it left behind", where, k, v)
			}
		}
	}
	t.Run("the post-pass", func(t *testing.T) {
		req := request(t, wav, waxflow.TranscodeOptions{Format: "flac"})
		m := &fakeMapper{info: stamped()}
		req.Mapper = m
		if _, err := Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if m.applied == nil {
			t.Fatal("no post-pass ran, so this cell covers nothing")
		}
		check(t, "the set the post-pass was handed", m.applied.Tags)
		if _, ok := m.info.Tags["ENCODER"]; !ok {
			t.Error("the mapper's own Info was edited in place")
		}
	})
	t.Run("the muxer", func(t *testing.T) {
		req := request(t, wav, waxflow.TranscodeOptions{Format: "wavpack"})
		req.Mapper = &fakeMapper{info: stamped()}
		if _, err := Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		check(t, "the tags the muxer embedded", probeOutput(t, req.Path).Tags)
	})
}

// TestUnwritableSourceTextIsRepairedOnceForBothDoors: a NUL or invalid UTF-8
// in a source's tag reaches a muxer as given, where the NUL splits an ID3
// value in two and lands raw in an MP4 atom. It is repaired where the
// metadata is read, so the muxer and the post-pass write one text, and the
// repair is a warning, since part of the value does not survive.
func TestUnwritableSourceTextIsRepairedOnceForBothDoors(t *testing.T) {
	wav := testutil.PkgFile("..", "..", "testdata", "sine-s16.wav")
	bad := func() *meta.Info {
		return &meta.Info{
			Tags:     map[string][]string{"TITLE": {"t"}, "ARTIST": {"cut\x00here", "whole"}, "ALBUM": {"bad\xffbyte"}},
			Chapters: []container.Chapter{{Start: 0, Title: "one\x00two"}},
		}
	}
	req := request(t, wav, waxflow.TranscodeOptions{Format: "wavpack"})
	m := &fakeMapper{info: bad()}
	req.Mapper = m
	var notes []Note
	req.Note = func(n Note) { notes = append(notes, n) }
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	tags := probeOutput(t, req.Path).Tags
	if got := tags["ARTIST"]; !slices.Equal(got, []string{"cut", "whole"}) {
		t.Errorf("the muxer wrote ARTIST = %q, want [cut whole]", got)
	}
	if got := tags["ALBUM"]; !slices.Equal(got, []string{"bad\uFFFDbyte"}) {
		t.Errorf("the muxer wrote ALBUM = %q, want the invalid byte replaced", got)
	}
	for _, what := range []string{"tag ARTIST", "tag ALBUM", "a chapter title"} {
		if n := countNotes(notes, what); n != 1 {
			t.Errorf("%d warnings name %q, want 1: %+v", n, what, notes)
		}
	}
	if countNotes(notes, "tag TITLE") != 0 {
		t.Errorf("a writable tag drew a warning: %+v", notes)
	}

	// The post-pass is handed the repaired set, not the source's.
	post := request(t, wav, waxflow.TranscodeOptions{Format: "flac"})
	pm := &fakeMapper{info: bad()}
	post.Mapper = pm
	if _, err := Run(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	if pm.applied == nil || !slices.Equal(pm.applied.Tags["ARTIST"], []string{"cut", "whole"}) ||
		len(pm.applied.Chapters) != 1 || pm.applied.Chapters[0].Title != "one" {
		t.Errorf("the post-pass was handed %+v, want the repaired text", pm.applied)
	}
}

// countNotes counts the metadata warnings whose text starts with what.
func countNotes(notes []Note, what string) int {
	n := 0
	for _, note := range notes {
		if note.Kind == NoteMetadataWarning && strings.HasPrefix(note.Text, what+" held") {
			n++
		}
	}
	return n
}

// TestATagTheMuxerHasNoFormForIsNotedOnlyWhereItIsLost: a muxer skips a key
// it has no form for and says nothing. Where it is the final tag writer the
// tag is gone, so the run says so; where the post-pass writes after it, the
// tag library takes the key and nothing is lost.
func TestATagTheMuxerHasNoFormForIsNotedOnlyWhereItIsLost(t *testing.T) {
	wav := testutil.PkgFile("..", "..", "testdata", "sine-s16.wav")
	for _, tc := range []struct {
		opts  waxflow.TranscodeOptions
		key   string
		noted bool
	}{
		// A custom key an MP4 freeform would read back as MUSICBRAINZ_TRACKID.
		{flatALAC, "MUSICBRAINZ TRACK ID", true},
		// An APEv2 item name is two characters at least.
		{waxflow.TranscodeOptions{Format: "wavpack"}, "X", true},
		{waxflow.TranscodeOptions{Format: "flac"}, "X", false},
	} {
		warning := "tag " + tc.key + " has no form in the output and was not embedded"
		req := request(t, wav, tc.opts)
		req.Mapper = &fakeMapper{info: &meta.Info{Tags: map[string][]string{"TITLE": {"t"}, tc.key: {"a value"}}}}
		var notes []Note
		req.Note = func(n Note) { notes = append(notes, n) }
		if _, err := Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		noted := slices.ContainsFunc(notes, func(n Note) bool { return n.Kind == NoteMetadataWarning && n.Text == warning })
		if noted != tc.noted {
			t.Errorf("%s: noted %t, want %t (notes %+v)", tc.opts.Format, noted, tc.noted, notes)
		}
		if tc.noted {
			tags := probeOutput(t, req.Path).Tags
			if !slices.Equal(tags["TITLE"], []string{"t"}) || len(tags) != 1 {
				t.Errorf("%s: the output holds %v, want TITLE alone", tc.opts.Format, tags)
			}
		}
	}
}

// TestWithNoMapperWhatTheMuxerLeavesOutIsNoted: with no mapper nothing
// writes tags after the muxer, so what the muxer leaves out is lost and is
// said, in one note: nothing for a FLAC, whose comment block takes both
// keys, the key with no frame for an MP3, and every key for a WAV. With a
// mapper the post-pass writes them and nothing is said.
func TestWithNoMapperWhatTheMuxerLeavesOutIsNoted(t *testing.T) {
	src := taggedWith(t, container.Tag{Key: "TITLE", Value: "t"}, container.Tag{Key: "MOOD", Value: "m"})
	for _, tc := range []struct {
		format string
		mapper meta.Mapper
		want   string
	}{
		{"flac", nil, ""},
		{"mp3", nil, "tag MOOD has no form in the output and was not embedded"},
		{"wav", nil, "tags MOOD, TITLE have no form in the output and were not embedded"},
		{"mp3", &fakeMapper{info: &meta.Info{Tags: map[string][]string{"TITLE": {"t"}, "MOOD": {"m"}}}}, ""},
	} {
		req := request(t, src, waxflow.TranscodeOptions{Format: tc.format})
		req.Mapper = tc.mapper
		var got []string
		req.Note = func(n Note) {
			if n.Kind == NoteMetadataWarning && strings.Contains(n.Text, "no form in the output") {
				got = append(got, n.Text)
			}
		}
		if _, err := Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		var want []string
		if tc.want != "" {
			want = []string{tc.want}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s, mapper %t: noted %q, want %q", tc.format, tc.mapper != nil, got, want)
		}
	}
}

func TestACancelInThePostPassIsACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := request(t, testutil.PkgFile("..", "..", "testdata", "sine-s16.wav"), waxflow.TranscodeOptions{Format: "flac"})
	req.Mapper = &fakeMapper{info: &meta.Info{Tags: map[string][]string{"TITLE": {"t"}}}, onApply: cancel, applyErr: context.Canceled}
	var notes []Note
	req.Note = func(n Note) { notes = append(notes, n) }
	if _, err := Run(ctx, req); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("Run = %v, want %s", err, waxerr.CodeCanceled)
	}
	if slices.ContainsFunc(notes, func(n Note) bool { return n.Kind == NotePostPassFailed }) {
		t.Errorf("notes = %+v, want the cancel alone", notes)
	}
}

func TestNoMetadataWritesNoneWhereANilMapperFolds(t *testing.T) {
	src := taggedSource(t)

	none := request(t, src, flatALAC)
	m := &fakeMapper{info: &meta.Info{
		Tags:     map[string][]string{"ARTIST": {"mapper"}},
		Pictures: []meta.Picture{{MIME: "image/png", Front: true, Data: []byte("\x89PNG\r\n\x1a\n")}},
	}}
	none.Mapper, none.NoMetadata = m, true
	if _, err := Run(context.Background(), none); err != nil {
		t.Fatal(err)
	}
	if m.reads != 0 || m.applies != 0 {
		t.Errorf("NoMetadata read %d times and post-passed %d", m.reads, m.applies)
	}
	if info := probeOutput(t, none.Path); len(info.Tags) != 0 || len(info.Chapters) != 0 {
		t.Errorf("NoMetadata wrote tags %v and chapters %v", info.Tags, info.Chapters)
	}

	nilMapper := request(t, src, flatALAC)
	if _, err := Run(context.Background(), nilMapper); err != nil {
		t.Fatal(err)
	}
	info := probeOutput(t, nilMapper.Path)
	if !slices.Equal(info.Tags["TITLE"], []string{"from the container"}) || len(info.Chapters) != 2 {
		t.Errorf("a nil mapper wrote tags %v and chapters %v, want the container's", info.Tags, info.Chapters)
	}
}

// longWAV writes a two-second stereo sine, long enough for an integrated
// loudness.
func longWAV(t *testing.T) string {
	t.Helper()
	const rate, frames = 44100, 2 * 44100
	data := make([]byte, 0, frames*4)
	for i := range frames {
		v := uint16(int16(8000 * math.Sin(2*math.Pi*997*float64(i)/rate)))
		data = binary.LittleEndian.AppendUint16(data, v)
		data = binary.LittleEndian.AppendUint16(data, v)
	}
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*4)
	b = binary.LittleEndian.AppendUint16(b, 4)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	path := filepath.Join(t.TempDir(), "long.wav")
	if err := os.WriteFile(path, append(b, data...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoudnessPatchesAnMP4Output(t *testing.T) {
	req := request(t, longWAV(t), flatALAC)
	req.Loudness = true
	phases := map[string]bool{}
	req.Progress = func(phase string, _, _ int64) { phases[phase] = true }
	res, err := Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputEstimated || len(res.RG) != 2 {
		t.Fatalf("result %+v, want a measured ReplayGain", res)
	}
	if res.RG[1].Value == meta.FormatPeak(0) {
		t.Fatalf("the measured peak is the placeholder %q", res.RG[1].Value)
	}
	tags := probeOutput(t, req.Path).Tags
	for _, rg := range res.RG {
		if got := tags[rg.Key]; !slices.Equal(got, []string{rg.Value}) {
			t.Errorf("%s reads back %q, want [%q]", rg.Key, got, rg.Value)
		}
	}
	for _, phase := range []string{"analyze", "transcode", "finalize"} {
		if !phases[phase] {
			t.Errorf("no %s progress", phase)
		}
	}
}

func TestTwoPassTranscodeReportsDamageOnce(t *testing.T) {
	raw, err := os.ReadFile(testutil.PkgFile("..", "..", "testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "damaged.mp3")
	if err := os.WriteFile(path, testutil.ZeroMiddle(raw, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, failEncode bool) (*Result, []string, error) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := request(t, path, waxflow.TranscodeOptions{Format: "flac"})
		req.Loudness = true
		var found []string
		req.Note = func(n Note) {
			switch {
			case n.Kind == NoteInputDamage:
				found = append(found, n.Text)
			case n.Kind == NoteLoudnessSource && failEncode:
				cancel()
			}
		}
		res, err := Run(ctx, req)
		return res, found, err
	}

	t.Run("each finding once", func(t *testing.T) {
		res, found, err := run(t, false)
		if err != nil {
			t.Fatal(err)
		}
		// Both reads cross the damage, so each finding is in both lists.
		if len(res.Source.InputWarnings) == 0 || len(res.Transcode.InputWarnings) == 0 {
			t.Fatalf("the pass found %q and the encode %q, want the damage in both", res.Source.InputWarnings, res.Transcode.InputWarnings)
		}
		want := slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(res.Source.InputWarnings), res.Transcode.InputWarnings...))))
		if got := slices.Sorted(slices.Values(found)); !slices.Equal(got, want) {
			t.Errorf("noted %q, want each finding once: %q", found, want)
		}
	})

	t.Run("the pass's findings survive a failed encode", func(t *testing.T) {
		res, found, err := run(t, true)
		if err == nil || res != nil {
			t.Fatalf("Run = %+v, %v; want the encode to fail", res, err)
		}
		if len(found) == 0 {
			t.Error("an encode that failed after the pass took the pass's findings with it")
		}
	})
}

// TestADroppedTagIsNotedOnlyWhereItIsLost: a source tag too large for the
// output's own block is dropped from what the muxer is handed, and noted
// only where no post-pass restores it: an embedding muxer, or no mapper to
// run one. An MP4's ilst has no cap, so it keeps the tag.
func TestADroppedTagIsNotedOnlyWhereItIsLost(t *testing.T) {
	wav := testutil.PkgFile("..", "..", "testdata", "sine-s16.wav")
	big := strings.Repeat("c", 60000)
	for _, tc := range []struct {
		name   string
		opts   waxflow.TranscodeOptions
		mapper bool
		noted  bool
	}{
		{"flac, post-passed", waxflow.TranscodeOptions{Format: "flac"}, true, false},
		{"alac, held by the muxer", flatALAC, true, false},
		{"wavpack, embedded by the muxer", waxflow.TranscodeOptions{Format: "wavpack"}, true, true},
		{"flac, no mapper", waxflow.TranscodeOptions{Format: "flac"}, false, true},
	} {
		req := request(t, wav, tc.opts)
		req.Probe.Tags = map[string][]string{"COMMENT": {big}}
		m := &fakeMapper{info: &meta.Info{}}
		if tc.mapper {
			req.Mapper = m
		}
		var notes []Note
		req.Note = func(n Note) { notes = append(notes, n) }
		if _, err := Run(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		noted := slices.ContainsFunc(notes, func(n Note) bool { return n.Kind == NoteTagTooLarge && n.Key == "COMMENT" })
		if noted != tc.noted {
			t.Errorf("%s: COMMENT noted dropped %t, want %t (notes %+v)", tc.name, noted, tc.noted, notes)
		}
		if tc.mapper && waxflow.OutputEmbedsTags(tc.opts.Format, tc.opts.Container) {
			raw, err := os.ReadFile(req.Path)
			if err != nil {
				t.Fatal(err)
			}
			if kept := bytes.Contains(raw, []byte(big)); kept == tc.noted {
				t.Errorf("%s: the output carries COMMENT %t, want %t", tc.name, kept, !tc.noted)
			}
		}
	}
}

// TestADroppedKeyIsNotedOncePerRun: a key is noted once whenever any of its
// values is dropped, however many were.
func TestADroppedKeyIsNotedOncePerRun(t *testing.T) {
	req := request(t, testutil.PkgFile("..", "..", "testdata", "sine-s16.wav"), waxflow.TranscodeOptions{Format: "wavpack"})
	req.Probe.Tags = map[string][]string{"COMMENT": {strings.Repeat("c", 60000), strings.Repeat("d", 60000)}}
	req.Mapper = &fakeMapper{info: &meta.Info{}}
	var notes []Note
	req.Note = func(n Note) { notes = append(notes, n) }
	if _, err := Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var dropped []string
	for _, n := range notes {
		if n.Kind == NoteTagTooLarge {
			dropped = append(dropped, n.Key)
		}
	}
	if !slices.Equal(dropped, []string{"COMMENT"}) {
		t.Errorf("noted dropped %q, want COMMENT once", dropped)
	}
}

// TestLoudnessMakesRoomForTheReplayGain: a source whose tags sit just under
// the embedding cap takes the ReplayGain the transcode embeds. Where the
// writer's block cannot hold both, the source's tags are left out in order
// until the set fits, each noted; an MP4 holds them all.
func TestLoudnessMakesRoomForTheReplayGain(t *testing.T) {
	comment := strings.Repeat("c", 49060)
	for _, tc := range []struct {
		opts    waxflow.TranscodeOptions
		dropped bool
	}{
		{waxflow.TranscodeOptions{Format: "wavpack"}, true},
		{waxflow.TranscodeOptions{Format: "ape"}, true},
		{waxflow.TranscodeOptions{Format: "flac", Container: waxflow.ContainerOgg}, true},
		{flatALAC, false},
	} {
		name := tc.opts.Format + "/" + tc.opts.Container
		req := request(t, longWAV(t), tc.opts)
		req.Loudness = true
		req.Mapper = &fakeMapper{info: &meta.Info{Tags: map[string][]string{"COMMENT": {comment}}}}
		var notes []Note
		req.Note = func(n Note) { notes = append(notes, n) }
		if _, err := Run(context.Background(), req); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		raw, err := os.ReadFile(req.Path)
		if err != nil {
			t.Fatal(err)
		}
		// An MP4 freeform spells the name in lowercase, as taggers do.
		upper := bytes.ToUpper(raw)
		for _, key := range []string{"REPLAYGAIN_TRACK_GAIN", "REPLAYGAIN_TRACK_PEAK"} {
			if !bytes.Contains(upper, []byte(key)) {
				t.Errorf("%s: the output carries no %s", name, key)
			}
		}
		if kept := bytes.Contains(raw, []byte(comment)); kept == tc.dropped {
			t.Errorf("%s: the output carries COMMENT %t, want %t", name, kept, !tc.dropped)
		}
		noted := slices.ContainsFunc(notes, func(n Note) bool { return n.Kind == NoteTagTooLarge && n.Key == "COMMENT" })
		if noted != tc.dropped {
			t.Errorf("%s: COMMENT noted dropped %t, want %t (notes %+v)", name, noted, tc.dropped, notes)
		}
	}
}
