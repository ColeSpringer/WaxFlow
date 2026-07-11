// Package admission bounds concurrent live pipeline work: one slot per
// interactive stream or sync one-shot (default max(1, NumCPU-1)).
// Cache-served and direct-play requests never acquire a slot; they cost
// no CPU. Async jobs are bounded by the job runner's own worker pool
// (jobSlots) and additionally yield between chunks while this pool is
// saturated, so interactive streams keep every core.
//
// Acquisition is non-blocking by design: over the limit the HTTP layer
// answers 503 with Retry-After rather than queueing, because a queued
// transcode holds the client's time-to-first-audio hostage.
package admission

import "sync"

// Pools is the live admission pool.
type Pools struct {
	live chan struct{}
}

// New sizes the pool. Sizes below one are raised to one.
func New(live int) *Pools {
	return &Pools{live: make(chan struct{}, max(1, live))}
}

// AcquireLive tries to take a live slot. On success the returned release
// is non-nil and idempotent; on saturation release is nil and ok false.
func (p *Pools) AcquireLive() (release func(), ok bool) { return acquire(p.live) }

// LiveInUse reports the occupied live slots (metrics).
func (p *Pools) LiveInUse() int { return len(p.live) }

// LiveSaturated reports a full live pool. Job pipelines check it between
// chunks and pause while interactive streams need every core (the
// job-yields-to-live admission rule).
func (p *Pools) LiveSaturated() bool { return len(p.live) == cap(p.live) }

func acquire(slots chan struct{}) (func(), bool) {
	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, true
	default:
		return nil, false
	}
}
