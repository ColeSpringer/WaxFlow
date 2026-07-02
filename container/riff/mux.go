package riff

import (
	"fmt"
	"io"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// MuxerOptions configures writing.
type MuxerOptions struct {
	// SizeLimit overrides RIFF's 32-bit size ceiling (default
	// 0xFFFFFFFF). Above it the muxer writes RF64. Tests shrink it to
	// exercise the RF64 path without 4 GiB files.
	SizeLimit int64
}

// Muxer writes one PCM track as a WAV file. NeedsSeek reports false: a
// plain io.Writer receives a compliant stream (RIFF with streaming-size
// placeholders when the length is unknown, RF64 when a known length
// projects past the RIFF limit). An io.WriteSeeker upgrades the result:
// headers are back-patched with exact sizes at End, including a
// RIFF-to-RF64 rewrite when the output turned out to cross the limit
// (the 28-byte JUNK reservation becomes the ds64 chunk).
type Muxer struct {
	w     io.Writer
	ws    io.WriteSeeker // nil when w cannot seek
	limit int64

	cfg        pcm.Config
	fmt        audio.Format
	frameBytes int
	projected  int64 // projected data bytes from Track.Samples, -1 unknown

	rf64    bool
	off     int64 // bytes written so far
	junkOff int64 // offset of the JUNK/ds64 chunk header, 0 if absent
	factOff int64 // offset of the fact chunk payload, 0 if absent
	dataOff int64 // offset of the data chunk header

	frames int64
	began  bool
	ended  bool
}

// NewMuxer returns a WAV muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, limit: size32Unknown}
	if ws, ok := w.(io.WriteSeeker); ok {
		m.ws = ws
	}
	if opts != nil && opts.SizeLimit > 0 {
		m.limit = opts.SizeLimit
	}
	return m
}

// NeedsSeek reports false: WAV has a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and writes headers.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "riff: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("riff: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.PCM {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("riff: cannot mux codec %q (WAV holds PCM)", t.Codec))
	}
	if t.Delay != 0 || t.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "riff: WAV cannot signal gapless trims")
	}
	cfg, err := pcm.ParseConfig(t.CodecConfig)
	if err != nil {
		return err
	}
	if err := m.checkWireConfig(cfg); err != nil {
		return err
	}
	if err := t.Fmt.Valid(); err != nil {
		return err
	}
	if t.Fmt.Rate > size32Unknown {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("riff: rate %d does not fit WAV", t.Fmt.Rate))
	}
	m.cfg = cfg
	m.fmt = t.Fmt
	m.frameBytes = cfg.BytesPerFrame(t.Fmt.Channels)

	m.projected = -1
	if t.Samples >= 0 {
		// The projection arithmetic below must not overflow int64: our
		// demuxers cap Samples at the source file size, but Track is
		// public API, so a nonsense length fails closed instead of
		// wrapping negative and skipping the RF64 upgrade. 4096 covers
		// every header shape plus the pad byte with room to spare.
		if t.Samples > (math.MaxInt64-4096)/int64(m.frameBytes) {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("riff: track length %d samples overflows the size projection", t.Samples))
		}
		m.projected = t.Samples * int64(m.frameBytes)
	}
	// Auto-select RF64 when a known length projects past the RIFF limit.
	// The projection includes the ds64 region, the worst-case header.
	if m.projected >= 0 {
		projRiff := m.projected + m.projected&1 + m.headerOverhead(true) - 8
		m.rf64 = projRiff > m.limit || m.projected > m.limit
	}

	m.began = true
	return m.writeHeaders()
}

// checkWireConfig rejects wire encodings WAV cannot hold.
func (m *Muxer) checkWireConfig(cfg pcm.Config) error {
	bad := func(msg string) error {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "riff: "+msg)
	}
	if cfg.BigEndian {
		return bad("WAV is little-endian")
	}
	switch cfg.Encoding {
	case pcm.UnsignedInt:
		if cfg.Bits != 8 {
			return bad("unsigned PCM must be 8-bit")
		}
	case pcm.SignedInt:
		if cfg.Bits == 8 {
			return bad("8-bit WAV is unsigned")
		}
	case pcm.Float:
	}
	return nil
}

func (m *Muxer) writeHeaders() error {
	hdrID := idRIFF
	if m.rf64 {
		hdrID = idRF64
	}
	// Seekable output gets patched at End; unseekable output with a known
	// length carries exact sizes up front (verified at End), and unknown
	// lengths use the streaming placeholder convention.
	riffSize := uint32(size32Unknown)
	if !m.rf64 && m.projected >= 0 {
		riffSize = clamp32(m.projected + m.projected&1 + m.headerOverhead(m.hasDS64Region()) - 8)
	}
	if err := m.write([]byte(hdrID), u32(riffSize), []byte(idWAVE)); err != nil {
		return err
	}

	// The 28-byte region after the header is the ds64 chunk in RF64
	// output, and a JUNK reservation when a seekable RIFF might still
	// need rewriting to RF64 at End.
	if m.rf64 {
		m.junkOff = m.off
		proj := uint64(0)
		if m.projected >= 0 {
			proj = uint64(m.projected)
		}
		riffProj := proj + proj&1 + uint64(m.headerOverhead(true)) - 8
		if err := m.write([]byte(idDS64), u32(ds64Payload),
			u64(riffProj), u64(proj), u64(uint64(projFrames(m.projected, m.frameBytes))), u32(0)); err != nil {
			return err
		}
	} else if m.ws != nil {
		m.junkOff = m.off
		if err := m.write([]byte(idJUNK), u32(ds64Payload), make([]byte, ds64Payload)); err != nil {
			return err
		}
	}

	if err := m.writeFmt(); err != nil {
		return err
	}

	if m.cfg.Encoding == pcm.Float {
		if err := m.write([]byte(idFact), u32(4)); err != nil {
			return err
		}
		m.factOff = m.off
		n := uint32(size32Unknown)
		if m.projected >= 0 {
			n = clamp32(projFrames(m.projected, m.frameBytes))
		}
		if err := m.write(u32(n)); err != nil {
			return err
		}
	}

	m.dataOff = m.off
	dataSize := uint32(size32Unknown)
	if !m.rf64 && m.projected >= 0 {
		dataSize = clamp32(m.projected)
	}
	return m.write([]byte(idData), u32(dataSize))
}

// hasDS64Region reports whether the output carries the 28-byte ds64/JUNK
// region: always for RF64, and on seekable writers as the reservation for
// a possible RIFF-to-RF64 rewrite.
func (m *Muxer) hasDS64Region() bool { return m.rf64 || m.ws != nil }

// headerOverhead is the byte count of everything before the data payload
// plus the data chunk header. It only depends on the wire config and the
// ds64 region decision, so Begin can use it before writing.
func (m *Muxer) headerOverhead(withDS64 bool) int64 {
	n := int64(12 + 8) // RIFF header + data chunk header
	if withDS64 {
		n += 8 + ds64Payload
	}
	n += 8 + int64(m.fmtPayloadSize())
	if m.cfg.Encoding == pcm.Float {
		n += 8 + 4
	}
	return n
}

// extensible reports whether the fmt chunk needs WAVE_FORMAT_EXTENSIBLE:
// more than two channels, partial valid bits, or a layout that is not the
// conventional guess for the channel count.
func (m *Muxer) extensible() bool {
	return m.fmt.Channels > 2 ||
		m.cfg.ValidBits != 0 ||
		(m.fmt.Layout != 0 && m.fmt.Layout != audio.DefaultLayout(m.fmt.Channels))
}

func (m *Muxer) fmtPayloadSize() int {
	if m.extensible() {
		return 40
	}
	return 16
}

func (m *Muxer) writeFmt() error {
	tag := uint16(tagPCM)
	if m.cfg.Encoding == pcm.Float {
		tag = tagIEEEFloat
	}
	ext := m.extensible()
	wireTag := tag
	if ext {
		wireTag = tagExtensible
	}
	byteRate := int64(m.fmt.Rate) * int64(m.frameBytes)
	if byteRate > size32Unknown {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "riff: byte rate does not fit WAV")
	}
	parts := [][]byte{
		[]byte(idFmt), u32(uint32(m.fmtPayloadSize())),
		u16(wireTag), u16(uint16(m.fmt.Channels)), u32(uint32(m.fmt.Rate)),
		u32(uint32(byteRate)), u16(uint16(m.frameBytes)), u16(uint16(m.cfg.Bits)),
	}
	if ext {
		valid := m.cfg.Bits
		if m.cfg.ValidBits != 0 {
			valid = m.cfg.ValidBits
		}
		layout := m.fmt.Layout
		if layout == 0 {
			layout = audio.DefaultLayout(m.fmt.Channels)
		}
		parts = append(parts,
			u16(22), u16(uint16(valid)), u32(uint32(layout)), u16(tag), guidTail[:])
	}
	return m.write(parts...)
}

// WritePacket appends raw interleaved PCM bytes.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "riff: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("riff: no track %d", pkt.Track))
	}
	if len(pkt.Data)%m.frameBytes != 0 {
		return waxerr.New(waxerr.CodeInternal, "riff: packet is not whole frames")
	}
	if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.frames += int64(len(pkt.Data)) / int64(m.frameBytes)
	return nil
}

// End finalizes sizes. With a seekable writer the headers are back-patched
// exactly, upgrading RIFF to RF64 if the output crossed the limit; with a
// plain writer the streaming placeholders stand, and a known-length
// projection that the actual stream missed is an error.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "riff: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != 0 || trailer.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "riff: WAV cannot signal gapless trims")
	}
	if trailer.Samples >= 0 && trailer.Samples != m.frames {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("riff: trailer says %d samples, wrote %d", trailer.Samples, m.frames))
	}
	dataBytes := m.frames * int64(m.frameBytes)
	if dataBytes%2 == 1 {
		if err := m.write([]byte{0}); err != nil {
			return err
		}
	}
	riffBytes := m.off - 8

	if m.ws == nil {
		if m.projected >= 0 && dataBytes != m.projected {
			return waxerr.New(waxerr.CodeInternal,
				fmt.Sprintf("riff: headers projected %d data bytes, wrote %d (unseekable output)", m.projected, dataBytes))
		}
		if !m.rf64 && riffBytes > m.limit {
			return waxerr.New(waxerr.CodeInternal, "riff: output crossed the RIFF size limit on an unseekable writer")
		}
		return nil
	}

	needRF64 := riffBytes > m.limit || dataBytes > m.limit
	if needRF64 && !m.rf64 {
		// Rewrite in place: RIFF becomes RF64 and the JUNK reservation
		// becomes the ds64 chunk.
		m.rf64 = true
		if err := m.patch(0, []byte(idRF64)); err != nil {
			return err
		}
		if err := m.patch(m.junkOff, []byte(idDS64)); err != nil {
			return err
		}
	}
	if m.rf64 {
		if err := m.patch(4, u32(size32Unknown)); err != nil {
			return err
		}
		if err := m.patch(m.junkOff+8, u64(uint64(riffBytes)), u64(uint64(dataBytes)), u64(uint64(m.frames))); err != nil {
			return err
		}
		if err := m.patch(m.dataOff+4, u32(size32Unknown)); err != nil {
			return err
		}
	} else {
		if err := m.patch(4, u32(uint32(riffBytes))); err != nil {
			return err
		}
		if err := m.patch(m.dataOff+4, u32(uint32(dataBytes))); err != nil {
			return err
		}
	}
	if m.factOff != 0 {
		if err := m.patch(m.factOff, u32(clamp32(m.frames))); err != nil {
			return err
		}
	}
	// Leave the writer positioned at the end of the file.
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "riff: seeking to end", err)
	}
	return nil
}

func (m *Muxer) write(parts ...[]byte) error {
	for _, p := range parts {
		n, err := m.w.Write(p)
		m.off += int64(n)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "riff: write", err)
		}
	}
	return nil
}

// patch rewrites bytes at an absolute offset on the seekable writer.
func (m *Muxer) patch(off int64, parts ...[]byte) error {
	if _, err := m.ws.Seek(off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "riff: seek for patch", err)
	}
	for _, p := range parts {
		if _, err := m.ws.Write(p); err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "riff: patch", err)
		}
	}
	return nil
}

func projFrames(dataBytes int64, frameBytes int) int64 {
	if dataBytes < 0 {
		return 0
	}
	return dataBytes / int64(frameBytes)
}

func clamp32(v int64) uint32 {
	if v > size32Unknown {
		return size32Unknown
	}
	return uint32(v)
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	le.PutUint16(b, v)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	le.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	le.PutUint64(b, v)
	return b
}
