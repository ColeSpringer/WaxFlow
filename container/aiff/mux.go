package aiff

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// Muxer writes one PCM track as AIFF (big-endian integers) or AIFF-C
// (floats). NeedsSeek reports true: AIFF has no streaming form, so the
// FORM size, COMM frame count, and SSND size are back-patched at End and
// the writer must be an io.WriteSeeker.
type Muxer struct {
	w  io.Writer
	ws io.WriteSeeker

	cfg        pcm.Config
	fmt        audio.Format
	frameBytes int
	aifc       bool

	off       int64
	formOff   int64 // offset of the FORM size field
	framesOff int64 // offset of COMM numSampleFrames
	ssndOff   int64 // offset of the SSND chunk header

	frames int64
	began  bool
	ended  bool
}

// NewMuxer returns an AIFF muxer writing to w. Begin fails unless w
// implements io.WriteSeeker; callers should check NeedsSeek and provide a
// file.
func NewMuxer(w io.Writer) *Muxer {
	m := &Muxer{w: w}
	if ws, ok := w.(io.WriteSeeker); ok {
		m.ws = ws
	}
	return m
}

// NeedsSeek reports true: AIFF cannot be written to a plain stream.
func (m *Muxer) NeedsSeek() bool { return true }

// Begin validates the track and writes headers.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "aiff: Begin called twice")
	}
	if m.ws == nil {
		return waxerr.New(waxerr.CodeInvalidRequest, "aiff: output requires a seekable destination (AIFF has no streaming form)")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("aiff: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.PCM {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("aiff: cannot mux codec %q (AIFF holds PCM)", t.Codec))
	}
	if t.Delay != 0 || t.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "aiff: AIFF cannot signal gapless trims")
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
	m.cfg = cfg
	m.fmt = t.Fmt
	m.frameBytes = cfg.BytesPerFrame(t.Fmt.Channels)
	m.aifc = cfg.Encoding == pcm.Float
	m.began = true
	return m.writeHeaders()
}

// checkWireConfig rejects wire encodings this muxer does not emit: plain
// AIFF holds big-endian signed integers (endianness is moot for 8-bit),
// AIFF-C adds big-endian floats. Little-endian and unsigned wires exist
// only on the read side (sowt, raw).
func (m *Muxer) checkWireConfig(cfg pcm.Config) error {
	bad := func(msg string) error {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "aiff: "+msg)
	}
	switch cfg.Encoding {
	case pcm.SignedInt:
		if !cfg.BigEndian && cfg.Bits > 8 {
			return bad("integer AIFF output is big-endian")
		}
	case pcm.Float:
		if !cfg.BigEndian {
			return bad("float AIFF output is big-endian")
		}
	default:
		return bad(fmt.Sprintf("no AIFF output for %v PCM", cfg.Encoding))
	}
	// COMM sampleSize is AIFF's only width field: readers derive storage
	// as ceil(sampleSize/8) bytes. A container word wider than that (for
	// example 24 valid bits in 32-bit words) cannot be represented; the
	// payload would misalign against the declared width.
	valid := cfg.ValidBits
	if valid == 0 {
		valid = cfg.Bits
	}
	if pcm.ContainerBits(valid) != cfg.Bits {
		return bad(fmt.Sprintf("%d valid bits in %d-bit words has no AIFF representation (repack to %d-bit words)",
			valid, cfg.Bits, pcm.ContainerBits(valid)))
	}
	return nil
}

func (m *Muxer) writeHeaders() error {
	form := idAIFF
	if m.aifc {
		form = idAIFC
	}
	m.formOff = 4
	if err := m.write([]byte(idFORM), u32be(0), []byte(form)); err != nil {
		return err
	}
	if m.aifc {
		if err := m.write([]byte(idFVER), u32be(4), u32be(fverTimestamp)); err != nil {
			return err
		}
	}

	// COMM. Valid bits go in sampleSize; the container packs them in
	// whole bytes, matching the wire config by construction.
	bits := m.cfg.Bits
	if m.cfg.ValidBits != 0 {
		bits = m.cfg.ValidBits
	}
	commSize := uint32(18)
	if m.aifc {
		commSize = 18 + 4 + 2 // compression type + empty pascal string
	}
	rate := toExt80(float64(m.fmt.Rate))
	if err := m.write([]byte(idCOMM), u32be(commSize), u16be(uint16(m.fmt.Channels))); err != nil {
		return err
	}
	m.framesOff = m.off
	if err := m.write(u32be(0), u16be(uint16(bits)), rate[:]); err != nil {
		return err
	}
	if m.aifc {
		comp := compFl32
		if m.cfg.Bits == 64 {
			comp = compFl64
		}
		// An empty pascal string is one count byte padded to even length.
		if err := m.write([]byte(comp), []byte{0, 0}); err != nil {
			return err
		}
	}

	m.ssndOff = m.off
	return m.write([]byte(idSSND), u32be(0), u32be(0), u32be(0)) // size, offset, blockSize
}

// WritePacket appends raw interleaved PCM bytes.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "aiff: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("aiff: no track %d", pkt.Track))
	}
	if len(pkt.Data)%m.frameBytes != 0 {
		return waxerr.New(waxerr.CodeInternal, "aiff: packet is not whole frames")
	}
	// Fail before writing terabytes that End would reject anyway: the
	// FORM size field cannot represent output past 4 GiB.
	if m.off+int64(len(pkt.Data))-8 > size32Max {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			"aiff: output would exceed AIFF's 4 GiB limit (use WAV, which upgrades to RF64)")
	}
	if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.frames += int64(len(pkt.Data)) / int64(m.frameBytes)
	return nil
}

// End back-patches the FORM size, COMM frame count, and SSND size.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "aiff: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != 0 || trailer.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "aiff: AIFF cannot signal gapless trims")
	}
	if trailer.Samples >= 0 && trailer.Samples != m.frames {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("aiff: trailer says %d samples, wrote %d", trailer.Samples, m.frames))
	}
	dataBytes := m.frames * int64(m.frameBytes)
	if dataBytes%2 == 1 {
		if err := m.write([]byte{0}); err != nil {
			return err
		}
	}
	if m.off-8 > size32Max {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "aiff: output exceeds AIFF's 4 GiB limit (use WAV, which upgrades to RF64)")
	}
	if err := m.patch(m.formOff, u32be(uint32(m.off-8))); err != nil {
		return err
	}
	if err := m.patch(m.framesOff, u32be(uint32(m.frames))); err != nil {
		return err
	}
	if err := m.patch(m.ssndOff+4, u32be(uint32(8+dataBytes))); err != nil {
		return err
	}
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "aiff: seeking to end", err)
	}
	return nil
}

func (m *Muxer) write(parts ...[]byte) error {
	for _, p := range parts {
		n, err := m.w.Write(p)
		m.off += int64(n)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "aiff: write", err)
		}
	}
	return nil
}

func (m *Muxer) patch(off int64, parts ...[]byte) error {
	if _, err := m.ws.Seek(off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "aiff: seek for patch", err)
	}
	for _, p := range parts {
		if _, err := m.ws.Write(p); err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "aiff: patch", err)
		}
	}
	return nil
}

func u16be(v uint16) []byte {
	b := make([]byte, 2)
	be.PutUint16(b, v)
	return b
}

func u32be(v uint32) []byte {
	b := make([]byte, 4)
	be.PutUint32(b, v)
	return b
}
