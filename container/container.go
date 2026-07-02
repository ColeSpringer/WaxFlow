// Package container defines the demuxer, muxer, and seeker interfaces and
// the track-routed packet model that wrap codec-level packets (ADR-0005).
// Import DAG: audio <- codec <- container <- format; codec never imports
// this package.
//
// Every demuxer obeys the hostile-input invariants: bounded nesting depth,
// size validation before any allocation, caps on metadata allocations, and
// a strict progress guarantee (every parse-loop iteration consumes input).
// Demuxers default tolerant of real-world mess, emitting structured
// Warnings; strict mode turns those into errors for conformance tests.
package container

import (
	"bytes"
	"io"
	"os"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

// Source is random-access input. Demuxers require io.ReaderAt because
// real files put indexes at either end (the moov-at-end reality); uploads
// and pipes spool to disk first.
type Source interface {
	io.ReaderAt
	Size() int64
}

// FileSource wraps an open file as a Source. The file must stay open for
// the Source's lifetime; the caller keeps ownership and closes it.
func FileSource(f *os.File) (Source, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "stat source", err)
	}
	return readerAtSource{f, fi.Size()}, nil
}

// BytesSource wraps an in-memory blob as a Source, mainly for tests and
// probes of spooled uploads. bytes.Reader already satisfies the interface
// (ReadAt plus Size).
func BytesSource(b []byte) Source {
	return bytes.NewReader(b)
}

type readerAtSource struct {
	io.ReaderAt
	size int64
}

func (s readerAtSource) Size() int64 { return s.size }

// ReadFull reads exactly len(p) bytes from src at off. It exists because
// the io.ReaderAt contract permits a read ending exactly at the end of
// the source to return io.EOF alongside a full buffer; demuxers that
// checked only the error would misread that as failure (or worse, as a
// clean end of stream once the io.EOF is unwrapped upstream). Full reads
// return nil, short ones io.ErrUnexpectedEOF, and real failures pass
// through.
func ReadFull(src io.ReaderAt, p []byte, off int64) error {
	n, err := src.ReadAt(p, off)
	switch {
	case n == len(p):
		return nil
	case err == nil || err == io.EOF:
		return io.ErrUnexpectedEOF
	default:
		return err
	}
}

// Track describes one elementary stream in a container.
type Track struct {
	// ID is the track identifier that packets reference: a demuxer must
	// tag every Packet.Track with the ID of the Track it belongs to.
	ID          int
	Codec       codec.ID
	CodecConfig []byte
	Fmt         audio.Format
	// Samples is the track length in samples after gapless trimming, or
	// -1 when unknown.
	Samples int64
	// Delay and Padding are the container-signaled gapless trims
	// (LAME tag, iTunSMPB, Opus pre-skip, edit lists), in samples.
	Delay   int64
	Padding int64
	// Default marks the container's designated default track.
	Default bool
}

// Packet is a codec packet routed to a track.
type Packet struct {
	Track int
	codec.Packet
}

// Demuxer yields a container's tracks and packets. ReadPacket returns
// the bare io.EOF sentinel after the last packet; consumers compare with
// ==, so wrapped errors that happen to contain io.EOF in their chain
// (an I/O failure mid-stream, say) are never mistaken for a clean end.
// Implementations may reuse pkt.Data across calls; consumers copy what
// they keep.
type Demuxer interface {
	Tracks() []Track
	ReadPacket(pkt *Packet) error
}

// Seeker is implemented by demuxers that can reposition. SeekSample lands
// on the nearest sync point at or before the target sample and returns the
// landed position; sample-exact landing is format.Media's job, via
// decode-and-discard pre-roll from there. When the stream has no sync
// point at or before the target (its first frame starts later, say after
// tolerated damage at the head), the landing is the earliest sync point
// and may exceed the target; consumers treat the returned position as
// authoritative either way.
type Seeker interface {
	SeekSample(track int, sample int64) (landed int64, err error)
}

// Warning is a structured note about tolerated input damage, surfaced
// through probe results.
type Warning struct {
	// Offset is the byte position of the oddity, -1 when not localized.
	Offset int64
	Msg    string
}

// Warner is implemented by demuxers that record tolerated damage.
type Warner interface {
	Warnings() []Warning
}

// Muxer writes one audio track to a container. Muxers are single-track by
// design; track selection happens upstream in the engine, and End takes
// that track's Trailer for gapless finalization.
//
// A muxer whose NeedsSeek reports true requires its writer to implement
// io.WriteSeeker for header back-patching; the engine gives jobs a file
// and refuses live streams. Muxers with NeedsSeek false write a compliant
// stream to a plain io.Writer and use seekability, when present, only to
// improve the result (exact sizes instead of streaming placeholders).
type Muxer interface {
	Begin(tracks []Track) error
	WritePacket(pkt Packet) error
	End(trailer codec.Trailer) error
	NeedsSeek() bool
}
