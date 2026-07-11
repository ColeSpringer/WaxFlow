// Package source resolves source references onto opened, validated files.
//
// A reference names a transcode input in one of three forms:
//
//	<root>/<relative/path>   a file under a configured library root
//	upload:<id>              a spooled one-shot upload (with the job store)
//	pid:<ULID>               a WaxBin catalog identifier (resolver flavor)
//
// The Resolver interface is public so the nested resolver module can
// implement the pid form against a WaxBin catalog; the main module ships
// Roots, which serves the first form and rejects the schemes it does not
// know with waxerr.CodeUnsupportedSource (HTTP 501).
//
// Every resolved file carries an Identity (size plus mtime in
// nanoseconds), the same identity that signed URLs embed (ADR-0003) and
// cache keys hash (ADR-0004): if the bytes behind a reference change, the
// identity changes with them.
package source

import (
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Identity pins the exact bytes a reference resolved to: file size plus
// modification time in nanoseconds. Content hashing was rejected (ADR-0003):
// hashing a 300 MB FLAC on first request defeats the time-to-first-audio
// budget; mtime granularity is the documented residual risk.
type Identity struct {
	Size    int64
	MtimeNS int64
}

// String renders the identity in the canonical "size-mtimeNS" form used
// in signed URLs and cache keys.
func (id Identity) String() string {
	return strconv.FormatInt(id.Size, 10) + "-" + strconv.FormatInt(id.MtimeNS, 10)
}

// ParseIdentity parses the canonical String form. Errors carry
// waxerr.CodeInvalidRequest.
func ParseIdentity(s string) (Identity, error) {
	sizeStr, mtimeStr, ok := strings.Cut(s, "-")
	if !ok {
		return Identity{}, waxerr.New(waxerr.CodeInvalidRequest, "source: identity is not size-mtimeNS")
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || size < 0 {
		return Identity{}, waxerr.New(waxerr.CodeInvalidRequest, "source: malformed identity size")
	}
	mtime, err := strconv.ParseInt(mtimeStr, 10, 64)
	if err != nil {
		return Identity{}, waxerr.New(waxerr.CodeInvalidRequest, "source: malformed identity mtime")
	}
	return Identity{Size: size, MtimeNS: mtime}, nil
}

// File is an opened, validated source. It implements container.Source
// (ReadAt plus Size) for the demuxers and exposes the underlying
// io.ReadSeeker for direct play, where the original bytes are served
// verbatim with HTTP range support.
type File struct {
	// Ref is the reference this file resolved from.
	Ref string
	// Ext is the lower-case extension without the dot, the format
	// sniffer's tiebreak hint.
	Ext string
	// ID is the resolved identity, captured at open.
	ID Identity

	f *os.File
}

// ReadAt implements container.Source.
func (f *File) ReadAt(p []byte, off int64) (int, error) { return f.f.ReadAt(p, off) }

// Size implements container.Source, from the identity captured at open.
func (f *File) Size() int64 { return f.ID.Size }

// ReadSeeker exposes the open file for http.ServeContent (direct play).
// It shares state with ReadAt callers only in position-independent ways:
// ReadAt never moves the seek offset.
func (f *File) ReadSeeker() io.ReadSeeker { return f.f }

// ModTime returns the identity mtime as a time, for HTTP validators.
func (f *File) ModTime() time.Time { return time.Unix(0, f.ID.MtimeNS) }

// Close releases the underlying file.
func (f *File) Close() error { return f.f.Close() }

var _ container.Source = (*File)(nil)

// Resolver opens the file behind a source reference. Implementations
// validate what they open (regular file, size cap) and reject references
// they do not serve with waxerr.CodeUnsupportedSource; unknown names and
// paths carry waxerr.CodeNotFound.
//
// Resolve carries no context: root and upload resolution are local file
// opens. Implementations that reach further (the WaxBin catalog
// resolver) bound their own queries with internal timeouts, at the cost
// of not observing request cancellation, a known limitation of this
// signature.
type Resolver interface {
	Resolve(ref string) (*File, error)
}

// scheme splits a reference of the form "<scheme>:<rest>" where the colon
// appears before any slash; root-relative paths may contain colons inside
// path segments.
func scheme(ref string) (string, bool) {
	head := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		head = ref[:i]
	}
	s, _, ok := strings.Cut(head, ":")
	return s, ok
}

// unsupportedScheme maps known-but-unavailable schemes onto precise
// errors, so the envelope tells the caller what is missing rather than
// guessing at typos.
func unsupportedScheme(s string) error {
	switch s {
	case "upload":
		return waxerr.New(waxerr.CodeUnsupportedSource, "source: upload references need the daemon's upload spool (uploads are disabled here)")
	case "pid":
		return waxerr.New(waxerr.CodeUnsupportedSource, "source: pid references require the WaxBin resolver flavor")
	default:
		return waxerr.New(waxerr.CodeUnsupportedSource, fmt.Sprintf("source: unknown source scheme %q", s))
	}
}

// extHint extracts the extension hint from a relative path.
func extHint(rel string) string {
	return strings.TrimPrefix(strings.ToLower(path.Ext(rel)), ".")
}

// OpenLocal opens a trusted local path (the upload spool) as a resolved
// File with the same regular-file validation as root resolution. ref is
// the reference the file answers for ("upload:<id>") and name supplies
// the extension hint (the client's original filename); path confinement
// is the caller's job, since the spool directory is daemon-owned, not a
// user-named root.
func OpenLocal(ref, path, name string) (*File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeNotFound, "source: no such file", err)
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
	return &File{
		Ref: ref,
		Ext: extHint(name),
		ID:  Identity{Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()},
		f:   f,
	}, nil
}
