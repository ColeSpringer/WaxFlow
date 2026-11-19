// Package lru is a least-recently-used cache, the one bounded memo the
// module's owners build their own rules on.
package lru

import "fmt"

// Cache maps keys to values and evicts the least recently used entry when a
// new key arrives at the bound. It is not safe for concurrent use: each owner
// holds its own lock. Every operation but the two that take a func is O(1).
// Build one with New or NewUnbounded; the zero value is not usable.
type Cache[K comparable, V any] struct {
	max     int // 0 is no bound
	entries map[K]*node[K, V]
	// root is the list's sentinel: root.next is the most recently used
	// entry and root.prev the least.
	root node[K, V]
}

type node[K comparable, V any] struct {
	k          K
	v          V
	prev, next *node[K, V]
}

// New returns a cache that holds at most max entries. It panics on a max
// below 1, so a bound lost to config or arithmetic cannot pass for none.
func New[K comparable, V any](max int) *Cache[K, V] {
	if max < 1 {
		panic(fmt.Sprintf("lru: New with max %d, want at least 1", max))
	}
	c := NewUnbounded[K, V]()
	c.max = max
	return c
}

// NewUnbounded returns a cache that never evicts on its own, for an owner
// that removes the oldest entries by a rule of its own.
func NewUnbounded[K comparable, V any]() *Cache[K, V] {
	c := &Cache[K, V]{entries: make(map[K]*node[K, V])}
	c.root.prev, c.root.next = &c.root, &c.root
	return c
}

// Get returns k's value and marks it the most recently used.
func (c *Cache[K, V]) Get(k K) (V, bool) {
	n, ok := c.entries[k]
	if !ok {
		var zero V
		return zero, false
	}
	c.unlink(n)
	c.pushFront(n)
	return n.v, true
}

// Peek returns k's value and leaves the order alone.
func (c *Cache[K, V]) Peek(k K) (V, bool) {
	n, ok := c.entries[k]
	if !ok {
		var zero V
		return zero, false
	}
	return n.v, true
}

// Put stores v under k as the most recently used entry. Replacing a held key
// evicts nothing; a new key at the bound drops the least recently used entry.
func (c *Cache[K, V]) Put(k K, v V) {
	if n, ok := c.entries[k]; ok {
		n.v = v
		c.unlink(n)
		c.pushFront(n)
		return
	}
	if c.max > 0 && len(c.entries) >= c.max {
		c.remove(c.root.prev)
	}
	n := &node[K, V]{k: k, v: v}
	c.entries[k] = n
	c.pushFront(n)
}

// RemoveOldest removes the least recently used entry and returns it, or
// reports false for an empty cache.
func (c *Cache[K, V]) RemoveOldest() (K, V, bool) {
	n := c.root.prev
	if n == &c.root {
		var k K
		var v V
		return k, v, false
	}
	c.remove(n)
	return n.k, n.v, true
}

// Delete removes k and reports whether it was held.
func (c *Cache[K, V]) Delete(k K) bool {
	n, ok := c.entries[k]
	if ok {
		c.remove(n)
	}
	return ok
}

// DeleteFunc removes every entry del reports true for and returns how many
// went. It visits the entries from least to most recently used, and del must
// not call back into the cache.
func (c *Cache[K, V]) DeleteFunc(del func(K, V) bool) int {
	removed := 0
	for n := c.root.prev; n != &c.root; {
		older := n
		n = n.prev
		if del(older.k, older.v) {
			c.remove(older)
			removed++
		}
	}
	return removed
}

// DeleteFirstFunc removes the least recently used entry del reports true for
// and stops there, reporting whether one went. del must not call back into
// the cache.
func (c *Cache[K, V]) DeleteFirstFunc(del func(K, V) bool) bool {
	for n := c.root.prev; n != &c.root; n = n.prev {
		if del(n.k, n.v) {
			c.remove(n)
			return true
		}
	}
	return false
}

// Len reports how many entries the cache holds.
func (c *Cache[K, V]) Len() int { return len(c.entries) }

func (c *Cache[K, V]) remove(n *node[K, V]) {
	c.unlink(n)
	delete(c.entries, n.k)
}

func (c *Cache[K, V]) unlink(n *node[K, V]) {
	n.prev.next, n.next.prev = n.next, n.prev
	n.prev, n.next = nil, nil
}

func (c *Cache[K, V]) pushFront(n *node[K, V]) {
	n.prev, n.next = &c.root, c.root.next
	n.prev.next, n.next.prev = n, n
}
