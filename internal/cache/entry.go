package cache

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// entryFile is the slice of *os.File an Entry writes and readers read.
// It is an interface so tests can inject write failures at exact offsets.
type entryFile interface {
	io.Writer
	io.ReaderAt
	io.Closer
	Sync() error
}

// ErrAbandoned stops a pipeline whose readers have all gone: a ring-fed one
// once its ring fills, a file-backed one once they have been gone for the
// store's AbandonAfter. Err reports it for an entry that abandoned.
var ErrAbandoned = errors.New("cache: all readers gone from entry")

// errClosedReader is returned from Read after Close, including the
// cross-goroutine Close that unblocks a reader whose client went away.
var errClosedReader = errors.New("cache: reader closed")

const (
	stateWriting = iota
	stateComplete
	stateFailed
)

// Entry is one in-flight or just-finished output stream. The pipeline
// appends through Write while any number of read-behind readers follow;
// everything is coordinated under one mutex and condition variable.
//
// File-backed entries (from Store.Begin) write through to the cache temp
// file. A write failure degrades the entry to a bounded in-memory ring
// retained until the slowest attached reader has consumed it: attached
// playback finishes, the entry just never promotes into the cache.
// Ring-only entries (NewMemEntry) start in that mode and carry
// uncacheable sync one-shots.
type Entry struct {
	mu   sync.Mutex
	cond sync.Cond

	store     *Store // nil for ring-only entries
	key       Key
	dir       string
	tmpPath   string
	finalPath string
	meta      Meta

	file      entryFile // nil once ring-only
	fileBytes int64     // durable bytes readable from file

	mem     []byte // ring contents, stream offsets [memBase, memBase+len)
	memBase int64
	ringCap int

	size     int64 // total readable stream bytes
	state    int
	err      error
	degraded bool // ring-only from birth or after a write failure
	promoted bool

	readers      map[*Reader]struct{}
	everAttached bool
	released     bool
	completedAt  time.Time

	abandoned bool       // nobody read it for the grace; the writer's next call fails it
	idle      *idleTimer // armed while the entry is writing with no reader
	onAbandon func()
}

// idleTimer is one arming of an entry's abandon timeout; a callback that finds
// its entry armed with another has been superseded.
type idleTimer struct{ stop func() bool }

// NewMemEntry returns a ring-only entry for uncacheable outputs. ringCap
// of 0 means DefaultRingBytes.
func NewMemEntry(ringCap int, meta Meta) *Entry {
	e := newEntry(nil, "", meta, ringCap)
	e.degraded = true
	return e
}

func newEntry(store *Store, key Key, meta Meta, ringCap int) *Entry {
	if ringCap <= 0 {
		ringCap = DefaultRingBytes
	}
	e := &Entry{
		store:   store,
		key:     Key(key),
		meta:    meta,
		ringCap: ringCap,
		readers: make(map[*Reader]struct{}),
	}
	e.cond.L = &e.mu
	return e
}

// Meta returns the entry's metadata as known at Begin (Bytes and Samples
// are finalized by Complete).
func (e *Entry) Meta() Meta {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.meta
}

// OnAbandon sets f to run once, under the entry's lock, when the entry
// abandons for want of readers. It runs whatever the writer is doing, so it
// is where a caller gives back what the run holds; f must not call into the
// entry.
func (e *Entry) OnAbandon(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onAbandon = f
}

// abandon stops the entry for want of readers: no reader attaches from here
// on, the registry forgets it, and onAbandon runs. It can land beside a Write
// that has the lock dropped, so the writer's next call fails the entry and
// releases its file. Locked.
func (e *Entry) abandon() {
	if e.abandoned {
		return
	}
	e.abandoned = true
	e.disarmIdle()
	if e.store != nil {
		e.store.unregister(e)
	}
	e.cond.Broadcast()
	if e.onAbandon != nil {
		e.onAbandon()
	}
}

// armIdle starts the abandon timeout now that the last reader has left.
// Locked.
func (e *Entry) armIdle() {
	e.disarmIdle()
	if e.store == nil || e.store.abandonAfter <= 0 || e.state != stateWriting || e.abandoned {
		return
	}
	it := &idleTimer{}
	it.stop = e.store.afterFunc(e.store.abandonAfter, func() { e.idleFire(it) })
	e.idle = it
}

// disarmIdle stops the abandon timeout. Locked.
func (e *Entry) disarmIdle() {
	if e.idle != nil {
		e.idle.stop()
		e.idle = nil
	}
}

// idleFire abandons the entry if the timeout that fired is still the one
// armed: a reader attaching and the entry ending both disarm it.
func (e *Entry) idleFire(it *idleTimer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idle == it {
		e.abandon()
	}
}

// now reads the store's clock, or the wall clock for a ring-only entry.
func (e *Entry) now() time.Time {
	if e.store != nil {
		return e.store.now()
	}
	return time.Now()
}

// Degraded reports whether the entry is (or became) ring-fed.
func (e *Entry) Degraded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.degraded
}

// FileBacked reports whether the entry started life against the cache
// (as opposed to a ring-only NewMemEntry), so callers can tell a
// degradation from a by-design ring.
func (e *Entry) FileBacked() bool { return e.store != nil }

// Write appends p to the stream (io.Writer for the muxer). It returns
// short only on a terminal condition; a cache write failure is absorbed
// by degrading to the ring, not surfaced, so the pipeline keeps going.
// The pipeline goroutine is the only writer; Write, Complete, and Fail
// are not for concurrent use with each other.
func (e *Entry) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateWriting {
		return 0, waxerr.New(waxerr.CodeInternal, "cache: write after terminal state")
	}
	if e.abandoned {
		e.fail(ErrAbandoned)
		return 0, ErrAbandoned
	}
	total := len(p)

	if !e.degraded {
		// The disk write happens outside the entry lock: readers of
		// already-published bytes must not block on the syscall. Safe
		// because only this goroutine mutates file state, readers see
		// fileBytes grow only after the relock, and release cannot fire
		// while the state is writing.
		file := e.file
		e.mu.Unlock()
		n, err := file.Write(p)
		e.mu.Lock()
		e.fileBytes += int64(n)
		e.size += int64(n)
		if err == nil {
			e.cond.Broadcast()
			return total, nil
		}
		// The cache volume failed (disk full, most likely). Downgrade to
		// ring-fed client-only streaming; playback survives, the entry
		// just never promotes.
		e.degraded = true
		e.memBase = e.size
		if e.store != nil {
			e.store.noteDegraded(e, err)
		}
		p = p[n:]
	}

	for len(p) > 0 {
		e.dropConsumed()
		space := e.ringCap - len(e.mem)
		if space == 0 {
			if e.everAttached && len(e.readers) == 0 {
				e.abandon()
				e.fail(ErrAbandoned)
				return total - len(p), ErrAbandoned
			}
			e.cond.Wait()
			if e.state != stateWriting {
				return total - len(p), waxerr.New(waxerr.CodeInternal, "cache: entry terminated under writer")
			}
			continue
		}
		n := min(space, len(p))
		e.mem = append(e.mem, p[:n]...)
		e.size += int64(n)
		p = p[n:]
		e.cond.Broadcast()
	}
	return total, nil
}

// dropConsumed advances the ring past bytes every attached reader has
// consumed. Locked. A reader still in the file region (pos below memBase)
// blocks all dropping: it will need the ring's start once it crosses over.
func (e *Entry) dropConsumed() {
	if len(e.readers) == 0 || len(e.mem) == 0 {
		return
	}
	minPos := int64(1<<63 - 1)
	for r := range e.readers {
		minPos = min(minPos, r.pos)
	}
	if minPos <= e.memBase {
		return
	}
	drop := min(minPos-e.memBase, int64(len(e.mem)))
	e.mem = e.mem[:copy(e.mem, e.mem[drop:])]
	e.memBase += drop
}

// Complete finalizes the stream. File-backed entries promote into the
// cache (sync, atomic rename, meta.json); degraded and ring-only entries
// just mark the end for their readers. samples is the finished output
// length for duration metadata.
func (e *Entry) Complete(samples int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateWriting {
		return waxerr.New(waxerr.CodeInternal, "cache: Complete after terminal state")
	}
	if e.abandoned {
		e.fail(ErrAbandoned)
		return ErrAbandoned
	}
	e.disarmIdle()
	e.meta.Bytes = e.size
	e.meta.Samples = samples

	if !e.degraded && e.store != nil {
		// Promotion (whole-file fsync, renames, meta write) runs with
		// the lock released: attached readers keep draining through the
		// open fd instead of stalling behind the fsync, and release
		// cannot fire because the state is still writing. The meta
		// snapshot keeps promote off shared fields.
		meta := e.meta
		e.mu.Unlock()
		err := e.store.promote(e, &meta)
		e.mu.Lock()
		if err != nil {
			// Promotion failed (rename or meta write): readers still have
			// every byte through the open fd, so playback finishes; the
			// entry is dropped like a degraded one afterward.
			e.degraded = true
			e.store.noteDegraded(e, err)
		} else {
			e.meta = meta
			e.promoted = true
		}
	}
	e.state = stateComplete
	if e.store != nil {
		e.store.unregister(e)
	}
	e.completedAt = e.now()
	e.cond.Broadcast()
	e.maybeRelease()
	return nil
}

// Err returns the failure that terminated the entry, or nil while it is
// healthy. Handlers attach late (the flight function hands them an entry
// that may have already failed and released); Err lets them surface the
// pipeline's real error instead of retrying blindly.
func (e *Entry) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.abandoned {
		return ErrAbandoned
	}
	if e.state == stateFailed {
		return e.err
	}
	return nil
}

// Fail aborts the stream: attached readers drain what is readable and
// then receive err; nothing promotes.
func (e *Entry) Fail(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fail(err)
}

// fail is Fail with the lock held.
func (e *Entry) fail(err error) {
	if e.state != stateWriting {
		return
	}
	e.state = stateFailed
	e.err = err
	e.disarmIdle()
	if e.store != nil {
		e.store.unregister(e)
	}
	e.cond.Broadcast()
	e.maybeRelease()
}

// NewReader attaches a read-behind reader at stream offset zero. It fails
// only when the entry has already been released (its backing resources
// are gone); the caller then re-runs the cache lookup, which finds the
// promoted entry.
func (e *Entry) NewReader() (*Reader, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.released {
		return nil, waxerr.New(waxerr.CodeInternal, "cache: entry already released")
	}
	if e.abandoned {
		return nil, waxerr.New(waxerr.CodeInternal, "cache: entry abandoned")
	}
	// Once a degraded ring has dropped consumed bytes, offset zero is
	// gone; a late attacher (raced against the degradation) must start
	// its own session instead.
	if e.degraded && e.memBase > e.fileBytes {
		return nil, waxerr.New(waxerr.CodeInternal, "cache: degraded entry no longer joinable")
	}
	r := &Reader{e: e}
	e.readers[r] = struct{}{}
	e.everAttached = true
	e.disarmIdle()
	return r, nil
}

// maybeRelease frees backing resources once the entry is terminal with no
// attached readers. Locked. Promoted entries only close the temp fd (the
// promoted file serves future requests); everything else removes its
// debris.
func (e *Entry) maybeRelease() {
	if e.released || e.state == stateWriting || len(e.readers) != 0 {
		return
	}
	e.released = true
	if e.file != nil {
		e.file.Close()
		e.file = nil
	}
	if e.store != nil && !e.promoted {
		e.store.dropAborted(e.dir, e.tmpPath)
	}
	e.mem = nil
}

// Reader is one read-behind consumer of an Entry. It blocks in Read until
// bytes append, the entry completes (io.EOF), the entry fails (the
// pipeline's error), or Close is called from any goroutine (the handler's
// context watcher uses that to unblock a reader whose client is gone).
type Reader struct {
	e      *Entry
	pos    int64
	closed bool
}

// Read implements io.Reader with read-behind blocking semantics.
func (r *Reader) Read(p []byte) (int, error) {
	e := r.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.closed {
			return 0, errClosedReader
		}
		if r.pos < e.size {
			if e.file != nil && r.pos < e.fileBytes {
				// The disk read happens outside the entry lock: a cold
				// ReadAt must stall neither the encoder nor the other
				// readers. Safe because ReadAt is position-independent,
				// fileBytes only grows, and os.File guards concurrent
				// Close (a release racing this read returns ErrClosed,
				// caught by the closed re-check below).
				want := int(min(int64(len(p)), e.fileBytes-r.pos))
				file := e.file
				pos := r.pos
				e.mu.Unlock()
				n, err := file.ReadAt(p[:want], pos)
				e.mu.Lock()
				if r.closed {
					return 0, errClosedReader
				}
				r.pos += int64(n)
				if n > 0 {
					return n, nil
				}
				if err != nil && err != io.EOF {
					return 0, err
				}
				// Unreachable by the ReaderAt contract while pos is below
				// fileBytes; fail loudly rather than risk a hot loop.
				return 0, io.ErrNoProgress
			}
			// Ring region. The retention invariant keeps memBase at or
			// below every attached reader's position.
			off := int(r.pos - e.memBase)
			avail := len(e.mem) - off
			if avail <= 0 || off < 0 {
				return 0, waxerr.New(waxerr.CodeInternal, "cache: ring retention invariant broken")
			}
			n := copy(p, e.mem[off:])
			r.pos += int64(n)
			e.cond.Broadcast() // the writer may be waiting for ring space
			return n, nil
		}
		switch e.state {
		case stateComplete:
			return 0, io.EOF
		case stateFailed:
			return 0, e.err
		}
		e.cond.Wait()
	}
}

// Close detaches the reader and unblocks a pending Read. Safe to call
// from a goroutine other than the reading one, and more than once.
func (r *Reader) Close() error {
	e := r.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	delete(e.readers, r)
	if len(e.readers) == 0 {
		e.armIdle()
	}
	e.cond.Broadcast()
	e.maybeRelease()
	return nil
}
