package cache

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// IdxStore is the source-index sidecar cache (plan section 10): probe
// byproducts too expensive to rebuild per session, today the MP3 lazy
// frame index, persist under cacheDir/idx keyed by source identity.
// Everything is best effort: a lost or rejected blob only costs a
// rebuild, so errors are swallowed, and writes are atomic (temp plus
// rename) so a crash cannot leave a torn blob that parses.
//
// The store is shared across request goroutines through the one Engine,
// so writes serialize under mu: concurrent Saves for the same identity
// would otherwise interleave on the same temp file and rename a torn
// blob into place. Load stays lock-free on purpose: rename is atomic, so
// a reader sees either the old or the new complete blob, and a read
// racing trim's removal just misses.
type IdxStore struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	// total tracks the directory's byte count so the media-close path
	// pays a full scan only at first use and under cap pressure, not on
	// every save. -1 until the first Save scans.
	total int64
}

// DefaultIdxMaxBytes bounds the sidecar directory; frame indexes run a
// couple of megabytes for pathological inputs and kilobytes typically.
const DefaultIdxMaxBytes = 256 << 20

// maxIdxBlob rejects absurd blobs on both paths.
const maxIdxBlob = 32 << 20

// OpenIdx opens (creating if needed) the sidecar store under dir.
func OpenIdx(dir string, maxBytes int64) (*IdxStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultIdxMaxBytes
	}
	return &IdxStore{dir: dir, maxBytes: maxBytes, total: -1}, nil
}

// path derives the blob location from a source identity string via the
// ADR-0004 key hash (the identity contains path separators and is
// unbounded; the hash is a clean filename).
func (s *IdxStore) path(identity string) string {
	return filepath.Join(s.dir, string(NewKey(identity, "idx", nil))+".idx")
}

// Load returns the saved blob for a source identity, or nil.
func (s *IdxStore) Load(identity string) []byte {
	path := s.path(identity)
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > maxIdxBlob {
		return nil
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Bump the mtime so trim's oldest-first eviction approximates LRU.
	// A blob whose consumer keeps rejecting it stays refreshed too; that
	// is what Drop is for.
	now := time.Now()
	os.Chtimes(path, now, now)
	return blob
}

// Save persists a blob for a source identity and enforces the directory
// cap by dropping the oldest blobs. An empty blob is a no-op (Drop is
// the explicit removal path).
func (s *IdxStore) Save(identity string, blob []byte) {
	if len(blob) == 0 || len(blob) > maxIdxBlob {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(identity)
	var old int64
	if fi, err := os.Stat(path); err == nil {
		old = fi.Size()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return
	}
	if s.total >= 0 {
		s.total += int64(len(blob)) - old
	}
	if s.total < 0 || s.total > s.maxBytes {
		s.trim()
	}
}

// Drop removes a source identity's blob: the path for consumers that
// found a stored index invalid, so it stops being LRU-refreshed forever.
func (s *IdxStore) Drop(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(identity)
	if fi, err := os.Stat(path); err == nil {
		if os.Remove(path) == nil && s.total >= 0 {
			s.total -= fi.Size()
		}
	}
}

// trim rebuilds the byte total and enforces maxBytes, oldest first by
// mtime. Leftover temp files (a crashed Save; no live one exists, the
// mutex is held) and oversized blobs Load would skip forever are deleted
// outright rather than counted against live entries.
func (s *IdxStore) trim() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	type blob struct {
		path string
		size int64
		mod  int64
	}
	var blobs []blob
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") || fi.Size() > maxIdxBlob {
			os.Remove(path)
			continue
		}
		blobs = append(blobs, blob{path, fi.Size(), fi.ModTime().UnixNano()})
		total += fi.Size()
	}
	if total > s.maxBytes {
		sort.Slice(blobs, func(i, j int) bool { return blobs[i].mod < blobs[j].mod })
		for _, b := range blobs {
			if total <= s.maxBytes {
				break
			}
			if os.Remove(b.path) == nil {
				total -= b.size
			}
		}
	}
	s.total = total
}
