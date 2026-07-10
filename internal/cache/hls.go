package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxflow/waxerr"
)

// KindHLS marks a Meta as an HLS variant directory: init.mp4 and
// seg-N.m4s files accumulating under one key instead of a single
// out.<ext>. Segments are individually complete, so the directory is
// valid with any subset present; that incremental accumulation IS the
// partial/seek cache. The empty Kind is a progressive entry.
const KindHLS = "hls"

// Variant is a handle on one HLS variant directory: the per-file
// operations the segment workers and handlers need, with the store's
// index and eviction accounting kept coherent underneath.
type Variant struct {
	s   *Store
	key Key
	dir string
}

// HLS opens (creating at first touch) the variant directory for key.
// Birth writes meta.json immediately, unlike progressive entries: a
// variant is never "complete", each finished segment is individually
// servable, and the boot scan must not sweep a crash-interrupted variant
// away with the abandoned partials.
func (s *Store) HLS(key Key, meta Meta) (*Variant, error) {
	dir := s.entryDir(key)
	v := &Variant{s: s, key: key, dir: dir}

	s.mu.Lock()
	known := s.index[key] != nil
	s.mu.Unlock()
	if known {
		return v, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: creating variant dir", err)
	}
	meta.Kind = KindHLS
	meta.CreatedAt = s.now()
	b, err := meta.marshal()
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf("meta.json.tmp-%d", s.tmpSeq.Add(1)))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: writing variant meta", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		os.Remove(tmp)
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: publishing variant meta", err)
	}

	// Registering with size 0 and growing per file keeps the eviction
	// budget honest without rewriting meta.json per segment; the boot
	// scan recomputes a variant's size from its files instead.
	s.mu.Lock()
	if s.index[key] == nil {
		s.index[key] = &indexEntry{lastAccess: s.now(), meta: meta}
	}
	s.mu.Unlock()
	return v, nil
}

// validName rejects anything but the flat file names the HLS layout uses;
// the handlers build names from parsed integers, so a violation is a
// wiring bug, not hostile input.
func validName(name string) error {
	if name == "" || name == "meta.json" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("cache: invalid variant file name %q", name))
	}
	return nil
}

// Has reports whether the named file is already cached.
func (v *Variant) Has(name string) bool {
	if validName(name) != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(v.dir, name))
	return err == nil && fi.Mode().IsRegular()
}

// WriteFile atomically publishes one file into the variant (tmp plus
// rename, like every cache write) and folds its size into the eviction
// accounting. Rewriting an existing file (a worker re-covering ground
// after a partial eviction raced it) replaces it and adjusts by the
// difference.
func (v *Variant) WriteFile(name string, data []byte) error {
	if err := validName(name); err != nil {
		return err
	}
	final := filepath.Join(v.dir, name)
	var prev int64
	if fi, err := os.Stat(final); err == nil {
		prev = fi.Size()
	}
	tmp := filepath.Join(v.dir, fmt.Sprintf("%s.tmp-%d", name, v.s.tmpSeq.Add(1)))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: writing variant file", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: publishing variant file", err)
	}

	s := v.s
	s.mu.Lock()
	if ie := s.index[v.key]; ie != nil {
		ie.size += int64(len(data)) - prev
		ie.meta.Bytes = ie.size
		s.totalBytes += int64(len(data)) - prev
	}
	over := s.maxBytes > 0 && s.totalBytes > s.maxBytes
	s.mu.Unlock()
	if over {
		s.gc()
	}
	return nil
}

// Open returns the named cached file for serving, touching the variant's
// last access, or nil on a miss. The caller closes the file. Hit and miss
// accounting mirrors Lookup: every segment-serving path resolves through
// here exactly once.
func (v *Variant) Open(name string) (*Cached, bool) {
	if validName(name) != nil {
		return nil, false
	}
	s := v.s
	s.mu.Lock()
	ie := s.index[v.key]
	var meta Meta
	stale := false
	now := s.now()
	if ie != nil {
		stale = now.Sub(ie.lastAccess) > touchInterval
		ie.lastAccess = now
		meta = ie.meta
	}
	s.mu.Unlock()
	if ie == nil {
		s.misses.Add(1)
		return nil, false
	}

	f, err := os.Open(filepath.Join(v.dir, name))
	if err != nil {
		// Not-yet-written segments are the normal miss; only the variant
		// itself evaporating warrants dropping the index entry, and that
		// resolves through the eviction path, not here.
		s.misses.Add(1)
		return nil, false
	}
	if stale {
		os.Chtimes(filepath.Join(v.dir, "meta.json"), now, now)
	}
	s.hits.Add(1)
	fi, err := f.Stat()
	mod := meta.CreatedAt
	if err == nil {
		mod = fi.ModTime()
	}
	return &Cached{File: f, Meta: meta, ModTime: mod}, true
}

// Pin excludes key from eviction while a worker is filling it; Unpin
// releases. Pins nest (a restarted worker overlaps its predecessor's
// exit).
func (s *Store) Pin(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned == nil {
		s.pinned = make(map[Key]int)
	}
	s.pinned[key]++
}

// Unpin releases one Pin.
func (s *Store) Unpin(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.pinned[key]; n > 1 {
		s.pinned[key] = n - 1
	} else {
		delete(s.pinned, key)
	}
}

// variantSize sums a variant directory's files for the boot scan (its
// meta.json Bytes is only maintained in memory).
func variantSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, ent := range entries {
		if info, err := ent.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// pruneVariantTemps removes write-interrupted temp files a crash left in
// a variant directory; the published files around them stay valid.
func pruneVariantTemps(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	for _, m := range matches {
		os.Remove(m)
	}
}
