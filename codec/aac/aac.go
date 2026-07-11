// Package aac implements an AAC-LC decoder (ISO/IEC 14496-3), written from
// the specification and Bosi/Goldberg (clean-room: AAC reference decoders
// were behavioral references only, never opened while implementing).
//
// Scope is Low Complexity only: no SBR, no PS, no gain control, no LTP. An
// AudioSpecificConfig announcing SBR or PS decodes its AAC-LC base layer at
// the base sample rate; the high band is not synthesized (documented
// limitation, not a silent one).
package aac

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on
// any change that alters decoded samples.
const Version = "aac-dec-1"

// Audio object types (ISO/IEC 14496-3 Table 1.17). Only LC is decoded.
const (
	aotAACMain = 1
	aotAACLC   = 2
	aotAACSSR  = 3
	aotAACLTP  = 4
	aotSBR     = 5
	aotPS      = 29
)

// sampleRates indexes the 4-bit samplingFrequencyIndex (Table 1.16).
var sampleRates = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// channelConfigs maps the 4-bit channelConfiguration to a channel count;
// 0 means the count is carried by an in-band program config element.
var channelConfigs = [...]int{0, 1, 2, 3, 4, 5, 6, 8}

// Config is a parsed AudioSpecificConfig.
type Config struct {
	ObjectType  int
	SampleRate  int // decoded (base) rate
	Channels    int // 0 when carried by an in-band PCE
	FrameLength int // 1024, or 960 with the short-frame flag
	ASC         []byte
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "aac: "+fmt.Sprintf(format, args...))
}

// ParseASC parses an AudioSpecificConfig, resolving the AAC-LC base rate,
// channel count, and frame length. SBR/PS wrappers are unwrapped to their
// base object type.
func ParseASC(b []byte) (Config, error) {
	if len(b) < 2 {
		return Config{}, malformed("AudioSpecificConfig of %d bytes", len(b))
	}
	r := ascReader{data: b}
	aot := r.objectType()
	rate, err := r.samplingRate()
	if err != nil {
		return Config{}, err
	}
	chanConfig := int(r.read(4))

	// SBR (5) and PS (29) wrap a base object type at a doubled rate. We
	// decode the base layer at the base rate, so unwrap to it.
	if aot == aotSBR || aot == aotPS {
		if _, err := r.samplingRate(); err != nil { // extensionSamplingFrequencyIndex
			return Config{}, err
		}
		aot = r.objectType()
		// The base rate is the extension rate halved for SBR; but without
		// high-band synthesis we output the core rate the base layer codes.
		rate /= 2
	}
	if aot != aotAACLC {
		// Main/SSR/LTP are not decoded, but the container still needs a
		// coherent format; report the object type honestly.
		return Config{}, malformed("audio object type %d is not AAC-LC", aot)
	}

	frameLen := 1024
	if r.read(1) != 0 { // frameLengthFlag (GASpecificConfig)
		frameLen = 960
	}

	channels := 0
	if chanConfig >= 1 && chanConfig < len(channelConfigs) {
		channels = channelConfigs[chanConfig]
	}
	if channels > 2 {
		// The decoder emits channels in bitstream order with no WAV remap,
		// so a multichannel layout would be silently wrong; refuse it. A
		// channel_configuration of 0 (in-band PCE) is left to decode time.
		return Config{}, malformed("channel configuration %d: only mono and stereo are supported", chanConfig)
	}
	if rate <= 0 {
		return Config{}, malformed("sampling frequency index reserved")
	}
	return Config{
		ObjectType:  aot,
		SampleRate:  rate,
		Channels:    channels,
		FrameLength: frameLen,
		ASC:         append([]byte(nil), b...),
	}, nil
}

// Format is the pipeline format the decoder emits: 48 kHz-class float,
// always 32-bit float domain.
func (c Config) Format() audio.Format {
	ch := c.Channels
	if ch < 1 {
		ch = 2 // a PCE-carried count defaults to stereo until decode resolves it
	}
	return audio.Format{
		Rate:     c.SampleRate,
		Channels: ch,
		Layout:   audio.DefaultLayout(ch),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// ascReader reads the AudioSpecificConfig's MSB-first bit fields.
type ascReader struct {
	data []byte
	pos  int
}

func (r *ascReader) read(n uint) uint32 {
	var v uint32
	for i := uint(0); i < n; i++ {
		bit := uint32(0)
		if idx := r.pos >> 3; idx < len(r.data) {
			bit = uint32(r.data[idx]>>(7-uint(r.pos&7))) & 1
		}
		v = v<<1 | bit
		r.pos++
	}
	return v
}

// objectType reads a 5-bit audioObjectType with the 6-bit escape.
func (r *ascReader) objectType() int {
	aot := int(r.read(5))
	if aot == 31 {
		aot = 32 + int(r.read(6))
	}
	return aot
}

// samplingRate reads a 4-bit samplingFrequencyIndex with the 24-bit escape.
func (r *ascReader) samplingRate() (int, error) {
	idx := r.read(4)
	if idx == 15 {
		return int(r.read(24)), nil
	}
	if idx >= uint32(len(sampleRates)) || sampleRates[idx] == 0 {
		return 0, malformed("reserved sampling frequency index %d", idx)
	}
	return sampleRates[idx], nil
}
