package flacn

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/muxseek"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// MuxerVersion is this muxer's term in the ADR-0004 cache key: a cached
// output of it regenerates when this bumps. Bump it for a change to what
// gets written around unchanged encoded packets, which is the one kind of
// change no encoder or DSP version can notice.
//
// flacn-mux-2: frames are numbered from 0 as they are written, so a copy of a
// stream that starts past frame 0 changes bytes.
const MuxerVersion = "flacn-mux-2"

// seekInterval is the target spacing between SEEKTABLE points in
// seconds, matching the reference encoder's default table.
const seekInterval = 10

// maxWriteSeekPoints caps the SEEKTABLE reservation (18 bytes a point;
// the cap covers 45 hours at the default spacing). Longer streams go
// without points past the cap, never a header that scales with an
// attacker-supplied Track.Samples.
const maxWriteSeekPoints = 1 << 14

// placeholderSample marks an unfilled seek point (RFC 9639 section
// 8.5); placeholders must trail real points, which holds because points
// fill in stream order.
const placeholderSample = ^uint64(0)

// seekRec is a recorded seek point: the frame's first sample, its byte
// offset from the first frame, and its length (the wire point's third
// field), all known at write time.
type seekRec struct {
	sample, off, dur int64
}

// MuxerOptions configures writing.
type MuxerOptions struct {
	// MD5 supplies the unencoded-PCM signature for the STREAMINFO
	// back-patch at End, typically the encoder's MD5 method (valid once
	// the engine has called Finish, which precedes End). When nil the
	// track's STREAMINFO signature stands; a packet-for-packet remux
	// wants exactly that, since identical audio keeps its signature. A
	// copy of a stream that starts past frame 0 hands a track whose
	// signature flac.ClearMD5 cleared, which says "not computed": the one
	// it inherits covers the stream it was cut from.
	MD5 func() [16]byte
	// Tags are written as a VORBIS_COMMENT block between STREAMINFO and
	// the audio. Vorbis comments are the canonical vocabulary already, so
	// every key passes through as KEY=value.
	Tags []container.Tag
}

// Muxer writes one FLAC track as a native FLAC stream. NeedsSeek
// reports false: a plain io.Writer receives a compliant stream whose
// STREAMINFO carries the totals known up front and zeros elsewhere. A
// destination that can seek upgrades the result: STREAMINFO is back-patched at End
// with exact totals, frame size bounds, and the MD5 signature, and a
// SEEKTABLE sized from the projected length is reserved at Begin and
// filled as frames pass.
type Muxer struct {
	w     io.Writer
	patch muxseek.Patcher
	opts  MuxerOptions

	si         flac.StreamInfo
	wroteTotal int64 // sample total written into the header at Begin
	off        int64 // bytes written so far
	firstFrame int64 // byte offset of the first audio frame

	seekOff  int64 // offset of the first seek point, 0 when absent
	points   []seekRec
	interval int64
	target   int64 // next seek point's sample threshold
	filled   int

	number *flac.Numberer // numbers the frames from 0

	minFrame, maxFrame int
	samples            int64
	began, ended       bool
	clearMD5           bool // ClearMD5 was called
}

// NewMuxer returns a FLAC muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek reports false: native FLAC has a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and writes the stream marker, STREAMINFO,
// and the SEEKTABLE reservation.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "flac: Begin called twice")
	}
	m.patch = muxseek.New(m.w, "flac")
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("flac: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.FLAC {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("flac: cannot mux codec %q", t.Codec))
	}
	if t.Delay != 0 || t.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "flac: FLAC signals no gapless trims (lossless streams have none)")
	}
	srcSI, err := flac.ParseStreamInfo(t.CodecConfig)
	if err != nil {
		return err
	}
	si := srcSI
	if want := si.PCMFormat(); t.Fmt != want {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("flac: track format %v does not match the STREAMINFO to write (want %v)", t.Fmt, want))
	}
	// The header claims the projection, never the source's STREAMINFO total.
	// The encoder cannot know the stream length, so the engine's projection
	// arrives via the track and is the only number that describes *this* run:
	// a source's own total is a claim about a different one, its own, which a
	// truncated source overstates and a cut shortens. Copying it across meant
	// a remux of a source the open had shrunk declared a count the frames
	// could not fill, and failed at End on a destination that cannot be
	// patched (the daemon's live cache entry is one).
	//
	// The 36-bit field caps what it can say; longer streams and unknown ones
	// stay 0, which every reader must handle anyway.
	si.Samples = 0
	if t.Samples > 0 && t.Samples < 1<<36 {
		si.Samples = t.Samples
	}
	m.si = si
	m.wroteTotal = si.Samples
	m.number = flac.NewNumberer(si, 0, 0)

	// The seek table is sized from the best length in hand, which is not the
	// same question as what the header may *declare*. A remux of a source whose
	// projected length is unknown (an advisory Matroska, say) still has the
	// source's own STREAMINFO total to size a table with, and sizing is a
	// capacity decision: unfilled slots keep their placeholders, which stay
	// legal. Gating the table on the projection instead would silently drop the
	// table from every such remux.
	tableSamples := si.Samples
	if tableSamples == 0 {
		tableSamples = srcSI.Samples
	}
	table := m.patch.Seekable() && tableSamples > 0
	// Before began: a Begin that refuses leaves the muxer unstarted, the way
	// every other check above it does.
	vc, err := vorbisCommentBlock(m.opts.Tags)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "flac", err)
	}
	m.began = true
	head := [4]byte{0x80} // STREAMINFO, last metadata block
	if table || vc != nil {
		head[0] = 0x00
	}
	head[1], head[2], head[3] = 0, 0, flac.StreamInfoLen
	body, err := si.MarshalBinary()
	if err != nil {
		return err
	}
	if err := m.write([]byte("fLaC"), head[:], body); err != nil {
		return err
	}

	if vc != nil {
		hdr := [4]byte{4, byte(len(vc) >> 16), byte(len(vc) >> 8), byte(len(vc))}
		if !table {
			hdr[0] |= 0x80 // last metadata block
		}
		if err := m.write(hdr[:], vc); err != nil {
			return err
		}
	}

	if table {
		m.interval = int64(seekInterval) * int64(si.Rate)
		n := (tableSamples + m.interval - 1) / m.interval
		n = min(n, maxWriteSeekPoints)
		m.points = make([]seekRec, 0, n)
		size := n * 18
		hdr := [4]byte{0x80 | 3, byte(size >> 16), byte(size >> 8), byte(size)}
		if err := m.write(hdr[:]); err != nil {
			return err
		}
		m.seekOff = m.off
		point := make([]byte, 18)
		binary.BigEndian.PutUint64(point, placeholderSample)
		for range n {
			if err := m.write(point); err != nil {
				return err
			}
		}
	}
	m.firstFrame = m.off
	return nil
}

// WritePacket appends one frame and accounts for the header patch and
// the seek table.
//
// Frames are numbered from 0 as they are written, in the stream's own blocking
// strategy, as RFC 9639 section 9.1.5 numbers a stream's. A copy of a stream
// that starts past frame 0 (what `ffmpeg -ss X -c copy` writes) would keep its
// source's numbers otherwise, which the seek table, counted from 0, would
// contradict. A frame already numbered so is written as it came.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "flac: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("flac: no track %d", pkt.Track))
	}
	if len(pkt.Data) == 0 || pkt.Dur <= 0 || pkt.Dur > flac.MaxBlockSize {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("flac: packet of %d bytes, %d samples", len(pkt.Data), pkt.Dur))
	}
	data, pos, err := m.number.Number(pkt.Data)
	if err != nil {
		return err
	}
	if m.points != nil && pos >= m.target && len(m.points) < cap(m.points) {
		m.points = append(m.points, seekRec{sample: pos, off: m.off - m.firstFrame, dur: pkt.Dur})
		m.target = (pos/m.interval + 1) * m.interval
	}
	if err := m.write(data); err != nil {
		return err
	}
	if n := len(data); n < 1<<24 {
		if m.minFrame == 0 || n < m.minFrame {
			m.minFrame = n
		}
		m.maxFrame = max(m.maxFrame, n)
	}
	m.samples += pkt.Dur
	return nil
}

// End finalizes the stream. With a seekable writer STREAMINFO and the
// seek table are back-patched exactly; with a plain writer the headers
// stand, and a known-length projection the stream missed is an error.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "flac: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != 0 || trailer.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "flac: FLAC signals no gapless trims")
	}
	if trailer.Samples >= 0 && trailer.Samples != m.samples {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("flac: trailer says %d samples, wrote %d", trailer.Samples, m.samples))
	}

	if !m.patch.Seekable() {
		if m.wroteTotal != 0 && m.wroteTotal != m.samples {
			return waxerr.New(waxerr.CodeInternal,
				fmt.Sprintf("flac: header promised %d samples, wrote %d (unseekable output)", m.wroteTotal, m.samples))
		}
		return nil
	}

	si := m.si
	si.MinFrame, si.MaxFrame = m.minFrame, m.maxFrame
	si.Samples = 0
	if m.samples < 1<<36 {
		si.Samples = m.samples
	}
	switch {
	case m.clearMD5:
		si.MD5 = [16]byte{}
	case m.opts.MD5 != nil:
		si.MD5 = m.opts.MD5()
	}
	body, err := si.MarshalBinary()
	if err != nil {
		return err
	}
	if err := m.patch.At(8, body); err != nil {
		return err
	}

	// Fill recorded seek points; the tail keeps its placeholders, which
	// stay legal (and sorted, trailing every real point).
	if len(m.points) > m.filled {
		buf := make([]byte, 18*(len(m.points)-m.filled))
		for i, p := range m.points[m.filled:] {
			b := buf[i*18:]
			binary.BigEndian.PutUint64(b, uint64(p.sample))
			binary.BigEndian.PutUint64(b[8:], uint64(p.off))
			b[16], b[17] = byte(p.dur>>8), byte(p.dur)
		}
		if err := m.patch.At(m.seekOff+int64(m.filled)*18, buf); err != nil {
			return err
		}
		m.filled = len(m.points)
	}
	return m.patch.Resume(m.off)
}

// ClearMD5 has End state no MD5 ("not computed"), for a run whose audio no
// longer matches the signature it was handed. It reaches the file where End
// can patch STREAMINFO.
func (m *Muxer) ClearMD5() { m.clearMD5 = true }

func (m *Muxer) write(parts ...[]byte) error {
	for _, p := range parts {
		n, err := m.w.Write(p)
		m.off += int64(n)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "flac: write", err)
		}
	}
	return nil
}

// maxCommentBytes bounds the VORBIS_COMMENT block body. The engine passes
// a minimal tag set; a comment past the cap is refused rather than growing
// the pre-audio headers without limit (the block length field itself only
// holds 24 bits).
const maxCommentBytes = 48 << 10

// vorbisCommentBlock renders tags as a VORBIS_COMMENT body (RFC 9639
// section 8.6: little-endian lengths, UTF-8 KEY=value comments), nil for
// no tags.
//
// A comment that does not fit is an error rather than a skip, for the reason
// apev2.Build refuses one: writing the block without it reports a tag the file
// does not carry.
func vorbisCommentBlock(tags []container.Tag) ([]byte, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	const vendor = "WaxFlow"
	body := binary.LittleEndian.AppendUint32(nil, uint32(len(vendor)))
	body = append(body, vendor...)
	countAt := len(body)
	body = binary.LittleEndian.AppendUint32(body, 0)
	count := uint32(0)
	for _, t := range tags {
		if !container.ValidTagKey(t.Key) {
			continue
		}
		c := t.Key + "=" + t.Value
		// The whole block's size, not this value's: a comment can overflow by
		// being large or by being the one that crossed a running total.
		if need := len(body) + 4 + len(c); need > maxCommentBytes {
			return nil, fmt.Errorf("%s does not fit: the VORBIS_COMMENT block would need %d bytes, over its %d-byte limit",
				t.Key, need, maxCommentBytes)
		}
		body = binary.LittleEndian.AppendUint32(body, uint32(len(c)))
		body = append(body, c...)
		count++
	}
	binary.LittleEndian.PutUint32(body[countAt:], count)
	return body, nil
}
