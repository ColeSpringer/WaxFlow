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
// CBR streams carry the "Info" marker (constant rate, no seek table). VBR
// streams carry "Xing" with the frame count, stream byte count, and the
// 100-point TOC: the streaming form writes a linear TOC (the neutral guess a
// player would make anyway), and a seekable destination back-patches the
// measured one at End alongside the exact gapless trailer.
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
	h   mp3.Header          // metadata frame header (bitrate adjusted for VBR)

	// VBR seek-table samples: every strideth audio frame's stream byte
	// offset, compacted by stride doubling so memory stays bounded on
	// arbitrarily long streams.
	frameOff []int64
	stride   int

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
	// VBR selects the "Xing" metadata form (frame count, byte count, and
	// the 100-point TOC) for a variable-bit-rate stream; the default
	// "Info" form marks constant rate.
	VBR bool
}

// tocSampleCap bounds the retained frame offsets; reaching it halves the
// samples and doubles the stride. 2048 points resolve a 100-entry TOC to
// well under a percent of the stream at any length.
const tocSampleCap = 2048

// NewMuxer returns an MP3 muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, stride: 1}
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
		if m.opts.VBR {
			// A VBR first frame's rate is whatever its content picked;
			// the metadata frame instead uses the smallest legal rate
			// whose frame holds the whole Xing layout, LAME's approach
			// (deterministic, so the End back-patch matches by length).
			m.h = xingHeader(h)
		}
		delay, padding, frames := m.projectGapless()
		// A nil frame (free format, or too small for even the Xing header)
		// means no metadata frame; the audio frames stream on their own.
		if info := m.buildInfoFrame(delay, padding, frames, nil, 0); info != nil {
			if err := m.write(info); err != nil {
				return err
			}
			m.infoLen = len(info)
		}
		m.wroteInfo = true
	}
	if m.opts.VBR {
		if m.audioFrames%m.stride == 0 {
			if len(m.frameOff) == tocSampleCap {
				for i := 0; i < tocSampleCap/2; i++ {
					m.frameOff[i] = m.frameOff[2*i]
				}
				m.frameOff = m.frameOff[:tocSampleCap/2]
				m.stride *= 2
			}
			m.frameOff = append(m.frameOff, m.off)
		}
	}
	if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.audioFrames++
	return nil
}

// End back-patches the metadata frame with the encoder's exact gapless
// trailer, audio-frame count, and (VBR) the measured byte count and TOC
// when the writer is seekable.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mpa: End outside Begin")
	}
	m.ended = true
	if m.ws == nil || m.infoLen == 0 {
		return nil // no metadata frame to back-patch; the projection stands
	}
	// Rebuild the metadata frame with the exact trailer, audio-frame
	// count, and measured seek table, then patch it in place at offset 0.
	var toc []byte
	if m.opts.VBR {
		toc = m.measureTOC()
	}
	info := m.buildInfoFrame(int(trailer.Delay), int(trailer.Padding), m.audioFrames, toc, m.off)
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

// measureTOC builds the Xing 100-point seek table from the sampled frame
// offsets: entry i is the stream byte offset at time fraction i/100, as a
// 0..255 fraction of the total byte count.
func (m *Muxer) measureTOC() []byte {
	toc := make([]byte, 100)
	if len(m.frameOff) == 0 || m.off <= 0 || m.audioFrames == 0 {
		for i := range toc {
			toc[i] = byte(min(i*256/100, 255))
		}
		return toc
	}
	for i := range toc {
		frame := m.audioFrames * i / 100
		j := min(frame/m.stride, len(m.frameOff)-1)
		toc[i] = byte(min(m.frameOff[j]*256/m.off, 255))
	}
	return toc
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

// xingHeader returns h with the smallest legal bit rate whose frame holds
// the full Xing layout (frames + bytes + TOC + the LAME extension) and no
// padding, the deterministic template for a VBR stream's metadata frame.
func xingHeader(h mp3.Header) mp3.Header {
	need := mp3.HeaderLen + h.SideInfoLen() + xingLayoutLen
	for _, kbps := range legalRates(h) {
		h.Bitrate = kbps * 1000
		h.Padding = false
		if h.Size() >= need {
			return h
		}
	}
	return h // largest rate; buildInfoFrame degrades if even that is short
}

// legalRates lists the layer's bit rates in kbit/s, ascending, for the
// header's MPEG version.
func legalRates(h mp3.Header) []int {
	if h.Version == mp3.MPEG1 {
		return []int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	}
	return []int{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
}

// Xing layout offsets past the magic: the optional fields present in the
// forms this muxer writes, then the 9-byte encoder string, 12 bytes of
// LAME info fields, and the 3-byte delay/padding pack (parseVBRTag reads
// the same shape back).
const (
	xingFlagFrames = 1
	xingFlagBytes  = 2
	xingFlagTOC    = 4
	xingLayoutLen  = 4 + 4 + 4 + 4 + 100 + 9 + 12 + 3 // magic..delay/padding, VBR form
)

// buildInfoFrame constructs the leading metadata frame: a valid silent
// frame carrying the "Info" (CBR) or "Xing" (VBR) marker, the audio-frame
// count, for VBR the stream byte count and TOC, and, when it fits, a
// LAME-format extension with the gapless delay and padding at the offsets
// the demuxer reads (parseVBRTag). The header bytes come from m.h (the
// first audio frame's header; VBR swaps in xingHeader's rate). It returns
// nil when no valid frame can hold even the Xing header (free format,
// whose Size is 0, or a frame too small), so the caller skips the metadata
// frame rather than emitting a bogus one.
//
// toc is the measured 100-byte seek table (nil before End: the streaming
// form carries the linear neutral guess); bytes is the total stream size,
// 0 when unknown.
func (m *Muxer) buildInfoFrame(delay, padding, frames int, toc []byte, bytes int64) []byte {
	h := m.h
	size := h.Size()
	off := mp3.HeaderLen + h.SideInfoLen() // protection forced off: no CRC slot
	if size == 0 || off+12 > size {
		return nil
	}
	frame := make([]byte, size)
	hdr := headerBytesFor(h, m.hdr)
	copy(frame[:mp3.HeaderLen], hdr[:])
	frame[1] |= 1 // protection bit set = no CRC-16

	magic, flags := "Info", uint32(xingFlagFrames)
	if m.opts.VBR {
		magic, flags = "Xing", xingFlagFrames|xingFlagBytes|xingFlagTOC
	}
	copy(frame[off:], magic)
	binary.BigEndian.PutUint32(frame[off+4:], flags)
	p := off + 8
	binary.BigEndian.PutUint32(frame[p:], uint32(frames))
	p += 4
	if flags&xingFlagBytes != 0 {
		if p+4 > size {
			return frame
		}
		if bytes > 0 && bytes <= int64(^uint32(0)) {
			binary.BigEndian.PutUint32(frame[p:], uint32(bytes))
		}
		p += 4
	}
	if flags&xingFlagTOC != 0 {
		if p+100 > size {
			return frame
		}
		if toc == nil {
			for i := 0; i < 100; i++ {
				frame[p+i] = byte(min(i*256/100, 255))
			}
		} else {
			copy(frame[p:], toc)
		}
		p += 100
	}
	if p+24 <= size {
		copy(frame[p:], "WaxFlow01") // 9-byte encoder tag, prefix "WaxF"
		// The 12 LAME info field bytes after the tag stay zero.
		if delay >= 0 && delay < 1<<12 && padding >= 0 && padding < 1<<12 {
			frame[p+21] = byte(delay >> 4)
			frame[p+22] = byte((delay&0xF)<<4 | (padding>>8)&0xF)
			frame[p+23] = byte(padding)
		}
	}
	return frame
}

// headerBytesFor renders h's four header bytes, reusing the template's
// non-derived bits (mode, emphasis flags) and replacing the bit rate and
// padding, so a VBR metadata frame's adjusted rate lands on the wire.
func headerBytesFor(h mp3.Header, tmpl [mp3.HeaderLen]byte) [mp3.HeaderLen]byte {
	b := tmpl
	bi := 0
	for i, kbps := range legalRates(h) {
		if kbps*1000 == h.Bitrate {
			bi = i + 1
			break
		}
	}
	pad := byte(0)
	if h.Padding {
		pad = 1
	}
	b[2] = byte(bi)<<4 | b[2]&0x0C | pad<<1
	return b
}
