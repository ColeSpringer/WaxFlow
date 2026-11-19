package server

import (
	"strconv"
	"testing"
)

// TestGridCacheEvictsLeastRecentlyUsed: a grid is a full walk to rebuild, so
// a sweep past the cap drops the entry used longest ago and keeps a hot one.
func TestGridCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newGridCache()
	if _, ok := c.get("absent"); ok {
		t.Error("zero-value cache reported a hit")
	}
	for i := range gridCacheCap {
		c.put(strconv.Itoa(i), i)
	}
	if g, ok := c.get("0"); !ok || g != 0 {
		t.Fatalf("key 0 = %d, %v, want the memoized zero grid", g, ok)
	}
	// A replace at the cap is a use and evicts nothing.
	c.put("1", 1024)
	c.put("new", 7)

	if _, ok := c.get("0"); !ok {
		t.Error("evicted the entry a get had just used")
	}
	if g, ok := c.get("1"); !ok || g != 1024 {
		t.Errorf("key 1 = %d, %v, want the replaced 1024", g, ok)
	}
	if _, ok := c.get("2"); ok {
		t.Error("key 2 survived; it was the least recently used")
	}
	if _, ok := c.get("3"); !ok {
		t.Error("one insert at the cap evicted more than one entry")
	}
}
