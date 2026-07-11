package resolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	binerr "github.com/colespringer/waxbin/waxerr"

	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// fakeCatalog implements the catalog interface without SQLite so the
// cache, poll, and error-mapping logic test in microseconds.
type fakeCatalog struct {
	mu       sync.Mutex
	items    map[model.PID]*model.ItemView
	dv       int64
	rows     []model.Change
	pageSize int
	getErr   error
	pollErr  error
	hang     chan struct{}
	gets     int
	pulls    int
	closed   bool
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{items: make(map[model.PID]*model.ItemView), dv: 1}
}

func (f *fakeCatalog) Get(_ context.Context, pid model.PID) (*model.ItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	iv, ok := f.items[pid]
	if !ok {
		return nil, binerr.New(binerr.CodeNotFound, "fake.Get", "no such item")
	}
	return iv, nil
}

func (f *fakeCatalog) DataVersion(ctx context.Context) (int64, error) {
	f.mu.Lock()
	hang := f.hang
	pollErr := f.pollErr
	dv := f.dv
	f.mu.Unlock()
	if hang != nil {
		// A hung database: signal the test, then block until the query
		// context ends.
		select {
		case hang <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if pollErr != nil {
		return 0, pollErr
	}
	return dv, nil
}

// hangPolls makes subsequent DataVersion calls signal on the returned
// channel and block until their context ends.
func (f *fakeCatalog) hangPolls() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = make(chan struct{}, 1)
	return f.hang
}

func (f *fakeCatalog) Changes(_ context.Context, sinceSeq int64) ([]model.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	var out []model.Change
	for _, ch := range f.rows {
		if ch.Seq > sinceSeq {
			out = append(out, ch)
			if f.pageSize > 0 && len(out) == f.pageSize {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeCatalog) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeCatalog) setItem(pid, filePID model.PID, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[pid] = &model.ItemView{PID: pid, FilePID: filePID, Path: []byte(path)}
}

// commit appends change rows and bumps the data version, like a WaxBin
// write transaction.
func (f *fakeCatalog) commit(rows ...model.Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, rows...)
	f.dv++
}

// nextStub records delegated refs.
type nextStub struct{ refs []string }

func (n *nextStub) Resolve(ref string) (*source.File, error) {
	n.refs = append(n.refs, ref)
	return nil, waxerr.New(waxerr.CodeNotFound, "stub: "+ref)
}

// newTestCatalog wires a Catalog over the fake exactly as Open does,
// minus waxbin.Open and the background loop.
func newTestCatalog(t *testing.T, f *fakeCatalog, next source.Resolver, maxBytes int64) *Catalog {
	t.Helper()
	if next == nil {
		next, _ = source.OpenRoots(nil, 0)
	}
	if maxBytes <= 0 {
		maxBytes = source.DefaultMaxBytes
	}
	queryCtx, stopQueries := context.WithCancel(context.Background())
	c := &Catalog{
		lib:         f,
		next:        next,
		maxBytes:    maxBytes,
		log:         slog.New(slog.DiscardHandler),
		queryCtx:    queryCtx,
		stopQueries: stopQueries,
		entries:     make(map[model.PID]*entry),
		byFile:      make(map[model.PID]model.PID),
	}
	if err := c.initCursor(context.Background()); err != nil {
		t.Fatalf("initCursor: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantCode(t *testing.T, err error, code waxerr.Code) {
	t.Helper()
	if waxerr.CodeOf(err) != code {
		t.Fatalf("error = %v (code %s), want %s", err, waxerr.CodeOf(err), code)
	}
}

func TestResolveCachesAndDelegates(t *testing.T) {
	fake := newFakeCatalog()
	pid, filePID := model.NewPID(), model.NewPID()
	path := writeTemp(t, "track.wav", "wav bytes")
	fake.setItem(pid, filePID, path)
	next := &nextStub{}
	c := newTestCatalog(t, fake, next, 0)

	f, err := c.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer f.Close()
	if f.Ext != "wav" || f.ID.Size != int64(len("wav bytes")) {
		t.Fatalf("resolved file = ext %q size %d", f.Ext, f.ID.Size)
	}

	// Second resolve is a cache hit: no catalog query.
	f2, err := c.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatalf("cached Resolve: %v", err)
	}
	f2.Close()
	if fake.gets != 1 {
		t.Fatalf("catalog queries = %d, want 1 (second resolve must hit the cache)", fake.gets)
	}

	// Non-pid references delegate untouched.
	if _, err := c.Resolve("lib/album/track.flac"); err == nil {
		t.Fatal("stub next should refuse")
	}
	if len(next.refs) != 1 || next.refs[0] != "lib/album/track.flac" {
		t.Fatalf("delegated refs = %v", next.refs)
	}

	// Malformed and unknown pids map onto the envelope codes.
	_, err = c.Resolve("pid:not-a-ulid")
	wantCode(t, err, waxerr.CodeInvalidRequest)
	_, err = c.Resolve("pid:" + string(model.NewPID()))
	wantCode(t, err, waxerr.CodeNotFound)

	// A failing catalog is catalog-unavailable, never a silent 404.
	fake.mu.Lock()
	fake.getErr = errors.New("disk on fire")
	fake.mu.Unlock()
	_, err = c.Resolve("pid:" + string(model.NewPID()))
	wantCode(t, err, waxerr.CodeCatalogUnavailable)
}

func TestResolveSizeCap(t *testing.T) {
	fake := newFakeCatalog()
	pid := model.NewPID()
	path := writeTemp(t, "big.wav", "way more than eight bytes")
	fake.setItem(pid, model.NewPID(), path)
	c := newTestCatalog(t, fake, nil, 8)

	_, err := c.Resolve("pid:" + string(pid))
	wantCode(t, err, waxerr.CodePayloadTooLarge)
}

func TestStalePathSelfHeals(t *testing.T) {
	fake := newFakeCatalog()
	pid, filePID := model.NewPID(), model.NewPID()
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.wav")
	if err := os.WriteFile(oldPath, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.setItem(pid, filePID, oldPath)
	c := newTestCatalog(t, fake, nil, 0)

	f, err := c.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	f.Close()

	// The file moves and the catalog knows, but no poll has run: the
	// cached path is stale. Resolve must drop it and re-ask the catalog
	// instead of failing not-found.
	newPath := filepath.Join(dir, "new.wav")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	fake.setItem(pid, filePID, newPath)

	f, err = c.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatalf("Resolve after rename: %v", err)
	}
	f.Close()
	if fake.gets != 2 {
		t.Fatalf("catalog queries = %d, want 2 (one initial, one self-heal)", fake.gets)
	}
	if path, ok := c.cached(pid); !ok || path != newPath {
		t.Fatalf("cache after self-heal = %q, %v", path, ok)
	}
}

func TestPollInvalidation(t *testing.T) {
	fake := newFakeCatalog()
	dir := t.TempDir()
	type row struct{ pid, filePID model.PID }
	items := make([]row, 3)
	for i := range items {
		items[i] = row{model.NewPID(), model.NewPID()}
		path := filepath.Join(dir, fmt.Sprintf("t%d.wav", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		fake.setItem(items[i].pid, items[i].filePID, path)
	}
	c := newTestCatalog(t, fake, nil, 0)
	for _, it := range items {
		f, err := c.Resolve("pid:" + string(it.pid))
		if err != nil {
			t.Fatalf("warm Resolve: %v", err)
		}
		f.Close()
	}

	// An unchanged DataVersion is a no-op poll: no Changes pull.
	pullsBefore := fake.pulls
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("no-op Poll: %v", err)
	}
	if fake.pulls != pullsBefore {
		t.Fatal("no-op poll pulled the change feed")
	}

	// One item row, one file row (how renames surface), one row of an
	// entity type a path cache does not care about.
	fake.commit(
		model.Change{Seq: 1, EntityType: "item", EntityPID: items[0].pid, Op: model.OpUpdate},
		model.Change{Seq: 2, EntityType: "file", EntityPID: items[1].filePID, Op: model.OpUpdate},
		model.Change{Seq: 3, EntityType: "play_state", EntityPID: model.NewPID(), Op: model.OpUpdate},
	)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if _, ok := c.cached(items[0].pid); ok {
		t.Fatal("item change row did not drop the cached path")
	}
	if _, ok := c.cached(items[1].pid); ok {
		t.Fatal("file change row did not drop the cached path (rename signal)")
	}
	if _, ok := c.cached(items[2].pid); !ok {
		t.Fatal("unrelated change dropped a live entry")
	}
}

func TestPollPagesToTheTail(t *testing.T) {
	fake := newFakeCatalog()
	fake.pageSize = 2
	pid, filePID := model.NewPID(), model.NewPID()
	path := writeTemp(t, "t.wav", "x")
	fake.setItem(pid, filePID, path)
	c := newTestCatalog(t, fake, nil, 0)
	f, err := c.Resolve("pid:" + string(pid))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Five rows across three pages; the one that names our item sits on
	// the last page, so stopping early would miss it.
	rows := make([]model.Change, 5)
	for i := range rows {
		rows[i] = model.Change{Seq: int64(i + 1), EntityType: "album", EntityPID: model.NewPID(), Op: model.OpUpdate}
	}
	rows[4] = model.Change{Seq: 5, EntityType: "item", EntityPID: pid, Op: model.OpUpdate}
	fake.commit(rows...)

	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if _, ok := c.cached(pid); ok {
		t.Fatal("poll stopped before the last change page")
	}
	c.mu.Lock()
	seq := c.sinceSeq
	c.mu.Unlock()
	if seq != 5 {
		t.Fatalf("cursor = %d, want 5", seq)
	}
}

func TestInitCursorStartsAtTail(t *testing.T) {
	fake := newFakeCatalog()
	fake.pageSize = 2
	for i := 1; i <= 5; i++ {
		fake.rows = append(fake.rows, model.Change{Seq: int64(i), EntityType: "item", EntityPID: model.NewPID(), Op: model.OpCreate})
	}
	c := newTestCatalog(t, fake, nil, 0)
	c.mu.Lock()
	seq := c.sinceSeq
	c.mu.Unlock()
	if seq != 5 {
		t.Fatalf("cursor after open = %d, want the feed tail 5", seq)
	}
}

func TestCloseAbortsHungPoll(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCatalog(t, fake, nil, 0)
	polling := fake.hangPolls()
	c.stop = make(chan struct{})
	c.pollDone = make(chan struct{})
	go c.pollLoop(time.Millisecond)
	<-polling

	// Close must cancel the in-flight query via the catalog's query
	// context, not wait out queryTimeout behind a hung database.
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v waiting out a hung poll; want immediate abort", elapsed)
	}
}

func TestCacheEvictsOldest(t *testing.T) {
	fake := newFakeCatalog()
	c := newTestCatalog(t, fake, nil, 0)

	pids := make([]model.PID, maxEntries+1)
	for i := range pids {
		pids[i] = model.PID(fmt.Sprintf("%026d", i))
		c.store(pids[i], fmt.Sprintf("/x/%d", i), model.PID(fmt.Sprintf("F%025d", i)))
	}
	if len(c.entries) != maxEntries {
		t.Fatalf("entries = %d, want the %d bound", len(c.entries), maxEntries)
	}
	if _, ok := c.cached(pids[0]); ok {
		t.Fatal("oldest entry survived insert at capacity")
	}
	if _, ok := c.cached(pids[1]); !ok {
		t.Fatal("second-oldest entry evicted too")
	}
}
