package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// keepingResolver keeps the last file it resolved.
type keepingResolver struct {
	source.Resolver
	last *source.File
}

func (k *keepingResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	f, err := k.Resolver.Resolve(ctx, ref)
	k.last = f
	return f, err
}

// memberServer is a server over a library holding one MP3, the resolver it
// opens through, the member that names the file, and the library's path.
func memberServer(t *testing.T) (*Server, *keepingResolver, memberSource, string) {
	t.Helper()
	const name = "sine-cbr128.mp3"
	raw, err := os.ReadFile(testutil.PkgFile("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res := &keepingResolver{Resolver: roots}
	s, err := New(Config{Addr: "127.0.0.1:4418", CacheDir: t.TempDir(), Resolver: res, LiveSlots: 1})
	if err != nil {
		roots.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		roots.Close()
	})
	f, err := res.Resolve(context.Background(), "lib/"+name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return s, res, memberSource{Ref: f.Ref, ID: f.ID, Ext: f.Ext}, root
}

// TestMemberMediaKeepsTheFilesWalk: an opened member's Media answers for the
// walk its file defers, where a wrapper that dropped it would say done.
func TestMemberMediaKeepsTheFilesWalk(t *testing.T) {
	s, _, m, _ := memberServer(t)
	ctx := context.Background()
	med, err := s.openMember(ctx, m, -1)
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

// A member whose file fails to close fails the read as an unreadable source,
// and the envelope a client reads names a timeline member by its index and
// no path of the daemon's.
func TestAMemberCloseErrorFailsTheReadAndNamesTheMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index int
		want  string
	}{
		{"a timeline member", 3, "member 3: source: closing the file: "},
		{"a single source", -1, "source: closing the file: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, res, m, root := memberServer(t)
			med, err := s.openMember(context.Background(), m, tc.index)
			if err != nil {
				t.Fatal(err)
			}
			// Closed under the Media, the file refuses the Media's close.
			if err := res.last.Close(); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			s.writeError(rec, med.Close())
			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != waxerr.CodeSourceUnreadable {
				t.Fatalf("the client read %s, want a %s envelope", rec.Body, waxerr.CodeSourceUnreadable)
			}
			if msg := body.Error; !strings.HasPrefix(msg, tc.want) || strings.Contains(msg, root) {
				t.Errorf("the client read %q, want it to start %q and hold no path under %s", msg, tc.want, root)
			}
		})
	}
}
