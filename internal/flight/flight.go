// Package flight provides keyed call deduplication: concurrent calls with
// the same key share one execution and its result. It covers pipeline
// spawn, probe, and (later) playlist generation, the places where a
// request stampede would otherwise start duplicate work.
//
// This is duplicate suppression, not caching: once a call completes, the
// next call with the same key runs again. singleflight lives in
// golang.org/x/sync, not the stdlib, so the family dependency promise
// requires writing the primitive ourselves.
package flight

import (
	"errors"
	"sync"
)

// ErrPanicked is what waiters receive when the executing call panicked.
// The panic itself propagates in the executing goroutine.
var ErrPanicked = errors.New("flight: shared call panicked")

type call[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// Group deduplicates concurrent calls by key. The zero value is ready to
// use.
type Group[V any] struct {
	mu sync.Mutex
	m  map[string]*call[V]
}

// Do invokes fn, ensuring only one execution per key is in flight at a
// time: concurrent callers with the same key block and share the first
// call's result. fn runs in the calling goroutine.
func (g *Group[V]) Do(key string, fn func() (V, error)) (V, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call[V])
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err
	}
	c := &call[V]{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()

	// Preset the error so waiters see a failure (not a zero result) if fn
	// panics past the assignment below; the deferred cleanup always runs.
	c.err = ErrPanicked
	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.val, c.err = fn()
	return c.val, c.err
}
