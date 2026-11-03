package flight

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// TestJoinOutlivesALeavingCaller: the shared call runs on a context of its
// own, so a caller that leaves gets its own cancel while the call goes on for
// the caller still waiting, who gets the result; fn ran once.
func TestJoinOutlivesALeavingCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		var calls atomic.Int32
		release := make(chan struct{})
		started := make(chan struct{})
		var fnCtx context.Context
		fn := func(ctx context.Context) (int, error) {
			calls.Add(1)
			fnCtx = ctx
			close(started)
			select {
			case <-release:
				return 42, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		ctxA, cancelA := context.WithCancel(context.Background())
		var errA error
		go func() { _, errA = g.Join(ctxA, "k", fn) }()
		<-started
		var gotB int
		var errB error
		doneB := make(chan struct{})
		go func() {
			gotB, errB = g.Join(context.Background(), "k", fn)
			close(doneB)
		}()
		synctest.Wait()
		cancelA()
		synctest.Wait()
		if !errors.Is(errA, context.Canceled) {
			t.Errorf("the caller that left got %v, want its own cancel", errA)
		}
		if fnCtx.Err() != nil {
			t.Error("the shared call's context ended with a caller that left while another still waits")
		}
		close(release)
		<-doneB
		if errB != nil || gotB != 42 {
			t.Errorf("the waiting caller got %d, %v; want the shared 42", gotB, errB)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("fn ran %d times, want once", n)
		}
	})
}

// TestJoinEndsWithItsLastCaller: when every caller has left, the shared
// call's context ends, and the next caller runs fn again.
func TestJoinEndsWithItsLastCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		var calls atomic.Int32
		var ended atomic.Bool
		fn := func(ctx context.Context) (int, error) {
			calls.Add(1)
			<-ctx.Done()
			ended.Store(true)
			return 0, ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		var err error
		done := make(chan struct{})
		go func() {
			_, err = g.Join(ctx, "k", fn)
			close(done)
		}()
		synctest.Wait()
		cancel()
		<-done
		synctest.Wait()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Join returned %v, want the caller's cancel", err)
		}
		if !ended.Load() {
			t.Error("the shared call's context did not end when its last caller left")
		}
		if _, err := g.Join(context.Background(), "k", func(context.Context) (int, error) { calls.Add(1); return 7, nil }); err != nil {
			t.Fatal(err)
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("fn ran %d times across two separate calls, want 2", n)
		}
	})
}

// TestJoinRecoversAPanic: a panic in the shared call reaches every waiter as
// ErrPanicked rather than taking the process down.
func TestJoinRecoversAPanic(t *testing.T) {
	var g Group[int]
	_, err := g.Join(context.Background(), "k", func(context.Context) (int, error) { panic("boom") })
	if !errors.Is(err, ErrPanicked) {
		t.Errorf("Join returned %v, want ErrPanicked", err)
	}
}

// TestJoinLeavesADeadCall: a call every caller has left is over, whatever it
// is still doing; a new caller starts a call of its own and the old one's
// cleanup does not take the new entry with it.
func TestJoinLeavesADeadCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		release := make(chan struct{})
		var calls atomic.Int32
		fn := func(ctx context.Context) (int, error) {
			n := calls.Add(1)
			if n == 1 {
				<-ctx.Done()
				<-release // the dead call lingers past its cancel
				return 0, ctx.Err()
			}
			return int(n), nil
		}
		ctxA, cancelA := context.WithCancel(context.Background())
		doneA := make(chan struct{})
		go func() {
			g.Join(ctxA, "k", fn)
			close(doneA)
		}()
		synctest.Wait()
		cancelA()
		<-doneA
		got, err := g.Join(context.Background(), "k", fn)
		if err != nil || got != 2 {
			t.Fatalf("a caller after the dead call got %d, %v; want a call of its own", got, err)
		}
		if g.Waiting("k") != 0 {
			t.Errorf("Waiting(k) = %d after the new call returned", g.Waiting("k"))
		}
		close(release)
		synctest.Wait()
		// The dead call's cleanup must not have removed a newer entry: a
		// third caller shares the call in flight, if any, or runs its own.
		if got, err := g.Join(context.Background(), "k", fn); err != nil || got != 3 {
			t.Errorf("after the dead call ended, Join got %d, %v; want a fresh call", got, err)
		}
	})
}

// TestJoinReportsItsWaiters: Waiting counts the callers blocked on a key.
func TestJoinReportsItsWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		release := make(chan struct{})
		fn := func(context.Context) (int, error) { <-release; return 1, nil }
		for range 3 {
			go g.Join(context.Background(), "k", fn)
		}
		synctest.Wait()
		if n := g.Waiting("k"); n != 3 {
			t.Errorf("Waiting(k) = %d with three callers blocked, want 3", n)
		}
		if n := g.Waiting("other"); n != 0 {
			t.Errorf("Waiting(other) = %d, want 0", n)
		}
		close(release)
		synctest.Wait()
		if n := g.Waiting("k"); n != 0 {
			t.Errorf("Waiting(k) = %d after the call returned, want 0", n)
		}
	})
}

// TestJoinKeepsThePanicsStack: the error a panic becomes names where it
// happened.
func TestJoinKeepsThePanicsStack(t *testing.T) {
	var g Group[int]
	_, err := g.Join(context.Background(), "k", func(context.Context) (int, error) { panic("boom") })
	if !errors.Is(err, ErrPanicked) || !strings.Contains(err.Error(), "join_test.go") {
		t.Errorf("Join returned %v, want ErrPanicked carrying the panic's stack", err)
	}
}

// TestJoinOnADoCall: a Join caller that attaches to a call Do started and then
// leaves gets its own cancel and leaves the call alone.
func TestJoinOnADoCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		release := make(chan struct{})
		go g.Do("k", func() (int, error) { <-release; return 1, nil })
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		var err error
		done := make(chan struct{})
		go func() {
			_, err = g.Join(ctx, "k", func(context.Context) (int, error) { return 2, nil })
			close(done)
		}()
		synctest.Wait()
		cancel()
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Join on a Do call returned %v after its cancel, want the cancel", err)
		}
		close(release)
		synctest.Wait()
	})
}

// TestJoinHoldingHoldsForTheCallsLife: hold runs in the caller's goroutine
// when its call starts, before JoinHolding returns, and is released when the
// call ends; a caller that joins the call in flight holds nothing.
func TestJoinHoldingHoldsForTheCallsLife(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		release := make(chan struct{})
		var held, released atomic.Int32
		hold := func() func() {
			held.Add(1)
			return func() { released.Add(1) }
		}
		fn := func(context.Context) (int, error) {
			if held.Load() != 1 || released.Load() != 0 {
				t.Errorf("fn ran with held=%d released=%d, want the hold taken and not released", held.Load(), released.Load())
			}
			<-release
			return 7, nil
		}
		ctxA, cancelA := context.WithCancel(context.Background())
		doneA := make(chan struct{})
		go func() {
			g.JoinHolding(ctxA, "k", hold, fn)
			close(doneA)
		}()
		synctest.Wait()
		if held.Load() != 1 {
			t.Fatalf("held = %d after the call started, want 1", held.Load())
		}
		doneB := make(chan struct{})
		var gotB int
		go func() {
			gotB, _ = g.JoinHolding(context.Background(), "k", hold, fn)
			close(doneB)
		}()
		synctest.Wait()
		if held.Load() != 1 {
			t.Errorf("held = %d after a second caller joined, want 1: joining holds nothing", held.Load())
		}
		cancelA()
		<-doneA
		if released.Load() != 0 {
			t.Error("the hold was released when a caller left while the call ran")
		}
		close(release)
		<-doneB
		if gotB != 7 || released.Load() != 1 {
			t.Errorf("after the call ended: value %d, released %d; want 7 and 1", gotB, released.Load())
		}
	})
}
