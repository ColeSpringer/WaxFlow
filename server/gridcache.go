package server

import (
	"math"
	"sync"

	"github.com/colespringer/waxflow/source"
)

// gridCacheCap bounds the packet-grid memo. An entry is one int against a
// string key, so the cap is about lifetime rather than size, exactly as
// trackCacheCap is: keyed by identity and invalidated only by replacement, the
// memo would otherwise grow once per file for the process's life and a
// library-wide sweep would pin the whole catalog.
//
// It sits beside trackCache rather than inside it because the two answer
// different questions at very different costs, and merging them would make the
// cheap one pay for the expensive one. A track is a header parse that every
// stream needs; a grid is a walk of every packet in the source that only a
// segmented remux asks for. A single entry would either force the walk on
// requests that never wanted it, or carry a "not walked yet" state that is a
// second cache in one struct.
const gridCacheCap = 4096

// gridCache is a bounded identity-keyed memo of packet grids.
//
// Eviction is least-recently-used, following trackCache and resolver.Catalog
// for the same reason both do: rebuilding an entry is expensive (a full walk of
// the source), so evicting a hot one is costly and a library-wide sweep must
// not push a live session's grid out of the memo.
type gridCache struct {
	mu      sync.Mutex
	entries map[string]*gridEntry
	clock   int64
}

type gridEntry struct {
	grid int
	used int64 // clock value at last access; the LRU order
}

func (c *gridCache) get(key string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return 0, false
	}
	c.clock++
	e.used = c.clock
	return e.grid, true
}

func (c *gridCache) put(key string, grid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*gridEntry, gridCacheCap)
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= gridCacheCap {
		c.evictOldestLocked()
	}
	c.clock++
	c.entries[key] = &gridEntry{grid: grid, used: c.clock}
}

// evictOldestLocked drops the least recently used entry: a linear scan, run
// only on insert at capacity, exactly as trackCache does it.
func (c *gridCache) evictOldestLocked() {
	var oldest string
	var found bool
	minUsed := int64(math.MaxInt64)
	for k, e := range c.entries {
		// found, not oldest != "": an empty key is a legal map key, so the zero
		// value cannot double as "nothing selected".
		if !found || e.used < minUsed {
			minUsed, oldest, found = e.used, k, true
		}
	}
	if found {
		delete(c.entries, oldest)
	}
}

// gridFor returns src's packet grid, memoized by source identity: the decode
// duration every packet shares, or 0 when they vary and no segmented remux can
// lay boundaries on them.
//
// A zero grid is memoized like any other answer, which is the point of storing
// the number rather than only the successes: "this source cannot be segment-
// remuxed" is exactly as expensive to learn as the affirmative, and a source
// that declines must not re-walk on every segment request forever.
//
// The key is the identity, which encodes its own invalidation: a replaced file
// has a new identity and misses, so a stale grid cannot be served. Misses are
// collapsed per key rather than merely memoized, for the reason trackFor gives:
// the memo is in-memory, so a daemon restart with live sessions is cold, and
// the resuming clients arrive as a burst of concurrent segment requests for one
// source, each of which would otherwise run its own walk.
func (s *Server) gridFor(src *source.File) (int, error) {
	key := identityString(src.Ref, src.ID)
	if g, ok := s.gridCache.get(key); ok {
		return g, nil
	}
	return s.gridFlight.Do(key, func() (int, error) {
		// Re-check under the flight: flight is duplicate suppression, not
		// caching, so a caller that missed just as the previous flight filled
		// the memo would otherwise walk again.
		if g, ok := s.gridCache.get(key); ok {
			return g, nil
		}
		grid, err := s.eng.PacketGrid(src, src.Ext)
		if err != nil {
			return 0, err
		}
		s.gridCache.put(key, grid)
		return grid, nil
	})
}
