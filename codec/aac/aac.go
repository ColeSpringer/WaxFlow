// Package aac implements an AAC-LC decoder (ISO/IEC 14496-3), written from
// the specification and Bosi/Goldberg (clean-room: AAC reference decoders
// were behavioral references only, never opened while implementing).
//
// Scope is Low Complexity only: no SBR, no PS, no gain control, no LTP. An
// AudioSpecificConfig announcing SBR or PS decodes its AAC-LC base layer at
// the base sample rate; the high band is not synthesized (documented
// limitation, not a silent one).
//
// Channel configurations 1 through 6 decode, remapped from AAC's
// centre-outward element order to WAVE channel order. Configuration 7, the
// reserved and later-amended configurations 8 through 15, and the in-band
// program config element (configuration 0, beyond a container-supplied
// mono or stereo count) are each refused by name rather than guessed at,
// because a wrong channel order is the one decoder error that produces a
// full-length, full-loudness file that is simply the wrong audio. For the
// same reason a multichannel frame's element sequence is checked against
// the one Table 1.19 fixes, since the remap routes by position and a
// deviant order would reroute rather than fail. The encoder stays mono and
// stereo.
//
// That limitation is signalled where it can be. Explicit hierarchical
// signalling (audioObjectType 5 or 29 in the ASC, which is how an M4A's esds
// carries HE-AAC) sets Config.SBR, and a demuxer that carries warnings emits
// one. Implicit signalling, where the ASC says AOT 2 and SBR lives in the
// bitstream, is how ADTS carries HE-AAC because ADTS has no ASC at all; it
// cannot be detected without parsing the extension payload, which would be
// implementing part of the non-goal. So an implicitly signalled source decodes
// its base layer with no warning. Both paths agree on the rate they report:
// the rate the base layer actually codes at.
package aac

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// Version is the decoder's cache-key version constant (ADR-0004): bump on
// any change that alters decoded samples. aac-dec-2 stopped halving the rate
// of an explicitly signalled SBR/PS config, which changes the reported rate,
// the rescaled sample timing, and so the decoded output for those sources.
const Version = "aac-dec-2"

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
	ObjectType int
	SampleRate int // decoded (base) rate
	Channels   int // 0 when carried by an in-band PCE
	// ChannelConfig is the raw channelConfiguration field, kept beside the
	// count it implies because the two are not interchangeable: it fixes the
	// speaker layout and the bitstream's element order, which a count alone
	// does not (configurations 4 and 7 both disagree with the conventional
	// layout for their count), and 0 means neither is known here.
	ChannelConfig int
	FrameLength   int // 1024, or 960 with the short-frame flag
	ASC           []byte
	// SBR reports that the ASC explicitly signalled SBR (audioObjectType 5)
	// or PS (29) wrapping the base object type, and PS narrows that to the
	// latter. The base layer decodes at SampleRate and the high band is not
	// synthesized, so a demuxer that can carry warnings should emit one.
	SBR bool
	PS  bool
	// ExtensionRate is the doubled output rate an SBR/PS config declares, or
	// 0 for none. It is what the source would play at with a full HE-AAC
	// decoder, and is carried only so a warning can name it; it is never the
	// rate this decoder emits.
	ExtensionRate int
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

	// SBR (5) and PS (29) wrap a base object type, and per ISO/IEC 14496-3
	// §1.6.2.1 the first samplingFrequencyIndex above already IS the core
	// rate the base layer codes at; extensionSamplingFrequencyIndex is the
	// doubled rate a full HE-AAC decoder would output. So unwrap to the base
	// object type and keep the rate as read. Do not halve it: that would
	// report an octave below the rate the base layer actually codes at.
	var sbr, ps bool
	extRate := 0
	if aot == aotSBR || aot == aotPS {
		sbr, ps = true, aot == aotPS
		er, err := r.samplingRate() // extensionSamplingFrequencyIndex
		if err != nil {
			return Config{}, err
		}
		extRate = er
		aot = r.objectType()
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
	if chanConfig >= len(channelConfigs) {
		// 8-15: reserved in the base specification, with 11 through 14 given
		// layouts (6.1, 7.1 rear surround, 22.2, 7.1 top) by later
		// amendments. None are decoded here, and none may fall through
		// unnamed: the channel count would stay zero and Format would hand
		// back a format with no channels, leaving the caller to report
		// something generic about a count instead of the configuration that
		// caused it.
		return Config{}, malformed("channel configuration %d is not supported", chanConfig)
	}
	if chanConfig == 7 {
		// Configuration 7 is refused because the field disagrees with the
		// specification about what it means. ISO/IEC 14496-3 Table 1.19
		// defines it as 7.1 with a second FRONT pair (a centre SCE, then
		// Lc/Rc, L/R, Ls/Rs, LFE); ffmpeg writes this configuration for its
		// own 7.1, which is a SIDE pair (FL FR FC LFE BL BR SL SR), and reads
		// it back the same way. Both readings decode at full length with
		// every channel present, so picking one silently sends half the
		// world's config-7 files to the wrong speakers -- the failure mode
		// this whole channel map exists to prevent. Nothing else needs it:
		// configuration 6 is the multichannel case that occurs, and real 7.1
		// otherwise arrives as configuration 0 with a program config element.
		return Config{}, malformed("channel configuration 7 is not supported: the specification and the common encoder convention disagree on its channel order")
	}
	if rate <= 0 {
		return Config{}, malformed("sampling frequency index reserved")
	}
	return Config{
		ObjectType:    aot,
		SampleRate:    rate,
		Channels:      channels,
		ChannelConfig: chanConfig,
		FrameLength:   frameLen,
		ASC:           append([]byte(nil), b...),
		SBR:           sbr,
		PS:            ps,
		ExtensionRate: extRate,
	}, nil
}

// SBRWarning returns the note a demuxer should record for an explicitly
// signalled SBR/PS config, or "" when there is nothing to warn about. The
// package decodes the base layer only, so the output is band-limited against
// what a full HE-AAC decoder would produce. A demuxer that carries warnings
// records this one, which is what makes the limitation visible at runtime
// rather than only in the package doc.
func (c Config) SBRWarning() string {
	if !c.SBR {
		return ""
	}
	name := "SBR"
	if c.PS {
		name = "SBR/PS"
	}
	if c.ExtensionRate > 0 {
		return fmt.Sprintf("%s signalled at %d Hz: high band not synthesized, decoding the AAC-LC base layer at %d Hz",
			name, c.ExtensionRate, c.SampleRate)
	}
	return fmt.Sprintf("%s high band not synthesized; decoding the AAC-LC base layer at %d Hz", name, c.SampleRate)
}

// channelLayouts gives the WAVE channel mask each channelConfiguration
// names (ISO/IEC 14496-3 Table 1.19), indexed by the configuration.
// Configurations 0 and 7 have no entry: 0's layout rides in an in-band
// program config element this package does not parse, and 7 is refused
// outright (see ParseASC).
//
// Most rows agree with audio.DefaultLayout for their count, and one does
// not: configuration 4 is AAC's 4.0 (centre, left/right, and a single back
// centre), where DefaultLayout(4) is quad (no centre, a back pair). Reading
// the count and taking the conventional layout for it would put that file's
// centre and surround into the wrong speakers, which is why the table is
// spelled out rather than derived.
var channelLayouts = [...]audio.ChannelMask{
	1: audio.FrontCenter,
	2: audio.FrontLeft | audio.FrontRight,
	3: audio.FrontLeft | audio.FrontRight | audio.FrontCenter,
	4: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackCenter,
	5: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackLeft | audio.BackRight,
	6: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.LowFrequency |
		audio.BackLeft | audio.BackRight,
}

// waveSlots maps an element's position in a frame's channel sequence to
// the output channel it writes, the AAC counterpart of vorbis's
// waveFromVorbis. AAC orders its elements centre-outward (Table 1.19)
// where WAVE orders channels by ascending speaker-mask bit, so everything
// past stereo is a permutation; returning nil means the order is not
// known and the stream must be refused rather than guessed at.
//
// channels is the resolved output count, which for configuration 0 comes
// from the container. Only mono and stereo are accepted there, and they
// are the two shapes where every element order agrees anyway.
func waveSlots(chanConfig, channels int) []int {
	switch chanConfig {
	case 0:
		if channels < 1 || channels > 2 {
			return nil
		}
		return identitySlots(channels)
	case 1: // SCE C
		return []int{0}
	case 2: // CPE L/R
		return []int{0, 1}
	case 3: // SCE C, CPE L/R
		return []int{2, 0, 1}
	case 4: // SCE C, CPE L/R, SCE Cs
		return []int{2, 0, 1, 3}
	case 5: // SCE C, CPE L/R, CPE Ls/Rs
		return []int{2, 0, 1, 3, 4}
	case 6: // SCE C, CPE L/R, CPE Ls/Rs, LFE
		return []int{2, 0, 1, 4, 5, 3}
	}
	return nil
}

func identitySlots(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

// channelElements names the element that codes each channel position: the
// same Table 1.19 sequence waveSlots describes, read by element type
// rather than by destination, so the two are the same length. A channel
// pair occupies two positions, both marked elCPE.
//
// The decoder checks against this because waveSlots routes by position,
// not by element type: a stream that sent its channel pair before its
// centre would have every channel written somewhere plausible and wrong,
// which is the one decoder failure that produces a full-length,
// full-loudness file of the wrong audio. Table 1.19 fixes the order, so a
// stream that deviates is non-conformant and is refused rather than
// silently rerouted.
//
// Only the configurations whose slot map is a permutation are listed.
// Mono and stereo route through the identity, so no element *order* can
// put a channel anywhere but where it belongs, and checking there would
// only reject streams it decodes correctly today: coding a stereo pair as
// two single-channel elements is a real habit, and it lands on 0 and 1
// either way. Element *type* still matters there for the LFE, which those
// layouts have no channel for; checkElement handles that separately.
func channelElements(chanConfig int) []int {
	switch chanConfig {
	case 3: // SCE C, CPE L/R
		return []int{elSCE, elCPE, elCPE}
	case 4: // SCE C, CPE L/R, SCE Cs
		return []int{elSCE, elCPE, elCPE, elSCE}
	case 5: // SCE C, CPE L/R, CPE Ls/Rs
		return []int{elSCE, elCPE, elCPE, elCPE, elCPE}
	case 6: // SCE C, CPE L/R, CPE Ls/Rs, LFE
		return []int{elSCE, elCPE, elCPE, elCPE, elCPE, elLFE}
	}
	return nil
}

// elementName renders a syntactic element type for a diagnostic.
func elementName(tag int) string {
	switch tag {
	case elSCE:
		return "a single channel element"
	case elCPE:
		return "a channel pair element"
	case elLFE:
		return "an LFE element"
	}
	return fmt.Sprintf("element type %d", tag)
}

// Format is the pipeline format the decoder emits: the base rate in the
// pipeline's 32-bit float domain, with the layout the channel
// configuration names.
//
// It fails rather than guessing a channel count. A configuration of 0
// carries both the count and the element order in an in-band program
// config element, which this package does not parse; the containers fill
// the count in from their own metadata, and mono and stereo are the only
// shapes where that is enough to decode correctly. Defaulting the rest to
// stereo, as this once did, turns an unreadable layout into audio that is
// silently the wrong channels.
//
// Every configuration with no layout of its own is refused by name too.
// ParseASC has already rejected those, so reaching that arm means a
// hand-built Config; it still names the configuration rather than
// returning a format with no channels, since the caller would otherwise
// report a channel count where the configuration is the cause.
func (c Config) Format() (audio.Format, error) {
	ch := c.Channels
	layout := audio.DefaultLayout(ch)
	switch {
	case c.ChannelConfig == 0:
		if ch < 1 {
			return audio.Format{}, malformed("channel configuration 0: the channel count is carried by an in-band program config element, which is not parsed")
		}
		if ch > 2 {
			return audio.Format{}, malformed("channel configuration 0 with %d channels: the element order is carried by an in-band program config element, which is not parsed", ch)
		}
	// Bounded on both sides: this arm is reached only by a hand-built
	// Config, where the field is whatever the caller put in it, and a
	// negative one would index out of the table rather than be refused.
	case c.ChannelConfig > 0 && c.ChannelConfig < len(channelLayouts) &&
		channelLayouts[c.ChannelConfig] != 0:
		layout = channelLayouts[c.ChannelConfig]
	default:
		return audio.Format{}, malformed("channel configuration %d is not supported", c.ChannelConfig)
	}
	return audio.Format{
		Rate:     c.SampleRate,
		Channels: ch,
		Layout:   layout,
		Type:     audio.Float,
		BitDepth: 32,
	}, nil
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
