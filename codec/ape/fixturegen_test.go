//go:build apefixtures

package ape_test

// Offline fixture generator. Run with:
//
//	go generate ./codec/ape
//
// which is wired to `go test -tags apefixtures -run ^TestGenerateFixtures$`.
// It needs the reference `mac` console tool on PATH, which is the only APE
// encoder there is.
//
// The committed fixtures exist so the decoder has a bit-exact gate on a
// machine with no tools at all: every one of them holds a signal
// internal/testutil can rebuild from its seed, and the tests compare the
// decode against that rather than against a stored decode. Regenerating them
// is therefore not a re-baselining: if a fixture changes, the same assertions
// hold or the file is wrong.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

func TestGenerateFixtures(t *testing.T) {
	mac := testutil.APETool(t)
	tmp := t.TempDir()
	for _, f := range apeFixtures {
		wav := filepath.Join(tmp, filepath.Base(f.path)+".wav")
		testutil.WriteWAV(t, wav, f.fmt, f.samples())
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(tmp, filepath.Base(f.path))
		args := []string{wav, out, fmt.Sprintf("-c%d", f.level)}
		if f.tags != "" {
			args = append(args, "-t", f.tags)
		}
		if b, err := exec.Command(mac, args...).CombinedOutput(); err != nil {
			t.Fatalf("mac %v: %v\n%s", args, err, b)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", f.path, len(raw))
	}
}
