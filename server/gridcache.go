package server

import (
	"context"
	"sync"

	"github.com/colespringer/waxflow/internal/lru"
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
// segmented remux or a cut asks for. A single entry would either force the
// walk on requests that never wanted it, or carry a "not walked yet" state
// that is a second cache in one struct.
const gridCacheCap = 4096

// gridCache is a bounded identity-keyed memo of packet grids.
//
// Eviction is least-recently-used, following trackCache for the same reason:
// rebuilding an entry is expensive (a full walk of the source), so evicting a
// hot one is costly and a library-wide sweep must not push a live session's
// grid out of the memo.
type gridCache struct {
	mu      sync.Mutex
	entries *lru.Cache[string, int]
}

func newGridCache() *gridCache {
	return &gridCache{entries: lru.New[string, int](gridCacheCap)}
}

func (c *gridCache) get(key string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries.Get(key)
}

func (c *gridCache) put(key string, grid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries.Put(key, grid)
}

// gridFor returns src's packet grid, memoized by source identity: the decode
// duration every packet shares, or 0 when they vary and neither a segmented
// remux nor a cut can lay its boundaries on them.
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
func (s *Server) gridFor(ctx context.Context, src *source.File) (int, error) {
	key := identityString(src.Ref, src.ID)
	if g, ok := s.gridCache.get(key); ok {
		return g, nil
	}
	// A joined flight, as trackFor's is: the walk runs for every caller
	// waiting on it, on a handle of its own.
	return untilOwnCancel(ctx, func() (int, error) {
		return s.gridFlight.JoinHolding(ctx, key, holdFile(src), func(ctx context.Context) (int, error) {
			// Re-check under the flight: flight is duplicate suppression, not
			// caching, so a caller that missed just as the previous flight
			// filled the memo would otherwise walk again.
			if g, ok := s.gridCache.get(key); ok {
				return g, nil
			}
			grid, err := s.eng.PacketGrid(ctx, src, src.Ext)
			if err != nil {
				return 0, err
			}
			s.gridCache.put(key, grid)
			return grid, nil
		})
	})
}
