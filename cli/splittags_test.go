package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestSplitMakesRoomForEachPiece: a rip whose tags sit just under the
// embedding cap splits, a long title included. A piece's writer is handed the
// rip's tags then the piece's own wherever its block holds them; where it
// cannot, the rip's are left out until the set fits. Where the post-pass
// rewrites a piece it writes the full set and nothing is warned; where the
// muxer is the final tag writer the loss is one warning for the split.
func TestSplitMakesRoomForEachPiece(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 100_000), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "album.flac")
	if code, _, errOut := run(t, "transcode", wav, src); code != 0 {
		t.Fatalf("building the source: %s", errOut)
	}
	comment := strings.Repeat("c", 49030)
	if err := label.New().Apply(context.Background(), src, &meta.Info{Tags: map[string][]string{
		"ALBUM": {"The Album"}, "COMMENT": {comment},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("t", 120)
	sheet := filepath.Join(dir, "album.cue")
	if err := os.WriteFile(sheet, []byte("FILE \"album.flac\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \""+long+"\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 00:01:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := []string{"--at", "50000"}
	cue := []string{"--cue", sheet}
	both, neither, second := [2]bool{true, true}, [2]bool{}, [2]bool{false, true}
	for _, tc := range []struct {
		name   string
		args   []string
		kept   [2]bool // whether each piece carries COMMENT
		warned bool
	}{
		{"wavpack", append([]string{"--format", "wavpack"}, at...), neither, true},
		{"alac", append([]string{"--format", "alac"}, at...), both, false},
		{"ogg flac", append([]string{"--format", "flac", "--container", "ogg"}, at...), both, false},
		{"flac", append([]string{"--format", "flac"}, at...), both, false},
		{"flac, a long title", append([]string{"--format", "flac"}, cue...), both, false},
		{"opus, a long title", append([]string{"--format", "opus"}, cue...), both, false},
		{"vorbis, a long title", append([]string{"--format", "vorbis"}, cue...), both, false},
		{"mp3, a long title", append([]string{"--format", "mp3"}, cue...), both, false},
		{"ogg flac, a long title", append([]string{"--format", "flac", "--container", "ogg"}, cue...), second, true},
		{"wavpack, a long title", append([]string{"--format", "wavpack"}, cue...), neither, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name)
			code, _, errOut := run(t, append([]string{"split", src, out}, tc.args...)...)
			if code != 0 {
				t.Fatalf("split exit = %d: %s", code, errOut)
			}
			n := strings.Count(errOut, "tag too large for the output, not embedded")
			if tc.warned && (n != 1 || !strings.Contains(errOut, "key=COMMENT")) || !tc.warned && n != 0 {
				t.Errorf("stderr reports %d dropped tags, want COMMENT %t:\n%s", n, tc.warned, errOut)
			}
			names := outputNames(t, out)
			if len(names) != 2 {
				t.Fatalf("the split wrote %q, want two pieces", names)
			}
			for i, name := range names {
				raw, err := os.ReadFile(filepath.Join(out, name))
				if err != nil {
					t.Fatal(err)
				}
				info, err := label.New().Read(context.Background(), container.BytesSource(raw),
					strings.TrimPrefix(filepath.Ext(name), "."), meta.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if kept := slices.Equal(info.Tags["COMMENT"], []string{comment}); kept != tc.kept[i] {
					t.Errorf("%s carries COMMENT %t, want %t", name, kept, tc.kept[i])
				}
				if i == 0 && slices.Contains(tc.args, "--cue") && !slices.Equal(info.Tags["TITLE"], []string{long}) {
					t.Errorf("%s is titled %.20q, want its own long title", name, info.Tags["TITLE"])
				}
			}
		})
	}
}

// TestTranscodeMakesRoomForItsReplayGain: a source whose tags sit just under
// the embedding cap transcodes with --loudness analyze and carries the
// ReplayGain it embeds. Where the writer's block cannot hold the source's
// tags beside it, the source's are left out until the set fits, with one
// line; an MP4 holds them all.
func TestTranscodeMakesRoomForItsReplayGain(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 2*44100), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "album.flac")
	if code, _, errOut := run(t, "transcode", wav, src); code != 0 {
		t.Fatalf("building the source: %s", errOut)
	}
	comment := strings.Repeat("c", 49060)
	if err := label.New().Apply(context.Background(), src, &meta.Info{Tags: map[string][]string{
		"COMMENT": {comment},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	const line = "metadata: tag COMMENT is too large for the output and was not embedded"
	for _, tc := range []struct {
		name, ext string
		args      []string
		dropped   bool
	}{
		{"wavpack", "wv", []string{"--format", "wavpack"}, true},
		{"ogg flac", "oga", []string{"--format", "flac", "--container", "ogg"}, true},
		{"alac", "m4a", []string{"--format", "alac"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, "out."+tc.ext)
			code, _, errOut := run(t, append([]string{"transcode", src, out, "--loudness", "analyze"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			want := 0
			if tc.dropped {
				want = 1
			}
			if n := strings.Count(errOut, line); n != want {
				t.Errorf("printed the drop %d times, want %d:\n%s", n, want, errOut)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			info, err := label.New().Read(context.Background(), container.BytesSource(raw), tc.ext, meta.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Tags["REPLAYGAIN_TRACK_GAIN"]) != 1 || len(info.Tags["REPLAYGAIN_TRACK_PEAK"]) != 1 {
				t.Errorf("the output carries ReplayGain %q / %q, want one of each",
					info.Tags["REPLAYGAIN_TRACK_GAIN"], info.Tags["REPLAYGAIN_TRACK_PEAK"])
			}
			if kept := slices.Equal(info.Tags["COMMENT"], []string{comment}); kept == tc.dropped {
				t.Errorf("the output carries COMMENT %t, want %t", kept, !tc.dropped)
			}
		})
	}
}

// TestSplitRefusesASheetTitleTooLargeToEmbed: a piece's title is the
// sheet's, so one the pieces' tag block cannot hold is a usage refusal
// before any piece is written. An MP4's ilst holds it.
func TestSplitRefusesASheetTitleTooLargeToEmbed(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "rip.wav")
	if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 6*44100), 0o644); err != nil {
		t.Fatal(err)
	}
	sheet := filepath.Join(dir, "rip.cue")
	if err := os.WriteFile(sheet, []byte("FILE \"rip.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \""+strings.Repeat("t", 60000)+"\"\n    INDEX 01 00:03:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "pieces")
	code, _, errOut := run(t, "split", wav, out, "--cue", sheet)
	if want := "cue: track 2: a 60000-byte title is too large to embed in a piece's tags"; code != 2 || !strings.Contains(errOut, want) {
		t.Errorf("split exit = %d, stderr %q; want 2 and %q", code, errOut, want)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused split left %s behind (%v)", out, err)
	}
	// An MP4's ilst holds the title, so the same sheet splits to alac.
	if code, _, errOut := run(t, "split", wav, out, "--cue", sheet, "--format", "alac"); code != 0 {
		t.Fatalf("alac split exit = %d: %s", code, errOut)
	}
	names := outputNames(t, out)
	if len(names) != 2 {
		t.Fatalf("the alac split wrote %q, want two pieces", names)
	}
	raw, err := os.ReadFile(filepath.Join(out, names[1]))
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(context.Background(), container.BytesSource(raw), "m4a", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Tags["TITLE"]; len(got) != 1 || got[0] != strings.Repeat("t", 60000) {
		t.Errorf("%s is titled %d values, want the sheet's 60000-byte title", names[1], len(got))
	}
}
