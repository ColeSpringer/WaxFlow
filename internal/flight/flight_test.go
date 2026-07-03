package flight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestSharesConcurrentCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		var calls atomic.Int32
		release := make(chan struct{})
		started := make(chan struct{})

		go g.Do("k", func() (int, error) {
			calls.Add(1)
			close(started)
			<-release
			return 42, nil
		})
		<-started

		const waiters = 8
		var wg sync.WaitGroup
		results := make([]int, waiters)
		for i := range waiters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, err := g.Do("k", func() (int, error) {
					calls.Add(1)
					return -1, nil
				})
				if err != nil {
					t.Errorf("waiter error: %v", err)
				}
				results[i] = v
			}()
		}
		// synctest: all waiters are durably blocked on the shared call
		// before the release, so sharing is deterministic, not scheduled.
		synctest.Wait()
		close(release)
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("fn executed %d times, want 1", got)
		}
		for i, v := range results {
			if v != 42 {
				t.Errorf("waiter %d got %d, want shared 42", i, v)
			}
		}
	})
}

func TestSequentialCallsRunAgain(t *testing.T) {
	var g Group[int]
	n := 0
	for range 3 {
		v, err := g.Do("k", func() (int, error) { n++; return n, nil })
		if err != nil || v != n {
			t.Fatalf("got %d, %v; want %d", v, err, n)
		}
	}
	if n != 3 {
		t.Fatalf("fn ran %d times, want 3 (suppression is not caching)", n)
	}
}

func TestDistinctKeysDoNotBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[string]
		blockA := make(chan struct{})
		startedA := make(chan struct{})
		go g.Do("a", func() (string, error) {
			close(startedA)
			<-blockA
			return "a", nil
		})
		<-startedA
		v, err := g.Do("b", func() (string, error) { return "b", nil })
		close(blockA)
		if err != nil || v != "b" {
			t.Fatalf("key b got %q, %v", v, err)
		}
	})
}

func TestErrorsShared(t *testing.T) {
	var g Group[int]
	want := errors.New("boom")
	_, err := g.Do("k", func() (int, error) { return 0, want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestPanicMarksWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g Group[int]
		started := make(chan struct{})
		proceed := make(chan struct{})
		go func() {
			defer func() { recover() }()
			g.Do("k", func() (int, error) {
				close(started)
				<-proceed
				panic("boom")
			})
		}()
		<-started
		errc := make(chan error, 1)
		go func() {
			_, err := g.Do("k", func() (int, error) { return 7, nil })
			errc <- err
		}()
		synctest.Wait() // waiter is durably parked on the shared call
		close(proceed)
		if err := <-errc; !errors.Is(err, ErrPanicked) {
			t.Fatalf("waiter got %v, want ErrPanicked", err)
		}
	})
}
