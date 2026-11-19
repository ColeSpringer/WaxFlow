package jobs

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/source"
)

// TestMemberMediaKeepsTheFilesWalk: a merge member's Media answers for the
// walk its file defers, where a wrapper that dropped it would say done.
func TestMemberMediaKeepsTheFilesWalk(t *testing.T) {
	const name = "sine-cbr128.mp3"
	root := t.TempDir()
	copyFixture(t, root, name)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: openRoots(t, root)})
	ctx := context.Background()
	med, err := r.openMemberMedia(ctx, "", Request{Srcs: []source.Ref{"lib/" + name}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if format.MediaWalked(med) {
		t.Fatal("a freshly opened MP3 member reads as walked")
	}
	if err := format.WalkMedia(ctx, med); err != nil || !format.MediaWalked(med) {
		t.Errorf("after Walk = %v the member reads walked %v, want nil and true", err, format.MediaWalked(med))
	}
}
