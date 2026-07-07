package mpa

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// Muxer writes one MP3 track as a bare Layer III elementary stream led by a
// Xing/Info metadata frame with a LAME-format gapless extension. NeedsSeek
// reports false: the leading frame is written on the first packet using the
// engine's projected sample count, so a plain io.Writer already carries exact
// gapless trims for a known-length transcode. An io.WriteSeeker refines them
// at End with the encoder's exact trailer, covering the unknown-length case.
//
// The metadata frame is a self-contained silent frame; the encoder's first
// audio frame references no reservoir before itself, so prepending it never
// disturbs the audio frames' back-references. Decoders (this package's
// demuxer, ffmpeg, browsers) recognize the tag and skip the frame as audio,
// so the gapless delay and padding apply to the audio frames alone.
type Muxer struct {
	w    io.Writer
	ws   io.WriteSeeker // nil when w cannot seek
	opts MuxerOptions

	samples int64 // engine's projected input sample count, -1 unknown
	rate    int
	off     int64 // bytes written
	infoLen int   // metadata frame length, for the End back-patch

	hdr [mp3.HeaderLen]byte // first frame's header, the metadata frame template
	h   mp3.Header

	audioFrames  int
	began, ended bool
	wroteInfo    bool
}

// MuxerOptions configures writing.
type MuxerOptions struct {
	// Delay is the encoder's gapless delay in samples (mp3.Encoder.Delay),
	// used to size the metadata frame's LAME extension before the exact
	// trailer arrives at End. Zero is a valid delay (a packet remux).
	Delay int
}

// NewMuxer returns an MP3 muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w}
	if ws, ok := w.(io.WriteSeeker); ok {
		m.ws = ws
	}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek reports false: the elementary stream has a compliant streaming
// form and the gapless tag is written up front from the projected length.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and records the projected length; the metadata
// frame is deferred to the first packet, whose header is its template.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "mpa: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mpa: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.MP3 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("mpa: cannot mux codec %q", t.Codec))
	}
	m.samples = t.Samples
	m.rate = t.Fmt.Rate
	m.began = true
	return nil
}

// WritePacket writes the metadata frame (once, from the first packet's
// header) followed by the audio frame.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mpa: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mpa: no track %d", pkt.Track))
	}
	if len(pkt.Data) < mp3.HeaderLen {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mpa: packet of %d bytes", len(pkt.Data)))
	}
	if !m.wroteInfo {
		h, err := mp3.ParseHeader(pkt.Data)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInternal, "mpa: first packet header", err)
		}
		copy(m.hdr[:], pkt.Data[:mp3.HeaderLen])
		m.h = h
		delay, padding, frames := m.projectGapless()
		// A nil frame (free format, or too small for even the Xing header)
		// means no metadata frame; the audio frames stream on their own.
		if info := buildInfoFrame(m.hdr[:], h, delay, padding, frames); info != nil {
			if err := m.write(info); err != nil {
				return err
			}
			m.infoLen = len(info)
		}
		m.wroteInfo = true
	}
	if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.audioFrames++
	return nil
}

// End back-patches the metadata frame with the encoder's exact gapless
// trailer and audio-frame count when the writer is seekable.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mpa: End outside Begin")
	}
	m.ended = true
	if m.ws == nil || m.infoLen == 0 {
		return nil // no metadata frame to back-patch; the projection stands
	}
	// Rebuild the metadata frame with the exact trailer and audio-frame
	// count, then patch it in place at offset 0.
	info := buildInfoFrame(m.hdr[:], m.h, int(trailer.Delay), int(trailer.Padding), m.audioFrames)
	if info == nil || len(info) != m.infoLen {
		return nil // unbuildable now (should not happen); leave the projection
	}
	if _, err := m.ws.Seek(0, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mpa: seek for patch", err)
	}
	if _, err := m.ws.Write(info); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mpa: patch", err)
	}
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mpa: seeking to end", err)
	}
	return nil
}

// projectGapless computes the metadata frame's gapless fields from the
// engine's projected length before the exact trailer is known. An unknown
// length yields delay-only trims (padding cannot be projected).
func (m *Muxer) projectGapless() (delay, padding, frames int) {
	delay = m.opts.Delay
	if m.samples < 0 {
		return delay, 0, 0
	}
	frames = mp3.FramesFor(m.samples, m.rate)
	total := int64(frames) * int64(m.h.SamplesPerFrame())
	if p := total - m.samples - int64(delay); p > 0 {
		padding = int(p)
	}
	return delay, padding, frames
}

func (m *Muxer) write(b []byte) error {
	n, err := m.w.Write(b)
	m.off += int64(n)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mpa: write", err)
	}
	return nil
}

// buildInfoFrame constructs the leading Xing/Info metadata frame: a valid
// silent frame carrying the CBR "Info" marker, the audio-frame count, and,
// when it fits, a LAME-format extension with the gapless delay and padding at
// the offsets the demuxer reads (parseVBRTag). The header bytes are copied
// from the first audio frame so version, rate, channels, and bit rate match.
// It returns nil when no valid frame can hold even the Xing header (free
// format, whose Size is 0, or a frame too small), so the caller skips the
// metadata frame rather than emitting a bogus one.
//
// The protection bit is forced off: we compute no CRC-16, so the metadata
// frame must not claim one (an external protected source would otherwise
// leave an invalid, unwritten CRC slot). The frame is always big enough for
// the "Info" marker, so even the smallest legal frames are recognized as
// metadata and skipped by decoders; only the gapless extension drops out on
// the very lowest bit rates, degrading to no trims rather than a decoded
// frame of silence.
func buildInfoFrame(hdr []byte, h mp3.Header, delay, padding, frames int) []byte {
	size := h.Size()
	off := mp3.HeaderLen + h.SideInfoLen() // protection forced off: no CRC slot
	if size == 0 || off+12 > size {
		return nil
	}
	frame := make([]byte, size)
	copy(frame[:mp3.HeaderLen], hdr[:mp3.HeaderLen])
	frame[1] |= 1 // protection bit set = no CRC-16

	copy(frame[off:], "Info")                    // CBR: constant bit rate marker
	binary.BigEndian.PutUint32(frame[off+4:], 1) // flags: frame count present
	binary.BigEndian.PutUint32(frame[off+8:], uint32(frames))
	if off+36 <= size {
		copy(frame[off+12:], "WaxFlow01") // 9-byte encoder tag, prefix "WaxF"
		// Bytes off+21..off+32 (LAME info fields) stay zero.
		if delay >= 0 && delay < 1<<12 && padding >= 0 && padding < 1<<12 {
			frame[off+33] = byte(delay >> 4)
			frame[off+34] = byte((delay&0xF)<<4 | (padding>>8)&0xF)
			frame[off+35] = byte(padding)
		}
	}
	return frame
}
