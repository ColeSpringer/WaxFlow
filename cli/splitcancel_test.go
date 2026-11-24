package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/meta"
)

// TestSplitCancelInThePostPassPublishesNothing: a cancel that lands in a
// piece's post-pass is a cancel. The piece is not published, no post-pass
// failure is printed, and the command exits 6 with "split canceled"; under
// --force the old piece is untouched.
func TestSplitCancelInThePostPassPublishesNothing(t *testing.T) {
	for _, tc := range cancelCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			wav := filepath.Join(dir, "in.wav")
			if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 100_000), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "pieces")
			keep := map[string][]byte{}
			args := []string{"split", wav, out, "--at", "50000"}
			if tc.force {
				old := []byte("the old piece")
				if err := os.MkdirAll(out, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, "01.flac"), old, 0o644); err != nil {
					t.Fatal(err)
				}
				keep["01.flac"] = old
				args = append(args, "--force")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var applied bool
			splitMapper = func(m meta.Mapper) meta.Mapper {
				return cancelingMapper{Mapper: m, cancel: cancel, applied: &applied}
			}
			defer func() { splitMapper = nil }()

			var stdout, stderr strings.Builder
			code := execute(ctx, "test", args, &stdout, &stderr, Flavor{})
			if !applied {
				t.Fatalf("the post-pass never ran; exit %d, stderr: %s", code, stderr.String())
			}
			if code != 6 || strings.Contains(stderr.String(), "post-pass failed") || !strings.Contains(stderr.String(), "split canceled") {
				t.Errorf("split exit = %d, stderr %q; want 6, \"split canceled\" and no post-pass failure", code, stderr.String())
			}
			wantNothingLeft(t, out, keep)
		})
	}
}

// TestSplitRefusesAnExistingPieceBeforeWritingAny: without --force a piece
// path that exists is refused before any piece is written, exit 2, so the
// refusal never leaves a partial set.
func TestSplitRefusesAnExistingPieceBeforeWritingAny(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, rampWAVBytes(t, 44100, 2, 100_000), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "pieces")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := []byte("an earlier piece")
	if err := os.WriteFile(filepath.Join(out, "02.flac"), theirs, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := execute(t.Context(), "test", []string{"split", wav, out, "--at", "50000"}, &stdout, &stderr, Flavor{})
	if code != 2 || !strings.Contains(stderr.String(), "02.flac: output exists (use --force to overwrite)") {
		t.Errorf("split exit = %d, stderr %q; want 2 and 02.flac: output exists", code, stderr.String())
	}
	wantNothingLeft(t, out, map[string][]byte{"02.flac": theirs})
}
