package hls

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// harness fakes the cache and the worker fleet: segments appear when the
// test says so, and every spawned worker is hand-driven (produce, fail,
// finish) so the state machine is tested deterministically.
type harness struct {
	mu       sync.Mutex
	have     map[int64]bool
	spawns   []*fakeWorker
	spawnErr error
	// autoTotal, when positive, self-drives every spawned worker: produce
	// from its start through autoTotal-1 (yielding between segments),
	// then a clean finish. Zero leaves workers hand-driven.
	autoTotal int64
}

type fakeWorker struct {
	h      *harness
	start  int64
	ctx    context.Context
	notify func(int64)
	exit   func(error)
	once   sync.Once
}

func newHarness() *harness { return &harness{have: map[int64]bool{}} }

func (h *harness) ops() Ops {
	return Ops{
		Has: func(n int64) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.have[n]
		},
		Spawn: func(start int64, notify func(int64), exit func(error)) (context.CancelFunc, error) {
			if h.spawnErr != nil {
				return nil, h.spawnErr
			}
			ctx, cancel := context.WithCancel(context.Background())
			w := &fakeWorker{h: h, start: start, ctx: ctx, notify: notify, exit: exit}
			h.mu.Lock()
			h.spawns = append(h.spawns, w)
			h.mu.Unlock()
			// Cancellation behaves like the real pipeline: the worker
			// notices and exits with a canceled error.
			go func() {
				<-ctx.Done()
				w.finish(waxerr.Wrap(waxerr.CodeCanceled, "worker canceled", ctx.Err()))
			}()
			if h.autoTotal > 0 {
				go func() {
					for n := start; n < h.autoTotal; n++ {
						select {
						case <-ctx.Done():
							return
						default:
						}
						w.produce(n)
						time.Sleep(50 * time.Microsecond)
					}
					w.finish(nil)
				}()
			}
			return cancel, nil
		},
	}
}

// produce marks segment n cached and wakes waiters, as the worker's write
// path does.
func (w *fakeWorker) produce(n int64) {
	w.h.mu.Lock()
	w.h.have[n] = true
	w.h.mu.Unlock()
	w.notify(n)
}

func (w *fakeWorker) finish(err error) { w.once.Do(func() { w.exit(err) }) }

func (h *harness) worker(t *testing.T, i int) *fakeWorker {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		if len(h.spawns) > i {
			w := h.spawns[i]
			h.mu.Unlock()
			return w
		}
		h.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("worker %d never spawned", i)
		}
		time.Sleep(time.Millisecond)
	}
}

// segmentAsync runs Segment on its own goroutine and returns the result
// channel.
func segmentAsync(m *Manager, key string, n int64, ops Ops) chan error {
	ch := make(chan error, 1)
	go func() { ch <- m.Segment(context.Background(), key, n, ops) }()
	return ch
}

func mustBlock(t *testing.T, ch chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func mustReturn(t *testing.T, ch chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Segment never returned")
		return nil
	}
}

func TestManagerHitNeedsNoWorker(t *testing.T) {
	h := newHarness()
	h.have[3] = true
	m := &Manager{}
	if err := m.Segment(context.Background(), "k", 3, h.ops()); err != nil {
		t.Fatal(err)
	}
	if len(h.spawns) != 0 {
		t.Fatalf("%d workers spawned for a cache hit", len(h.spawns))
	}
	if m.Variants() != 0 {
		t.Fatal("variant state leaked")
	}
}

func TestManagerSpawnsAndWaits(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 0, h.ops())
	w := h.worker(t, 0)
	if w.start != 0 {
		t.Fatalf("worker started at %d, want 0", w.start)
	}
	mustBlock(t, ch)
	w.produce(0)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}

	// Within the lookahead window: the same worker serves, no restart.
	ch2 := segmentAsync(m, "k", 2, h.ops())
	mustBlock(t, ch2)
	w.produce(1)
	mustBlock(t, ch2)
	w.produce(2)
	if err := mustReturn(t, ch2); err != nil {
		t.Fatal(err)
	}
	if len(h.spawns) != 1 {
		t.Fatalf("%d workers for linear play, want 1", len(h.spawns))
	}
	w.finish(nil)
}

func TestManagerRestartsOutOfWindow(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 0, h.ops())
	w0 := h.worker(t, 0)
	w0.produce(0)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}

	// Far past the window: the old worker is canceled, a new one starts
	// exactly at the requested segment.
	ch = segmentAsync(m, "k", 50, h.ops())
	w1 := h.worker(t, 1)
	if w1.start != 50 {
		t.Fatalf("restarted worker at %d, want 50", w1.start)
	}
	select {
	case <-w0.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("old worker never canceled")
	}
	w1.produce(50)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	w1.finish(nil)
}

func TestManagerRestartsBehindWorker(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 10, h.ops())
	w0 := h.worker(t, 0)
	w0.produce(10)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	// Segment 2 is behind the worker and not cached (evicted): restart.
	ch = segmentAsync(m, "k", 2, h.ops())
	w1 := h.worker(t, 1)
	if w1.start != 2 {
		t.Fatalf("restarted worker at %d, want 2", w1.start)
	}
	w1.produce(2)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	w1.finish(nil)
}

func TestManagerWorkerFailurePropagates(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 0, h.ops())
	w := h.worker(t, 0)
	mustBlock(t, ch)
	w.finish(waxerr.New(waxerr.CodeSourceUnreadable, "disk died"))
	err := mustReturn(t, ch)
	if waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Fatalf("err %v, want the worker's failure", err)
	}
}

func TestManagerPastEndOfStream(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 7, h.ops())
	w := h.worker(t, 0)
	mustBlock(t, ch)
	w.finish(nil) // clean end of stream without ever reaching 7
	err := mustReturn(t, ch)
	if waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("err %v, want not-found", err)
	}
}

func TestManagerSpawnErrorPropagates(t *testing.T) {
	h := newHarness()
	h.spawnErr = waxerr.New(waxerr.CodeOverloaded, "slots full")
	m := &Manager{}
	err := m.Segment(context.Background(), "k", 0, h.ops())
	if waxerr.CodeOf(err) != waxerr.CodeOverloaded {
		t.Fatalf("err %v, want overloaded", err)
	}
	if m.Variants() != 0 {
		t.Fatal("variant state leaked after spawn failure")
	}
}

func TestManagerClientCancel(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- m.Segment(ctx, "k", 0, h.ops()) }()
	w := h.worker(t, 0)
	cancel()
	err := mustReturn(t, ch)
	if waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("err %v, want canceled", err)
	}
	// The worker keeps encoding for the cache; only the waiter left.
	select {
	case <-w.ctx.Done():
		t.Fatal("client cancellation killed the worker")
	default:
	}
	w.finish(nil)
}

func TestManagerWaitTimeout(t *testing.T) {
	h := newHarness()
	m := &Manager{WaitTimeout: 30 * time.Millisecond}
	ch := segmentAsync(m, "k", 0, h.ops())
	w := h.worker(t, 0) // spawned but never produces
	err := mustReturn(t, ch)
	if waxerr.CodeOf(err) != waxerr.CodeInternal {
		t.Fatalf("err %v, want the timeout", err)
	}
	w.finish(nil)
}

// TestManagerStaleHasServesFromProgress pins the lock-consistent serve:
// Has runs outside the manager lock, so its answer can predate the
// worker's progress. Once the worker has landed n (notify follows the
// write, and the variant is pinned while it runs), Segment must serve
// from the manager's own bookkeeping even when Has still says no, and
// must neither cancel the healthy worker nor spawn another.
func TestManagerStaleHasServesFromProgress(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 0, h.ops())
	w := h.worker(t, 0)
	w.produce(0)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	// The worker lands 1 and 2, but only through notify: the harness Has
	// keeps answering false, a stat frozen in the past.
	w.notify(1)
	w.notify(2)
	if err := m.Segment(context.Background(), "k", 1, h.ops()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.ctx.Done():
		t.Fatal("healthy worker canceled on a stale stat")
	default:
	}
	if len(h.spawns) != 1 {
		t.Fatalf("%d workers, want the original only", len(h.spawns))
	}
	w.finish(nil)
}

// staleOnce wraps Ops so the first Has answers false and every later one
// tells the truth as true: the exact shape of a stat that raced the
// worker's write.
func staleOnce(ops Ops) Ops {
	first := true
	ops.Has = func(int64) bool {
		if first {
			first = false
			return false
		}
		return true
	}
	return ops
}

// TestManagerRecheckBeforeCancel: a request behind a healthy worker whose
// first Has was stale must not kill the worker once the re-check finds
// the segment.
func TestManagerRecheckBeforeCancel(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	ch := segmentAsync(m, "k", 10, h.ops())
	w := h.worker(t, 0)
	w.produce(10)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	if err := m.Segment(context.Background(), "k", 2, staleOnce(h.ops())); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.ctx.Done():
		t.Fatal("healthy worker canceled on a stale stat")
	default:
	}
	if len(h.spawns) != 1 {
		t.Fatalf("%d workers, want the original only", len(h.spawns))
	}
	w.finish(nil)
}

// TestManagerRecheckBeforeSpawn: with no worker running, a stale miss
// must not pay for a redundant worker when the re-check finds the
// segment.
func TestManagerRecheckBeforeSpawn(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	if err := m.Segment(context.Background(), "k", 3, staleOnce(h.ops())); err != nil {
		t.Fatal(err)
	}
	if len(h.spawns) != 0 {
		t.Fatalf("%d workers spawned for a segment that was on disk", len(h.spawns))
	}
	if m.Variants() != 0 {
		t.Fatal("variant state leaked")
	}
}

// TestManagerConcurrent hammers one variant from many goroutines under
// the race detector: every waiter must end in a hit or a retryable
// cancellation (a racing seek restarting the worker under it), and the
// bookkeeping must drain once everyone leaves.
func TestManagerConcurrent(t *testing.T) {
	const total = 40
	h := newHarness()
	h.autoTotal = total
	m := &Manager{WaitTimeout: 5 * time.Second}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				n := int64((g*7 + i*5) % total)
				if err := m.Segment(context.Background(), "k", n, h.ops()); err != nil {
					switch waxerr.CodeOf(err) {
					case waxerr.CodeCanceled, waxerr.CodeOverloaded:
						// A racing restart can cancel the worker under a
						// waiter; both are retryable answers.
					default:
						errs <- fmt.Errorf("segment %d: %w", n, err)
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.Variants() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d variants still tracked after all waiters left", m.Variants())
		}
		time.Sleep(time.Millisecond)
	}
}
