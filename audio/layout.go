package audio

import (
	"math/bits"
	"strings"
)

// ChannelMask assigns speaker positions to channels using
// WAVE_FORMAT_EXTENSIBLE bit numbering, which maps 1:1 onto WAV, MP4, and
// Matroska channel masks. Channels are ordered by ascending bit position.
type ChannelMask uint32

const (
	FrontLeft ChannelMask = 1 << iota
	FrontRight
	FrontCenter
	LowFrequency
	BackLeft
	BackRight
	FrontLeftOfCenter
	FrontRightOfCenter
	BackCenter
	SideLeft
	SideRight
	TopCenter
	TopFrontLeft
	TopFrontCenter
	TopFrontRight
	TopBackLeft
	TopBackCenter
	TopBackRight
)

var maskNames = []string{
	"FL", "FR", "FC", "LFE", "BL", "BR", "FLC", "FRC", "BC",
	"SL", "SR", "TC", "TFL", "TFC", "TFR", "TBL", "TBC", "TBR",
}

// Count returns the number of assigned positions.
func (m ChannelMask) Count() int {
	return bits.OnesCount32(uint32(m))
}

// String renders the mask as position abbreviations in channel order,
// for example "FL|FR|FC|LFE|BL|BR".
func (m ChannelMask) String() string {
	if m == 0 {
		return "unknown"
	}
	var b strings.Builder
	for i := 0; i < 32; i++ {
		if m&(1<<i) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		if i < len(maskNames) {
			b.WriteString(maskNames[i])
		} else {
			b.WriteString("?")
		}
	}
	return b.String()
}

// DefaultLayout returns the conventional layout for a channel count
// (mono, stereo, quad, 5.0, 5.1, 6.1, 7.1), or zero when there is no
// convention.
func DefaultLayout(channels int) ChannelMask {
	switch channels {
	case 1:
		return FrontCenter
	case 2:
		return FrontLeft | FrontRight
	case 3:
		return FrontLeft | FrontRight | FrontCenter
	case 4:
		return FrontLeft | FrontRight | BackLeft | BackRight
	case 5:
		return FrontLeft | FrontRight | FrontCenter | BackLeft | BackRight
	case 6:
		return FrontLeft | FrontRight | FrontCenter | LowFrequency | BackLeft | BackRight
	case 7:
		return FrontLeft | FrontRight | FrontCenter | LowFrequency | BackCenter | SideLeft | SideRight
	case 8:
		return FrontLeft | FrontRight | FrontCenter | LowFrequency | BackLeft | BackRight | SideLeft | SideRight
	default:
		return 0
	}
}
