package hls

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// DefaultLookahead is how far ahead of a worker's position a segment
// request may run and still wait for it rather than force a restart:
// linear playback with a few parallel player fetches stays on one encoder
// state, while a genuine seek restarts immediately.
const DefaultLookahead = 3

// defaultWaitTimeout bounds one Segment call's total wait. Encoding runs
// well above realtime, so a segment not materializing within this is a
// wedged worker, and erroring beats holding the client forever.
const defaultWaitTimeout = 60 * time.Second

// maxSpawnsPerWait bounds how many workers one request may start before
// giving up: the second spawn covers a worker that was canceled under us
// (a competing seek); needing a third means the variant is thrashing.
const maxSpawnsPerWait = 2

// Ops are the per-variant callbacks the server supplies to Segment. The
// same key must always arrive with equivalent Ops.
type Ops struct {
	// Has reports whether segment n is already served from cache.
	Has func(n int64) bool
	// Spawn starts a worker producing segments start, start+1, ... until
	// end of stream or cancellation. The worker calls notify(n) as each
	// segment becomes servable and exit(err) exactly once when it stops
	// (nil after a clean end of stream). Spawn returns the worker's
	// cancel plus nil, or an error without starting (admission full).
	// It must not call notify or exit synchronously.
	Spawn func(start int64, notify func(int64), exit func(error)) (context.CancelFunc, error)
}

// Manager serializes variant workers: at most one per variant key, with
// waiters parked on segment arrival. State is only bookkeeping; the
// segments themselves live in the cache (Ops.Has is the ground truth), so
// a restarted daemon reconstructs everything from disk.
type Manager struct {
	// Lookahead overrides DefaultLookahead when positive.
	Lookahead int64
	// WaitTimeout overrides defaultWaitTimeout when positive.
	WaitTimeout time.Duration

	mu       sync.Mutex
	variants map[string]*variant
}

type variant struct {
	waiters int
	cur     *worker // nil when no worker is running
	lastErr error   // the most recent worker's terminal error, nil after a clean end
	notify  chan struct{}
}

type worker struct {
	start    int64 // the segment the worker began at
	next     int64 // the segment the worker is producing now
	produced bool  // it has landed at least one segment since starting
	cancel   context.CancelFunc
	exited   chan struct{}
}

// Segment blocks until segment n of the keyed variant is servable
// (Ops.Has returns true): served straight off a hit, waited for when the
// running worker is within the lookahead window, else the worker is
// restarted at n with the old one canceled. The error is the worker's
// real failure when it died producing n.
func (m *Manager) Segment(ctx context.Context, key string, n int64, ops Ops) error {
	if n < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "hls: negative segment index")
	}
	timeout := m.WaitTimeout
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lookahead := m.Lookahead
	if lookahead <= 0 {
		lookahead = DefaultLookahead
	}

	v := m.acquire(key)
	defer m.release(key, v)

	// The Has check runs outside the manager lock (a stat, possibly on a
	// slow volume, must not stall every variant), so by the time the lock
	// is held its answer can be stale: the worker may have written n,
	// advanced, even exited in between. Every destructive or terminal
	// branch below therefore re-establishes the truth first — from the
	// manager's own lock-consistent bookkeeping where possible (notify
	// happens strictly after the segment write, and the variant is pinned
	// against eviction while its worker runs, so start <= n < next proves
	// the file exists), and by re-checking Has otherwise — so a stale stat
	// can never 404 a written segment, kill a healthy worker, or spawn a
	// redundant one.
	spawns := 0
	for {
		if ops.Has(n) {
			return nil
		}
		m.mu.Lock()
		w := v.cur
		switch {
		case w != nil && n >= w.start && n < w.next:
			// The running worker already landed n: the Has that brought us
			// here predates the write. Serve.
			m.mu.Unlock()
			return nil

		case w != nil && (!w.produced || (n >= w.next && n <= w.next+lookahead)):
			// Within the window: the worker will get there; park until
			// something changes (a segment lands or the worker exits). A
			// worker that has not landed its first segment yet is also
			// waited on, never canceled: competing seeks could otherwise
			// cancel each other's workers before any segment materializes
			// and livelock the variant, whereas guaranteed first-segment
			// progress makes every restart storm converge.
			ch := v.notify
			m.mu.Unlock()
			if err := waitSignal(ctx, ch); err != nil {
				return err
			}

		case w != nil:
			// Out of the window (behind after an eviction, or a seek far
			// ahead): this worker is the wrong one. Re-check before the
			// kill, then cancel and wait for the slot. Canceling without
			// the lock is safe: a worker that already exited (or was
			// replaced, which only happens after exit) ignores the cancel
			// and its exited channel is closed.
			m.mu.Unlock()
			if ops.Has(n) {
				return nil
			}
			w.cancel()
			if err := waitSignal(ctx, w.exited); err != nil {
				return err
			}

		case spawns >= maxSpawnsPerWait:
			err := v.lastErr
			m.mu.Unlock()
			if ops.Has(n) {
				return nil
			}
			if err != nil {
				return err
			}
			return waxerr.New(waxerr.CodeOverloaded, "hls: variant worker contention; retry")

		case spawns > 0:
			// Our worker came and went without producing n. Its failure is
			// the honest answer; a clean end of stream means n is past the
			// end (a lying playlist consumer, or a source that shrank).
			err := v.lastErr
			m.mu.Unlock()
			if ops.Has(n) {
				return nil
			}
			if err != nil {
				return err
			}
			return waxerr.New(waxerr.CodeNotFound, fmt.Sprintf("hls: segment %d is past the end of the stream", n))

		default:
			// No worker: n's absence may itself be the stale part (the
			// last worker's final write racing our check), so confirm
			// before paying for a spawn.
			m.mu.Unlock()
			if ops.Has(n) {
				return nil
			}
			m.mu.Lock()
			if v.cur != nil {
				// Someone spawned while we re-checked; re-evaluate.
				m.mu.Unlock()
				continue
			}
			// Start one at n. The lock is held across Spawn (it only
			// acquires a slot and launches a goroutine) so no second
			// waiter can double-spawn; the callbacks lock the manager
			// themselves, which is why Spawn must not call them inline.
			wk := &worker{start: n, next: n, exited: make(chan struct{})}
			cancelWorker, err := ops.Spawn(n, m.notifyFunc(v, wk), m.exitFunc(key, v, wk))
			if err != nil {
				m.mu.Unlock()
				return err
			}
			wk.cancel = cancelWorker
			v.cur = wk
			v.lastErr = nil
			spawns++
			m.mu.Unlock()
		}
	}
}

// notifyFunc records the worker's progress and wakes every waiter.
func (m *Manager) notifyFunc(v *variant, wk *worker) func(int64) {
	return func(n int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if v.cur == wk {
			wk.next = n + 1
			wk.produced = true
		}
		m.wake(v)
	}
}

// exitFunc retires the worker: replaced workers (v.cur moved on) only
// close their exited channel, the current one also records its terminal
// error and unregisters.
func (m *Manager) exitFunc(key string, v *variant, wk *worker) func(error) {
	return func(err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if v.cur == wk {
			v.cur = nil
			v.lastErr = err
		}
		close(wk.exited)
		m.wake(v)
		m.maybeDrop(key, v)
	}
}

// wake replaces the notify channel, releasing everyone parked on the old
// one. Locked.
func (m *Manager) wake(v *variant) {
	close(v.notify)
	v.notify = make(chan struct{})
}

func (m *Manager) acquire(key string) *variant {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.variants == nil {
		m.variants = make(map[string]*variant)
	}
	v := m.variants[key]
	if v == nil {
		v = &variant{notify: make(chan struct{})}
		m.variants[key] = v
	}
	v.waiters++
	return v
}

func (m *Manager) release(key string, v *variant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v.waiters--
	m.maybeDrop(key, v)
}

// maybeDrop removes an idle variant's bookkeeping: no waiters, no worker.
// Locked. The cached segments stay, of course; only the in-memory state
// goes ("only active variant workers live in memory").
func (m *Manager) maybeDrop(key string, v *variant) {
	if v.waiters == 0 && v.cur == nil && m.variants[key] == v {
		delete(m.variants, key)
	}
}

// waitSignal parks until ch closes or ctx ends, mapping the context's end
// onto envelope codes: a vanished client is a cancellation, a blown
// deadline is a wedged worker.
func waitSignal(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return waxerr.New(waxerr.CodeInternal, "hls: timed out waiting for the segment worker")
		}
		return waxerr.Wrap(waxerr.CodeCanceled, "hls: canceled waiting for a segment", ctx.Err())
	}
}

// Variants reports the number of tracked variants (tests and metrics).
func (m *Manager) Variants() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.variants)
}
