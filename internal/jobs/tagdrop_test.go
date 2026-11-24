package jobs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/source"
)

// writeBigCommentM4A writes the sine fixture as a flat m4a whose COMMENT is
// larger than any embedding muxer holds; MP4 itself has room for it.
func writeBigCommentM4A(t *testing.T, root string) string {
	t.Helper()
	copyFixture(t, root, fixtureWAV)
	in, err := os.Open(filepath.Join(root, fixtureWAV))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	src, err := container.FileSource(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(root, "big.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := waxflow.New().Transcode(context.Background(), src, "wav", out,
		waxflow.TranscodeOptions{Format: "alac", Container: waxflow.ContainerProgressive},
		waxflow.WithTags([]container.Tag{{Key: "COMMENT", Value: strings.Repeat("c", 60000)}})); err != nil {
		t.Fatal(err)
	}
	return "lib/big.m4a"
}

// TestTranscodeJobNotesADroppedTagOnlyWhereItIsLost: a source tag too large
// for the output's block is a warning only where nothing restores it. The
// post-pass rewrites a FLAC output with the full set, so it carries the tag
// and no warning; an MP4's ilst holds it; a WavPack muxer, or a daemon with
// no mapper, loses it, and says so.
func TestTranscodeJobNotesADroppedTagOnlyWhereItIsLost(t *testing.T) {
	root := t.TempDir()
	ref := writeBigCommentM4A(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	const warning = "tag COMMENT is too large for the output and was not embedded"
	for _, tc := range []struct {
		name   string
		params outshape.Params
		mapper bool
		warned bool
	}{
		{"flac, post-passed", outshape.Params{Format: "flac", FLACLevel: -1}, true, false},
		{"alac, held by the muxer", outshape.Params{Format: "alac", Container: waxflow.ContainerProgressive}, true, false},
		{"wavpack, embedded by the muxer", outshape.Params{Format: "wavpack"}, true, true},
		{"flac, no mapper", outshape.Params{Format: "flac", FLACLevel: -1}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordingMapper{}
			cfg := Config{Dir: t.TempDir(), Resolver: res}
			if tc.mapper {
				cfg.Meta = m
			}
			r := openRunner(t, cfg)
			j, err := r.Create(Request{Type: TypeTranscode, Src: source.Ref(ref), SourceID: srcID, Params: tc.params})
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			if got := slices.Contains(done.Warnings, warning); got != tc.warned {
				t.Errorf("warned %t, want %t: %q", got, tc.warned, done.Warnings)
			}
			switch {
			case tc.params.Format == "wavpack":
				if probeTags(t, r.OutputPath(done, 0))["COMMENT"] != nil {
					t.Error("the WavPack output carries a COMMENT its muxer could not hold")
				}
			case tc.params.Format == "alac":
				if got := probeTags(t, r.OutputPath(done, 0))["COMMENT"]; len(got) != 1 || len(got[0]) != 60000 {
					t.Error("the m4a output lost a COMMENT its ilst holds")
				}
			case tc.mapper:
				if got := m.applied["out.flac"]["COMMENT"]; len(got) != 1 || len(got[0]) != 60000 {
					t.Error("the post-pass did not write the source's COMMENT")
				}
			}
		})
	}
}

// TestSplitJobKeepsATagThePiecesWriterHolds: a rip tag too large for a
// capped block is kept on every piece an MP4 writer holds it on, with no
// warning, and left out of every WavPack piece with one warning.
func TestSplitJobKeepsATagThePiecesWriterHolds(t *testing.T) {
	root := t.TempDir()
	ref := writeBigCommentM4A(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	const warning = "tag COMMENT is too large for the output and was not embedded"
	for _, tc := range []struct {
		name   string
		params outshape.Params
		kept   bool
	}{
		{"alac", outshape.Params{Format: "alac", Container: waxflow.ContainerProgressive}, true},
		{"wavpack", outshape.Params{Format: "wavpack"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: &recordingMapper{}})
			j, err := r.Create(Request{Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID, Params: tc.params, Cuts: []int64{1000}})
			if err != nil {
				t.Fatal(err)
			}
			done := waitJob(t, r, j.ID, StateDone)
			var want []string
			if !tc.kept {
				want = []string{warning}
			}
			if !slices.Equal(done.Warnings, want) {
				t.Errorf("warnings = %q, want %q", done.Warnings, want)
			}
			if len(done.Outputs) != 2 {
				t.Fatalf("%d pieces written, want 2", len(done.Outputs))
			}
			for i := range done.Outputs {
				got := probeTags(t, r.OutputPath(done, i))["COMMENT"]
				if kept := len(got) == 1 && len(got[0]) == 60000; kept != tc.kept {
					t.Errorf("piece %d carries COMMENT %t, want %t", i, kept, tc.kept)
				}
			}
		})
	}
}

// TestSplitJobTakesATitleItsWriterHolds: a stored split's piece title too
// large for a capped block fails the run only where the pieces' writer has
// the cap; an MP4's ilst holds it, and the piece carries it.
func TestSplitJobTakesATitleItsWriterHolds(t *testing.T) {
	root := t.TempDir()
	ref := writeBigCommentM4A(t, root)
	res := openRoots(t, root)
	srcID := pinID(t, res, ref)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, Meta: &recordingMapper{}})
	title := strings.Repeat("t", 60000)
	split := func(p outshape.Params) Request {
		return Request{Type: TypeSplit, Src: source.Ref(ref), SourceID: srcID, Params: p, Cuts: []int64{1000},
			Pieces: TitledPieces([]string{title, ""})}
	}
	j, err := r.Create(split(outshape.Params{Format: "wavpack"}))
	if err != nil {
		t.Fatal(err)
	}
	const refusal = "pieces[0]: a 60000-byte title is too large to embed in a piece's tags"
	if failed := waitJob(t, r, j.ID, StateFailed); failed.Error == nil || failed.Error.Message != refusal {
		t.Errorf("wavpack: the job failed with %+v, want %q", failed.Error, refusal)
	}
	j, err = r.Create(split(outshape.Params{Format: "alac", Container: waxflow.ContainerProgressive}))
	if err != nil {
		t.Fatalf("alac: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	if got := probeTags(t, r.OutputPath(done, 0))["TITLE"]; len(got) != 1 || got[0] != title {
		t.Errorf("piece 0 is titled %d values, want its own long title", len(got))
	}
}
