package adts

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// MuxerVersion is this muxer's term in the ADR-0004 cache key. Bump it for a
// change to what gets written around unchanged encoded packets, which no
// encoder or DSP version can notice.
const MuxerVersion = "adts-mux-1"

// Muxer writes one AAC track as an ADTS elementary stream: each access
// unit gets the fixed 7-byte header (no CRC) and nothing else. ADTS has
// no gapless signaling, so the trailer is accepted and discarded; streams
// decode from the first sample including encoder priming. This container
// is the format=aac legacy opt-out; progressive fMP4 is the default for
// exactly this reason.
//
// HE-AAC tracks mux too, as the implicit form: ADTS has no ASC to carry
// explicit SBR signalling, so the header states the core LC layer at the
// core rate and the SBR extension rides inside the payload, which is how
// ADTS has always carried HE-AAC. The demuxer's first-frame probe is what
// identifies such a stream on the way back in. Downsampled SBR is the one
// HE shape refused: implicit signalling implies the dual rate, so its
// units cannot be re-framed honestly.
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
	var err error
	if m.rateIdx, m.channels, err = headerFields(tracks[0]); err != nil {
		return err
	}
	m.began = true
	return nil
}

// TrackFits reports whether the muxer can frame track t: nil, or Begin's own
// refusal.
func TrackFits(t container.Track) error {
	_, _, err := headerFields(t)
	return err
}

// headerFields derives the header's rate index and channel configuration
// from t's AudioSpecificConfig, refusing a track ADTS cannot frame.
func headerFields(t container.Track) (rateIdx, channels int, err error) {
	if t.Codec != codec.AACLC && t.Codec != codec.HEAAC {
		return 0, 0, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: cannot mux codec %q", t.Codec))
	}
	if len(t.CodecConfig) < 2 {
		return 0, 0, waxerr.New(waxerr.CodeInvalidRequest, "adts: track carries no AudioSpecificConfig")
	}
	// ParseASC rather than fixed bit positions, so the hierarchical AOT-5
	// and sync-extension HE forms land on the same core fields as plain LC
	// (its first samplingFrequencyIndex is the core rate in every form).
	cfg, err := aac.ParseASC(t.CodecConfig)
	if err != nil {
		return 0, 0, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "adts: track's AudioSpecificConfig", err)
	}
	if cfg.FrameLength != samplesPerFrame {
		return 0, 0, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("adts: %d-sample frames; ADTS carries only the %d-sample form", cfg.FrameLength, samplesPerFrame))
	}
	if cfg.SBR && cfg.ExtensionRate == cfg.SampleRate {
		// Implicit signalling is ADTS's only HE form, and implicit means
		// dual-rate by convention: these access units re-framed here would
		// legally decode at double their real rate. fdk refuses the same
		// combination on the encode side.
		return 0, 0, waxerr.New(waxerr.CodeUnsupportedFormat,
			"adts: downsampled (single-rate) SBR has no ADTS form; implicit signalling implies the dual rate")
	}
	rateIdx, channels = rateIndex(cfg.SampleRate), cfg.ChannelConfig
	if rateIdx < 0 {
		return 0, 0, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: no sampling frequency index for %d Hz", cfg.SampleRate))
	}
	if channels < 1 || channels > 7 {
		return 0, 0, waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("adts: channel configuration %d", channels))
	}
	return rateIdx, channels, nil
}

// rateIndex maps a core sample rate to its samplingFrequencyIndex (Table
// 1.16), or -1. Indexes 13+ are reserved or escape codes ADTS cannot state.
func rateIndex(rate int) int {
	for i, r := range [13]int{96000, 88200, 64000, 48000, 44100, 32000,
		24000, 22050, 16000, 12000, 11025, 8000, 7350} {
		if r == rate {
			return i
		}
	}
	return -1
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
