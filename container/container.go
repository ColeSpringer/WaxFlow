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
	// (LAME tag, iTunSMPB, Opus pre-skip, edit lists), in samples. For a
	// container that states its trims per packet (Packet.Padding), Padding is
	// the *last* packet's, which is the only one an end trim can mean.
	Delay   int64
	Padding int64
	// MidPadding is the samples a walk found trimmed inside the run, by a
	// per-packet trim on packets before the last (Packet.Padding). It is zero
	// until a walk has run and on every container that states no trim per
	// packet, which is all of them but Matroska.
	//
	// It is what a packet copy has to know and a trailer cannot say. The
	// delivered audio is already right either way (a reader drops each
	// packet's own trim in place), and the trims are already out of Samples,
	// so nothing about the decoded stream needs this. What needs it is a rung
	// that moves packets instead of samples: an output container carries one
	// end trim, so a copy into anything but Matroska would play the trimmed
	// frames back as audio. The copy rung declines on it for every other
	// destination, and a cut carries the same answer for the trims its kept
	// packets hold (see MidTrims); the transcode rung, which trims in PCM,
	// serves what they decline.
	MidPadding int64
	// MidTrims lists the trims MidPadding sums, one per packet in stream
	// order, each placed on the raw decode timeline (see PacketTrim). A cut
	// needs the positions and a copy does not: past an inner trim every packet
	// starts at a grid position minus the trims so far, so a window edge
	// computed on the track's own timeline lands inside a packet unless the
	// trims before it are added back, which is what the list is for.
	//
	// A walk records a bounded number of them and keeps summing MidPadding
	// past that, so a list whose samples add up to less than MidPadding is
	// incomplete; MidTrimsComplete says which. Nil until a walk has run.
	MidTrims []PacketTrim
	// SamplesExact marks Samples as an authoritative length rather than a
	// declared or advisory one: something read the payload and this is what it
	// found. Where the decoder over-produces past it (Ogg-Vorbis and Ogg-Opus,
	// whose last page granule is the playable end) it is also a truncation
	// instruction, and format.Media caps the decode there.
	//
	// It is set by a measurement, never by a header alone. A demuxer that
	// verifies its declared total against the payload at open sets it (flacn
	// and wv scan their tail for the closing frame, Ogg-FLAC reads the final
	// page granule), and one that defers that work sets it when Walk finishes.
	// A total nothing checked leaves it false, so a mismatch stays a tolerated
	// oddity rather than a truncation.
	SamplesExact bool
	// SamplesAdvisory marks Samples as a total the decode is not expected to
	// match: fit for a duration to display, unfit for arithmetic that has to
	// add up. It and SamplesExact are mutually exclusive, since one says to
	// trim the decode to this number and the other says not to trust it; a
	// measurement that sets the first clears the second. It is a claim about
	// precision, which is what SamplesExact is not (that one is a truncation
	// instruction). An MP3's Xing count leaves both false: it is declared, not
	// estimated, and one that disagrees with the frames is damage rather than
	// imprecision. A WAV whose payload is byte-linear, and a FLAC or WavPack
	// whose open verified its declared total against the payload, are exact
	// outright.
	//
	// Four shapes reach it, for three different reasons. Two containers state a
	// duration in a time unit and have no sample count at all: ASF, in
	// 100-nanosecond ticks its own muxer accumulates from
	// millisecond-truncated packet times, and Matroska when it falls back to
	// the Info Duration. Measured on the ASF corpus, the declared total lands
	// on either side of the audio the packets hold, by up to a millisecond and
	// a frame; it is neither the source length nor the coded capacity, and no
	// reading of the file improves on it.
	//
	// The third is a count that is exact about the wrong thing: a WAV carrying
	// MP3 frames states its length in a fact chunk, which counts everything
	// those frames decode to, encoder delay and tail padding included, while
	// nothing in the file says where inside them the audio begins or ends. The
	// number is not rounded and it is not the track's length either, which is
	// the same practical answer: display it, do not sum it.
	//
	// The fourth is the second shape as any caller meets it: a Matroska Opus
	// or Vorbis track at open, whose exact total is a cluster walk away. No
	// open pays that walk, since the tail trim rides on the block that carries
	// it (see Packet.Padding), so the read is gapless either way and only the
	// number waits for Walk. A file with no Info Duration to fall back on
	// reports -1 with neither flag rather than an estimate it does not have.
	//
	// The fifth is a fragmented MP4's header duration, which is the same
	// reason in a third spelling: mvhd, mehd and mdhd state a duration about
	// the presentation rather than about the fragments, and every ffmpeg
	// fragmented shape writes zero there, so a writer that fills one (Smooth
	// Streaming, YouTube's itag 140) is making a claim nothing in the file
	// checks. A segment index is not advisory, because it counts subsegments
	// of this file; it is declared, like a sample table. Walk settles either.
	//
	// Three things read it. A timeline refuses such a member, since a prefix
	// sum cannot survive the drift; measure it first (see ConcatSource.Track).
	// format.Media will not cap a decode at a rounded total, which it would
	// otherwise do for a track that also signals a gapless trim. And a
	// transcode projects no length into its muxer's headers from one, since a
	// projection the encoder then misses is a write failure.
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

// PacketTrim is one trim inside a run: a Packet.Padding on a packet something
// followed, placed on the raw decode timeline, which is the sum of every
// packet's Dur and the timeline a packet grid is laid on. The trimmed samples
// occupy [Pos, Pos+Samples), the tail of the packet ending at Pos+Samples, so
// a trim never crosses a packet boundary and Samples never exceeds the packet.
type PacketTrim struct {
	Pos     int64
	Samples int64
}

// MidTrimsComplete reports whether MidTrims accounts for the whole of
// MidPadding: every inner trim the walk summed is listed with its position.
// A walk that hit its recording bound leaves the list short, and a track
// nothing walked has no list at all; both say the same thing to a cut, which
// cannot place windows around trims it cannot see.
func (t Track) MidTrimsComplete() bool {
	var n int64
	for _, tr := range t.MidTrims {
		n += tr.Samples
	}
	return n == t.MidPadding
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
	// Padding is the number of samples at the end of this packet's decoded
	// output that are not audio, stated by the container per packet
	// (Matroska's DiscardPadding). A reader drops them from that packet's
	// output, so delivery is gapless with no total in hand.
	//
	// The timeline excludes them: the next packet starts at
	//
	//	PTS + Dur - min(Padding, Dur)
	//
	// so a position a demuxer reports, a cluster anchor a walk stamps, and a
	// landing a seek returns are all on the timeline the reader delivers, and
	// a seek across an inner trim lands where it says. Dur stays the frame's
	// own decoded length, which is what a decoder produces before the trim.
	// A walk's raw total counts the *final* packet's trim and no other, since
	// that one is the track's end trim and SettleLength takes it off again.
	//
	// A muxer writes it only if its container states trims per packet
	// (Matroska's does); the rest never see one, because the copy rungs
	// decline before a packet carrying an inner trim moves (see
	// Track.MidPadding).
	Padding int64
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

// Walker is implemented by demuxers whose open defers a walk of the payload:
// a frame index built lazily (MP3, bare or inside a WAV or an AIFF-C; ADTS),
// a cluster walk behind an advisory length (Matroska), or a fragmented MP4's
// run of moof headers behind whatever its head declared. Walk finishes that
// walk through the owner's normal warn path, so under Strict damage past the
// head becomes the malformed error and under tolerance it lands in Warnings.
// Walked reports whether the walk has run to the end of the payload: a run
// read to its end, a restored complete index, a walk taken at open. A walk
// that ended early, refused under Strict or stopped by a read failure, is
// not finished, and Walk called again stops at the same place. A finished
// walk has measured the payload, and the track reports that length. A
// strict probe calls Walk and the daemon's job gate reads Walked; they are
// two views of one state on the same types, which is why one interface
// carries both. A demuxer that confirms everything at open does not
// implement it, and Open never calls Walk, and no open reads a payload: a
// Matroska track's tail trim rides on the block that carries it
// (Packet.Padding), so its exact total can wait for Walk. A read finds damage
// where it lies. Walk leaves the packet position where it was.
//
// A Walk after a complete restore costs nothing and still settles: the
// sidecar's index already reaches the end, so the walk has nothing to do and
// only the measurement it implies is applied.
type Walker interface {
	Walk() error
	Walked() bool
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
