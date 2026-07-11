// Package resolver serves pid:<ULID> source references from a WaxBin
// catalog. It is the nested module behind the waxflow-waxbin flavor:
// the main module's tree stays free of WaxBin's SQLite dependency,
// while this module implements source.Resolver over a read-only
// waxbin.Open, with a PID-to-path cache invalidated by polling the
// catalog's change feed (a cheap DataVersion read, then Changes rows).
//
// The resolved file's identity stays size+mtimeNS, exactly as in path
// mode: the PID rides inside the reference, which signed URLs and cache
// keys already cover, and catalog state deliberately never enters the
// identity, because a rename must not kill URLs or cache entries for
// unchanged bytes.
package resolver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	binerr "github.com/colespringer/waxbin/waxerr"

	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultPollInterval is the DataVersion poll cadence: cheap (one pragma
// read per tick) and short enough that a library reorganization is
// picked up before anyone notices stale paths.
const DefaultPollInterval = 5 * time.Second

const (
	// maxEntries bounds the PID-to-path cache; oldest-out beyond it.
	maxEntries = 4096
	// queryTimeout bounds every catalog query issued without a caller
	// context (Resolve has none; the interface is Resolve(ref)).
	queryTimeout = 10 * time.Second
)

// catalog is the slice of *waxbin.Library the resolver consumes,
// narrowed so unit tests can fake the catalog without SQLite.
type catalog interface {
	Get(ctx context.Context, pid model.PID) (*model.ItemView, error)
	DataVersion(ctx context.Context) (int64, error)
	Changes(ctx context.Context, sinceSeq int64) ([]model.Change, error)
	Close() error
}

// Options configures Open.
type Options struct {
	// DBPath is the WaxBin catalog database, opened read-only. The file
	// must already exist: WaxBin creates it, so start WaxBin first.
	DBPath string

	// Next serves every non-pid reference (the configured roots). Nil
	// resolves them all not-found.
	Next source.Resolver

	// MaxBytes caps each resolved file, like the roots cap theirs; 0
	// means source.DefaultMaxBytes.
	MaxBytes int64

	// PollInterval is the background DataVersion poll cadence; 0 means
	// DefaultPollInterval, negative disables the background loop (tests
	// drive Poll directly).
	PollInterval time.Duration

	// Logger, nil discards.
	Logger *slog.Logger
}

// Catalog resolves pid:<ULID> references against a WaxBin catalog and
// delegates everything else. It is safe for concurrent use.
type Catalog struct {
	lib      catalog
	next     source.Resolver
	maxBytes int64
	log      *slog.Logger

	// queryCtx parents every catalog query (Resolve lookups and poll
	// pulls), so Close aborts in-flight work immediately instead of
	// waiting out queryTimeout on a hung database.
	queryCtx    context.Context
	stopQueries context.CancelFunc

	// pollMu serializes poll cycles (the background loop and direct
	// Poll calls); mu guards the cache maps and the change cursor.
	pollMu sync.Mutex
	mu     sync.Mutex
	// entries caches item PID -> resolved path; byFile maps the path's
	// file PID back to the items resolving through it, because renames
	// and moves arrive on the change feed as file-entity rows carrying
	// the file PID. The value is a set: nothing in the catalog contract
	// promises two items never share a file, and a single-value index
	// would let a file row invalidate only the last item stored.
	entries map[model.PID]*entry
	byFile  map[model.PID]map[model.PID]struct{}
	// invalGen counts item/file invalidation rows. Resolve snapshots it
	// before its catalog lookup and store refuses to cache a result from
	// before a newer invalidation: otherwise a change row consumed
	// between the lookup and the store would leave a stale path with its
	// invalidating row already spent (the poll never revisits a seq).
	invalGen uint64
	clock    int64
	sinceSeq int64
	dataVer  int64

	stop     chan struct{}
	pollDone chan struct{}
	closed   sync.Once
	closeErr error
	// ownedNext is set when Open created the fallback next itself;
	// a caller-supplied Next stays the caller's to close.
	ownedNext io.Closer
}

type entry struct {
	path    string
	filePID model.PID
	used    int64
}

var _ source.Resolver = (*Catalog)(nil)

// Open opens the catalog read-only (never taking WaxBin's write lock,
// so it coexists with a running WaxBin daemon), establishes the change
// cursor at the current feed tail, and starts the background poll.
func Open(ctx context.Context, opts Options) (*Catalog, error) {
	if opts.DBPath == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "resolver: catalog DB path is required")
	}
	next := opts.Next
	var ownedNext io.Closer
	if next == nil {
		roots, err := source.OpenRoots(nil, 0)
		if err != nil {
			return nil, err
		}
		next, ownedNext = roots, roots
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = source.DefaultMaxBytes
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: opts.DBPath, ReadOnly: true, Logger: log})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeCatalogUnavailable,
			fmt.Sprintf("resolver: opening catalog %s", opts.DBPath), err)
	}
	// The query context outlives Open's ctx (which may be a startup
	// context): it is the Catalog's lifetime, ended by Close.
	queryCtx, stopQueries := context.WithCancel(context.Background())
	c := &Catalog{
		lib:         lib,
		next:        next,
		maxBytes:    maxBytes,
		log:         log,
		queryCtx:    queryCtx,
		stopQueries: stopQueries,
		entries:     make(map[model.PID]*entry),
		byFile:      make(map[model.PID]map[model.PID]struct{}),
		ownedNext:   ownedNext,
	}
	if err := c.initCursor(ctx); err != nil {
		c.Close()
		return nil, waxerr.Wrap(waxerr.CodeCatalogUnavailable, "resolver: reading catalog change feed", err)
	}
	interval := opts.PollInterval
	if interval == 0 {
		interval = DefaultPollInterval
	}
	if interval > 0 {
		c.stop = make(chan struct{})
		c.pollDone = make(chan struct{})
		go c.pollLoop(interval)
	}
	return c, nil
}

// initCursor walks the change feed to its tail so the first poll only
// sees changes made after this resolver opened; the cache is empty now,
// so nothing older can name a stale entry. Loops until an empty page
// rather than assuming the store's page size.
func (c *Catalog) initCursor(ctx context.Context) error {
	dv, err := c.lib.DataVersion(ctx)
	if err != nil {
		return err
	}
	var seq int64
	for {
		rows, err := c.lib.Changes(ctx, seq)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		seq = rows[len(rows)-1].Seq
	}
	c.sinceSeq, c.dataVer = seq, dv
	return nil
}

// Resolve implements source.Resolver: pid refs against the catalog,
// everything else through next.
func (c *Catalog) Resolve(ref string) (*source.File, error) {
	id, ok := strings.CutPrefix(ref, "pid:")
	if !ok {
		return c.next.Resolve(ref)
	}
	pid := model.PID(id)
	if !pid.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: pid %q is not a ULID", id))
	}
	if path, hit := c.cached(pid); hit {
		f, err := c.open(ref, path)
		if err == nil || !isNotFound(err) {
			return f, err
		}
		// The cached path is gone: a rename landed between polls. Drop
		// the entry and fall through to a fresh catalog lookup, which
		// self-heals without waiting for the next poll tick.
		c.drop(pid)
	}
	gen := c.generation()
	ctx, cancel := context.WithTimeout(c.queryCtx, queryTimeout)
	defer cancel()
	iv, err := c.lib.Get(ctx, pid)
	if err != nil {
		if binerr.Is(err, binerr.CodeNotFound) {
			return nil, waxerr.New(waxerr.CodeNotFound,
				fmt.Sprintf("source: no catalog item %s", pid))
		}
		return nil, waxerr.Wrap(waxerr.CodeCatalogUnavailable, "source: catalog lookup", err)
	}
	path := string(iv.Path)
	c.store(pid, path, iv.FilePID, gen)
	return c.open(ref, path)
}

// open opens a catalog-resolved absolute path with the same validation
// as root resolution: regular file (via source.OpenLocal) and size cap.
func (c *Catalog) open(ref, path string) (*source.File, error) {
	f, err := source.OpenLocal(ref, path, path)
	if err != nil {
		return nil, err
	}
	if f.ID.Size > c.maxBytes {
		f.Close()
		return nil, waxerr.New(waxerr.CodePayloadTooLarge,
			fmt.Sprintf("source: %d bytes exceeds the %d-byte source cap", f.ID.Size, c.maxBytes))
	}
	return f, nil
}

// Poll runs one poll cycle: a cheap DataVersion read and, when it
// moved, a Changes pull that drops the cached paths the changed rows
// name. The background loop calls it every PollInterval; tests call it
// directly to observe invalidation deterministically.
func (c *Catalog) Poll(ctx context.Context) error {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()

	dv, err := c.lib.DataVersion(ctx)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeCatalogUnavailable, "resolver: data_version poll", err)
	}
	c.mu.Lock()
	seq, same := c.sinceSeq, dv == c.dataVer
	c.mu.Unlock()
	if same {
		return nil
	}
	dropped := 0
	for {
		rows, err := c.lib.Changes(ctx, seq)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeCatalogUnavailable, "resolver: change feed pull", err)
		}
		if len(rows) == 0 {
			break
		}
		seq = rows[len(rows)-1].Seq
		dropped += c.invalidate(rows)
	}
	c.mu.Lock()
	c.sinceSeq, c.dataVer = seq, dv
	c.mu.Unlock()
	if dropped > 0 {
		c.log.Debug("catalog changes invalidated cached paths", "dropped", dropped, "seq", seq)
	}
	return nil
}

// invalidate drops the cache entries a batch of change rows names and
// reports how many it dropped. Item rows carry the item PID directly;
// file rows (how renames and moves surface) map back through byFile.
// Every other entity type (albums, playlists, play state) is noise to a
// path cache.
func (c *Catalog) invalidate(rows []model.Change) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for _, ch := range rows {
		switch ch.EntityType {
		case "item":
			c.invalGen++
			dropped += c.dropLocked(ch.EntityPID)
		case "file":
			c.invalGen++
			for itemPID := range c.byFile[ch.EntityPID] {
				dropped += c.dropLocked(itemPID)
			}
		}
	}
	return dropped
}

// generation snapshots the invalidation counter for a store that follows
// an unlocked catalog lookup.
func (c *Catalog) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalGen
}

func (c *Catalog) cached(pid model.PID) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[pid]
	if !ok {
		return "", false
	}
	c.clock++
	e.used = c.clock
	return e.path, true
}

// store caches a resolved path. gen is the invalidation snapshot taken
// before the catalog lookup that produced it; a lookup raced by newer
// item/file change rows is not cached (its invalidating rows are already
// consumed, so a stale insert would survive until the next change).
func (c *Catalog) store(pid model.PID, path string, filePID model.PID, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalGen != gen {
		return
	}
	if _, exists := c.entries[pid]; !exists && len(c.entries) >= maxEntries {
		c.evictOldestLocked()
	}
	if old := c.entries[pid]; old != nil {
		c.unindexLocked(old.filePID, pid)
	}
	c.clock++
	c.entries[pid] = &entry{path: path, filePID: filePID, used: c.clock}
	if filePID != "" {
		items, ok := c.byFile[filePID]
		if !ok {
			items = make(map[model.PID]struct{}, 1)
			c.byFile[filePID] = items
		}
		items[pid] = struct{}{}
	}
}

// unindexLocked removes one item from a file's reverse index, deleting
// the set when it empties.
func (c *Catalog) unindexLocked(filePID, pid model.PID) {
	items, ok := c.byFile[filePID]
	if !ok {
		return
	}
	delete(items, pid)
	if len(items) == 0 {
		delete(c.byFile, filePID)
	}
}

func (c *Catalog) drop(pid model.PID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(pid)
}

func (c *Catalog) dropLocked(pid model.PID) int {
	e, ok := c.entries[pid]
	if !ok {
		return 0
	}
	delete(c.entries, pid)
	c.unindexLocked(e.filePID, pid)
	return 1
}

// evictOldestLocked is the cache bound: linear oldest-out, run only on
// insert at capacity, over entries that are just two strings each.
func (c *Catalog) evictOldestLocked() {
	var oldest model.PID
	minUsed := int64(math.MaxInt64)
	for pid, e := range c.entries {
		if e.used < minUsed {
			minUsed, oldest = e.used, pid
		}
	}
	if oldest != "" {
		c.dropLocked(oldest)
	}
}

// pollLoop ticks Poll until Close. Failures keep serving cached paths
// (signed identities still guard byte changes) and log once per outage,
// not once per tick; the outage flag is goroutine-local, no lock.
func (c *Catalog) pollLoop(interval time.Duration) {
	defer close(c.pollDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	down := false
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.queryCtx, queryTimeout)
			err := c.Poll(ctx)
			cancel()
			switch {
			case err != nil && !down:
				c.log.Warn("catalog poll failing; serving cached paths", "err", err)
			case err == nil && down:
				c.log.Info("catalog poll recovered")
			}
			down = err != nil
		}
	}
}

// Close aborts in-flight catalog queries, stops the poll loop, and
// releases the catalog plus any fallback next it created. Idempotent.
func (c *Catalog) Close() error {
	c.closed.Do(func() {
		c.stopQueries()
		if c.stop != nil {
			close(c.stop)
			<-c.pollDone
		}
		c.closeErr = c.lib.Close()
		if c.ownedNext != nil {
			if err := c.ownedNext.Close(); err != nil && c.closeErr == nil {
				c.closeErr = err
			}
		}
	})
	return c.closeErr
}

// isNotFound reports whether an open failed because the path is gone
// (source.OpenLocal wraps ENOENT as CodeNotFound).
func isNotFound(err error) bool {
	return waxerr.CodeOf(err) == waxerr.CodeNotFound
}
