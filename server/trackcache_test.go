package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
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

// TestMeasureLengthTakesTheCheapestRoute pins the three routes to an exact
// length, which differ only in what they cost: a decode is the last resort,
// not the first.
//
// The MP3 cell is also the correctness half. Its Xing count promises audio the
// truncated file does not hold, and the measure has to come back with the
// audio rather than the promise.
func TestMeasureLengthTakesTheCheapestRoute(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	cut := raw[:len(raw)-100]
	// The walk's own answer, which a tolerant consumer is the only one to get:
	// the file is damaged, so a strict probe of it refuses outright.
	tol, err := format.Open(container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tol.Close()
	if err := tol.(format.Walker).Walk(); err != nil {
		t.Fatal(err)
	}
	want := tol.Info().Default().Samples
	if want >= 22050 {
		t.Fatalf("the walk settled at %d; this cell needs a count short of the declared 22050", want)
	}

	med, err := format.Open(container.BytesSource(cut), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	got, err := measureLength(med)
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != want {
		t.Errorf("measured %d, the walk settles at %d", got.Samples, want)
	}

	// A Matroska Opus track takes the walk route: no open pays the cluster
	// walk, so the estimate the headers state has to be replaced by reading,
	// and the walk is the cheapest reading there is (no decode, no bisection).
	webm, err := os.ReadFile(filepath.Join("..", "container", "mka", "testdata", "seed-opus.webm"))
	if err != nil {
		t.Fatal(err)
	}
	cs := &testutil.CountingSource{Src: container.BytesSource(webm)}
	wm, err := format.Open(cs, "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wm.Close()
	opened := wm.Info().Default()
	if !opened.SamplesAdvisory {
		t.Fatalf("this cell needs an open that estimates; got %d (exact %v)", opened.Samples, opened.SamplesExact)
	}
	cs.Reset()
	n, err := measureLength(wm)
	if err != nil {
		t.Fatal(err)
	}
	strict, err := format.Probe(container.BytesSource(webm), "webm", &format.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := strict.Default().Samples; n.Samples != want {
		t.Errorf("measured %d, a strict probe walks to %d", n.Samples, want)
	}
	if cs.Reads == 0 {
		t.Error("the measure read nothing; the walk it is supposed to take reads the clusters")
	}
}

// TestMeasureAgreesWithTheDecodeOnARoundedContainer is the fourth route, and
// the reason it exists. ASF names its positions in milliseconds, so a seek past
// the end answers where the landing claims to be plus the frames decoded from
// there, and the rounding up to that landing lands in the total: sine-s16.wma
// comes back 33 samples under a read of the same file.
//
// Those 33 are the kind of wrong this measure exists to prevent. A timeline's
// prefix sum carries them into every member after it, a bounded slice drops
// them, and an HLS playlist promises a tail it will not serve. So a container
// whose own positions are rounded is decoded instead.
func TestMeasureAgreesWithTheDecodeOnARoundedContainer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wma"))
	if err != nil {
		t.Fatal(err)
	}
	open := func() format.Media {
		med, err := format.Open(container.BytesSource(raw), "wma", nil)
		if err != nil {
			t.Fatal(err)
		}
		return med
	}

	med := open()
	defer med.Close()
	if tr := med.Info().Default(); !tr.SamplesAdvisory {
		t.Fatalf("the fixture's length is not advisory (%+v); this cell needs a rounded container", tr)
	}
	got, err := measureLength(med)
	if err != nil {
		t.Fatal(err)
	}

	// The oracle is a read of the same file, which is what every consumer of
	// this number will get.
	lin := open()
	defer lin.Close()
	buf := audio.Get(lin.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var want int64
	for {
		err := lin.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		want += int64(buf.N)
	}
	if got.Samples != want {
		t.Errorf("measured %d, a read of the same file delivers %d", got.Samples, want)
	}

	// And the shortcut it declines is still wrong, so the route is doing work
	// rather than arriving at the same place by luck.
	sk := open()
	defer sk.Close()
	if landed, err := sk.SeekSample(measureCeiling); err == nil && landed == want {
		t.Errorf("a seek past the end answered %d too; this cell no longer pins anything", landed)
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
			track, err := s.trackFor(f, false)
			if err != nil {
				t.Fatal(err)
			}
			if !track.SamplesAdvisory {
				t.Fatalf("this cell needs a source whose headers only estimate; got exact=%v", track.SamplesExact)
			}
			// And the same source measures when asked, which is the playlist's
			// second call: the memo does not pin the estimate in place.
			exact, err := s.trackFor(f, true)
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
	track, err := s.trackFor(f, true)
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

	probe, err := s.trackFor(f, false)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Padding != 0 || probe.MidPadding != 0 {
		t.Fatalf("the unmeasured track already carries trims (%d/%d); no open walks",
			probe.Padding, probe.MidPadding)
	}

	track, err := s.trackFor(f, true)
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
