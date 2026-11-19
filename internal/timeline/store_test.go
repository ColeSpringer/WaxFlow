package timeline

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/posixfs"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func members(refs ...string) []Member {
	out := make([]Member, len(refs))
	for i, r := range refs {
		out[i] = Member{Src: source.Ref(r), ID: "100-200"}
	}
	return out
}

func later() time.Time { return time.Now().Add(time.Hour) }

// TestDigestIsIdentity pins what makes the digest usable as a cache key: it
// is a function of the members and of nothing else, and every part of a
// member is inside it. If a member could change without the digest changing,
// a stale timeline would keep serving under a key that says it is current,
// which is exactly what ADR-0004 forbids.
func TestDigestIsIdentity(t *testing.T) {
	base := []Member{{Src: "a.flac", ID: "1-2"}, {Src: "b.flac", ID: "3-4"}}
	d := Digest(base)

	if d != Digest([]Member{{Src: "a.flac", ID: "1-2"}, {Src: "b.flac", ID: "3-4"}}) {
		t.Fatal("the same members digest differently on a second call")
	}
	if len(d) != digestLen || !ValidDigest(d) {
		t.Fatalf("Digest returned %q, which is not a well-formed digest", d)
	}

	for _, tc := range []struct {
		name string
		in   []Member
	}{
		{"a member's identity changed (its file was replaced)",
			[]Member{{Src: "a.flac", ID: "9-9"}, {Src: "b.flac", ID: "3-4"}}},
		{"a member's reference changed",
			[]Member{{Src: "c.flac", ID: "1-2"}, {Src: "b.flac", ID: "3-4"}}},
		{"the order changed, which is a different timeline",
			[]Member{{Src: "b.flac", ID: "3-4"}, {Src: "a.flac", ID: "1-2"}}},
		{"a member was dropped", []Member{{Src: "a.flac", ID: "1-2"}}},
		{"a window was added, which names different samples of the same file",
			[]Member{{Src: "a.flac", ID: "1-2", From: 100, To: 200}, {Src: "b.flac", ID: "3-4"}}},
		{"a window's end moved, so the member is a different track",
			[]Member{{Src: "a.flac", ID: "1-2", To: 300}, {Src: "b.flac", ID: "3-4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if Digest(tc.in) == d {
				t.Fatal("the digest did not change, so the old cache key would survive the change")
			}
		})
	}
}

// TestDigestUnchangedForWholeFileMembers pins the literal pre-window digest,
// computed on main before Member gained From/To. Whole-file members marshal
// without the window fields (omitempty), so every digest, stored document,
// and tl: cache identity minted before the change must survive it
// byte-identically; a drift here orphans all of them at once.
func TestDigestUnchangedForWholeFileMembers(t *testing.T) {
	got := Digest([]Member{{Src: "a.flac", ID: "1-2"}, {Src: "b.flac", ID: "3-4"}})
	if want := "J8nqSMd1b9sKJGl38axuitrabpSbf1S5oNFxC7Jjq8M"; got != want {
		t.Fatalf("a whole-file timeline digests to %q, want the pre-window %q; "+
			"every stored timeline and tl: cache entry is now orphaned", got, want)
	}
}

// TestValidDigestRejectsPathEscapes is a containment test, not a formatting
// one: a digest becomes a path component, so nothing else stands between a
// crafted tl= parameter and a file outside the store.
func TestValidDigestRejectsPathEscapes(t *testing.T) {
	good := Digest(members("a.flac"))
	for _, bad := range []string{
		"", "..", "../../etc/passwd",
		strings.Repeat("../", 14) + "etc/pass", // the right length, the wrong bytes
		good[:len(good)-1],                     // too short
		good + "a",                             // too long
		good[:len(good)-1] + "/",
		good[:len(good)-1] + ".",
		good[:len(good)-1] + "+", // base64 standard, not url
	} {
		if ValidDigest(bad) {
			t.Errorf("ValidDigest(%q) is true; that string can name a file outside the store", bad)
		}
	}
	if !ValidDigest(good) {
		t.Errorf("ValidDigest rejected a real digest %q", good)
	}
}

// TestPutIsIdempotent pins the property content-addressing buys: minting the
// same queue twice is the same timeline, not two, so a client that re-mints
// (after a restart, or because it did not keep the digest) costs nothing.
func TestPutIsIdempotent(t *testing.T) {
	s := testStore(t)
	first, err := s.Put(members("a.flac", "b.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(members("a.flac", "b.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("re-minting the same queue gave %q then %q", first, second)
	}
	if n := s.Entries(); n != 1 {
		t.Fatalf("the store holds %d timelines after minting one queue twice, want 1", n)
	}
}

func TestMembersRoundTrip(t *testing.T) {
	s := testStore(t)
	// One windowed member among whole-file ones: the window must survive the
	// round trip verbatim (To 0 staying 0, not resolving to anything), and
	// the load path's content-address re-check must pass over a document
	// whose canonical JSON now carries window fields.
	want := []Member{{Src: "a.flac", ID: "1-2"}, {Src: "b.flac", ID: "3-4", From: 4410, To: 88200}, {Src: "c.flac", ID: "5-6", From: 100}}
	digest, err := s.Put(want, later())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Members(digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d members, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("member %d read back as %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestPutRefusesBadWindows pins the store's own window sanity, which guards
// Put and load alike: a window no source could satisfy must never be stored,
// whichever door it arrived through.
func TestPutRefusesBadWindows(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct {
		name string
		m    Member
	}{
		{"a negative start", Member{Src: "a.flac", ID: "1-2", From: -1}},
		{"a negative end", Member{Src: "a.flac", ID: "1-2", To: -1}},
		{"an end before its start", Member{Src: "a.flac", ID: "1-2", From: 200, To: 100}},
		{"an end equal to its start", Member{Src: "a.flac", ID: "1-2", From: 100, To: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Put([]Member{tc.m}, later()); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Fatalf("Put stored a member with %s: %v", tc.name, err)
			}
		})
	}
}

// TestMembersUnknownIsNotFound pins the re-mint contract: an unknown digest
// is a 404 a client answers by minting again, not an error it must surface.
func TestMembersUnknownIsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.Members(Digest(members("never-stored.flac")))
	if waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("an unknown digest gave %v, want a not-found", err)
	}
}

// TestExpiryOnlyExtends is the store's whole GC rule: a timeline outlives
// every URL minted against it. So a later expiry raises it and an earlier one
// leaves it alone, which is what stops a short-lived URL cutting a
// long-lived one's timeline out from under it.
func TestExpiryOnlyExtends(t *testing.T) {
	s := testStore(t)
	long := time.Now().Add(24 * time.Hour)
	digest, err := s.Put(members("a.flac"), long)
	if err != nil {
		t.Fatal(err)
	}
	// A shorter URL against the same timeline must not shorten it.
	if _, err := s.Put(members("a.flac"), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s.Touch(digest, time.Now().Add(time.Minute))
	if got := s.expiryOf(t, digest); !got.Equal(long) {
		t.Fatalf("expiry is %v after a shorter URL was minted, want the longer %v", got, long)
	}
	// A longer one must extend it, or the URL would outlive its timeline.
	longer := time.Now().Add(72 * time.Hour)
	s.Touch(digest, longer)
	if got := s.expiryOf(t, digest); !got.Equal(longer) {
		t.Fatalf("expiry is %v after a longer URL was minted, want %v", got, longer)
	}
	// The extension is durable, not only in memory: a restart must not
	// resurrect the shorter bound.
	reopened, err := Open(s.dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.expiryOf(t, digest); !got.Equal(longer) {
		t.Fatalf("after a restart the expiry is %v, want the extended %v", got, longer)
	}
}

// expiryOf reads a digest's stored expiry, from disk so the test cannot pass
// on an in-memory value that was never persisted.
func (s *Store) expiryOf(t *testing.T, digest string) time.Time {
	t.Helper()
	d, err := s.load(digest)
	if err != nil {
		t.Fatal(err)
	}
	return d.Expires.Local()
}

// TestGCSweepsOnlyExpired pins that the sweep cannot take a timeline a URL
// can still reach, which is what makes "signed URLs pin bytes" true by
// construction rather than a documented hope.
func TestGCSweepsOnlyExpired(t *testing.T) {
	s := testStore(t)
	dead, err := s.Put(members("old.flac"), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.Put(members("new.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	if n := s.GC(); n != 1 {
		t.Fatalf("GC removed %d timelines, want 1", n)
	}
	if _, err := s.Members(dead); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("an expired timeline survived the sweep: %v", err)
	}
	if _, err := s.Members(live); err != nil {
		t.Fatalf("the sweep took a timeline that is still reachable: %v", err)
	}
	if _, err := os.Stat(s.path(dead)); !os.IsNotExist(err) {
		t.Fatal("the expired timeline's file is still on disk")
	}
}

// TestOpenSweepsExpired pins that expiry is enforced across a restart too: a
// daemon that was down past a timeline's expiry must not serve it on boot.
func TestOpenSweepsExpired(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.Put(members("old.flac"), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := reopened.Entries(); n != 0 {
		t.Fatalf("a reopened store holds %d expired timelines, want 0", n)
	}
	if _, err := os.Stat(s.path(digest)); !os.IsNotExist(err) {
		t.Fatal("the expired timeline's file survived the boot sweep")
	}
}

// TestOpenSweepsTemps: a temp a crashed write left behind goes at open, in
// both spellings a timeline has been staged under, and the timelines beside
// them stay.
func TestOpenSweepsTemps(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.Put(members("a.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	other := Digest(members("b.flac"))
	if name := filepath.Base(s.path(digest)); posixfs.IsTemp(name) {
		t.Fatalf("a timeline is published as %s, which reads as a temp", name)
	}
	temps := []string{
		digest + docExt + ".tmp",
		other + docExt + ".tmp",
		digest + docExt + ".tmp-0123456789abcdef",
		other + docExt + ".tmp-fedcba9876543210",
	}
	for _, name := range temps {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("torn"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range temps {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived open: %v", name, err)
		}
	}
	got, err := reopened.Members(digest)
	if err != nil || !slices.Equal(got, members("a.flac")) {
		t.Errorf("the stored timeline after open = %v (%v), want it intact", got, err)
	}
}

// TestEvictionIsLeastRecentlyUsed pins the backstop's order. Eviction only
// fires under pathological growth, and there the entry worth keeping is the
// one a session is still using, not the one minted most recently: a read, a
// Touch and a Put of a held timeline each count as a use.
func TestEvictionIsLeastRecentlyUsed(t *testing.T) {
	for name, use := range map[string]func(s *Store, a string, exp time.Time) error{
		"a read": func(s *Store, a string, _ time.Time) error {
			_, err := s.Members(a)
			return err
		},
		"a Touch": func(s *Store, a string, exp time.Time) error {
			s.Touch(a, exp)
			return nil
		},
		// The same expiry, so the store holds it already and writes nothing.
		"a Put of a held timeline": func(s *Store, a string, exp time.Time) error {
			again, err := s.Put(members("a.flac"), exp)
			if err == nil && again != a {
				err = fmt.Errorf("the second Put minted %s, want %s", again, a)
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir(), Options{MaxEntries: 2})
			if err != nil {
				t.Fatal(err)
			}
			exp := later()
			a, err := s.Put(members("a.flac"), exp)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.Put(members("b.flac"), later())
			if err != nil {
				t.Fatal(err)
			}
			// a is the oldest by mint order; using it makes b the oldest by use.
			if err := use(s, a, exp); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Put(members("c.flac"), later()); err != nil {
				t.Fatal(err)
			}
			// b first: reading a would itself be a use.
			if _, err := s.Members(b); waxerr.CodeOf(err) != waxerr.CodeNotFound {
				t.Errorf("the least recently used timeline survived eviction: %v", err)
			}
			if _, err := s.Members(a); err != nil {
				t.Errorf("the store evicted the timeline a session was using: %v", err)
			}
			if n := s.Entries(); n != 2 {
				t.Errorf("the store holds %d timelines, want its 2-entry cap", n)
			}
		})
	}
}

// TestEvictionRemovesTheFile: an evicted timeline leaves the disk with the
// index, or the store dir would grow past the cap it enforces.
func TestEvictionRemovesTheFile(t *testing.T) {
	s, err := Open(t.TempDir(), Options{MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Put(members("a.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Put(members("b.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path(a)); !os.IsNotExist(err) {
		t.Errorf("the evicted timeline's file is still there (stat: %v)", err)
	}
	if _, err := os.Stat(s.path(b)); err != nil {
		t.Errorf("the stored timeline's file: %v", err)
	}
}

// TestOpenKeepsEntriesOverTheCapUntilAPut: a restart has no use order to
// evict by, so Open holds everything unexpired and the first new Put brings
// the store back to its cap, files included. After a restart the entries
// stand in directory order until they are used.
func TestOpenKeepsEntriesOverTheCapUntilAPut(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{MaxEntries: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"a.flac", "b.flac", "c.flac", "d.flac"} {
		if _, err := s.Put(members(ref), later()); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(dir, Options{MaxEntries: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Entries(); n != 4 {
		t.Fatalf("the reopened store holds %d timelines, want all 4", n)
	}
	e, err := s.Put(members("e.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Entries(); n != 2 {
		t.Fatalf("the store holds %d timelines after a Put, want its 2-entry cap", n)
	}
	if _, err := s.Members(e); err != nil {
		t.Errorf("the timeline just stored was evicted: %v", err)
	}
	docs, err := filepath.Glob(filepath.Join(dir, "*"+docExt))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Errorf("%d timeline files on disk, want 2", len(docs))
	}
}

// TestLoadRejectsTamperedMembers pins the check content-addressing gives for
// free: a document whose members do not hash to its own name has been
// corrupted or hand-edited, and serving it would hand out a timeline under an
// identity that is not its own.
func TestLoadRejectsTamperedMembers(t *testing.T) {
	s := testStore(t)
	digest, err := s.Put(members("a.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.path(digest))
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), "a.flac", "z.flac", 1)
	if tampered == string(raw) {
		t.Fatal("the fixture did not actually change")
	}
	if err := os.WriteFile(s.path(digest), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Members(digest); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("a tampered timeline was served under its old digest: %v", err)
	}
}

// TestLoadRejectsTamperedWindow is the same check for the window fields: they
// are inside the digest, so a stored From edited by hand cannot keep its
// timeline. Without this a tampered window would quietly serve different
// samples under the identity minted for the original ones.
func TestLoadRejectsTamperedWindow(t *testing.T) {
	s := testStore(t)
	digest, err := s.Put([]Member{{Src: "a.flac", ID: "1-2", From: 100, To: 200}}, later())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.path(digest))
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"from":100`, `"from":101`, 1)
	if tampered == string(raw) {
		t.Fatal("the fixture did not actually change")
	}
	if err := os.WriteFile(s.path(digest), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Members(digest); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("a timeline with a tampered window was served under its old digest: %v", err)
	}
}

// TestFailedPutDestroysNothing pins that a Put which cannot write leaves the
// store exactly as it found it.
//
// Making room is destructive: at the cap, storing a new timeline evicts
// another one from disk. Doing that before the write would mean a write that
// then failed had destroyed a live timeline on behalf of one that was never
// stored, which is the worst trade available: the failure is the operator's
// disk, and the cost lands on a session that had nothing to do with it.
func TestFailedPutDestroysNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.Put(members("keep.flac"), later())
	if err != nil {
		t.Fatal(err)
	}
	// At the cap, so the next Put must evict keep to make room. Make the write
	// fail: a read-only store directory is what a full or unwritable disk
	// looks like from here.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the store dir read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := s.Put(members("new.flac"), later()); err == nil {
		t.Skip("the store dir is still writable (running as root?), so the write cannot be made to fail")
	}

	if _, err := s.Members(keep); err != nil {
		t.Fatalf("a Put that failed to write evicted an unrelated timeline anyway: %v", err)
	}
	if n := s.Entries(); n != 1 {
		t.Fatalf("the store holds %d timelines after a failed Put, want the 1 it held before", n)
	}
}

// TestPutBoundsMembers pins the member bound: a timeline is a play queue, not
// a library, and the bound is what keeps a mint's worst case (a decode per
// member) something a caller can reason about.
func TestPutBoundsMembers(t *testing.T) {
	s := testStore(t)
	if _, err := s.Put(nil, later()); err == nil {
		t.Error("an empty timeline was stored")
	}
	refs := make([]string, MaxMembers+1)
	for i := range refs {
		refs[i] = filepath.Join("lib", string(rune('a'+i%26))+".flac")
	}
	if _, err := s.Put(members(refs...), later()); err == nil {
		t.Errorf("a timeline of %d members was stored past the %d bound", len(refs), MaxMembers)
	}
}

// TestMembersCarryAnyReference: the store and the digest are JSON, which
// would turn a name that is not UTF-8 into another file's, and two such
// names into one digest.
func TestMembersCarryAnyReference(t *testing.T) {
	s := testStore(t)
	want := []Member{{Src: "lib/caf\xe9.flac", ID: "1-2"}, {Src: "lib/100% \xff.flac", ID: "3-4"}}
	digest, err := s.Put(want, later())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Members(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("members came back %#v, want %#v", got, want)
	}
	if other := Digest([]Member{{Src: "lib/caf\xe8.flac", ID: "1-2"}, want[1]}); other == digest {
		t.Fatal("two queues that differ in one stray byte share a digest")
	}
}
