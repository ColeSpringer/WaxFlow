// Package adts demuxes the ADTS elementary stream framing for AAC (ISO/IEC
// 14496-3 1.A). Each frame carries a fixed 7- or 9-byte header (syncword,
// profile, sampling-frequency index, channel configuration, frame length)
// followed by one raw_data_block, which is handed to the AAC decoder as a
// packet. The AudioSpecificConfig is synthesized from the first frame's
// header, since ADTS transports no out-of-band config.
//
// ADTS carries no gapless signaling at all, so streams play from the first
// sample including encoder priming; this is why format=aac defaults to
// progressive fMP4 elsewhere and ADTS is a legacy opt-out. Every AAC frame
// is independently decodable, so seeking lands one frame early for the
// decoder's IMDCT overlap and format.Media pre-rolls the remainder.
package adts

import (
	"fmt"

	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/waxerr"
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// headerLen is the fixed ADTS header size without the optional CRC.
	headerLen = 7
	// maxResync bounds the scan for the next syncword after damage.
	maxResync = 1 << 20
	// maxID3Tags bounds leading ID3v2 tag skipping.
	maxID3Tags = 8
	// samplesPerFrame is the AAC-LC frame length; ADTS never carries the
	// 960-sample variant.
	samplesPerFrame = 1024
)

// MatchNeed is how many leading bytes Match inspects: two full frame
// headers so a stray syncword in other data does not false-positive.
const MatchNeed = 9

// Match reports whether head begins with a valid ADTS frame whose declared
// length points at a second valid frame (or the end of the buffer). The
// two-header confirmation keeps the 12-bit syncword from matching arbitrary
// payloads, mirroring the mpa row's second-header rule.
func Match(head []byte) bool {
	h, ok := parseHeader(head)
	if !ok {
		return false
	}
	if h.frameLen >= len(head) {
		return true // the first frame runs past the sniff buffer; accept it
	}
	nh, ok := parseHeader(head[h.frameLen:])
	return ok && h.kin(nh)
}

// header is a parsed ADTS frame header.
type header struct {
	profile  int // 0 main, 1 LC, 2 SSR, 3 (reserved/LTP)
	rateIdx  int // samplingFrequencyIndex
	channels int // channel_configuration (0 means in-band PCE)
	frameLen int // whole frame length in bytes (header + payload + CRC)
	hdrLen   int // 7, or 9 with the protection CRC
	blocks   int // number_of_raw_data_blocks_in_frame (0 means one block)
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "adts: "+fmt.Sprintf(format, args...))
}

// parseHeader parses an ADTS header, validating the syncword, layer,
// sampling index, and frame length.
func parseHeader(b []byte) (header, bool) {
	if len(b) < headerLen {
		return header{}, false
	}
	// syncword: 0xFFF (12 bits); layer must be 0.
	if b[0] != 0xFF || b[1]&0xF0 != 0xF0 || (b[1]>>1)&0x3 != 0 {
		return header{}, false
	}
	protectionAbsent := b[1] & 1
	var h header
	h.profile = int(b[2]>>6) & 0x3
	h.rateIdx = int(b[2]>>2) & 0xF
	h.channels = int(b[2]&1)<<2 | int(b[3]>>6)
	h.frameLen = int(b[3]&0x3)<<11 | int(b[4])<<3 | int(b[5]>>5)
	h.blocks = int(b[6] & 0x3)
	h.hdrLen = headerLen
	if protectionAbsent == 0 {
		h.hdrLen = 9
	}
	if h.rateIdx >= 13 || h.frameLen < h.hdrLen {
		return header{}, false
	}
	return h, true
}

// kin reports whether two headers describe the same stream (a resync
// heuristic: sample rate, channels, and profile must agree).
func (h header) kin(o header) bool {
	return h.rateIdx == o.rateIdx && h.channels == o.channels && h.profile == o.profile
}

// asc synthesizes the 2-byte AudioSpecificConfig for the stream: the object
// type from the profile, plus the sampling-frequency index and channel
// configuration, with an all-zero GASpecificConfig (1024-sample frames).
func (h header) asc() []byte {
	aot := h.profile + 1 // ADTS profile 1 (LC) maps to audio object type 2
	return []byte{
		byte(aot<<3) | byte(h.rateIdx>>1),
		byte(h.rateIdx&1)<<7 | byte(h.channels)<<3,
	}
}

// config parses the synthesized ASC into an aac.Config for the track format.
func (h header) config() (aac.Config, error) {
	cfg, err := aac.ParseASC(h.asc())
	if err != nil {
		return aac.Config{}, err
	}
	if cfg.Channels == 0 {
		// channel_configuration 0 means the layout is carried in-band; ADTS
		// gives no fallback, so treat it as unsupported rather than guess.
		return aac.Config{}, malformed("channel configuration 0 (in-band PCE) is unsupported")
	}
	return cfg, nil
}
