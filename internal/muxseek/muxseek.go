// Package muxseek holds the muxers' shared back-patch convention. A container
// header states sizes, durations, and checksums that are only known once the
// bytes they cover have gone out, so the muxer writes a placeholder and seeks
// back to correct it at End.
//
// Every offset a Patcher takes is into the stream, where byte 0 is the muxer's
// first byte, and so is every offset a container states about itself. The
// output is therefore the byte range the muxer wrote: given a destination it
// did not start, its bytes are a file once extracted, not in place.
//
// Seekability is probed rather than read off the method set, since *os.File
// carries io.WriteSeeker for a pipe too and a muxer that trusted the type
// would promise in its header what the first patch then fails to deliver. The
// package sits outside container/ because the engine's pre-flight check needs
// the same probe.
package muxseek

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/waxerr"
)

// Patcher rewrites bytes a muxer has already written.
type Patcher struct {
	ws   io.WriteSeeker
	base int64 // where the stream starts in the destination
	name string
}

// New probes w and returns the patcher for it. Muxers call it from Begin, not
// from their constructor: the base it records is where the destination sits
// now, so probing earlier would fix it before the caller had finished
// positioning. name prefixes the errors and is the muxer's own name for itself
// ("wav", "flac").
func New(w io.Writer, name string) Patcher {
	ws, ok := w.(io.WriteSeeker)
	if !ok {
		return Patcher{name: name}
	}
	at, err := ws.Seek(0, io.SeekCurrent)
	if err != nil || at < 0 {
		return Patcher{name: name}
	}
	return Patcher{ws: ws, base: at, name: name}
}

// CanSeek reports whether w is a destination a Patcher could rewrite, for a
// caller that wants to refuse a doomed output before any work starts on it.
func CanSeek(w io.Writer) bool { return New(w, "").Seekable() }

// Seekable reports whether the destination can be patched. A muxer that gets
// false writes the form of its container that needs no patch, or refuses.
func (p Patcher) Seekable() bool { return p.ws != nil }

// Field returns a patcher whose errors name the field being rewritten, for a
// muxer with several patch sites worth telling apart.
func (p Patcher) Field(name string) Patcher {
	p.name += ": " + name
	return p
}

// At rewrites the bytes at a stream offset and leaves the write position
// there; Resume is what puts the writer back at the stream's end.
func (p Patcher) At(off int64, parts ...[]byte) error {
	if err := p.seek(off, "seek for patch"); err != nil {
		return err
	}
	for _, b := range parts {
		if _, err := p.ws.Write(b); err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, p.name+": patch", err)
		}
	}
	return nil
}

// Resume leaves the writer at a stream offset, which callers pass the count of
// bytes they have written: the end of the stream, where writing continues. On
// a destination that cannot seek it does nothing, since nothing moved the
// writer off the end in the first place.
func (p Patcher) Resume(off int64) error {
	if p.ws == nil {
		return nil
	}
	return p.seek(off, "seeking to end")
}

// seek positions the destination at a stream offset. A muxer that reached here
// without a seekable destination, or with the -1 that stands for a slot it
// never reserved, has a bug: the output fails rather than the seek landing
// somewhere arbitrary.
func (p Patcher) seek(off int64, what string) error {
	if p.ws == nil {
		return waxerr.New(waxerr.CodeInternal, p.name+": patch on a destination that cannot seek")
	}
	if off < 0 {
		return waxerr.New(waxerr.CodeInternal, p.name+": patch at a negative stream offset")
	}
	// The return says where the destination now is, and a writer that reports
	// somewhere else would have every later byte land somewhere else too.
	at, err := p.ws.Seek(p.base+off, io.SeekStart)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, p.name+": "+what, err)
	}
	if at != p.base+off {
		return waxerr.New(waxerr.CodeOutputUnwritable,
			fmt.Sprintf("%s: seek to %d landed at %d", p.name, p.base+off, at))
	}
	return nil
}
