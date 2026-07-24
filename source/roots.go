package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/colespringer/waxflow/waxerr"
)

// DefaultMaxBytes is the per-source open cap when none is configured.
const DefaultMaxBytes = 4 << 30

// Root names a library directory. References address it as
// "<name>/<relative/path>".
type Root struct {
	Name string
	Path string
}

// mounted is one open root: the configured path it was opened from (so a
// reload can tell an unchanged root from a re-pointed one) and its
// kernel-confined directory handle.
type mounted struct {
	path string
	root *os.Root
}

// Roots resolves root-relative references. Confinement is precisely
// scoped: os.Root gives kernel-enforced no-escape
// including symlink traversal out of the root, and every open is
// additionally validated as a regular file via fstat (a FIFO or device
// node could hang an open or a read) and capped by maxBytes. Symlinks
// that stay within the root remain allowed; in-place libraries use them.
//
// The set is live-mutable: Reload reconciles it against a fresh config
// while resolution continues. mu guards the maps and maxBytes for both
// readers and the swap; reloadMu serializes whole Reload calls so their
// snapshot-open-swap phases never interleave. Reloads are rare operator
// actions, so the extra lock costs nothing on the hot path, which only
// takes mu.RLock.
type Roots struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	maxBytes int64
	order    []string
	roots    map[string]mounted
}

// validateRootName enforces the reference syntax's separators on a root
// name. OpenRoots and Reload share it so the rule cannot drift.
func validateRootName(name string) error {
	if name == "" || strings.ContainsAny(name, "/:") {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: root name %q must be non-empty without '/' or ':'", name))
	}
	return nil
}

// openMount validates a root's name and opens its path into a mounted.
// The one place OpenRoots and Reload go from a Root to an open handle, so
// the validate-and-open logic stays single-sourced.
func openMount(root Root) (mounted, error) {
	if err := validateRootName(root.Name); err != nil {
		return mounted{}, err
	}
	or, err := os.OpenRoot(root.Path)
	if err != nil {
		return mounted{}, waxerr.Wrap(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: opening root %q at %s", root.Name, root.Path), err)
	}
	return mounted{path: root.Path, root: or}, nil
}

// OpenRoots opens the named roots. maxBytes caps each resolved file's
// size; 0 means DefaultMaxBytes. Root names must be non-empty and free of
// '/' and ':' (the reference syntax's separators).
func OpenRoots(roots []Root, maxBytes int64) (*Roots, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	r := &Roots{maxBytes: maxBytes, roots: make(map[string]mounted, len(roots))}
	for _, root := range roots {
		if _, dup := r.roots[root.Name]; dup {
			r.Close()
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: duplicate root name %q", root.Name))
		}
		m, err := openMount(root)
		if err != nil {
			r.Close()
			return nil, err
		}
		r.roots[root.Name] = m
		r.order = append(r.order, root.Name)
	}
	return r, nil
}

// ReloadResult reports what a Reload changed. Added, Changed, and Roots
// are in configuration order; Removed is sorted for a stable log line.
// Roots is the full set of root names after the reload.
type ReloadResult struct {
	Added, Removed, Changed, Roots []string
}

// errReloadAfterClose is returned by Reload when it races or follows Close:
// the set was torn down, so there is nothing to reconcile. It wraps
// fs.ErrClosed (errors.Is-matchable) under an internal code.
var errReloadAfterClose = waxerr.Wrap(waxerr.CodeInternal, "source: reload after close", fs.ErrClosed)

// Reload reconciles the live root set to desired and replaces the size cap
// with maxBytes (0 means DefaultMaxBytes, as OpenRoots). A name present in
// both with an unchanged path is left untouched; a new name is opened, a
// dropped name closed, and a name whose path changed is re-pointed (the old
// handle closed, the new one opened). It re-reads exactly the fields a
// restart would, so a reload produces byte-for-byte what a restart loads.
//
// The slow opens run outside the write lock, so a network-mounted root's
// os.OpenRoot never stalls live resolution; the write lock is held only for
// the pointer-swap. If any new path fails to open, nothing is swapped and
// the live set, order, and cap are untouched (atomic, no fd leak).
//
// In-flight streams already resolved from a removed or re-pointed root keep
// working: their open file handle is independent of the closed root dir
// handle. Later requests for a removed root 404.
func (r *Roots) Reload(desired []Root, maxBytes int64) (ReloadResult, error) {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	// Phase 1: snapshot the current name->path under a brief read lock,
	// then validate + dup-check desired and classify against the snapshot.
	// A closed set (r.roots == nil) short-circuits here, before phase 2's
	// opens, so a reload after Close never does the slow open work only to
	// discard it in phase 3.
	r.mu.RLock()
	if r.roots == nil {
		r.mu.RUnlock()
		return ReloadResult{}, errReloadAfterClose
	}
	current := make(map[string]string, len(r.roots))
	for name, m := range r.roots {
		current[name] = m.path
	}
	r.mu.RUnlock()

	seen := make(map[string]bool, len(desired))
	var toOpen []Root
	var result ReloadResult
	for _, root := range desired {
		if err := validateRootName(root.Name); err != nil {
			return ReloadResult{}, err
		}
		if seen[root.Name] {
			return ReloadResult{}, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: duplicate root name %q", root.Name))
		}
		seen[root.Name] = true
		result.Roots = append(result.Roots, root.Name)
		if oldPath, ok := current[root.Name]; ok {
			if oldPath == root.Path {
				continue // unchanged: keep the open handle
			}
			result.Changed = append(result.Changed, root.Name)
		} else {
			result.Added = append(result.Added, root.Name)
		}
		toOpen = append(toOpen, root)
	}
	for name := range current {
		if !seen[name] {
			result.Removed = append(result.Removed, name)
		}
	}
	slices.Sort(result.Removed)

	// Phase 2: open the new and changed paths with no lock held, so a slow
	// open never blocks resolution. On any failure, close every handle
	// opened so far and abort with nothing swapped.
	opened := make(map[string]mounted, len(toOpen))
	for _, root := range toOpen {
		m, err := openMount(root)
		if err != nil {
			for _, o := range opened {
				o.root.Close()
			}
			return ReloadResult{}, err
		}
		opened[root.Name] = m
	}

	// Phase 3: swap under the write lock (pointer writes only), collecting
	// the old handles that are now unreachable.
	r.mu.Lock()
	if r.roots == nil {
		// Close won the race between phase 1 and here and tore the set down.
		// Do not resurrect it: close what phase 2 opened (else those fds leak
		// with nothing left to close them) and report the closed state.
		r.mu.Unlock()
		for _, m := range opened {
			m.root.Close()
		}
		return ReloadResult{}, errReloadAfterClose
	}
	old := r.roots
	newRoots := make(map[string]mounted, len(desired))
	newOrder := make([]string, 0, len(desired))
	for _, root := range desired {
		if m, ok := opened[root.Name]; ok {
			newRoots[root.Name] = m
		} else {
			newRoots[root.Name] = old[root.Name] // kept
		}
		newOrder = append(newOrder, root.Name)
	}
	var toClose []*os.Root
	for name, m := range old {
		if _, replaced := opened[name]; replaced {
			toClose = append(toClose, m.root) // re-pointed: drop the old handle
		} else if _, kept := newRoots[name]; !kept {
			toClose = append(toClose, m.root) // removed
		}
	}
	r.roots = newRoots
	r.order = newOrder
	r.maxBytes = maxBytes
	r.mu.Unlock()

	// Phase 4: close the retired handles with no lock held. The swap made
	// them unreachable from the map, so no new Resolve can find them, and
	// os.Root.Close is safe against a Resolve that captured a handle before
	// the swap and is opening through it now: os.Root's own reference count
	// defers the fd close until that open finishes (or fails it with
	// fs.ErrClosed), so no fd is reused under an in-flight open.
	for _, or := range toClose {
		or.Close()
	}
	return result, nil
}

// Names lists the configured root names in configuration order.
func (r *Roots) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Close releases the held root directories and clears the set, so a
// double-close is a no-op and any post-close Resolve is an ordinary
// unknown-root error rather than a use of a closed handle.
func (r *Roots) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, m := range r.roots {
		if err := m.root.Close(); err != nil && first == nil {
			first = err
		}
	}
	r.roots = nil
	r.order = nil
	return first
}

// Resolve implements Resolver for "<root>/<relative/path>" references.
// ctx is ignored: opening a local regular file does not block on
// anything cancelable.
func (r *Roots) Resolve(_ context.Context, ref string) (*File, error) {
	if ref == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "source: empty source reference")
	}
	if s, ok := scheme(ref); ok {
		return nil, unsupportedScheme(s)
	}
	name, rel, ok := strings.Cut(ref, "/")
	if !ok || rel == "" {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: reference %q is not <root>/<path>", ref))
	}

	// The read lock guards only the map lookup, the order read that builds
	// the unknown-root message, and the maxBytes capture; it is released
	// before OpenFile. The open runs unlocked because os.Root carries its
	// own reference count that makes OpenFile safe against a concurrent
	// Close (a reload re-pointing or removing this root): the open either
	// completes on the still-valid handle, or returns fs.ErrClosed if Close
	// won, never a use of a reused fd. Holding the lock across a
	// potentially slow open (a network-mounted root) would instead let one
	// slow open plus a waiting reload stall every other resolution.
	r.mu.RLock()
	m, exists := r.roots[name]
	if !exists {
		msg := fmt.Sprintf("source: unknown root %q (configured: %s)", name, strings.Join(r.order, ", "))
		r.mu.RUnlock()
		return nil, waxerr.New(waxerr.CodeNotFound, msg)
	}
	maxBytes := r.maxBytes
	r.mu.RUnlock()

	// O_NONBLOCK (unix) keeps the open itself from hanging on a FIFO; the
	// fstat below then rejects anything that is not a regular file. On a
	// regular file the flag is a no-op.
	f, err := m.root.OpenFile(rel, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, waxerr.Wrap(waxerr.CodeNotFound, "source: no such file", err)
		case errors.Is(err, fs.ErrClosed):
			// Lost a race with a reload that removed or re-pointed this root
			// between the map read and the open: the root is gone as far as
			// this request is concerned, so it reads as not-found (a retry
			// resolves against the new set).
			return nil, waxerr.Wrap(waxerr.CodeNotFound, "source: root reloaded away", err)
		case errors.Is(err, fs.ErrPermission):
			return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "source: permission denied", err)
		default:
			// os.Root escape refusals and malformed paths land here; both
			// are requests for something no valid reference names.
			return nil, waxerr.Wrap(waxerr.CodeInvalidRequest, "source: unresolvable path", err)
		}
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "source: stat", err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, waxerr.New(waxerr.CodeUnsupportedSource,
			fmt.Sprintf("source: %q is a %s, not a regular file", ref, modeWord(fi.Mode())))
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

func modeWord(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeDevice != 0:
		return "device"
	case m&fs.ModeSocket != 0:
		return "socket"
	default:
		return "special file"
	}
}
