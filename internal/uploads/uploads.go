// Package uploads is the spool for one-shot upload sources: a client
// POSTs bytes, receives a ULID, and references it as src=upload:<id>.
// The spool is a staging area, not a cache, so eviction is TTL from
// creation, never access-based.
package uploads

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxflow/internal/ulid"
	"github.com/colespringer/waxflow/waxerr"
)

// Options configures a Store.
type Options struct {
	// MaxBytes caps one upload's size; 0 means no per-upload cap.
	MaxBytes int64
	// MaxTotalBytes caps the aggregate spool size; 0 means no cap.
	MaxTotalBytes int64
	// TTL evicts uploads this long after creation; 0 means no TTL.
	TTL time.Duration
	// Logger receives janitor notes; nil discards.
	Logger *slog.Logger
}

// Item describes one spooled upload.
type Item struct {
	ID      string
	Name    string // client-supplied filename, may be ""
	Bytes   int64
	Created time.Time
}

// maxNameBytes bounds the client-supplied filename. The name is sidecar
// metadata only, never a path component, so length is the only limit.
const maxNameBytes = 255

// sidecar is the on-disk metadata for one upload, stored next to the
// payload as <id>.json.
type sidecar struct {
	Name    string `json:"name"`
	Created int64  `json:"created"` // unix nanoseconds
}

// Store is the on-disk upload spool: <dir>/<ulid> holds the raw bytes
// and <dir>/<ulid>.json the sidecar metadata. The in-memory index is
// authoritative after Open; the filesystem is only consulted at boot.
type Store struct {
	dir           string
	maxBytes      int64
	maxTotalBytes int64
	ttl           time.Duration
	log           *slog.Logger

	mu         sync.Mutex
	items      map[string]Item
	totalBytes int64

	janitorStop chan struct{}
	janitorWG   sync.WaitGroup
	closeOnce   sync.Once
}

// Open prepares the spool directory (creating it), adopts existing
// uploads from a previous run, removes stray temp files and half-written
// pairs, and starts the TTL janitor.
func Open(dir string, opts Options) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: resolving spool dir", err)
	}
	s := &Store{
		dir:           abs,
		maxBytes:      opts.MaxBytes,
		maxTotalBytes: opts.MaxTotalBytes,
		ttl:           opts.TTL,
		log:           opts.Logger,
		items:         make(map[string]Item),
		janitorStop:   make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: creating spool dir", err)
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	if s.ttl > 0 {
		s.janitorWG.Add(1)
		go s.janitor()
	}
	return s, nil
}

// scan rebuilds the index at boot. Put publishes the payload before the
// sidecar, so a crash leaves either a complete pair, a ".tmp" file, or
// an orphan half; everything but a complete pair is debris.
func (s *Store) scan() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "uploads: scanning spool dir", err)
	}
	payloads := make(map[string]int64)
	sidecars := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(s.dir, name)
		switch {
		case strings.HasSuffix(name, ".tmp"):
			s.log.Info("uploads: removing stray temp file", "file", name)
			os.Remove(path)
		case strings.HasSuffix(name, ".json") && ulid.Valid(strings.TrimSuffix(name, ".json")):
			sidecars[strings.TrimSuffix(name, ".json")] = true
		case ulid.Valid(name):
			fi, err := e.Info()
			if err != nil {
				os.Remove(path)
				continue
			}
			payloads[name] = fi.Size()
		default:
			s.log.Info("uploads: removing stray file", "file", name)
			os.Remove(path)
		}
	}
	for id, size := range payloads {
		if !sidecars[id] {
			s.log.Info("uploads: removing orphan payload", "id", id)
			os.Remove(filepath.Join(s.dir, id))
			continue
		}
		delete(sidecars, id)
		b, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
		var sc sidecar
		if err != nil || json.Unmarshal(b, &sc) != nil {
			s.log.Info("uploads: removing upload with unreadable sidecar", "id", id)
			os.Remove(filepath.Join(s.dir, id))
			os.Remove(filepath.Join(s.dir, id+".json"))
			continue
		}
		s.items[id] = Item{ID: id, Name: sc.Name, Bytes: size, Created: time.Unix(0, sc.Created)}
		s.totalBytes += size
	}
	for id := range sidecars {
		s.log.Info("uploads: removing orphan sidecar", "id", id)
		os.Remove(filepath.Join(s.dir, id+".json"))
	}
	return nil
}

// Put spools r to disk under a fresh ULID. It enforces MaxBytes while
// copying (CodePayloadTooLarge past the cap) and MaxTotalBytes both
// during the copy and authoritatively at registration (an upload that
// would push the spool over the cap fails with CodePayloadTooLarge).
func (s *Store) Put(r io.Reader, name string) (*Item, error) {
	if len(name) > maxNameBytes {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "uploads: name exceeds 255 bytes")
	}
	id, err := ulid.New()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: minting id", err)
	}
	tmp := filepath.Join(s.dir, id+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: creating spool file", err)
	}
	written, err := s.spool(f, r)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: syncing spool file", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: closing spool file", err)
	}

	// Publish the payload before the sidecar: boot rescan treats a
	// payload without a sidecar as debris, so a crash between the two
	// renames cannot resurrect a half-registered upload.
	payload := filepath.Join(s.dir, id)
	if err := os.Rename(tmp, payload); err != nil {
		os.Remove(tmp)
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: publishing spool file", err)
	}
	created := time.Now()
	if err := s.writeSidecar(id, sidecar{Name: name, Created: created.UnixNano()}); err != nil {
		os.Remove(payload)
		return nil, waxerr.Wrap(waxerr.CodeInternal, "uploads: writing sidecar", err)
	}

	s.mu.Lock()
	// The in-copy aggregate check races concurrent Puts; this one, under
	// the lock that guards totalBytes, is the authoritative gate.
	if s.maxTotalBytes > 0 && s.totalBytes+written > s.maxTotalBytes {
		s.mu.Unlock()
		os.Remove(payload)
		os.Remove(filepath.Join(s.dir, id+".json"))
		return nil, waxerr.New(waxerr.CodePayloadTooLarge, "uploads: spool full")
	}
	it := Item{ID: id, Name: name, Bytes: written, Created: created}
	s.items[id] = it
	s.totalBytes += written
	s.mu.Unlock()
	return &it, nil
}

// spool copies r to w, enforcing the per-upload cap and, best effort,
// the aggregate cap per chunk so an oversized or endless body stops
// early instead of filling the disk.
func (s *Store) spool(w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var n int64
	for {
		k, rerr := r.Read(buf)
		if k > 0 {
			n += int64(k)
			if s.maxBytes > 0 && n > s.maxBytes {
				return n, waxerr.New(waxerr.CodePayloadTooLarge, "uploads: upload exceeds size cap")
			}
			if s.maxTotalBytes > 0 && s.Bytes()+n > s.maxTotalBytes {
				return n, waxerr.New(waxerr.CodePayloadTooLarge, "uploads: spool full")
			}
			if _, werr := w.Write(buf[:k]); werr != nil {
				return n, waxerr.Wrap(waxerr.CodeInternal, "uploads: writing spool file", werr)
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, waxerr.Wrap(waxerr.CodeSourceUnreadable, "uploads: reading upload", rerr)
		}
	}
}

// writeSidecar persists an upload's metadata atomically. The ".json.tmp"
// suffix keeps a torn write inside scan's ".tmp" debris sweep.
func (s *Store) writeSidecar(id string, sc sidecar) error {
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, id+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, id+".json")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Get returns the item and the absolute path of its spooled bytes.
// Unknown or malformed ids return CodeNotFound; validating before any
// path is built keeps traversal attempts off the filesystem entirely.
func (s *Store) Get(id string) (*Item, string, error) {
	if !ulid.Valid(id) {
		return nil, "", waxerr.New(waxerr.CodeNotFound, "uploads: no such upload")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return nil, "", waxerr.New(waxerr.CodeNotFound, "uploads: no such upload")
	}
	return &it, filepath.Join(s.dir, id), nil
}

// Delete removes an upload. Unknown or malformed ids return CodeNotFound.
func (s *Store) Delete(id string) error {
	if !ulid.Valid(id) {
		return waxerr.New(waxerr.CodeNotFound, "uploads: no such upload")
	}
	s.mu.Lock()
	it, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return waxerr.New(waxerr.CodeNotFound, "uploads: no such upload")
	}
	delete(s.items, id)
	s.totalBytes -= it.Bytes
	s.mu.Unlock()
	s.removeFiles(id)
	return nil
}

// removeFiles deletes an upload's pair, best effort: a failure only
// leaves debris that the next boot rescan re-adopts or sweeps.
func (s *Store) removeFiles(id string) {
	if err := os.Remove(filepath.Join(s.dir, id)); err != nil {
		s.log.Warn("uploads: removing payload", "id", id, "err", err)
	}
	if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil {
		s.log.Warn("uploads: removing sidecar", "id", id, "err", err)
	}
}

// Items lists current uploads, sorted by id (which is creation order),
// for diagnostics and tests.
func (s *Store) Items() []*Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, &it)
	}
	slices.SortFunc(out, func(a, b *Item) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Bytes reports the aggregate spooled size.
func (s *Store) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

// janitor expires uploads periodically. The interval is short relative
// to the TTL so expiry lag stays small, but never longer than a minute
// and never shorter than a second: a tiny TTL must not spin the sweep
// (and NewTicker panics outright on the zero a sub-4ns TTL would yield).
func (s *Store) janitor() {
	defer s.janitorWG.Done()
	t := time.NewTicker(max(min(s.ttl/4, time.Minute), time.Second))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweep()
		case <-s.janitorStop:
			return
		}
	}
}

// sweep removes uploads older than the TTL, measured from creation: the
// spool is a staging area, so access never extends a lease.
func (s *Store) sweep() {
	now := time.Now()
	s.mu.Lock()
	var victims []string
	for id, it := range s.items {
		if now.Sub(it.Created) > s.ttl {
			victims = append(victims, id)
			delete(s.items, id)
			s.totalBytes -= it.Bytes
		}
	}
	s.mu.Unlock()
	for _, id := range victims {
		s.removeFiles(id)
		s.log.Info("uploads: expired", "id", id)
	}
}

// Close stops the janitor. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.janitorStop)
		s.janitorWG.Wait()
	})
	return nil
}
