package hls

import (
	"context"
	"errors"
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
	// manualExit leaves a canceled worker running until the test finishes it.
	manualExit bool
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
		Spawn: func(start int64, notify func(int64), exit func(error)) (context.CancelCauseFunc, error) {
			if h.spawnErr != nil {
				return nil, h.spawnErr
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			w := &fakeWorker{h: h, start: start, ctx: ctx, notify: notify, exit: exit}
			h.mu.Lock()
			h.spawns = append(h.spawns, w)
			manual := h.manualExit
			h.mu.Unlock()
			// Cancellation behaves like the real pipeline: the worker
			// notices and exits with a canceled error.
			go func() {
				<-ctx.Done()
				if !manual {
					w.finish(waxerr.Wrap(waxerr.CodeCanceled, "worker canceled", ctx.Err()))
				}
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

func (h *harness) worker(t testing.TB, i int) *fakeWorker {
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
	if errors.Is(context.Cause(w0.ctx), ErrIdle) {
		t.Error("a restart canceled the old worker as idle")
	}
	w1.produce(50)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	w1.finish(nil)
}

// TestManagerSeekWaitsForTheFirstSegment: a worker that has not landed its
// first segment is waited on by every request, a far seek included, so
// competing seeks cannot cancel each other's workers before any segment
// exists. Once it has produced, the seek restarts it as usual.
func TestManagerSeekWaitsForTheFirstSegment(t *testing.T) {
	h := newHarness()
	m := &Manager{}
	near := segmentAsync(m, "k", 0, h.ops())
	w0 := h.worker(t, 0)
	far := segmentAsync(m, "k", 50, h.ops())
	mustBlock(t, far)
	if w0.ctx.Err() != nil {
		t.Fatal("a seek canceled a worker that had not produced its first segment")
	}
	h.mu.Lock()
	spawned := len(h.spawns)
	h.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("%d workers while the first had produced nothing, want 1", spawned)
	}
	w0.produce(0)
	if err := mustReturn(t, near); err != nil {
		t.Fatal(err)
	}
	w1 := h.worker(t, 1)
	if w1.start != 50 {
		t.Fatalf("restarted worker at %d, want 50", w1.start)
	}
	select {
	case <-w0.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the seek never canceled the worker once it had produced")
	}
	w1.produce(50)
	if err := mustReturn(t, far); err != nil {
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
	ft := &fakeTimers{}
	m := &Manager{afterFunc: ft.afterFunc}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- m.Segment(ctx, "k", 0, h.ops()) }()
	w := h.worker(t, 0)
	cancel()
	err := mustReturn(t, ch)
	if waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("err %v, want canceled", err)
	}
	// With no idle timeout the worker keeps encoding for the cache; only the
	// waiter left, and nothing will stop the worker for it.
	select {
	case <-w.ctx.Done():
		t.Fatal("client cancellation killed the worker")
	default:
	}
	if n := len(ft.armed()); n != 0 {
		t.Errorf("%d idle timers armed with no idle timeout", n)
	}
	w.finish(nil)
}

// fakeTimers is the manager's idle timer seam under the test's hand: a timer
// fires only when the test runs its func.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	ft      *fakeTimers
	d       time.Duration
	f       func()
	stopped bool
	fired   bool
}

func (ft *fakeTimers) afterFunc(d time.Duration, f func()) func() bool {
	tm := &fakeTimer{ft: ft, d: d, f: f}
	ft.mu.Lock()
	ft.timers = append(ft.timers, tm)
	ft.mu.Unlock()
	return func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		was := !tm.stopped && !tm.fired
		tm.stopped = true
		return was
	}
}

// armed returns the timers neither stopped nor fired.
func (ft *fakeTimers) armed() []*fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []*fakeTimer
	for _, tm := range ft.timers {
		if !tm.stopped && !tm.fired {
			out = append(out, tm)
		}
	}
	return out
}

// only returns the one armed timer.
func (ft *fakeTimers) only(t *testing.T) *fakeTimer {
	t.Helper()
	armed := ft.armed()
	if len(armed) != 1 {
		t.Fatalf("%d idle timers armed, want 1", len(armed))
	}
	return armed[0]
}

func (tm *fakeTimer) isStopped() bool {
	tm.ft.mu.Lock()
	defer tm.ft.mu.Unlock()
	return tm.stopped
}

// fire runs the timer's func as its expiry would, whether or not it was
// stopped: a real timer can expire just as it is stopped.
func (tm *fakeTimer) fire() {
	tm.ft.mu.Lock()
	tm.fired = true
	tm.ft.mu.Unlock()
	tm.f()
}

// idleManager is a Manager with a one-minute idle timeout on fake timers.
func idleManager() (*Manager, *fakeTimers) {
	ft := &fakeTimers{}
	return &Manager{IdleTimeout: time.Minute, afterFunc: ft.afterFunc}, ft
}

// served fetches segment n, which the harness worker w produces, and fails
// unless it is served.
func served(t *testing.T, m *Manager, h *harness, n int64, produce func()) {
	t.Helper()
	ch := segmentAsync(m, "k", n, h.ops())
	produce()
	if err := mustReturn(t, ch); err != nil {
		t.Fatalf("segment %d: %v", n, err)
	}
}

func waitVariants(t *testing.T, m *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for m.Variants() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d variants tracked, want %d", m.Variants(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func mustBeCanceled(t *testing.T, w *fakeWorker) {
	t.Helper()
	select {
	case <-w.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the worker was not canceled")
	}
}

func mustBeRunning(t *testing.T, w *fakeWorker) {
	t.Helper()
	select {
	case <-w.ctx.Done():
		t.Fatalf("the worker was canceled (%v)", context.Cause(w.ctx))
	default:
	}
}

// TestManagerIdleCancel: a worker nobody has fetched from for the idle timeout
// is canceled, with ErrIdle as the cause, and the variant's bookkeeping goes
// with its exit.
func TestManagerIdleCancel(t *testing.T) {
	h := newHarness()
	m, ft := idleManager()
	var w *fakeWorker
	served(t, m, h, 0, func() { w = h.worker(t, 0); w.produce(0) })
	tm := ft.only(t)
	if tm.d != time.Minute {
		t.Fatalf("idle timer armed for %v, want the idle timeout", tm.d)
	}
	tm.fire()
	mustBeCanceled(t, w)
	if cause := context.Cause(w.ctx); !errors.Is(cause, ErrIdle) {
		t.Errorf("cause %v, want ErrIdle", cause)
	}
	waitVariants(t, m, 0)
}

// TestManagerIdleDisarmedByWaiter: a fetch waiting on the worker disarms the
// timeout, which then cancels nothing even if it fires; the wait's end arms a
// fresh one.
func TestManagerIdleDisarmedByWaiter(t *testing.T) {
	h := newHarness()
	m, ft := idleManager()
	var w *fakeWorker
	served(t, m, h, 0, func() { w = h.worker(t, 0); w.produce(0) })
	tm := ft.only(t)

	ch := segmentAsync(m, "k", 1, h.ops())
	deadline := time.Now().Add(2 * time.Second)
	for !tm.isStopped() {
		if time.Now().After(deadline) {
			t.Fatal("a waiter did not disarm the idle timeout")
		}
		time.Sleep(time.Millisecond)
	}
	tm.fire()
	mustBeRunning(t, w)
	w.produce(1)
	if err := mustReturn(t, ch); err != nil {
		t.Fatal(err)
	}
	if again := ft.only(t); again == tm {
		t.Error("the wait's end did not arm a fresh timeout")
	}
	w.finish(nil)
}

// TestManagerTouchResetsIdle: a fetch served from the cache restarts the
// timeout from the touch, so a listener whose every fetch is a hit keeps the
// worker; the timer the touch replaced does nothing should it fire anyway. A
// touch with nothing armed, before the worker or after its cancel, arms
// nothing.
func TestManagerTouchResetsIdle(t *testing.T) {
	h := newHarness()
	h.manualExit = true
	m, ft := idleManager()
	m.Touch("k")
	if n := len(ft.armed()); n != 0 || m.Variants() != 0 {
		t.Fatalf("a touch of an unknown variant armed %d timers, tracks %d variants", n, m.Variants())
	}
	var w *fakeWorker
	served(t, m, h, 0, func() { w = h.worker(t, 0); w.produce(0) })
	first := ft.only(t)

	m.Touch("k")
	if !first.isStopped() {
		t.Error("the touch left the release's timer armed")
	}
	second := ft.only(t)
	if second == first || second.d != time.Minute {
		t.Fatalf("the touch armed %v for %v, want a fresh timer for the idle timeout", second, second.d)
	}
	first.fire()
	mustBeRunning(t, w)
	second.fire()
	mustBeCanceled(t, w)
	m.Touch("k")
	if n := len(ft.armed()); n != 0 {
		t.Errorf("a touch after the cancel armed %d timers for the dying worker", n)
	}
	w.finish(waxerr.Wrap(waxerr.CodeCanceled, "worker canceled", context.Cause(w.ctx)))
}

// TestManagerIdleWaitsForTheLastWaiter: the timeout arms only once nobody waits
// on the worker, not when the first of several waiters is served.
func TestManagerIdleWaitsForTheLastWaiter(t *testing.T) {
	h := newHarness()
	m, ft := idleManager()
	var w *fakeWorker
	served(t, m, h, 0, func() { w = h.worker(t, 0); w.produce(0) })
	ch1 := segmentAsync(m, "k", 1, h.ops())
	ch2 := segmentAsync(m, "k", 2, h.ops())
	mustBlock(t, ch1)
	mustBlock(t, ch2)
	w.produce(1)
	if err := mustReturn(t, ch1); err != nil {
		t.Fatal(err)
	}
	if n := len(ft.armed()); n != 0 {
		t.Fatalf("%d idle timers armed with a fetch still waiting", n)
	}
	w.produce(2)
	if err := mustReturn(t, ch2); err != nil {
		t.Fatal(err)
	}
	ft.only(t)
	w.finish(nil)
}

// TestManagerExitStopsIdle: a worker that ends on its own leaves no timeout
// armed.
func TestManagerExitStopsIdle(t *testing.T) {
	h := newHarness()
	m, ft := idleManager()
	var w *fakeWorker
	served(t, m, h, 0, func() { w = h.worker(t, 0); w.produce(0) })
	tm := ft.only(t)
	w.finish(nil)
	waitVariants(t, m, 0)
	if !tm.isStopped() {
		t.Error("the worker's exit left its idle timeout armed")
	}
	if err := m.Segment(context.Background(), "k", 0, h.ops()); err != nil {
		t.Fatal(err)
	}
	if n := len(ft.armed()); n != 0 {
		t.Errorf("a fetch served with no worker running armed %d timers", n)
	}
}

// TestManagerIdleCancelLeavesNoError: the cancel an idle timeout causes is no
// one's answer. The next fetch spawns afresh and is served, whether it comes
// after the canceled worker exited or while it is still on its way out.
func TestManagerIdleCancelLeavesNoError(t *testing.T) {
	for _, exited := range []bool{true, false} {
		name := "after the exit"
		if !exited {
			name = "before the exit"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.manualExit = !exited
			m, ft := idleManager()
			var w0 *fakeWorker
			served(t, m, h, 0, func() { w0 = h.worker(t, 0); w0.produce(0) })
			ft.only(t).fire()
			mustBeCanceled(t, w0)
			if exited {
				waitVariants(t, m, 0)
			}
			ch := segmentAsync(m, "k", 1, h.ops())
			if !exited {
				mustBlock(t, ch)
				w0.finish(waxerr.Wrap(waxerr.CodeCanceled, "worker canceled", context.Cause(w0.ctx)))
			}
			w1 := h.worker(t, 1)
			if w1.start != 1 {
				t.Fatalf("the next worker started at %d, want 1", w1.start)
			}
			w1.produce(1)
			if err := mustReturn(t, ch); err != nil {
				t.Fatalf("the fetch after an idle cancel = %v, want served", err)
			}
			w1.finish(nil)
		})
	}
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
// bookkeeping must drain once everyone leaves. A millisecond idle timeout on
// real timers races its cancels against all of it.
func TestManagerConcurrent(t *testing.T) {
	for _, idle := range []time.Duration{0, time.Millisecond} {
		t.Run(fmt.Sprintf("idle %v", idle), func(t *testing.T) { testManagerConcurrent(t, idle) })
	}
}

func testManagerConcurrent(t *testing.T, idle time.Duration) {
	const total = 40
	h := newHarness()
	h.autoTotal = total
	m := &Manager{WaitTimeout: 5 * time.Second, IdleTimeout: idle}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 8 {
				n := int64((g*7 + i*5) % total)
				if err := m.Segment(context.Background(), "k", n, h.ops()); err != nil {
					errs <- fmt.Errorf("segment %d: %w", n, err)
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

// BenchmarkTouchArmed is the hit path's cost with a worker running and its
// idle timeout armed: one lock, a timer stopped and another armed.
func BenchmarkTouchArmed(b *testing.B) {
	h := newHarness()
	m := &Manager{IdleTimeout: time.Minute}
	ch := segmentAsync(m, "k", 0, h.ops())
	w := h.worker(b, 0)
	w.produce(0)
	if err := <-ch; err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		m.Touch("k")
	}
	b.StopTimer()
	w.finish(nil)
}
