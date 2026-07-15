package server

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/source"
)

func trackWithSamples(n int64) container.Track {
	return container.Track{Samples: n, Default: true}
}

// TestTrackCacheEvictsLeastRecentlyUsed pins the policy, not just the bound.
// Oldest-inserted eviction would drop the hot entry here, which is the case
// that matters: a library-wide sweep must not push a live session's track out
// of the memo, because rebuilding one can cost a full decode.
func TestTrackCacheEvictsLeastRecentlyUsed(t *testing.T) {
	var c trackCache
	for i := range trackCacheCap {
		c.put(strconv.Itoa(i), trackWithSamples(int64(i)))
	}
	// Key "0" is the oldest-inserted. Touch it, so it is also the most
	// recently used: the two policies now disagree about it.
	if _, ok := c.get("0"); !ok {
		t.Fatal("key 0 missing before eviction")
	}
	// Insert at capacity, forcing one eviction.
	c.put("new", trackWithSamples(-1))

	if _, ok := c.get("0"); !ok {
		t.Error("evicted the most recently used entry: the policy is oldest-inserted, not LRU")
	}
	if _, ok := c.get("1"); ok {
		t.Error("key 1 survived; it was the least recently used and should have been evicted")
	}
	if len(c.entries) != trackCacheCap {
		t.Errorf("cache holds %d entries, want the %d cap", len(c.entries), trackCacheCap)
	}
}

func TestTrackCacheGetReturnsStoredTrack(t *testing.T) {
	var c trackCache
	if _, ok := c.get("absent"); ok {
		t.Error("zero-value cache reported a hit")
	}
	c.put("k", trackWithSamples(42))
	got, ok := c.get("k")
	if !ok {
		t.Fatal("miss after put")
	}
	if got.Samples != 42 {
		t.Errorf("Samples = %d, want 42", got.Samples)
	}
}

// TestTrackCacheBoundHolds is the cap's own guard: an unbounded memo keyed by
// identity grows once per file for the process's life, so a library sweep would
// pin the whole catalog.
func TestTrackCacheBoundHolds(t *testing.T) {
	var c trackCache
	// Enough past the cap to force many evictions, but not so far that the
	// linear evict scan (O(cap) per insert once full) dominates the suite.
	for i := range trackCacheCap + 500 {
		c.put(strconv.Itoa(i), trackWithSamples(int64(i)))
	}
	if len(c.entries) > trackCacheCap {
		t.Errorf("cache grew to %d entries past the %d cap", len(c.entries), trackCacheCap)
	}
}

// countingIdx counts stream opens through the engine's index cache. The engine
// consults it once per OpenStream of indexable media (an MP3 frame table), and
// inside trackFor the only thing that opens a stream is measureSamples. So Load
// calls count actual measure passes: the expensive work the flight exists to
// collapse.
type countingIdx struct {
	mu    sync.Mutex
	loads int
}

func (c *countingIdx) Load(container.Source) []byte {
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return nil
}
func (c *countingIdx) Save(container.Source, []byte) {}
func (c *countingIdx) Drop(container.Source)         {}

func (c *countingIdx) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

// trackForEnv builds a Server wired to a real engine over a root holding the
// named fixtures, plus the open-counting index cache.
//
// sine-untagged.mp3 is the fixture that matters: it has no Xing header, so its
// declared length is -1 and trackFor must measure it, which is a full decode.
// That is the cost a stampede multiplies.
func trackForEnv(t *testing.T, names ...string) (*Server, *countingIdx, *source.Roots) {
	t.Helper()
	root := t.TempDir()
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join("..", "testdata", "sine-untagged.mp3"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	idx := &countingIdx{}
	return &Server{eng: waxflow.New(waxflow.WithIndexCache(idx))}, idx, roots
}

// TestTrackForCollapsesConcurrentMisses drives the real trackFor: many
// concurrent callers for one cold source must produce exactly one measure pass.
//
// The scenario is a daemon restart with live sessions. The memo is in-memory so
// it is cold, and the resuming clients arrive as a burst of concurrent segment
// requests for one source; without the flight each would run its own probe and
// its own full decode.
//
// Counting through the engine's index cache is what makes this a test of
// trackFor rather than of a reimplementation of it: it observes the real
// function doing (or not doing) the real work.
func TestTrackForCollapsesConcurrentMisses(t *testing.T) {
	s, idx, roots := trackForEnv(t, "a.mp3")
	src, err := roots.Resolve(context.Background(), "lib/a.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	const goroutines = 32
	got := make([]container.Track, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together, so the miss is genuinely concurrent
			got[i], errs[i] = s.trackFor(src, false)
		}()
	}
	close(start)
	wg.Wait()

	if n := idx.count(); n != 1 {
		t.Errorf("measured %d times for %d concurrent callers on one source, want 1", n, goroutines)
	}
	for i := range goroutines {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got[i].Samples <= 0 {
			t.Errorf("caller %d got Samples = %d, want the measured length", i, got[i].Samples)
		}
		if got[i].Samples != got[0].Samples {
			t.Errorf("caller %d got Samples = %d, caller 0 got %d: every caller must see one result",
				i, got[i].Samples, got[0].Samples)
		}
	}

	// Warm: the memo now serves without measuring again.
	if _, err := s.trackFor(src, false); err != nil {
		t.Fatal(err)
	}
	if n := idx.count(); n != 1 {
		t.Errorf("a warm call measured again (%d total): the memo is not being consulted", n)
	}
}

// TestTrackForKeysBySource guards the over-correction: deduplicating on
// anything coarser than the identity would collapse distinct sources into one
// another's results, which is far worse than a duplicate probe.
func TestTrackForKeysBySource(t *testing.T) {
	s, idx, roots := trackForEnv(t, "a.mp3", "b.mp3")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, ref := range []string{"lib/a.mp3", "lib/b.mp3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src, err := roots.Resolve(context.Background(), ref)
			if err != nil {
				t.Error(err)
				return
			}
			defer src.Close()
			<-start
			if _, err := s.trackFor(src, false); err != nil {
				t.Errorf("%s: %v", ref, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Two distinct sources: two measures. One would mean a shared key served
	// one file's track for the other.
	if n := idx.count(); n != 2 {
		t.Errorf("measured %d times for 2 distinct sources, want 2", n)
	}
}
