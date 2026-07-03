// Package mp3 implements an MPEG-1/2/2.5 Layer III audio decoder (ISO/IEC
// 11172-3 and 13818-3) in pure Go.
//
// One packet is one whole MP3 frame, header included. Frames are NOT
// independently decodable: Layer III's bit reservoir lets a frame's main
// data begin up to 511 bytes inside earlier frames, so the decoder keeps
// a rolling reservoir and a frame whose reservoir reference is not
// satisfied (the first packets after a seek) decodes to silence while
// still contributing its own bytes. Seek landings therefore back off far
// enough that the reservoir and the filterbank history converge before
// the target; container/mpa owns that arithmetic.
//
// The frame header helpers are exported for containers: like FLAC, the
// same framing appears in more than one place (the mpa elementary stream
// today, MP4 and Matroska tracks later), and header parsing is the codec's
// knowledge.
//
// Implementation notes: the decode pipeline structure follows the public
// domain PDMP3 via hajimehoshi/go-mp3 (Apache-2.0), with the low sampling
// frequency (MPEG-2/2.5) scalefactor, intensity stereo, and band edge
// handling ported from minimp3 (CC0). See THIRD-PARTY-NOTICES.md.
package mp3

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on
// any change that alters decoded samples.
const Version = "mp3-dec-1"

// HeaderLen is the fixed frame header size in bytes.
const HeaderLen = 4

// MPEGVersion distinguishes the three header generations. Layer III frame
// layout differs between MPEG-1 and the low-sampling-frequency pair
// (MPEG-2 and 2.5, which share everything except the sample rate shift).
type MPEGVersion uint8

const (
	MPEG1  MPEGVersion = 1
	MPEG2  MPEGVersion = 2
	MPEG25 MPEGVersion = 3
)

func (v MPEGVersion) String() string {
	switch v {
	case MPEG1:
		return "MPEG-1"
	case MPEG2:
		return "MPEG-2"
	case MPEG25:
		return "MPEG-2.5"
	default:
		return fmt.Sprintf("MPEGVersion(%d)", uint8(v))
	}
}

// Channel modes (header mode field).
const (
	ModeStereo = 0
	ModeJoint  = 1
	ModeDual   = 2
	ModeMono   = 3
)

// Header is a parsed MPEG audio frame header. Only Layer III headers
// parse; Layer I/II carry the same sync but a different frame layout, and
// the pipeline does not decode them.
type Header struct {
	// rateIdx is the raw header rate index, kept for band-table rows.
	rateIdx int

	Version MPEGVersion
	// Rate is the sample rate in Hz.
	Rate int
	// Channels is 1 or 2.
	Channels int
	// Mode is the raw channel mode; ModeExt qualifies joint stereo.
	Mode    int
	ModeExt int
	// Bitrate is the frame's bit rate in bits per second, 0 for the free
	// format (frame size fixed by the stream, not the header).
	Bitrate int
	// Padding reports the padding slot bit.
	Padding bool
	// Protected reports a CRC-16 between header and side info. The value
	// is skipped, not verified: neither reference implementation checks
	// it, and a false negative would silence a good frame.
	Protected bool
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "mp3: "+fmt.Sprintf(format, args...))
}

// bitrateKbps is the Layer III bit rate table, in kbit/s: one row for
// MPEG-1, one shared by MPEG-2 and 2.5 (ISO 11172-3 and 13818-3). Index 0
// is the free format and index 15 is forbidden.
var bitrateKbps = [2][16]int{
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1},
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
}

// rateHz maps the header rate index to Hz for MPEG-1; MPEG-2 halves it
// and MPEG-2.5 quarters it.
var rateHz = [4]int{44100, 48000, 32000, -1}

// ParseHeader parses a Layer III frame header from the first HeaderLen
// bytes of b.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderLen {
		return Header{}, malformed("truncated frame header")
	}
	if b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return Header{}, malformed("missing frame sync")
	}
	var h Header
	switch b[1] >> 3 & 3 {
	case 0:
		h.Version = MPEG25
	case 2:
		h.Version = MPEG2
	case 3:
		h.Version = MPEG1
	default:
		return Header{}, malformed("reserved MPEG version")
	}
	if layer := b[1] >> 1 & 3; layer != 1 {
		return Header{}, malformed("not Layer III (layer bits %d)", layer)
	}
	h.Protected = b[1]&1 == 0

	lsf := 0
	if h.Version != MPEG1 {
		lsf = 1
	}
	bi := int(b[2] >> 4)
	if kbps := bitrateKbps[lsf][bi]; kbps < 0 {
		return Header{}, malformed("forbidden bit rate index")
	} else {
		h.Bitrate = kbps * 1000
	}
	ri := int(b[2] >> 2 & 3)
	if rateHz[ri] < 0 {
		return Header{}, malformed("reserved sample rate")
	}
	h.rateIdx = ri
	h.Rate = rateHz[ri]
	if h.Version != MPEG1 {
		h.Rate >>= 1
	}
	if h.Version == MPEG25 {
		h.Rate >>= 1
	}
	h.Padding = b[2]&2 != 0

	h.Mode = int(b[3] >> 6)
	h.ModeExt = int(b[3] >> 4 & 3)
	h.Channels = 2
	if h.Mode == ModeMono {
		h.Channels = 1
	}
	if b[3]&3 == 2 {
		return Header{}, malformed("reserved emphasis")
	}
	return h, nil
}

// SamplesPerFrame is the PCM frame count one MP3 frame decodes to: 1152
// for MPEG-1, 576 for the single-granule MPEG-2/2.5 layout.
func (h Header) SamplesPerFrame() int {
	if h.Version == MPEG1 {
		return 1152
	}
	return 576
}

// Size is the whole frame length in bytes, header included, or 0 for the
// free format (where the size is a property of the stream: the distance
// to the next sync).
func (h Header) Size() int {
	if h.Bitrate == 0 {
		return 0
	}
	n := h.SamplesPerFrame() / 8 * h.Bitrate / h.Rate
	if h.Padding {
		n++
	}
	return n
}

// SideInfoLen is the side information length in bytes for this header,
// following the header and the optional CRC-16.
func (h Header) SideInfoLen() int {
	if h.Version == MPEG1 {
		if h.Channels == 1 {
			return 17
		}
		return 32
	}
	if h.Channels == 1 {
		return 9
	}
	return 17
}

// PCMFormat is the pipeline format frames with this header decode to.
// Layer III reconstruction is floating point, so the lossy convention
// applies: float32 in nominal [-1, 1].
func (h Header) PCMFormat() audio.Format {
	return audio.Format{
		Rate:     h.Rate,
		Channels: h.Channels,
		Layout:   audio.DefaultLayout(h.Channels),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// Kin reports whether o plausibly belongs to the same stream: equal
// version, rate, and channel count. Bit rate, padding, and joint stereo
// flags legitimately vary frame to frame (VBR, mode switching), so they
// do not participate. Containers use this to validate sync candidates.
func (h Header) Kin(o Header) bool {
	return h.Version == o.Version && h.Rate == o.Rate && h.Channels == o.Channels
}

// rateRow maps a header to its row in the shared band tables: 11.025 and
// 12 kHz share a row (their ISO band tables are identical), then 8,
// 22.05, 24, 16, 44.1, 48, and 32 kHz. The layout follows the ISO band
// tables as arranged by minimp3.
func (h Header) rateRow() int {
	base := 0
	switch h.Version {
	case MPEG2:
		base = 3
	case MPEG1:
		base = 6
	}
	row := base + h.rateIdx
	if row != 0 {
		row-- // fold 12 kHz onto the 11.025 kHz row
	}
	return row
}
