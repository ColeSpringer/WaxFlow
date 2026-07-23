// Package timeline is the content-addressed store behind multi-source HLS
// timelines: the {src, id} member lists that an HLS descriptor's tl digest
// names.
//
// A timeline is stored rather than inlined so a playlist URL stays the size
// it is today whatever the queue's length. Content-addressing does the rest
// of the work: the write is idempotent, it survives a restart, and there is
// no separate list digest to get wrong. Because the digest covers the
// members' own ADR-0003 identities, changing any member (or replacing its
// file) yields new identities, a new digest, and a new cache key, which is
// exactly what ADR-0004 asks of an identity.
//
// Lifetime is bounded by the signatures issued against a timeline rather
// than by an arbitrary TTL. A stored timeline expires no sooner than the
// longest-lived URL minted against it, so no still-valid signed URL can
// outlive the timeline it names, and "signed URLs pin bytes" stays true by
// construction instead of becoming a documented weakening. See
// docs/adr/0009-multi-source-timelines.md.
package timeline

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// SchemaVersion is the stored document's schema.
const SchemaVersion = 1

// MaxMembers bounds one timeline. A timeline is a play queue, not a library:
// the bound is what keeps a mint's cost (a resolve, a probe, and possibly a
// full measuring decode per member) something a caller can reason about.
const MaxMembers = 1000

// digestLen is a digest's length: SHA-256, base64url, unpadded.
const digestLen = 43

// docExt is the stored document's extension.
const docExt = ".json"

// maxDocBytes bounds a document read back off disk. A thousand members of a
// few hundred bytes each is the largest legitimate document by a wide
// margin; past that the file is not one we wrote.
const maxDocBytes = 4 << 20

// defaultMaxEntries bounds the number of stored timelines. It is a backstop
// against pathological growth rather than the real bound, which is the
// expiry sweep: a timeline nobody can still reach is already gone. Only the
// expiry and the LRU clock live in memory (the members are read from disk
// per request), so a full store costs tens of kilobytes.
const defaultMaxEntries = 4096

// Member is one source in a timeline, pinned at mint time.
type Member struct {
	// Src is the source reference, as the caller gave it.
	Src string `json:"src"`
	// ID is the member's ADR-0003 source identity (size-mtimeNS). It is
	// inside the digest, which is what makes the digest an identity rather
	// than a name: a member that changes on disk cannot keep its timeline.
	ID string `json:"id"`
	// From and To bound the member to the sample window [From, To) of its
	// own source: To is exclusive, 0 runs to the end, and the zero pair is
	// the whole file. Stored verbatim as declared rather than resolved to
	// the measured end, so the mint stays pure of measurement and the same
	// body names the same digest whichever path minted it.
	//
	// The window is inside the digest, and that is the opposite call from
	// the crossfade by the same doctrine: a crossfade renders a timeline, a
	// window says which samples are this member, so it is content identity
	// (two windowings of one file are two timelines). omitempty is what
	// keeps every whole-file member's canonical JSON, and therefore every
	// pre-window digest, stored document, and tl: cache identity,
	// byte-identical: Digest marshals struct field order, so these fields
	// are appended after ID and never reordered.
	From int64 `json:"from,omitempty"`
	To   int64 `json:"to,omitempty"`
}

// doc is the stored document.
type doc struct {
	SchemaVersion int `json:"schemaVersion"`
	// Expires is the latest expiry of any URL minted against this timeline.
	// It sits outside the digest deliberately: extending a lifetime must not
	// mint a new identity, or every URL already issued would be orphaned by
	// the act of keeping them alive.
	Expires time.Time `json:"expires"`
	Members []Member  `json:"members"`
}

// Digest is a timeline's identity: the SHA-256 of the canonical JSON of its
// members, base64url encoded.
func Digest(members []Member) string {
	b, err := json.Marshal(struct {
		SchemaVersion int      `json:"schemaVersion"`
		Members       []Member `json:"members"`
	}{SchemaVersion, members})
	if err != nil {
		// Plain data with no cycles; Marshal cannot fail on it.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidDigest reports whether s is a well-formed digest.
//
// A digest is a path component, so this is a containment check and not only
// a courtesy: nothing else stands between a crafted tl= parameter and a file
// outside the store.
func ValidDigest(s string) bool {
	if len(s) != digestLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Options configures a Store.
type Options struct {
	// MaxEntries bounds the number of stored timelines; 0 means the default.
	MaxEntries int
	// Logger receives sweep and repair notes; nil discards.
	Logger *slog.Logger
}

// Store is the timeline store. It is safe for concurrent use.
type Store struct {
	dir string
	log *slog.Logger
	max int

	mu      sync.Mutex
	entries map[string]*entry
	clock   int64
}

// entry is what the store keeps in memory for one timeline: when it expires,
// and when it was last used.
//
// The members themselves stay on disk and are read per request. That is the
// trade the hot path wants: a segment request already resolves and identity-
// checks every member, so one more file read is noise beside it, while
// caching the documents would put a thousand-member queue's hundred kilobytes
// in memory for every timeline the process has ever seen.
type entry struct {
	expires time.Time
	used    int64 // clock value at last access; the LRU order
}

// Open loads the store's index, sweeping anything already expired.
//
// It enforces expiry but not the entry cap, and the asymmetry is deliberate.
// The cap evicts by least-recently-used, and that order is in-memory: it does
// not survive a restart. So evicting at boot would delete timelines in
// whatever order the directory listing happened to take, which is no order at
// all, and boot is exactly when sessions are resuming against them. Expiry is
// the real bound and is enforced here; the cap converges on the first Put,
// which evicts in a loop and by then has real use data to evict by. Sitting
// over the cap until then costs about a hundred bytes an entry, which is what
// makes waiting the cheaper mistake.
func Open(dir string, opts Options) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "timeline: creating store dir", err)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	s := &Store{dir: dir, log: log, max: maxEntries, entries: map[string]*entry{}}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "timeline: scanning store dir", err)
	}
	now := time.Now()
	for _, name := range names {
		digest, ok := strings.CutSuffix(name.Name(), docExt)
		if name.IsDir() || !ok || !ValidDigest(digest) {
			continue
		}
		d, err := s.load(digest)
		if err != nil {
			// A timeline is re-mintable from the client's own queue, so an
			// unreadable one is dropped rather than quarantined the way a
			// job directory is: there is no product here to preserve, and
			// the 404 contract already covers a timeline that is gone.
			log.Warn("timeline: dropping an unreadable timeline", "tl", digest, "err", err)
			s.removeFile(digest)
			continue
		}
		if !d.Expires.After(now) {
			s.removeFile(digest)
			continue
		}
		s.clock++
		s.entries[digest] = &entry{expires: d.Expires, used: s.clock}
	}
	return s, nil
}

// Put stores members and returns the timeline's digest, extending the stored
// expiry when the timeline is already held and expires sooner than exp.
// Writing is idempotent by construction: the same members are the same
// digest are the same file.
//
// exp must be the expiry of the URL being minted against the timeline, which
// is what bounds its lifetime (see the package doc).
func (s *Store) Put(members []Member, exp time.Time) (string, error) {
	if err := checkMembers(members); err != nil {
		return "", err
	}
	digest := Digest(members)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, known := s.entries[digest]
	if known && !exp.After(e.expires) {
		// Held already, and already outliving this URL: nothing to write.
		s.markLocked(e)
		return digest, nil
	}
	// Write before making room, because making room is destructive: eviction
	// deletes other timelines from disk, so evicting first would let a write
	// that then failed take them with it, for a timeline that was never
	// stored. Nothing after the write can fail, so this order leaves a failed
	// Put having changed nothing at all.
	if err := s.writeLocked(digest, exp, members); err != nil {
		return "", err
	}
	if !known {
		for len(s.entries) >= s.max {
			if !s.evictOldestLocked() {
				break
			}
		}
		e = &entry{}
		s.entries[digest] = e
	}
	e.expires = exp
	s.markLocked(e)
	return digest, nil
}

// Members reads a stored timeline and marks it used.
//
// A digest the store does not hold is CodeNotFound, which is the re-mint
// contract: the client mints the timeline again from the queue it still has.
// A correct client does not reach this during normal playback, because a
// timeline outlives every URL minted against it.
func (s *Store) Members(digest string) ([]Member, error) {
	if !ValidDigest(digest) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "timeline: malformed tl digest")
	}
	s.mu.Lock()
	e, ok := s.entries[digest]
	if ok {
		s.markLocked(e)
	}
	s.mu.Unlock()
	if !ok {
		return nil, errUnknown()
	}
	d, err := s.load(digest)
	if err != nil {
		// The index says we hold it and the disk disagrees. Believe the
		// disk, drop the index entry, and answer what is true.
		s.log.Warn("timeline: indexed timeline is unreadable", "tl", digest, "err", err)
		s.mu.Lock()
		delete(s.entries, digest)
		s.mu.Unlock()
		return nil, errUnknown()
	}
	return d.Members, nil
}

// Touch marks a timeline used and, when exp outlives what is stored, extends
// it to exp. Unknown digests are ignored: touching is upkeep, and a request
// for a timeline that is gone fails on its own terms in Members.
//
// Marking on every read rather than only at the mint is what makes the LRU
// backstop mean "unused" instead of "minted long ago". A long session (a
// 40-hour audiobook, or any session paused across the window) keeps fetching
// segments against a timeline nobody re-mints, and evicting it mid-playback
// would 404 the player at a buffer refill rather than at a natural boundary.
func (s *Store) Touch(digest string, exp time.Time) {
	s.mu.Lock()
	e, ok := s.entries[digest]
	if ok {
		s.markLocked(e)
	}
	extend := ok && exp.After(e.expires)
	s.mu.Unlock()
	if !extend {
		return
	}
	// Extending rewrites the document, so it needs the members back. This is
	// off the common path: every URL of one playback session carries the
	// session's own expiry, so the first request extends and the rest do not.
	//
	// The load happens outside the lock, so a concurrent GC or eviction can
	// take the timeline between the decision to extend and the Put that
	// extends it, putting it back. That is the outcome to want, not a race to
	// close: the entry was there when this read it, and the only reason to
	// extend is a URL minted against it, so a timeline swept out from under a
	// live session belongs back. Content-addressing makes the rewrite
	// byte-identical, and Put re-evicts to stay at the cap, so the return
	// costs neither correctness nor room.
	d, err := s.load(digest)
	if err == nil {
		_, err = s.Put(d.Members, exp)
	}
	if err != nil {
		// Failing to extend never fails the request that touched it. The
		// worst case is that the timeline is swept early and the client
		// re-mints, which is a contract it already has.
		s.log.Warn("timeline: extending a timeline's expiry failed", "tl", digest, "err", err)
	}
}

// GC removes timelines past their expiry and reports how many went.
//
// Nothing still reachable can be swept: an entry's expiry is at least the
// expiry of every URL minted against it, so a URL that still verifies still
// has its timeline.
func (s *Store) GC() int {
	now := time.Now()
	s.mu.Lock()
	var dead []string
	for digest, e := range s.entries {
		if !e.expires.After(now) {
			dead = append(dead, digest)
		}
	}
	for _, digest := range dead {
		delete(s.entries, digest)
	}
	s.mu.Unlock()
	for _, digest := range dead {
		s.removeFile(digest)
	}
	return len(dead)
}

// Entries reports how many timelines the store holds (metrics and tests).
func (s *Store) Entries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func checkMembers(members []Member) error {
	switch {
	case len(members) == 0:
		return waxerr.New(waxerr.CodeInvalidRequest, "timeline: a timeline needs at least one member")
	case len(members) > MaxMembers:
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"timeline: %d members is past the %d-member bound; a timeline is a play queue, not a library",
			len(members), MaxMembers))
	}
	for i, m := range members {
		if m.Src == "" || m.ID == "" {
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("timeline: member %d has no source reference or no identity", i))
		}
		// Window sanity, guarding Put and load alike: the store must never
		// hold a window no source could satisfy, whether it arrived through a
		// handler or through a hand edit the digest re-check would also catch.
		switch {
		case m.From < 0:
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("timeline: member %d has a negative window start %d", i, m.From))
		case m.To < 0:
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("timeline: member %d has a negative window end %d", i, m.To))
		case m.To > 0 && m.To <= m.From:
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("timeline: member %d window [%d, %d) ends before it starts", i, m.From, m.To))
		}
	}
	return nil
}

func errUnknown() error {
	return waxerr.New(waxerr.CodeNotFound,
		"timeline unknown; re-create it with POST /hls/timeline (the queue position is unaffected)")
}

func (s *Store) path(digest string) string { return filepath.Join(s.dir, digest+docExt) }

func (s *Store) removeFile(digest string) {
	if err := os.Remove(s.path(digest)); err != nil && !os.IsNotExist(err) {
		s.log.Warn("timeline: removing a timeline failed", "tl", digest, "err", err)
	}
}

func (s *Store) load(digest string) (doc, error) {
	f, err := os.Open(s.path(digest))
	if err != nil {
		return doc{}, err
	}
	defer f.Close()
	var d doc
	dec := json.NewDecoder(io.LimitReader(f, maxDocBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return doc{}, err
	}
	if d.SchemaVersion != SchemaVersion {
		return doc{}, fmt.Errorf("timeline: %s is schema %d, want %d", digest, d.SchemaVersion, SchemaVersion)
	}
	if err := checkMembers(d.Members); err != nil {
		return doc{}, err
	}
	// Content-addressing is a check as well as a naming scheme, and it is
	// free here: a document whose members do not hash to its own name has
	// been corrupted or hand-edited, and serving it would hand out a
	// timeline under an identity that is not its own.
	if got := Digest(d.Members); got != digest {
		return doc{}, fmt.Errorf("timeline: %s holds members that digest to %s", digest, got)
	}
	return d, nil
}

func (s *Store) writeLocked(digest string, exp time.Time, members []Member) error {
	b, err := json.Marshal(doc{SchemaVersion: SchemaVersion, Expires: exp, Members: members})
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "timeline: encoding a timeline", err)
	}
	tmp := s.path(digest) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "timeline: writing a timeline", err)
	}
	if err := os.Rename(tmp, s.path(digest)); err != nil {
		os.Remove(tmp)
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "timeline: promoting a timeline", err)
	}
	return nil
}

func (s *Store) markLocked(e *entry) {
	s.clock++
	e.used = s.clock
}

// evictOldestLocked drops the least recently used timeline, reporting
// whether it found one. Eviction is least-recently-used rather than
// soonest-to-expire: expiry is already the real bound (GC sweeps it), so
// this only fires under pathological growth, and there the entry worth
// keeping is the one a session is still reading.
func (s *Store) evictOldestLocked() bool {
	var oldest string
	var found bool
	minUsed := int64(math.MaxInt64)
	for digest, e := range s.entries {
		if !found || e.used < minUsed {
			minUsed, oldest, found = e.used, digest, true
		}
	}
	if !found {
		return false
	}
	delete(s.entries, oldest)
	s.removeFile(oldest)
	return true
}
