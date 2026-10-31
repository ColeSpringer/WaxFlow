package cache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testMeta(ext string) Meta {
	return Meta{Ref: "lib/a.flac", Identity: "10-20", Params: "format=wav", Ext: ext, ContentType: "audio/wav", Rate: 48000}
}

func openStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// pattern generates deterministic bytes so readers can verify stream
// integrity end to end.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 31)
	}
	return b
}

func TestNewKeyDiscriminates(t *testing.T) {
	base := NewKey("lib/a.flac|100-200", "format=wav&rate=48000", []string{"resample-hq-1", "pcm-1"})
	same := NewKey("lib/a.flac|100-200", "format=wav&rate=48000", []string{"resample-hq-1", "pcm-1"})
	if base != same {
		t.Fatal("identical inputs must derive identical keys")
	}
	variants := []Key{
		NewKey("lib/a.flac|100-201", "format=wav&rate=48000", []string{"resample-hq-1", "pcm-1"}),
		NewKey("lib/a.flac|100-200", "format=wav&rate=44100", []string{"resample-hq-1", "pcm-1"}),
		NewKey("lib/a.flac|100-200", "format=wav&rate=48000", []string{"resample-hq-2", "pcm-1"}),
	}
	for i, v := range variants {
		if v == base {
			t.Errorf("variant %d collided with base", i)
		}
	}
	if len(base) != 64 {
		t.Fatalf("key length %d, want 64 hex chars", len(base))
	}
}

func TestWriteThroughAndReadBehind(t *testing.T) {
	s := openStore(t, Options{})
	key := NewKey("id", "p", nil)
	e, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	want := pattern(300_000)

	// The reader attaches before a byte exists and follows the writer.
	r, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		got <- b
	}()

	for i := 0; i < len(want); i += 7000 {
		if _, err := e.Write(want[i:min(i+7000, len(want))]); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	if err := e.Complete(12345); err != nil {
		t.Fatal(err)
	}
	if b := <-got; !bytes.Equal(b, want) {
		t.Fatalf("read-behind stream mismatch: %d bytes, want %d", len(b), len(want))
	}
	r.Close()

	// The entry promoted: Lookup finds it with finalized meta.
	c := s.Lookup(key)
	if c == nil {
		t.Fatal("promoted entry missed")
	}
	defer c.File.Close()
	b, err := io.ReadAll(c.File)
	if err != nil || !bytes.Equal(b, want) {
		t.Fatalf("cached file mismatch (%v)", err)
	}
	if c.Meta.Bytes != int64(len(want)) || c.Meta.Samples != 12345 {
		t.Fatalf("meta = %+v", c.Meta)
	}
	if st := s.Stats(); st.Entries != 1 || st.Bytes != int64(len(want)) || st.Hits != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSecondReaderAttachesMidFlight(t *testing.T) {
	s := openStore(t, Options{})
	key := NewKey("id2", "p", nil)
	e, _ := s.Begin(key, testMeta("wav"))
	want := pattern(64_000)

	e.Write(want[:32_000])
	if got := s.InFlight(key); got != e {
		t.Fatal("InFlight must return the writing entry")
	}
	r1, _ := e.NewReader()
	half := make([]byte, 32_000)
	if _, err := io.ReadFull(r1, half); err != nil || !bytes.Equal(half, want[:32_000]) {
		t.Fatalf("first half: %v", err)
	}

	r2, _ := e.NewReader() // attaches later, still starts at offset zero
	e.Write(want[32_000:])
	e.Complete(1)

	rest, err := io.ReadAll(r1)
	if err != nil || !bytes.Equal(rest, want[32_000:]) {
		t.Fatalf("r1 tail: %v", err)
	}
	all, err := io.ReadAll(r2)
	if err != nil || !bytes.Equal(all, want) {
		t.Fatalf("r2 full stream: %v", err)
	}
	r1.Close()
	r2.Close()
}

func TestFailPropagatesAfterDrain(t *testing.T) {
	s := openStore(t, Options{})
	key := NewKey("id3", "p", nil)
	e, _ := s.Begin(key, testMeta("wav"))
	want := pattern(10_000)
	e.Write(want)
	r, _ := e.NewReader()
	boom := errors.New("pipeline exploded")
	e.Fail(boom)

	b := make([]byte, len(want))
	if _, err := io.ReadFull(r, b); err != nil || !bytes.Equal(b, want) {
		t.Fatalf("pre-failure bytes must drain: %v", err)
	}
	if _, err := r.Read(b); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the pipeline error", err)
	}
	r.Close()

	if s.Lookup(key) != nil {
		t.Fatal("failed entry must not promote")
	}
	if s.InFlight(key) != nil {
		t.Fatal("failed entry must unregister")
	}
}

// TestDropAbortedSparesASuccessorsTemp: a failed entry whose last reader
// leaves after a successor began on the same key removes its own temp file and
// nothing else, so the successor still promotes.
func TestDropAbortedSparesASuccessorsTemp(t *testing.T) {
	s := openStore(t, Options{})
	key := NewKey("id-successor", "p", nil)
	a, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	a.Fail(errors.New("pipeline exploded"))
	b, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // releases a

	if _, err := os.Stat(a.tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed entry's temp file survived its release: %v", err)
	}
	if _, err := os.Stat(b.tmpPath); err != nil {
		t.Fatalf("the predecessor's release deleted the successor's temp file: %v", err)
	}
	b.Write(pattern(1000))
	if err := b.Complete(10); err != nil {
		t.Fatal(err)
	}
	if b.Degraded() {
		t.Error("the successor degraded")
	}
	c := s.Lookup(key)
	if c == nil {
		t.Fatal("the successor did not promote")
	}
	c.File.Close()
}

// TestUnregisterSparesASuccessor: an entry that degraded (and so left the
// registry) and a successor began on its key; the predecessor's end must not
// drop the successor's registration, or the next request starts a duplicate.
func TestUnregisterSparesASuccessor(t *testing.T) {
	s := openStore(t, Options{})
	create := s.createFile
	s.createFile = func(path string) (entryFile, error) {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return nil, err
		}
		return &failAfter{f: f, limit: 100}, nil
	}
	key := NewKey("id-successor-registry", "p", nil)
	p, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	s.createFile = create
	mustWrite(t, p, pattern(1000))
	if !p.Degraded() || s.InFlight(key) != nil {
		t.Fatal("the predecessor did not degrade out of the registry")
	}
	succ, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	p.Fail(errors.New("pipeline exploded"))
	if got := s.InFlight(key); got != succ {
		t.Fatalf("after the predecessor failed, InFlight = %p, want the successor %p", got, succ)
	}
	succ.Fail(errors.New("cleanup"))
}

// TestBeginSurvivesAPredecessorsDirRemoval: a predecessor's release can empty
// and remove the key's directory after a successor's MkdirAll and before its
// create. The successor still begins on the cache, not on a ring.
func TestBeginSurvivesAPredecessorsDirRemoval(t *testing.T) {
	s := openStore(t, Options{})
	key := NewKey("id-dir-race", "p", nil)
	a, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	a.Fail(errors.New("pipeline exploded"))
	create := s.createFile
	s.createFile = func(path string) (entryFile, error) {
		r.Close() // releases a, which removes the now empty directory
		return create(path)
	}
	b, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatalf("a predecessor's release failed the successor's Begin: %v", err)
	}
	b.Write(pattern(1000))
	if err := b.Complete(10); err != nil || b.Degraded() {
		t.Fatalf("the successor did not promote cleanly (err %v, degraded %v)", err, b.Degraded())
	}
}

// failAfter is the disk-full seam: writes succeed until n bytes, then
// fail persistently.
type failAfter struct {
	f     *os.File
	limit int
	n     int
}

func (w *failAfter) Write(p []byte) (int, error) {
	if w.n >= w.limit {
		return 0, errors.New("injected: no space left on device")
	}
	take := min(len(p), w.limit-w.n)
	n, err := w.f.Write(p[:take])
	w.n += n
	if err == nil && take < len(p) {
		err = errors.New("injected: no space left on device")
	}
	return n, err
}

func (w *failAfter) ReadAt(p []byte, off int64) (int, error) { return w.f.ReadAt(p, off) }
func (w *failAfter) Close() error                            { return w.f.Close() }
func (w *failAfter) Sync() error                             { return w.f.Sync() }

// TestDegradationNeverKillsPlayback: a cache-volume write failure
// mid-stream downgrades the session to ring-fed streaming and every
// attached reader still receives the whole stream, byte-exact.
func TestDegradationNeverKillsPlayback(t *testing.T) {
	s := openStore(t, Options{RingBytes: 8192})
	s.createFile = func(path string) (entryFile, error) {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return nil, err
		}
		return &failAfter{f: f, limit: 10_000}, nil
	}
	key := NewKey("id4", "p", nil)
	e, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	want := pattern(200_000) // far beyond both the file limit and the ring

	var readers [2]*Reader
	results := make(chan []byte, len(readers))
	for i := range readers {
		r, err := e.NewReader()
		if err != nil {
			t.Fatal(err)
		}
		readers[i] = r
		go func() {
			b, _ := io.ReadAll(r)
			results <- b
		}()
	}

	go func() {
		for i := 0; i < len(want); i += 3000 {
			if _, err := e.Write(want[i:min(i+3000, len(want))]); err != nil {
				t.Errorf("write after degradation must keep working: %v", err)
				e.Fail(err)
				return
			}
		}
		e.Complete(7)
	}()

	for range readers {
		if b := <-results; !bytes.Equal(b, want) {
			t.Fatalf("degraded stream mismatch: got %d bytes, want %d", len(b), len(want))
		}
	}
	for _, r := range readers {
		r.Close()
	}

	if !e.Degraded() {
		t.Fatal("entry should report degradation")
	}
	if s.Lookup(key) != nil {
		t.Fatal("degraded entry must not promote")
	}
	// Degradation unregisters so new requests start fresh sessions.
	if s.InFlight(key) != nil {
		t.Fatal("degraded entry still registered")
	}
	// The temp file debris is gone once the last reader detached.
	matches, _ := filepath.Glob(filepath.Join(s.entryDir(key), "out.*"))
	if len(matches) != 0 {
		t.Fatalf("leftover debris: %v", matches)
	}
}

func TestMemEntrySyncOneShot(t *testing.T) {
	e := NewMemEntry(4096, testMeta("wav"))
	r, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	want := pattern(50_000) // multiple ring generations
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		got <- b
	}()
	for i := 0; i < len(want); i += 1234 {
		if _, err := e.Write(want[i:min(i+1234, len(want))]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	e.Complete(1)
	if b := <-got; !bytes.Equal(b, want) {
		t.Fatal("mem entry stream mismatch")
	}
	r.Close()
}

func TestAbandonedRingStopsWriter(t *testing.T) {
	e := NewMemEntry(1024, testMeta("wav"))
	abandons := 0
	e.OnAbandon(func() { abandons++ })
	r, _ := e.NewReader()
	buf := pattern(600)
	if _, err := e.Write(buf); err != nil {
		t.Fatal(err)
	}
	r.Close() // client went away

	// The ring fills, then the writer learns everyone is gone.
	var err error
	for range 10 {
		if _, err = e.Write(buf); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrAbandoned) {
		t.Fatalf("got %v, want ErrAbandoned", err)
	}
	// Failed by the time the writer hears it, so nothing can attach between
	// the writer's return and its Fail.
	if !errors.Is(e.Err(), ErrAbandoned) {
		t.Errorf("Err() = %v once the writer was told, want ErrAbandoned", e.Err())
	}
	if _, err := e.NewReader(); err == nil {
		t.Error("a reader attached to the abandoned ring")
	}
	if abandons != 1 {
		t.Errorf("onAbandon ran %d times, want 1", abandons)
	}
	e.Fail(err)
}

// fakeTimers is the store's abandon timer seam: a timer fires only when the
// test fires it.
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
		t.Fatalf("%d abandon timers armed, want 1", len(armed))
	}
	return armed[0]
}

// fire runs the timer's func as its expiry would, stopped or not: a real
// timer can expire just as it is stopped.
func (tm *fakeTimer) fire() {
	tm.ft.mu.Lock()
	tm.fired = true
	tm.ft.mu.Unlock()
	tm.f()
}

// graceStore is a store whose entries abandon after grace, on timers the test
// fires by hand.
func graceStore(t *testing.T, grace time.Duration) (*Store, *fakeTimers) {
	t.Helper()
	s := openStore(t, Options{AbandonAfter: grace})
	ft := &fakeTimers{}
	s.afterFunc = ft.afterFunc
	return s, ft
}

func mustWrite(t *testing.T, e *Entry, p []byte) {
	t.Helper()
	if _, err := e.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestFileEntryAbandonsAfterGrace: a grace after the last reader leaves, the
// entry abandons: its onAbandon runs at that moment (whatever the writer is
// doing), nothing new attaches, and the writer's next write fails and takes
// the files with it. A ring that has not filled abandons the same way.
func TestFileEntryAbandonsAfterGrace(t *testing.T) {
	for _, degrade := range []bool{false, true} {
		name := "on the cache"
		if degrade {
			name = "degraded to the ring"
		}
		t.Run(name, func(t *testing.T) {
			const grace = 5 * time.Second
			s, ft := graceStore(t, grace)
			if degrade {
				s.createFile = func(path string) (entryFile, error) {
					f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
					if err != nil {
						return nil, err
					}
					return &failAfter{f: f, limit: 1000}, nil
				}
			}
			key := NewKey("id-abandon", "p", nil)
			e, err := s.Begin(key, testMeta("wav"))
			if err != nil {
				t.Fatal(err)
			}
			abandons := 0
			e.OnAbandon(func() { abandons++ })
			r, err := e.NewReader()
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, e, pattern(2000))
			if e.Degraded() != degrade {
				t.Fatalf("degraded = %v, want %v", e.Degraded(), degrade)
			}
			r.Close()

			tm := ft.only(t)
			if tm.d != grace {
				t.Fatalf("abandon timer armed for %v, want the grace", tm.d)
			}
			mustWrite(t, e, pattern(100))
			tm.fire()
			if abandons != 1 {
				t.Errorf("onAbandon ran %d times at the abandon, want 1", abandons)
			}
			if !errors.Is(e.Err(), ErrAbandoned) {
				t.Errorf("Err() = %v, want ErrAbandoned", e.Err())
			}
			if s.InFlight(key) != nil {
				t.Error("the abandoned entry is still registered for joiners")
			}
			if _, err := e.NewReader(); err == nil {
				t.Error("a reader attached to the abandoned entry")
			}
			if _, err := e.Write(pattern(100)); !errors.Is(err, ErrAbandoned) {
				t.Fatalf("the writer's next write = %v, want ErrAbandoned", err)
			}
			if _, err := os.Stat(e.tmpPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the abandoned entry's temp file survived: %v", err)
			}
			if _, err := os.Stat(e.dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the abandoned entry's directory survived: %v", err)
			}
			if abandons != 1 {
				t.Errorf("onAbandon ran %d times in all, want 1", abandons)
			}
		})
	}
}

// TestFileEntryReattachWithinGraceKeepsWriting: a reader attaching inside the
// grace disarms the abandon, so the timeout firing late changes nothing; the
// last reader leaving again starts a fresh grace; the entry promotes whole.
func TestFileEntryReattachWithinGraceKeepsWriting(t *testing.T) {
	const grace = 5 * time.Second
	s, ft := graceStore(t, grace)
	key := NewKey("id-reattach", "p", nil)
	e, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	want := pattern(4000)
	r1, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, e, want[:1000])
	r1.Close()
	first := ft.only(t)

	r2, err := e.NewReader()
	if err != nil {
		t.Fatalf("a reader inside the grace could not attach: %v", err)
	}
	if n := len(ft.armed()); n != 0 {
		t.Fatalf("%d abandon timers armed with a reader attached", n)
	}
	first.fire()
	mustWrite(t, e, want[1000:2000])
	r2.Close()
	if again := ft.only(t); again == first {
		t.Fatal("the second leave did not start a fresh grace")
	}
	mustWrite(t, e, want[2000:3000])

	r3, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	mustWrite(t, e, want[3000:])
	if err := e.Complete(1); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r3); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the reattached reader read %d bytes (%v), want the %d written", len(got), err, len(want))
	}
	c := s.Lookup(key)
	if c == nil {
		t.Fatal("the entry did not promote")
	}
	c.File.Close()
}

// TestFileEntryNeverAbandonsWithoutGrace: a zero grace keeps today's rule, an
// encode runs on into the cache however long its readers have been gone.
func TestFileEntryNeverAbandonsWithoutGrace(t *testing.T) {
	s, ft := graceStore(t, 0)
	key := NewKey("id-no-grace", "p", nil)
	e, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, e, pattern(1000))
	r.Close()
	if n := len(ft.armed()); n != 0 {
		t.Fatalf("%d abandon timers armed with no grace", n)
	}
	mustWrite(t, e, pattern(1000))
	if err := e.Complete(1); err != nil {
		t.Fatal(err)
	}
	c := s.Lookup(key)
	if c == nil {
		t.Fatal("the entry did not promote")
	}
	c.File.Close()
}

// TestFileEntryNeverAbandonsBeforeFirstReader: the grace runs from a reader
// leaving, so a pipeline writing before its first reader attaches keeps going.
func TestFileEntryNeverAbandonsBeforeFirstReader(t *testing.T) {
	s, ft := graceStore(t, 5*time.Second)
	key := NewKey("id-unread", "p", nil)
	e, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	want := pattern(2000)
	mustWrite(t, e, want[:1000])
	if n := len(ft.armed()); n != 0 {
		t.Fatalf("%d abandon timers armed before any reader", n)
	}
	r, err := e.NewReader()
	if err != nil {
		t.Fatalf("the first reader could not attach: %v", err)
	}
	defer r.Close()
	mustWrite(t, e, want[1000:])
	if err := e.Complete(1); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the first reader read %d bytes (%v), want the %d written", len(got), err, len(want))
	}
}

// heldFile is an entry file whose first write waits for the test, so an
// abandon can land while the writer is inside it.
type heldFile struct {
	*os.File
	entered, release chan struct{}
	held             sync.Once
	closed           atomic.Bool
}

func (f *heldFile) Write(p []byte) (int, error) {
	f.held.Do(func() {
		close(f.entered)
		<-f.release
	})
	return f.File.Write(p)
}

func (f *heldFile) Close() error {
	f.closed.Store(true)
	return f.File.Close()
}

// TestAbandonLeavesTheFileToTheWriter: an abandon that lands while the writer
// is inside its file write, the entry lock dropped, closes nothing under it;
// the writer's next call fails the entry and closes the file.
func TestAbandonLeavesTheFileToTheWriter(t *testing.T) {
	s, ft := graceStore(t, 5*time.Second)
	hf := &heldFile{entered: make(chan struct{}), release: make(chan struct{})}
	s.createFile = func(path string) (entryFile, error) {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		hf.File = f
		return hf, err
	}
	e, err := s.Begin(NewKey("id-mid-write", "p", nil), testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	wrote := make(chan error, 1)
	go func() {
		_, err := e.Write(pattern(100))
		wrote <- err
	}()
	<-hf.entered
	ft.only(t).fire()
	if hf.closed.Load() {
		t.Fatal("the abandon closed the file under the writer")
	}
	close(hf.release)
	if err := <-wrote; err != nil {
		t.Fatalf("the write the abandon landed in = %v, want it to finish", err)
	}
	if _, err := e.Write(pattern(100)); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("the writer's next write = %v, want ErrAbandoned", err)
	}
	if !hf.closed.Load() {
		t.Error("the writer's failing call left the file open")
	}
}

// TestAbandonedEntryCompletesNothing: an encode that ends after its entry
// abandoned does not promote.
func TestAbandonedEntryCompletesNothing(t *testing.T) {
	s, ft := graceStore(t, 5*time.Second)
	key := NewKey("id-late-complete", "p", nil)
	e, err := s.Begin(key, testMeta("wav"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, e, pattern(1000))
	r.Close()
	ft.only(t).fire()
	if err := e.Complete(1); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("Complete after the abandon = %v, want ErrAbandoned", err)
	}
	if s.Lookup(key) != nil {
		t.Fatal("an abandoned entry promoted")
	}
	if _, err := os.Stat(e.tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the abandoned entry's temp file survived: %v", err)
	}
}

func TestReaderCloseUnblocksRead(t *testing.T) {
	e := NewMemEntry(1024, testMeta("wav"))
	r, _ := e.NewReader()
	done := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 10))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the read park
	r.Close()
	select {
	case err := <-done:
		if !errors.Is(err, errClosedReader) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock on Close")
	}
	e.Fail(errors.New("cleanup"))
}

func TestEvictionLRUAndAge(t *testing.T) {
	s := openStore(t, Options{MaxBytes: 25_000})
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }

	writeEntry := func(name string, size int) Key {
		key := NewKey(name, "p", nil)
		e, err := s.Begin(key, testMeta("wav"))
		if err != nil {
			t.Fatal(err)
		}
		e.Write(pattern(size))
		if err := e.Complete(1); err != nil {
			t.Fatal(err)
		}
		return key
	}

	k1 := writeEntry("one", 10_000)
	now = now.Add(time.Minute)
	k2 := writeEntry("two", 10_000)
	now = now.Add(time.Minute)
	// Touch k1 so k2 becomes the LRU victim.
	if s.Lookup(k1) == nil {
		t.Fatal("k1 missed")
	}
	now = now.Add(time.Minute)
	k3 := writeEntry("three", 10_000) // 30k > 25k budget: evicts k2

	if s.Lookup(k2) != nil {
		t.Fatal("k2 (least recently used) should have been evicted")
	}
	if s.Lookup(k1) == nil || s.Lookup(k3) == nil {
		t.Fatal("k1/k3 should have survived")
	}
	if st := s.Stats(); st.Entries != 2 || st.Bytes != 20_000 {
		t.Fatalf("stats after budget eviction: %+v", st)
	}

	// Age-based eviction through GC.
	s.maxAge = time.Hour
	now = now.Add(2 * time.Hour)
	removed, freed := s.GC()
	if removed != 2 || freed != 20_000 {
		t.Fatalf("age gc removed %d freed %d", removed, freed)
	}
	if st := s.Stats(); st.Entries != 0 || st.Bytes != 0 {
		t.Fatalf("stats after age gc: %+v", st)
	}
}

func TestBootScanRebuildsIndexAndDropsPartials(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	key := NewKey("boot", "p", nil)
	e, _ := s.Begin(key, testMeta("wav"))
	want := pattern(5000)
	e.Write(want)
	e.Complete(9)

	// A crashed partial: entry dir with a tmp file and no meta.json.
	crashed := filepath.Join(s.versionDir(), "zz", "zzdeadbeef")
	os.MkdirAll(crashed, 0o755)
	os.WriteFile(filepath.Join(crashed, "out.wav.tmp-1"), []byte("junk"), 0o644)
	s.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	c := s2.Lookup(key)
	if c == nil {
		t.Fatal("rebooted store lost the completed entry")
	}
	c.File.Close()
	if c.Meta.Samples != 9 || c.Meta.Bytes != 5000 {
		t.Fatalf("rebooted meta = %+v", c.Meta)
	}
	if _, err := os.Stat(crashed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned partial survived the boot scan")
	}
	if st := s2.Stats(); st.Entries != 1 || st.Bytes != 5000 {
		t.Fatalf("rebooted stats: %+v", st)
	}
}

// TestBootScanPrunesTempsBesideACompletedEntry: a temp file left beside a
// promoted entry (a runtime removal that failed) goes at the next boot, since
// nothing is in flight then; the entry stays.
func TestBootScanPrunesTempsBesideACompletedEntry(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	key := NewKey("boot-orphan", "p", nil)
	e, _ := s.Begin(key, testMeta("wav"))
	e.Write(pattern(5000))
	e.Complete(9)
	orphan := filepath.Join(s.entryDir(key), "out.wav.tmp-77")
	if err := os.WriteFile(orphan, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the boot scan kept a temp file beside a completed entry: %v", err)
	}
	c := s2.Lookup(key)
	if c == nil {
		t.Fatal("the boot scan lost the completed entry")
	}
	c.File.Close()
}

// TestConcurrentAttachEvictComplete is a randomized concurrent-operation
// test of the flight/cache state machine: writers,
// attaching readers, lookups, and gc racing under -race, asserting stream
// integrity throughout.
func TestConcurrentAttachEvictComplete(t *testing.T) {
	s := openStore(t, Options{MaxBytes: 60_000})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := NewKey(fmt.Sprintf("conc-%d", i%4), fmt.Sprintf("run-%d", i), nil)
			want := pattern(20_000 + i*1000)

			if c := s.Lookup(key); c != nil {
				b, err := io.ReadAll(c.File)
				c.File.Close()
				if err != nil || !bytes.Equal(b, want) {
					t.Errorf("cached bytes mismatch for %s", key[:8])
				}
				return
			}
			var e *Entry
			if e = s.InFlight(key); e == nil {
				var err error
				if e, err = s.Begin(key, testMeta("wav")); err != nil {
					return // lost a Begin race; fine
				}
				go func() {
					for o := 0; o < len(want); o += 4096 {
						if _, err := e.Write(want[o:min(o+4096, len(want))]); err != nil {
							e.Fail(err)
							return
						}
					}
					e.Complete(int64(i))
				}()
			}
			r, err := e.NewReader()
			if err != nil {
				return
			}
			defer r.Close()
			b, err := io.ReadAll(r)
			if err == nil && !bytes.Equal(b, want) {
				t.Errorf("streamed bytes mismatch for %s", key[:8])
			}
		}()
	}
	wg.Wait()
	s.GC()
}
