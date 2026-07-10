package adts

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// Muxer writes one AAC-LC track as an ADTS elementary stream: each
// access unit gets the fixed 7-byte header (no CRC) and nothing else.
// ADTS has no gapless signaling, so the trailer is accepted and
// discarded; streams decode from the first sample including encoder
// priming. This container is the format=aac legacy opt-out; progressive
// fMP4 is the default for exactly this reason.
type Muxer struct {
	w            io.Writer
	rateIdx      int
	channels     int
	began, ended bool
}

// NewMuxer returns an ADTS muxer writing to w.
func NewMuxer(w io.Writer) *Muxer { return &Muxer{w: w} }

// NeedsSeek reports false: ADTS is pure streaming framing.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and derives the header fields from the
// AudioSpecificConfig (the inverse of the demuxer's header.asc).
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "adts: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("adts: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.AACLC {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: cannot mux codec %q", t.Codec))
	}
	if len(t.CodecConfig) < 2 {
		return waxerr.New(waxerr.CodeInvalidRequest, "adts: track carries no AudioSpecificConfig")
	}
	aot := int(t.CodecConfig[0] >> 3)
	if aot != 2 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: audio object type %d is not AAC-LC", aot))
	}
	m.rateIdx = int(t.CodecConfig[0]&0x7)<<1 | int(t.CodecConfig[1]>>7)
	m.channels = int(t.CodecConfig[1]>>3) & 0xF
	if m.rateIdx >= 13 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: sampling frequency index %d", m.rateIdx))
	}
	if m.channels < 1 || m.channels > 7 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: channel configuration %d", m.channels))
	}
	m.began = true
	return nil
}

// WritePacket frames one access unit.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "adts: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("adts: no track %d", pkt.Track))
	}
	frameLen := headerLen + len(pkt.Data)
	if frameLen > 1<<13-1 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("adts: %d-byte frame exceeds the 13-bit length field", frameLen))
	}
	// Fixed header: syncword, MPEG-4, layer 0, no CRC; profile is
	// AOT-1; buffer fullness 0x7FF signals VBR; one raw_data_block.
	var h [headerLen]byte
	h[0] = 0xFF
	h[1] = 0xF1
	h[2] = byte(1)<<6 | byte(m.rateIdx)<<2 | byte(m.channels>>2)
	h[3] = byte(m.channels&0x3)<<6 | byte(frameLen>>11)
	h[4] = byte(frameLen >> 3)
	h[5] = byte(frameLen&0x7)<<5 | 0x1F
	h[6] = 0xFC
	if _, err := m.w.Write(h[:]); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "adts: header", err)
	}
	if _, err := m.w.Write(pkt.Data); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "adts: payload", err)
	}
	return nil
}

// End completes the stream. ADTS carries no gapless trailer, so the
// trims are dropped here by design (the capability matrix's "none"
// cell); callers wanting gapless AAC use the fMP4 path.
func (m *Muxer) End(codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "adts: End outside Begin")
	}
	m.ended = true
	return nil
}
