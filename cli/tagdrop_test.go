package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestTranscodeNotesADroppedTagOnlyWhereItIsLost: a source tag too large
// for the output's block is reported only where nothing restores it. The
// post-pass rewrites a FLAC output with the full set, so it carries the tag
// and nothing is printed; an MP4's ilst holds it; a WavPack muxer is the
// final tag writer and loses it, and says so.
func TestTranscodeNotesADroppedTagOnlyWhereItIsLost(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 44100), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "big.m4a")
	if code, _, errOut := run(t, "transcode", wav, src, "--format", "alac"); code != 0 {
		t.Fatalf("building the source: %s", errOut)
	}
	if err := label.New().Apply(context.Background(), src, &meta.Info{Tags: map[string][]string{
		"COMMENT": {strings.Repeat("c", 60000)},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	const line = "metadata: tag COMMENT is too large for the output and was not embedded"
	for _, tc := range []struct {
		ext    string
		args   []string
		warned bool
	}{{"flac", nil, false}, {"m4a", []string{"--format", "alac"}, false}, {"wv", nil, true}} {
		out := filepath.Join(dir, "out."+tc.ext)
		code, _, errOut := run(t, append([]string{"transcode", src, out}, tc.args...)...)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.ext, code, errOut)
		}
		if got := strings.Contains(errOut, line); got != tc.warned {
			t.Errorf("%s: printed the drop %t, want %t:\n%s", tc.ext, got, tc.warned, errOut)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		info, err := label.New().Read(context.Background(), container.BytesSource(raw), tc.ext, meta.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if kept := len(info.Tags["COMMENT"]) == 1; kept == tc.warned {
			t.Errorf("%s carries COMMENT %t, want %t", tc.ext, kept, !tc.warned)
		}
	}
}

// TestSplitKeepsATagThePiecesWriterHolds: a rip tag too large for a capped
// block is kept on every piece an MP4 writer holds it on, with nothing
// printed, and left out of every WavPack piece with one line for the split.
func TestSplitKeepsATagThePiecesWriterHolds(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 100_000), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "album.m4a")
	if code, _, errOut := run(t, "transcode", wav, src, "--format", "alac"); code != 0 {
		t.Fatalf("building the source: %s", errOut)
	}
	comment := strings.Repeat("c", 60000)
	if err := label.New().Apply(context.Background(), src, &meta.Info{Tags: map[string][]string{
		"COMMENT": {comment},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		format string
		kept   bool
	}{{"alac", true}, {"wavpack", false}} {
		out := filepath.Join(dir, tc.format)
		code, _, errOut := run(t, "split", src, out, "--format", tc.format, "--at", "50000")
		if code != 0 {
			t.Fatalf("%s: split exit = %d: %s", tc.format, code, errOut)
		}
		want := 1
		if tc.kept {
			want = 0
		}
		if n := strings.Count(errOut, "tag too large for the output, not embedded"); n != want {
			t.Errorf("%s: stderr reports %d dropped tags, want %d:\n%s", tc.format, n, want, errOut)
		}
		names := outputNames(t, out)
		if len(names) != 2 {
			t.Fatalf("%s: the split wrote %q, want two pieces", tc.format, names)
		}
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			info, err := label.New().Read(context.Background(), container.BytesSource(raw),
				strings.TrimPrefix(filepath.Ext(name), "."), meta.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if kept := len(info.Tags["COMMENT"]) == 1 && info.Tags["COMMENT"][0] == comment; kept != tc.kept {
				t.Errorf("%s carries COMMENT %t, want %t", name, kept, tc.kept)
			}
		}
	}
}
