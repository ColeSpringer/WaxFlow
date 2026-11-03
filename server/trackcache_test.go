package server

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
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
		c.put(strconv.Itoa(i), trackWithSamples(int64(i)), nil)
	}
	// Key "0" is the oldest-inserted. Touch it, so it is also the most
	// recently used: the two policies now disagree about it.
	if _, ok := c.get("0"); !ok {
		t.Fatal("key 0 missing before eviction")
	}
	// Insert at capacity, forcing one eviction.
	c.put("new", trackWithSamples(-1), nil)

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
	c.put("k", trackWithSamples(42), nil)
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
		c.put(strconv.Itoa(i), trackWithSamples(int64(i)), nil)
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
	// loaded, when set, is closed on the first Load: the first measure began.
	// hold, when set, blocks every Load until it is closed, which holds the
	// measure inside its OpenStream.
	loaded, hold chan struct{}
}

func (c *countingIdx) Load(container.Source) []byte {
	c.mu.Lock()
	c.loads++
	first := c.loads == 1
	c.mu.Unlock()
	if first && c.loaded != nil {
		close(c.loaded)
	}
	if c.hold != nil {
		<-c.hold
	}
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
	idx := &countingIdx{loaded: make(chan struct{})}
	return &Server{eng: waxflow.New(waxflow.WithIndexCache(idx))}, idx, roots
}

// TestTrackForLetsALeavingCallerGo: the shared measure runs on a context and
// a source handle of its own. The caller that started it leaves mid-measure,
// gets its own cancel at once and closes its file; the caller still waiting
// gets the measured track from the one measure, which is neither stopped nor
// run again.
func TestTrackForLetsALeavingCallerGo(t *testing.T) {
	s, idx, roots := trackForEnv(t, "a.mp3")
	idx.hold = make(chan struct{})
	release := sync.OnceFunc(func() { close(idx.hold) })
	defer release()
	srcA, err := roots.Resolve(context.Background(), "lib/a.mp3")
	if err != nil {
		t.Fatal(err)
	}
	srcB, err := roots.Resolve(context.Background(), "lib/a.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer srcB.Close()

	ctxA, cancelA := context.WithCancel(t.Context())
	errA := make(chan error, 1)
	go func() {
		_, err := s.trackFor(ctxA, srcA, false)
		srcA.Close() // as a handler's deferred close does
		errA <- err
	}()
	<-idx.loaded // A's measure is held inside its OpenStream; A owns the flight

	var got container.Track
	var errB error
	doneB := make(chan struct{})
	go func() {
		got, errB = s.trackFor(t.Context(), srcB, false)
		close(doneB)
	}()
	key := identityString(srcB.Ref, srcB.ID)
	for deadline := time.Now().Add(5 * time.Second); s.trackFlight.Waiting(key) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("B never joined A's flight")
		}
		time.Sleep(time.Millisecond)
	}
	cancelA()
	select {
	case err := <-errA:
		if waxerr.CodeOf(err) != waxerr.CodeCanceled {
			t.Errorf("the caller that left got %v, want its own cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a caller that left is held until the shared measure ends")
	}

	release()
	<-doneB
	if errB != nil {
		t.Fatalf("the waiting caller got %v, want the measured track", errB)
	}
	if got.Samples <= 0 {
		t.Errorf("the waiting caller got Samples = %d, want the measured length", got.Samples)
	}
	if n := idx.count(); n != 1 {
		t.Errorf("the measure ran %d times; a caller that left must not stop it for the one still waiting", n)
	}
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
			got[i], errs[i] = s.trackFor(t.Context(), src, false)
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
	if _, err := s.trackFor(t.Context(), src, false); err != nil {
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
			if _, err := s.trackFor(t.Context(), src, false); err != nil {
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

// trackForFile is trackForEnv for one fixture copied verbatim from anywhere in
// the tree, for the cells whose point is a format other than a bare MP3.
func trackForFile(t *testing.T, src, name string) (*Server, *source.File) {
	t.Helper()
	root := t.TempDir()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	f, err := roots.Resolve(context.Background(), "lib/"+name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &Server{eng: waxflow.New()}, f
}

// TestTrackForKeepsAnAdvisoryTotalForANonExactCaller pins the asymmetry
// between an absent length and a rounded one. A caller that did not ask for
// exact gets what the headers state, because two of them mean it: a split job
// bounds its cuts with the declared total on purpose, since SpanTrack bounds
// the run with the same number off a fresh open, and containerTagsFor wants no
// length at all. An absent length is still measured, since there is nothing
// there to hand back.
//
// The caller that cannot use a rounded total is the HLS playlist, and it asks
// for a measured one by name; see the cell below.
func TestTrackForKeepsAnAdvisoryTotalForANonExactCaller(t *testing.T) {
	for _, tc := range []struct{ src, name string }{
		{filepath.Join("..", "container", "mka", "testdata", "seed-opus.webm"), "seed.webm"},
		{filepath.Join("..", "testdata", "sine-s16.wma"), "sine.wma"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := trackForFile(t, tc.src, tc.name)
			track, err := s.trackFor(t.Context(), f, false)
			if err != nil {
				t.Fatal(err)
			}
			if !track.SamplesAdvisory {
				t.Fatalf("this cell needs a source whose headers only estimate; got exact=%v", track.SamplesExact)
			}
			// And the same source measures when asked, which is the playlist's
			// second call: the memo does not pin the estimate in place.
			exact, err := s.trackFor(t.Context(), f, true)
			if err != nil {
				t.Fatal(err)
			}
			if !exact.SamplesExact || exact.SamplesAdvisory || exact.Samples <= 0 {
				t.Errorf("exact trackFor returned %d (exact %v advisory %v)",
					exact.Samples, exact.SamplesExact, exact.SamplesAdvisory)
			}
		})
	}
}

// TestTimelineGateReadsTheHeadOfAWebMMember is the gate's own cost and its own
// verdict on a member whose length is a cluster walk away. The gate opens each
// member to ask whether measuring it is slow; the open is a head read, the
// answer is slow, and there is nothing to memoize because nothing was paid.
func TestTimelineGateReadsTheHeadOfAWebMMember(t *testing.T) {
	s, f := trackForFile(t, filepath.Join("..", "container", "mka", "testdata", "seed-opus.webm"), "seed.webm")
	if s.trackIsExact(f) {
		t.Fatal("the memo is warm before the gate ran")
	}
	needs, err := s.timelineNeedsJob([]*source.File{f})
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("a WebM Opus member whose exact total is a cluster walk away is a job")
	}
	if s.trackIsExact(f) {
		t.Error("the gate memoized an exact length it never measured")
	}
	// And the measure, when the job runs it, is a walk that settles.
	track, err := s.trackFor(t.Context(), f, true)
	if err != nil {
		t.Fatal(err)
	}
	if !track.SamplesExact || track.Samples <= 0 {
		t.Errorf("measured track = %d (exact %v), want the walk's count", track.Samples, track.SamplesExact)
	}
}

// TestTrackForCarriesTheWalksTrims: a measured Matroska track brings the walk's
// trim fields into the memo, not just its length. Everything below the memo
// plans from that track, and the two copy rungs read exactly these two fields
// (see container.Track.MidPadding).
func TestTrackForCarriesTheWalksTrims(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "midtrim.webm")
	want := MidTrimWebM(t, path, true)

	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	f, err := roots.Resolve(context.Background(), "lib/midtrim.webm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	s := &Server{eng: waxflow.New()}

	probe, err := s.trackFor(t.Context(), f, false)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Padding != 0 || probe.MidPadding != 0 {
		t.Fatalf("the unmeasured track already carries trims (%d/%d); no open walks",
			probe.Padding, probe.MidPadding)
	}

	track, err := s.trackFor(t.Context(), f, true)
	if err != nil {
		t.Fatal(err)
	}
	if track.Samples != want || !track.SamplesExact {
		t.Errorf("measured %d samples (exact %v), want %d", track.Samples, track.SamplesExact, want)
	}
	if track.MidPadding != MidTrimPad {
		t.Errorf("the memo carries MidPadding %d, want the walk's %d", track.MidPadding, MidTrimPad)
	}
	if track.Padding != MidTrimEndPad {
		t.Errorf("the memo carries Padding %d, want the final block's %d", track.Padding, MidTrimEndPad)
	}
}

// TestUntilOwnCancel: a measure canceled by another caller's leaving is run
// again for a caller still waiting, and one's own cancel ends the wait.
func TestUntilOwnCancel(t *testing.T) {
	canceled := waxerr.New(waxerr.CodeCanceled, "the flight's owner left")
	tries := 0
	try := func() (int, error) {
		if tries++; tries == 1 {
			return 0, canceled
		}
		return 7, nil
	}
	if v, err := untilOwnCancel(t.Context(), try); err != nil || v != 7 || tries != 2 {
		t.Errorf("got %d (%v) after %d tries, want 7 after the canceled one", v, err, tries)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tries = 0
	if _, err := untilOwnCancel(ctx, try); waxerr.CodeOf(err) != waxerr.CodeCanceled || tries != 1 {
		t.Errorf("%v after %d tries, want the caller's own cancel after one", err, tries)
	}
	failed := waxerr.New(waxerr.CodeSourceUnreadable, "read failed")
	tries = 0
	if _, err := untilOwnCancel(t.Context(), func() (int, error) { tries++; return 0, failed }); err != failed || tries != 1 {
		t.Errorf("%v after %d tries, want the failure itself after one", err, tries)
	}
}
