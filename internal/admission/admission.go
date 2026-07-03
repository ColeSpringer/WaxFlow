// Package admission bounds concurrent pipeline work with two weighted
// pools: live slots for interactive streams and sync one-shots (default
// max(1, NumCPU-1)), and job slots for async full-file work (default 2,
// first used once the job store lands). Cache-served and direct-play
// requests never acquire a slot; they cost no CPU.
//
// Acquisition is non-blocking by design: over the limit the HTTP layer
// answers 503 with Retry-After rather than queueing, because a queued
// transcode holds the client's time-to-first-audio hostage.
package admission

import "sync"

// Pools is the pair of admission pools.
type Pools struct {
	live chan struct{}
	job  chan struct{}
}

// New sizes the pools. Sizes below one are raised to one.
func New(live, job int) *Pools {
	return &Pools{
		live: make(chan struct{}, max(1, live)),
		job:  make(chan struct{}, max(1, job)),
	}
}

// AcquireLive tries to take a live slot. On success the returned release
// is non-nil and idempotent; on saturation release is nil and ok false.
func (p *Pools) AcquireLive() (release func(), ok bool) { return acquire(p.live) }

// AcquireJob tries to take a job slot, with AcquireLive's contract.
func (p *Pools) AcquireJob() (release func(), ok bool) { return acquire(p.job) }

// LiveInUse reports the occupied live slots (metrics).
func (p *Pools) LiveInUse() int { return len(p.live) }

// JobInUse reports the occupied job slots (metrics).
func (p *Pools) JobInUse() int { return len(p.job) }

func acquire(slots chan struct{}) (func(), bool) {
	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, true
	default:
		return nil, false
	}
}
