package cache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
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
	e.Fail(err)
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

// TestConcurrentAttachEvictComplete is the randomized concurrent-operation
// test the plan calls for on the flight/cache state machine: writers,
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
