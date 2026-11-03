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
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// ErrPanicked is what waiters receive when the executing call panicked. Under
// Do the panic itself propagates in the executing goroutine; under Join it is
// recovered, and the error carries the panic's value and stack.
var ErrPanicked = errors.New("flight: shared call panicked")

type call[V any] struct {
	done chan struct{}
	val  V
	err  error
	// Join's calls: the shared context's cancel, and how many callers wait.
	cancel  context.CancelFunc
	waiting int
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

// Join is Do for work that reads a context: fn runs once per key, in a
// goroutine of its own, on a context that ends when the last caller waiting
// on it has left, so a caller that leaves gets its own context's error and
// the callers still waiting keep the work. A call every caller has left is
// over, whatever it is still doing: the next caller starts one of its own.
// fn's context carries ctx's values and none of its cancel. A panic in fn
// reaches every caller as ErrPanicked, with the panic's stack in the message.
func (g *Group[V]) Join(ctx context.Context, key string, fn func(context.Context) (V, error)) (V, error) {
	return g.JoinHolding(ctx, key, nil, fn)
}

// JoinHolding is Join for a call that holds something for its life, a file
// handle say: when a call starts, hold runs in the calling goroutine, before
// JoinHolding returns, and what it returns releases the hold when the call
// ends. A caller that joins a call in flight holds nothing. A nil hold holds
// nothing.
func (g *Group[V]) JoinHolding(ctx context.Context, key string, hold func() (release func()), fn func(context.Context) (V, error)) (V, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call[V])
	}
	c, ok := g.m[key]
	if ok && c.cancel != nil && c.waiting == 0 {
		ok = false // every caller left; the call is ending
	}
	if !ok {
		shared, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c = &call[V]{done: make(chan struct{}), cancel: cancel}
		g.m[key] = c
		release := func() {}
		if hold != nil {
			release = hold()
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.err = fmt.Errorf("%w: %v\n%s", ErrPanicked, r, debug.Stack())
				}
				release()
				g.mu.Lock()
				if g.m[key] == c {
					delete(g.m, key)
				}
				g.mu.Unlock()
				cancel()
				close(c.done)
			}()
			c.val, c.err = fn(shared)
		}()
	}
	c.waiting++
	g.mu.Unlock()
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		g.mu.Lock()
		if c.waiting--; c.waiting == 0 && c.cancel != nil {
			c.cancel()
		}
		g.mu.Unlock()
		var zero V
		return zero, ctx.Err()
	}
}

// Waiting reports how many callers wait on key's call in flight, 0 for none.
func (g *Group[V]) Waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.m[key]; ok {
		return c.waiting
	}
	return 0
}
