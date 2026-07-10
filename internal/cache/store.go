package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// Options configures a Store.
type Options struct {
	// MaxBytes bounds the cache; 0 means the config default is applied by
	// the caller, so here 0 means unbounded.
	MaxBytes int64
	// MaxAge evicts entries idle longer than this; 0 disables.
	MaxAge time.Duration
	// RingBytes bounds degradation rings; 0 means DefaultRingBytes.
	RingBytes int
	// Logger receives eviction and degradation notes; nil discards.
	Logger *slog.Logger
}

type indexEntry struct {
	size       int64
	lastAccess time.Time
	meta       Meta
}

// Store is the on-disk transcode cache: an eviction index over completed
// entries plus the registry of in-flight ones.
type Store struct {
	dir       string
	maxBytes  int64
	maxAge    time.Duration
	ringBytes int
	log       *slog.Logger

	// now and createFile are seams for eviction-clock and disk-failure
	// tests.
	now        func() time.Time
	createFile func(path string) (entryFile, error)

	mu         sync.Mutex
	index      map[Key]*indexEntry
	writing    map[Key]*Entry
	pinned     map[Key]int // eviction-excluded while a worker fills the entry
	totalBytes int64
	tmpSeq     atomic.Uint64

	hits, misses atomic.Uint64

	janitorStop chan struct{}
	janitorWG   sync.WaitGroup
}

// Open opens (creating if needed) the cache at dir and rebuilds the
// eviction index from the meta.json scan. Directories without a meta.json
// are abandoned partials from a crash and are deleted.
func Open(dir string, opts Options) (*Store, error) {
	s := &Store{
		dir:       dir,
		maxBytes:  opts.MaxBytes,
		maxAge:    opts.MaxAge,
		ringBytes: opts.RingBytes,
		log:       opts.Logger,
		now:       time.Now,
		createFile: func(path string) (entryFile, error) {
			// O_RDWR, not O_WRONLY: read-behind readers ReadAt this same
			// descriptor while the pipeline appends.
			return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		},
		index:       make(map[Key]*indexEntry),
		writing:     make(map[Key]*Entry),
		janitorStop: make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.ringBytes <= 0 {
		s.ringBytes = DefaultRingBytes
	}
	if err := os.MkdirAll(s.versionDir(), 0o755); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: creating cache dir", err)
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	s.janitorWG.Add(1)
	go s.janitor()
	return s, nil
}

func (s *Store) versionDir() string {
	return filepath.Join(s.dir, fmt.Sprintf("v%d", SchemaVersion))
}

func (s *Store) entryDir(key Key) string {
	return filepath.Join(s.versionDir(), string(key[:2]), string(key))
}

// scan rebuilds the index at boot.
func (s *Store) scan() error {
	shards, err := os.ReadDir(s.versionDir())
	if err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "cache: scanning", err)
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(s.versionDir(), shard.Name())
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			dir := filepath.Join(shardPath, ent.Name())
			meta, mtime, err := readMeta(dir)
			if err != nil {
				// No (or unreadable) meta.json: an abandoned partial.
				s.log.Info("cache: removing abandoned partial", "dir", dir)
				os.RemoveAll(dir)
				continue
			}
			if meta.Kind == KindHLS {
				// A variant's size lives in its files, not its meta (born
				// before its segments); interrupted temp writes are debris.
				pruneVariantTemps(dir)
				meta.Bytes = variantSize(dir)
			}
			s.index[Key(ent.Name())] = &indexEntry{size: meta.Bytes, lastAccess: mtime, meta: meta}
			s.totalBytes += meta.Bytes
		}
	}
	return nil
}

func readMeta(dir string) (Meta, time.Time, error) {
	path := filepath.Join(dir, "meta.json")
	fi, err := os.Stat(path)
	if err != nil {
		return Meta{}, time.Time{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, time.Time{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil || m.SchemaVersion != SchemaVersion {
		return Meta{}, time.Time{}, errors.New("cache: malformed meta.json")
	}
	return m, fi.ModTime(), nil
}

// Cached is a completed entry opened for serving.
type Cached struct {
	File    *os.File
	Meta    Meta
	ModTime time.Time
}

// touchInterval throttles persisted last-access updates: restart LRU
// only needs coarse recency, not an inode write per request.
const touchInterval = 5 * time.Minute

// Lookup opens the completed entry for key, touching its last access, or
// returns nil on a miss. The caller closes the file. Lookup owns hit and
// miss accounting: every serving path resolves through it exactly once.
func (s *Store) Lookup(key Key) *Cached {
	s.mu.Lock()
	ie := s.index[key]
	if ie == nil || ie.meta.Kind == KindHLS {
		// HLS variants have no out.<ext>; a progressive Lookup on one is a
		// key-derivation bug upstream and must miss, not drop the variant.
		s.mu.Unlock()
		s.misses.Add(1)
		return nil
	}
	dir := s.entryDir(key)
	path := filepath.Join(dir, "out."+ie.meta.Ext)
	now := s.now()
	stale := now.Sub(ie.lastAccess) > touchInterval
	ie.lastAccess = now
	meta := ie.meta
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		// The entry evaporated behind our back (manual deletion, another
		// process): drop it and report a miss.
		s.log.Warn("cache: indexed entry unreadable, dropping", "key", key, "err", err)
		s.dropIndexed(key)
		s.misses.Add(1)
		return nil
	}
	// Last access rides on meta.json's mtime so LRU survives restarts;
	// coarse recency suffices, so hot entries skip the inode write.
	if stale {
		os.Chtimes(filepath.Join(dir, "meta.json"), now, now)
	}
	s.hits.Add(1)
	return &Cached{File: f, Meta: meta, ModTime: meta.CreatedAt}
}

// Contains reports whether a completed entry exists for key, without
// opening it or counting a hit or miss. The stream handler's flight
// function uses it for its completion-race check; counting there would
// double-book the caller's own Lookup.
func (s *Store) Contains(key Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index[key] != nil
}

// InFlight returns the in-flight entry for key, or nil.
func (s *Store) InFlight(key Key) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writing[key]
}

// Begin creates a file-backed in-flight entry for key. The caller falls
// back to NewMemEntry when the cache volume cannot even open a file.
func (s *Store) Begin(key Key, meta Meta) (*Entry, error) {
	dir := s.entryDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: creating entry dir", err)
	}
	// The tmp name is unique per attempt: a degraded predecessor's open
	// file must never be truncated by a retry on the same key.
	tmp := filepath.Join(dir, fmt.Sprintf("out.%s.tmp-%d", meta.Ext, s.tmpSeq.Add(1)))
	f, err := s.createFile(tmp)
	if err != nil {
		os.Remove(dir) // best effort; fails harmlessly when non-empty
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "cache: creating entry file", err)
	}

	e := newEntry(s, key, meta, s.ringBytes)
	e.dir = dir
	e.tmpPath = tmp
	e.finalPath = filepath.Join(dir, "out."+meta.Ext)
	e.file = f

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writing[key] != nil {
		f.Close()
		os.Remove(tmp)
		return nil, waxerr.New(waxerr.CodeInternal, "cache: duplicate Begin for key")
	}
	s.writing[key] = e
	return e, nil
}

// promote publishes a completed entry: fsync, atomic rename, meta.json.
// Called by Entry.Complete with the entry lock RELEASED (readers keep
// draining during the fsync); meta is Complete's snapshot, so nothing
// here touches entry fields that readers share. The paths and file are
// stable once the writer is done.
func (s *Store) promote(e *Entry, meta *Meta) error {
	if err := e.file.Sync(); err != nil {
		return err
	}
	if err := os.Rename(e.tmpPath, e.finalPath); err != nil {
		return err
	}
	meta.CreatedAt = s.now()
	b, err := meta.marshal()
	if err != nil {
		return err
	}
	metaTmp := filepath.Join(e.dir, fmt.Sprintf("meta.json.tmp-%d", s.tmpSeq.Add(1)))
	if err := os.WriteFile(metaTmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(metaTmp, filepath.Join(e.dir, "meta.json")); err != nil {
		os.Remove(metaTmp)
		return err
	}

	s.mu.Lock()
	if old := s.index[e.key]; old != nil {
		s.totalBytes -= old.size
	}
	s.index[e.key] = &indexEntry{size: meta.Bytes, lastAccess: s.now(), meta: *meta}
	s.totalBytes += meta.Bytes
	over := s.maxBytes > 0 && s.totalBytes > s.maxBytes
	s.mu.Unlock()

	if over {
		s.gc()
	}
	return nil
}

// unregister drops an in-flight entry from the registry so later requests
// re-resolve through Lookup/Begin. Degraded entries unregister at the
// moment of degradation: nobody new should attach to a ring that has
// moved on.
func (s *Store) unregister(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.writing, key)
}

// noteDegraded logs a degradation and unregisters the key.
func (s *Store) noteDegraded(key Key, err error) {
	s.log.Warn("cache: write failure, session degraded to ring-fed streaming", "key", key, "err", err)
	s.unregister(key)
}

// dropAborted removes a never-promoted entry's debris.
func (s *Store) dropAborted(dir string) {
	if dir == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "out.*.tmp-*"))
	for _, m := range matches {
		os.Remove(m)
	}
	os.Remove(dir) // only if empty; a promoted sibling keeps it
}

// dropIndexed removes one completed entry (eviction or invalidation).
func (s *Store) dropIndexed(key Key) {
	s.mu.Lock()
	ie := s.index[key]
	if ie != nil {
		s.totalBytes -= ie.size
		delete(s.index, key)
	}
	s.mu.Unlock()
	if ie != nil {
		os.RemoveAll(s.entryDir(key))
	}
}

// Stats is the /cache/stats surface.
type Stats struct {
	Entries int
	Bytes   int64
	Hits    uint64
	Misses  uint64
}

// Stats reports the current cache shape.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Entries: len(s.index),
		Bytes:   s.totalBytes,
		Hits:    s.hits.Load(),
		Misses:  s.misses.Load(),
	}
}

// GC enforces MaxBytes and MaxAge now and reports what it removed.
func (s *Store) GC() (removed int, freed int64) {
	return s.gc()
}

func (s *Store) gc() (removed int, freed int64) {
	type victim struct {
		key        Key
		size       int64
		lastAccess time.Time
	}
	s.mu.Lock()
	all := make([]victim, 0, len(s.index))
	for k, ie := range s.index {
		if s.pinned[k] > 0 {
			continue // a worker is filling it; evicting under it would race
		}
		all = append(all, victim{k, ie.size, ie.lastAccess})
	}
	total := s.totalBytes
	s.mu.Unlock()

	slices.SortFunc(all, func(a, b victim) int { return a.lastAccess.Compare(b.lastAccess) })
	now := s.now()
	for _, v := range all {
		tooOld := s.maxAge > 0 && now.Sub(v.lastAccess) > s.maxAge
		overBudget := s.maxBytes > 0 && total > s.maxBytes
		if !tooOld && !overBudget {
			// Sorted oldest-first: everything further is newer, and the
			// budget only improves by removing, so nothing else qualifies.
			break
		}
		s.dropIndexed(v.key)
		total -= v.size
		removed++
		freed += v.size
	}
	if removed > 0 {
		s.log.Info("cache: gc", "removed", removed, "freedBytes", freed)
	}
	return removed, freed
}

// janitor enforces MaxAge (and any budget drift) periodically.
func (s *Store) janitor() {
	defer s.janitorWG.Done()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.gc()
		case <-s.janitorStop:
			return
		}
	}
}

// Close stops the janitor. In-flight entries are owned by their pipelines
// and finish independently.
func (s *Store) Close() error {
	close(s.janitorStop)
	s.janitorWG.Wait()
	return nil
}
