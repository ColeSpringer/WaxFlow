package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultMaxBytes is the per-source open cap when none is configured.
const DefaultMaxBytes = 4 << 30

// attemptWait bounds how long a caller (start-up, a request, a reload, the
// sweep) waits on one check of a root's folder. A network mount that stops
// answering costs a caller this once, while the check carries on in the
// background and commits whatever it finds. Tests shorten it.
var attemptWait = 5 * time.Second

// maxLooks bounds the lookups one open makes while writers keep retiring its
// root's handle.
const maxLooks = 8

// Root names a library directory. References address it as
// "<name>/<relative/path>".
type Root struct {
	Name string
	Path string
}

// RootStatus reports one configured root.
type RootStatus struct {
	Name, Path string
	// Mounted reports whether the root is open. An unmounted root answers
	// waxerr.CodeRootUnavailable until a check finds its folder serving.
	Mounted bool
	// Err is why an unmounted root is not open; nil when mounted.
	Err error
	// Since is when the root entered its current state.
	Since time.Time
}

// ErrRootEmpty is why a root whose folder opens but holds nothing is not
// mounted: that is what a mount point is before its drive or share arrives,
// and serving it would answer every request not-found.
var ErrRootEmpty = errors.New("the folder is empty")

var (
	// errOpening is the state of a root whose folder is still being opened.
	errOpening = errors.New("still opening the folder")
	// errNotAnswering is why a root whose check ran out its bound is down.
	errNotAnswering = errors.New("the folder is not answering")
	// errChanging is why an open gave up on a root reloads kept replacing.
	errChanging = errors.New("the root kept changing during the open")
)

// rootState is one configured root: the path it was configured with (so a
// reload can tell an unchanged root from a re-pointed one), its
// kernel-confined directory handle (nil while unmounted), why it is not
// open, when it entered this state, and a number no other state carries, so
// a commit can tell this state from an equal-looking later one.
type rootState struct {
	path  string
	root  *os.Root
	err   error
	since time.Time
	seq   uint64
}

func (s rootState) status(name string) RootStatus {
	return RootStatus{Name: name, Path: s.path, Mounted: s.root != nil, Err: s.err, Since: s.since}
}

// attempt is one check of a root's state against its folder, run in its own
// goroutine and shared by every caller that wants that state checked
// meanwhile. A caller waits on it until its deadline at most, so a hung
// network mount strands only this goroutine.
type attempt struct {
	seen     rootState
	deadline time.Time
	done     chan struct{}
	// changed and ok are what it committed, read only once done is closed.
	changed RootStatus
	ok      bool
}

// await waits for the attempt until its deadline or ctx ends, and reports
// whether it ended.
func (a *attempt) await(ctx context.Context) bool {
	select {
	case <-a.done:
		return true
	default:
	}
	t := time.NewTimer(time.Until(a.deadline))
	defer t.Stop()
	select {
	case <-a.done:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// Roots resolves root-relative references. Confinement is precisely
// scoped: os.Root gives kernel-enforced no-escape
// including symlink traversal out of the root, and every open is
// additionally validated as a regular file via fstat (a FIFO or device
// node could hang an open or a read) and capped by maxBytes. Symlinks
// that stay within the root remain allowed; in-place libraries use them.
//
// A root whose folder does not open, or opens empty, is kept unmounted rather
// than refused: it answers waxerr.CodeRootUnavailable until a check finds its
// folder serving. A check runs in its own goroutine, one per root state at a
// time, and commits what it finds only if the root is still in the state it
// checked; every caller waits on it for attemptWait at most. So no
// filesystem call runs under a lock, and a folder that stops answering before
// it is mounted holds up nothing but its own check. One that hangs while
// mounted holds the requests reading through it, as any blocked read does.
//
// The set is live-mutable: Reload reconciles it against a fresh config
// while resolution continues. mu guards the map, the order and maxBytes, and
// the hot path only takes mu.RLock; reloadMu serializes Reloads so their
// reconciles never interleave.
type Roots struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	maxBytes int64
	order    []string
	roots    map[string]rootState
	seq      atomic.Uint64
	now      func() time.Time
	watch    atomic.Pointer[func(RootStatus)]
	// watchMu holds a commit through its report, so one root's reports
	// arrive in the order its changes landed.
	watchMu sync.Mutex

	attemptMu sync.Mutex
	attempts  map[string]*attempt
}

// validateRoots checks each root's name against the reference syntax's
// separators, for duplicates, and for a path. OpenRoots and Reload share it
// so the rules cannot drift.
func validateRoots(roots []Root) error {
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if root.Name == "" || strings.ContainsAny(root.Name, "/:") {
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: root name %q must be non-empty without '/' or ':'", root.Name))
		}
		if seen[root.Name] {
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: duplicate root name %q", root.Name))
		}
		if root.Path == "" {
			return waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: root %q has no path", root.Name))
		}
		seen[root.Name] = true
	}
	return nil
}

// newState returns a state for path that no other state shares.
func (r *Roots) newState(path string, root *os.Root, err error) rootState {
	return rootState{path: path, root: root, err: err, since: r.now(), seq: r.seq.Add(1)}
}

// mountAt opens path and checks that it can serve: listing it through the new
// root needs search and read permission on the folder, and an empty folder is
// not served (ErrRootEmpty).
func (r *Roots) mountAt(path string) rootState {
	or, err := os.OpenRoot(path)
	if err == nil {
		if err = listable(or, path); err != nil {
			or.Close()
			or = nil
		}
	}
	return r.newState(path, or, err)
}

// listable reads a root's folder until it finds an entry that is not a
// placeholder, naming the folder in its error.
func listable(or *os.Root, path string) error {
	op := "open"
	d, err := or.Open(".")
	if err == nil {
		op = "read"
		err = ErrRootEmpty
		for err == ErrRootEmpty {
			names, rerr := d.Readdirnames(16)
			if slices.ContainsFunc(names, func(n string) bool { return !placeholder(n) }) {
				err = nil
			} else if rerr != nil {
				err = rerr
			}
		}
		d.Close()
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		err = ErrRootEmpty
	default:
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
	}
	return &fs.PathError{Op: op, Path: path, Err: err}
}

// placeholder reports an entry a bare mount point can hold without its drive:
// a hidden file a desktop left there, or a filesystem's own bookkeeping.
func placeholder(name string) bool {
	switch name {
	case "lost+found", "System Volume Information", "$RECYCLE.BIN":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// OpenRoots opens the named roots. maxBytes caps each resolved file's size;
// 0 means DefaultMaxBytes. Names must be non-empty, unique and free of '/'
// and ':' (the reference syntax's separators), and every root needs a path.
// A path that does not open is not an error: its root starts unmounted (see
// Status), as does one whose folder does not answer within attemptWait.
func OpenRoots(roots []Root, maxBytes int64) (*Roots, error) {
	if err := validateRoots(roots); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	r := &Roots{
		maxBytes: maxBytes,
		roots:    make(map[string]rootState, len(roots)),
		attempts: make(map[string]*attempt),
		now:      time.Now,
	}
	for _, root := range roots {
		r.roots[root.Name] = r.newState(root.Path, nil, errOpening)
		r.order = append(r.order, root.Name)
	}
	r.settle(r.order)
	return r, nil
}

// ReloadResult reports what a Reload changed. Removed is sorted for a stable
// log line; the rest are in configuration order. Roots is the full set of
// root names after the reload, Unmounted the ones among them that are not
// mounted once its checks have settled, and Remounted the unchanged roots
// that were unmounted before the reload and are mounted after it.
type ReloadResult struct {
	Added, Removed, Changed, Roots []string
	Unmounted, Remounted           []string
}

// errReloadAfterClose is returned by Reload when it races or follows Close:
// the set was torn down, so there is nothing to reconcile. It wraps
// fs.ErrClosed (errors.Is-matchable) under an internal code.
var errReloadAfterClose = waxerr.Wrap(waxerr.CodeInternal, "source: reload after close", fs.ErrClosed)

// Reload reconciles the live root set to desired and replaces the size cap
// with maxBytes (0 means DefaultMaxBytes, as OpenRoots). A name present in
// both with an unchanged path is left untouched; a new name is opened, a
// dropped name closed, a name whose path changed is re-pointed (the old
// handle closed, the new one opened), and an unmounted root is checked again.
// It re-reads exactly the fields a restart would, so a reload produces what a
// restart loads.
//
// An invalid desired set (a bad or duplicate name, an empty path) is refused
// with nothing changed. A path that does not open leaves its root unmounted
// while the rest of the reload lands; a re-pointed root whose new path fails
// stops serving the old one, which would be the wrong library. A kept
// mounted root is not checked, and every open runs in a check that Reload
// waits on for attemptWait at most, so a hung network mount cannot hold it.
//
// In-flight streams already resolved from a removed or re-pointed root keep
// working: their open file handle is independent of the closed root dir
// handle. Later requests for a removed root 404.
func (r *Roots) Reload(desired []Root, maxBytes int64) (ReloadResult, error) {
	if err := validateRoots(desired); err != nil {
		return ReloadResult{}, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	// Swap in the new set with no filesystem call: a new or re-pointed root
	// starts as still opening, and every other root keeps its state.
	var result ReloadResult
	wasDown := make(map[string]bool)
	r.mu.Lock()
	if r.roots == nil {
		r.mu.Unlock()
		return ReloadResult{}, errReloadAfterClose
	}
	old := r.roots
	next := make(map[string]rootState, len(desired))
	for _, root := range desired {
		result.Roots = append(result.Roots, root.Name)
		st, known := old[root.Name]
		switch {
		case !known:
			result.Added = append(result.Added, root.Name)
			st = r.newState(root.Path, nil, errOpening)
		case st.path != root.Path:
			result.Changed = append(result.Changed, root.Name)
			st = r.newState(root.Path, nil, errOpening)
		case st.root == nil:
			wasDown[root.Name] = true
		}
		next[root.Name] = st
	}
	var retired []*os.Root
	for name, st := range old {
		if _, kept := next[name]; !kept {
			result.Removed = append(result.Removed, name)
		}
		if st.root != nil && next[name].root != st.root {
			retired = append(retired, st.root)
		}
	}
	var down []string // checks write to next once it is live, so read it now
	for _, name := range result.Roots {
		if next[name].root == nil {
			down = append(down, name)
		}
	}
	r.roots, r.order, r.maxBytes = next, slices.Clone(result.Roots), maxBytes
	r.mu.Unlock()
	slices.Sort(result.Removed)

	// The swap made the retired handles unreachable, and os.Root.Close is safe
	// against a Resolve opening through one now: os.Root's own reference count
	// defers the fd close until that open finishes (or fails it with
	// fs.ErrClosed, which Resolve answers by looking again). They close off
	// the reload's path, since closing a dead share's handle can wait on it.
	go closeAll(retired)

	r.settle(down)
	r.mu.RLock()
	for _, name := range result.Roots {
		switch st := r.roots[name]; {
		case st.root == nil:
			result.Unmounted = append(result.Unmounted, name)
		case wasDown[name]:
			result.Remounted = append(result.Remounted, name)
		}
	}
	r.mu.RUnlock()
	return result, nil
}

// Names lists the configured root names in configuration order.
func (r *Roots) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Status reports every configured root, in configuration order. An
// unmounted root whose check has run past its bound reads as not answering.
func (r *Roots) Status() []RootStatus {
	r.mu.RLock()
	out := make([]RootStatus, 0, len(r.order))
	seqs := make([]uint64, 0, len(r.order))
	for _, name := range r.order {
		st := r.roots[name]
		out = append(out, st.status(name))
		seqs = append(seqs, st.seq)
	}
	r.mu.RUnlock()
	now := time.Now()
	r.attemptMu.Lock()
	defer r.attemptMu.Unlock()
	for i := range out {
		if a := r.attempts[out[i].Name]; !out[i].Mounted && a != nil && a.seen.seq == seqs[i] && now.After(a.deadline) {
			out[i].Err = errNotAnswering
		}
	}
	return out
}

// Remount checks every root against its folder, mounting the unmounted ones
// that now open and reopening or demoting the mounted ones whose folder is
// gone or replaced, and returns what changed: a mount, an unmount, a new
// handle or a new reason. It waits on each root's check for attemptWait at
// most, so one hung network mount holds up no other root.
func (r *Roots) Remount() []RootStatus {
	return r.settle(r.Names())
}

// Watch has fn called with every change a check commits (a mount, an
// unmount, a new handle or a new reason), whichever caller the check ran
// for. It replaces any earlier fn; fn runs on the check's goroutine and must
// not block.
func (r *Roots) Watch(fn func(RootStatus)) {
	r.watch.Store(&fn)
}

// settle checks the named roots at once, waits on each check until its
// deadline, and returns the changes the finished ones committed.
func (r *Roots) settle(names []string) []RootStatus {
	var as []*attempt
	for _, name := range names {
		if a := r.kick(name); a != nil {
			as = append(as, a)
		}
	}
	var changed []RootStatus
	for _, a := range as {
		if a.await(context.Background()) && a.ok {
			changed = append(changed, a.changed)
		}
	}
	return changed
}

// kick starts a check of the named root's current state, or returns the one
// already running on that state; nil means the root is not configured.
func (r *Roots) kick(name string) *attempt {
	r.mu.RLock()
	st, ok := r.roots[name]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	r.attemptMu.Lock()
	defer r.attemptMu.Unlock()
	if a := r.attempts[name]; a != nil && a.seen.seq == st.seq {
		return a
	}
	a := &attempt{seen: st, deadline: time.Now().Add(attemptWait), done: make(chan struct{})}
	r.attempts[name] = a
	go r.run(name, a)
	return a
}

// run checks the state its attempt saw and commits what it finds, so a change
// is reported before anyone waiting on the attempt sees it end.
func (r *Roots) run(name string, a *attempt) {
	if next, changed := r.check(a.seen); changed {
		a.changed, a.ok = r.commit(name, a.seen, next)
	}
	r.attemptMu.Lock()
	if r.attempts[name] == a {
		delete(r.attempts, name)
	}
	r.attemptMu.Unlock()
	close(a.done)
}

// check compares a root's state with its folder: one still mounted on the
// folder its path names is unchanged, and anything else is opened again. A
// root still down for the reason it went down is unchanged too; one down for
// a new reason keeps the time it went down.
func (r *Roots) check(seen rootState) (rootState, bool) {
	if seen.root != nil && healthy(seen) {
		return seen, false
	}
	next := r.mountAt(seen.path)
	if next.root == nil && seen.root == nil {
		if next.err.Error() == seen.err.Error() {
			return seen, false
		}
		next.since = seen.since
	}
	return next, true
}

// commit installs next for name if the root is still in state seen, reports
// the change, closes the handle that lost (the one it replaced, or its own)
// once no lock is held, and returns the change.
func (r *Roots) commit(name string, seen, next rootState) (RootStatus, bool) {
	r.watchMu.Lock()
	r.mu.Lock()
	live, ok := r.roots[name] // a closed set reads as no root
	won := ok && live.seq == seen.seq
	if won {
		r.roots[name] = next
	}
	r.mu.Unlock()
	var st RootStatus
	loser := next.root
	if won {
		st, loser = next.status(name), seen.root
		if fn := r.watch.Load(); fn != nil {
			(*fn)(st)
		}
	}
	r.watchMu.Unlock()
	if loser != nil {
		loser.Close()
	}
	return st, won
}

// healthy reports whether a mounted root's handle still names the directory
// at its path: false once that directory is removed, replaced or unreachable.
// It compares the handle's own stat with a fresh stat of the path.
func healthy(st rootState) bool {
	held, err := st.root.Stat(".")
	if err != nil {
		return false
	}
	now, err := os.Stat(st.path)
	return err == nil && os.SameFile(held, now)
}

// Close releases the held root directories and clears the set, so a
// double-close is a no-op and any post-close Resolve is an ordinary
// unknown-root error rather than a use of a closed handle. A check still
// running finds no root to commit to and closes what it opened.
func (r *Roots) Close() error {
	r.mu.Lock()
	var held []*os.Root
	for _, st := range r.roots {
		if st.root != nil {
			held = append(held, st.root)
		}
	}
	r.roots = nil
	r.order = nil
	r.mu.Unlock()
	return closeAll(held)
}

// closeAll closes the handles and returns the first error.
func closeAll(held []*os.Root) error {
	var first error
	for _, or := range held {
		if err := or.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Resolve implements Resolver for "<root>/<relative/path>" references. ctx
// bounds the wait on a root's check; opening a local regular file does not
// block on anything cancelable.
func (r *Roots) Resolve(ctx context.Context, ref string) (*File, error) {
	if ref == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "source: empty source reference")
	}
	if s, ok := scheme(ref); ok {
		return nil, unsupportedScheme(s)
	}
	name, rel, ok := strings.Cut(ref, "/")
	if !ok || name == "" || rel == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: reference %q is not <root>/<path>", ref))
	}
	f, maxBytes, err := r.open(ctx, name, rel)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "source: stat", err)
	}
	if err := container.CheckRegular(ref, fi.Mode()); err != nil {
		f.Close()
		return nil, err
	}
	if fi.Size() > maxBytes {
		f.Close()
		return nil, waxerr.New(waxerr.CodePayloadTooLarge,
			fmt.Sprintf("source: %d bytes exceeds the %d-byte source cap", fi.Size(), maxBytes))
	}
	return &File{
		Ref: ref,
		Ext: extHint(rel),
		ID:  Identity{Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()},
		f:   f,
	}, nil
}

// open opens rel under the named root. A root that is unmounted, or whose
// open fails with its folder gone, replaced or shut, is checked (mounted
// again where it can be) and the open tried again, so a folder that reappears
// serves on the first request; a handle a writer retired mid-open is simply
// looked up again. Each state the open meets is checked once at most, the
// health check runs on error paths only, and no lock is held across a
// filesystem call.
func (r *Roots) open(ctx context.Context, name, rel string) (*os.File, int64, error) {
	var checked uint64 // the state this open last checked; seqs start at 1
	var cause error
	for range maxLooks {
		st, maxBytes, err := r.lookup(name)
		if err != nil {
			return nil, 0, err
		}
		cause = st.err
		if st.root != nil {
			// O_NONBLOCK (unix) keeps the open itself from hanging on a FIFO;
			// Resolve's fstat then rejects anything that is not a regular file.
			f, err := st.root.OpenFile(rel, os.O_RDONLY|container.OpenNonblock, 0)
			if err == nil {
				return f, maxBytes, nil
			}
			if errors.Is(err, fs.ErrClosed) && !r.current(name, st) {
				continue
			}
			if healthy(st) {
				// The root is fine, so the answer is about the file.
				return nil, 0, openFault(err)
			}
			cause = err
		}
		if st.seq == checked {
			break
		}
		checked = st.seq
		if a := r.kick(name); a != nil && !a.await(ctx) {
			if err := ctx.Err(); err != nil {
				return nil, 0, waxerr.Wrap(waxerr.CodeCanceled, fmt.Sprintf("source: waiting on root %q", name), err)
			}
			return nil, 0, unavailable(name, errNotAnswering)
		}
	}
	if cause == nil {
		cause = errChanging // every look met a handle a reload had just retired
	}
	return nil, 0, unavailable(name, cause)
}

// lookup returns the named root's state and the size cap.
func (r *Roots) lookup(name string) (rootState, int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.roots[name]
	if !ok {
		return rootState{}, 0, waxerr.New(waxerr.CodeNotFound,
			fmt.Sprintf("source: unknown root %q (configured: %s)", name, strings.Join(r.order, ", ")))
	}
	return st, r.maxBytes, nil
}

// current reports whether the named root is still in state st.
func (r *Roots) current(name string, st rootState) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	live, ok := r.roots[name]
	return ok && live.seq == st.seq
}

// RootDownError is the cause inside a root-unavailable error. It names the
// root, so a caller holding the error wrapped (the pid lookup, a CLI hint)
// can still tell which one is down.
type RootDownError struct {
	Root string
	Err  error
}

func (e *RootDownError) Error() string { return e.Err.Error() }
func (e *RootDownError) Unwrap() error { return e.Err }

// unavailable answers for a configured root that is not open. It names the
// root but not its path, since a signed playback URL hands this message to a
// client.
func unavailable(name string, cause error) error {
	if pe, ok := errors.AsType[*fs.PathError](cause); ok {
		cause = pe.Err
	}
	return waxerr.Wrap(waxerr.CodeRootUnavailable, fmt.Sprintf("source: root %q is unavailable", name),
		&RootDownError{Root: name, Err: cause})
}

// openFault codes an open error on a healthy root. A missing file is
// not-found, an unreadable one the library's fault, and running out of file
// handles the daemon's, worth a retry; the rest, os.Root's escape refusals and
// malformed paths, ask for something no valid reference names.
func openFault(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return waxerr.Wrap(waxerr.CodeNotFound, "source: no such file", err)
	case errors.Is(err, fs.ErrPermission):
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "source: permission denied", err)
	case isIOFault(err):
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "source: reading the library", err)
	case isOutOfHandles(err):
		return waxerr.Wrap(waxerr.CodeOverloaded, "source: out of file handles", err)
	default:
		return waxerr.Wrap(waxerr.CodeInvalidRequest, "source: unresolvable path", err)
	}
}
