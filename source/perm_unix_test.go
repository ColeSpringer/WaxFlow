//go:build unix

package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestRootThatLosesSearchPermission pins a root folder an operator chmods
// shut: a request answers root-unavailable, not a 500 per file, the root is
// demoted, and the sweep does not reopen it on every pass.
func TestRootThatLosesSearchPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := context.Background()
	dir := rootDir(t, map[string]string{"x.wav": "RIFFdata"})
	r, err := OpenRoots([]Root{{Name: "B", Path: dir}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	_, err = r.Resolve(ctx, "B/x.wav")
	if waxerr.CodeOf(err) != waxerr.CodeRootUnavailable || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("resolve under a shut root = %v, want root-unavailable for its permission", err)
	}
	if st := statusOf(t, r, "B"); st.Mounted {
		t.Fatalf("B = %+v, want demoted", st)
	}
	for i := range 3 {
		if got := r.Remount(); len(got) != 0 {
			t.Fatalf("sweep %d over the shut root = %+v, want no change", i, got)
		}
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := r.Resolve(ctx, "B/x.wav")
	if err != nil {
		t.Fatalf("resolve once the folder opens again = %v", err)
	}
	f.Close()
}
