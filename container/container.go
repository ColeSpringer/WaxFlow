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
	"context"
	"errors"
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

// Contextual is implemented by a Source whose reads can be bound to a context,
// as a network-backed source can. io.ReaderAt has no ctx by construction, so a
// struct field is the only place one can live; this gate makes that handoff
// explicit instead of implicit. A file-backed source does not implement it, so
// the assertion is an honest capability gate rather than a universal wrapper.
//
// The ctx bound here must be the engine's, never a request's. Live pipelines
// resolve under the server's base context by design, so that read-behind can
// finish an encode after the client has left; binding a request ctx to a
// Source would make those reads die on disconnect.
//
// Bind at the outermost Source, before handing it to Open or Probe. A Source
// may be wrapped internally (skipping a leading ID3v2 tag hides the tag from
// drivers behind an offsetting wrapper), and such a wrapper carries the binding
// for free by delegating its reads inward: it need not implement Contextual
// itself, and does not. The invariant is the ordering, not the delegation. A
// wrapper that is itself handed to BindContext, rather than wrapping something
// already bound, silently drops the ctx.
type Contextual interface {
	// WithContext returns a Source whose reads honor ctx. The receiver is
	// unchanged, so a caller may hold both.
	WithContext(ctx context.Context) Source
}

// BindContext binds ctx to src when src is Contextual, and returns src
// unchanged otherwise. It is the assertion side of the Contextual gate, in one
// place so callers do not each rewrite it.
//
// Pass the owning pipeline's context, not a request's: see Contextual.
func BindContext(ctx context.Context, src Source) Source {
	if c, ok := src.(Contextual); ok {
		return c.WithContext(ctx)
	}
	return src
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

// ShortRead classifies a [ReadFull] failure on a demuxer's own structure. The
// two cases wear the same error type and mean opposite things: a source that
// ended early is a file too short for what it declares, which is damage a
// caller cannot convert away, while anything else is the bytes not being
// fetchable at all, which says nothing about the file. Wrapping both as one
// code told an operator to re-rip a healthy file whose disk had a bad day, and
// told a monitoring dashboard that a truncated upload was an I/O incident.
func ShortRead(msg string, err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return waxerr.Wrap(waxerr.CodeMalformedInput, msg, err)
	}
	return waxerr.Wrap(waxerr.CodeSourceUnreadable, msg, err)
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
	// SamplesExact marks Samples as an authoritative hard length the decoder
	// must be trimmed to, not an advisory total. Ogg-Vorbis and Ogg-Opus set
	// it (the last page granule is exact and the decoder over-produces past
	// it); formats whose declared total can lie (a bad FLAC STREAMINFO) leave
	// it false so a mismatch stays a tolerated oddity rather than a truncation.
	SamplesExact bool
	// SamplesAdvisory marks Samples as a rounded total the decode is not
	// expected to match: fit for a duration to display, unfit for arithmetic
	// that has to add up. It and SamplesExact are mutually exclusive, since
	// one says to trim the decode to this number and the other says not to
	// trust it; a measurement that sets the first clears the second. It is a claim about precision, which is what
	// SamplesExact is not (that one is a truncation instruction). WAV and FLAC
	// leave both false: their totals are counted, not rounded, and a total
	// that disagrees with the stream is damage rather than imprecision.
	//
	// Two containers state a duration in a time unit and have no sample count
	// at all: ASF, in 100-nanosecond ticks its own muxer accumulates from
	// millisecond-truncated packet times, and Matroska when it falls back to
	// the Info Duration. Measured on the ASF corpus, the declared total lands
	// on either side of the audio the packets hold, by up to a millisecond and
	// a frame; it is neither the source length nor the coded capacity, and no
	// reading of the file improves on it.
	//
	// Two things read it. A timeline refuses such a member, since a prefix sum
	// cannot survive the drift; measure it first (see ConcatSource.Track). And
	// format.Media will not cap a decode at a rounded total, which it would
	// otherwise do for a track that also signals a gapless trim.
	SamplesAdvisory bool
	// SourceBitDepth is the depth the source stores samples at when that
	// differs from Fmt.BitDepth, 0 when the two agree. Two cases reach it.
	// audio.Format carries floats as float32, so a 64-bit float source
	// decodes at BitDepth 32 and probing it reports a number the file does
	// not hold. And a WavPack stream that stripped constant zero LSBs codes
	// a narrower depth than the word width its samples come back in, so a
	// 20-bit source decodes in 24-bit words. The pipeline reads Fmt.BitDepth
	// as before; only reporting surfaces prefer this.
	SourceBitDepth int
	// Default marks the container's designated default track.
	Default bool
}

// UnusableFormat wraps an [audio.Format] Valid failure on a format a demuxer
// read out of a file, and picks the code Valid cannot.
//
// Valid answers one question with one code, and a file asks two. A format past
// this build's caps (more channels than the pipeline mixes, an integer depth
// wider than it carries) is a stream we decline: unsupported-format, and a
// caller can act on it. A rate of zero, no channels at all, or a layout that
// contradicts the channel count is the file stating something no format
// allows: malformed-input, and nobody can act on it. Valid's own
// invalid-request belongs to a caller-supplied format and never to a file's.
func UnusableFormat(prefix string, f audio.Format, err error) error {
	// Walked in the order [audio.Format.Valid] checks, so the code always
	// describes the failure the message names. Deciding from the shape alone
	// would classify {Rate: 0, Channels: 9} as unsupported while the message
	// said "rate 0 must be positive".
	code := waxerr.CodeMalformedInput
	switch {
	case f.Rate <= 0, f.Channels < 1:
	case f.Channels > audio.MaxChannels:
		code = waxerr.CodeUnsupportedFormat
	case f.Layout != 0 && f.Layout.Count() != f.Channels:
	case f.Type == audio.Int && f.BitDepth > 32:
		code = waxerr.CodeUnsupportedFormat
	}
	return waxerr.Wrap(code, prefix+": unusable format", err)
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

// Indexer is implemented by demuxers whose seeking builds an expensive
// source index (exact frame tables, seek tables) worth persisting across
// sessions: the cacheDir/idx sidecar. Both methods are cheap when there
// is nothing to do.
type Indexer interface {
	// IndexSnapshot serializes the index built so far, or nil when it is
	// not worth keeping (too small, or unchanged since RestoreIndex).
	IndexSnapshot() []byte
	// RestoreIndex adopts a previously snapshotted index and reports
	// whether the blob was accepted. Implementations validate the blob
	// against the open source and reject anything inconsistent, so a
	// stale or foreign blob degrades to a fresh walk, never to bad
	// positions.
	RestoreIndex(blob []byte) bool
}

// WarningKind separates the two things a demuxer has to say about a file it
// accepted. Strict mode escalates one and not the other, and a probe reports
// them under different names, so the kind is carried rather than inferred from
// wording.
type WarningKind int

const (
	// Damage is the file deviating from its own format in a way this demuxer
	// worked around: a truncated table, a duplicate chunk, a sample running
	// past the end. Strict mode escalates it to an error, because strict
	// exists to reject real-world mess for conformance runs.
	//
	// It is the zero value on purpose: a Warning built out of tree without
	// naming a kind lands on the side that gets escalated rather than the one
	// that is silently tolerated.
	Damage WarningKind = iota
	// Note is the file being well formed and this build doing something with
	// it a caller should know: ignoring a stream it cannot use, capping a list
	// at its own limit, rescaling a timeline, or synthesizing less than the
	// codec describes. Strict never escalates a Note, since refusing a
	// conformant file because a decoder is scoped below it would say the file
	// is broken when it is not.
	Note
)

// Warning is a structured remark about input this demuxer accepted but a
// caller should know about, surfaced through probe results. Kind says which of
// the two it is; see [WarningKind].
type Warning struct {
	// Offset is the byte position of the oddity, -1 when not localized.
	Offset int64
	Msg    string
	Kind   WarningKind
}

// Warner is implemented by demuxers that record Warnings: tolerated damage, or
// a decoder limitation against a well-formed file.
type Warner interface {
	Warnings() []Warning
}

// Muxer writes one audio track to a container. Muxers are single-track by
// design; track selection happens upstream in the engine, and End takes
// that track's Trailer for gapless finalization.
//
// A muxer whose NeedsSeek reports true requires a writer it can seek for
// header back-patching; the engine gives jobs a file and refuses live
// streams. Muxers with NeedsSeek false write a compliant stream to a plain
// io.Writer and use seekability, when present, only to improve the result
// (exact sizes instead of streaming placeholders). Seekability is probed,
// not read off the method set (see internal/muxseek).
//
// WritePacket must not retain pkt.Data past the call: it writes the payload
// through, or copies what it holds. This is the reciprocal of the Demuxer
// contract above, and remux is what makes it load-bearing rather than
// incidental. An encoder's packets are borrowed for the emit callback alone
// (see codec.Encoder), and a demuxer's are reused across ReadPacket calls, so
// a muxer feeding straight from either sees its payload overwritten under it.
// The corruption would be cross-packet and silent, which no test over a single
// packet can catch, so the rule is stated rather than left to hold by
// construction.
type Muxer interface {
	Begin(tracks []Track) error
	WritePacket(pkt Packet) error
	End(trailer codec.Trailer) error
	NeedsSeek() bool
}
