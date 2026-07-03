package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

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

// Roots resolves root-relative references. Confinement is precisely
// scoped: os.Root gives kernel-enforced no-escape
// including symlink traversal out of the root, and every open is
// additionally validated as a regular file via fstat (a FIFO or device
// node could hang an open or a read) and capped by maxBytes. Symlinks
// that stay within the root remain allowed; in-place libraries use them.
type Roots struct {
	maxBytes int64
	order    []string
	roots    map[string]*os.Root
}

// OpenRoots opens the named roots. maxBytes caps each resolved file's
// size; 0 means DefaultMaxBytes. Root names must be non-empty and free of
// '/' and ':' (the reference syntax's separators).
func OpenRoots(roots []Root, maxBytes int64) (*Roots, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	r := &Roots{maxBytes: maxBytes, roots: make(map[string]*os.Root, len(roots))}
	for _, root := range roots {
		if root.Name == "" || strings.ContainsAny(root.Name, "/:") {
			r.Close()
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: root name %q must be non-empty without '/' or ':'", root.Name))
		}
		if _, dup := r.roots[root.Name]; dup {
			r.Close()
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: duplicate root name %q", root.Name))
		}
		or, err := os.OpenRoot(root.Path)
		if err != nil {
			r.Close()
			return nil, waxerr.Wrap(waxerr.CodeInvalidRequest,
				fmt.Sprintf("source: opening root %q at %s", root.Name, root.Path), err)
		}
		r.roots[root.Name] = or
		r.order = append(r.order, root.Name)
	}
	return r, nil
}

// Names lists the configured root names in configuration order.
func (r *Roots) Names() []string {
	return append([]string(nil), r.order...)
}

// Close releases the held root directories.
func (r *Roots) Close() error {
	var first error
	for _, or := range r.roots {
		if err := or.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Resolve implements Resolver for "<root>/<relative/path>" references.
func (r *Roots) Resolve(ref string) (*File, error) {
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
	root, exists := r.roots[name]
	if !exists {
		return nil, waxerr.New(waxerr.CodeNotFound,
			fmt.Sprintf("source: unknown root %q (configured: %s)", name, strings.Join(r.order, ", ")))
	}

	// O_NONBLOCK (unix) keeps the open itself from hanging on a FIFO; the
	// fstat below then rejects anything that is not a regular file. On a
	// regular file the flag is a no-op.
	f, err := root.OpenFile(rel, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, waxerr.Wrap(waxerr.CodeNotFound, "source: no such file", err)
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
	if fi.Size() > r.maxBytes {
		f.Close()
		return nil, waxerr.New(waxerr.CodePayloadTooLarge,
			fmt.Sprintf("source: %d bytes exceeds the %d-byte source cap", fi.Size(), r.maxBytes))
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
