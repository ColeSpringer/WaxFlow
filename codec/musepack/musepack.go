// Package musepack implements a Musepack decoder for stream versions 7 and 8,
// ported from the reference libmpcdec (see THIRD-PARTY-NOTICES.md): the
// Huffman books, the frame syntax and requantisation, the noise-substitution
// generator and the 32-band polyphase synthesis, so decodes match the
// reference's float output.
//
// Decode scope is every stream the reference plays: SV7 (mppenc) and SV8
// (mpcenc) at the format's four rates, mono and stereo, with or without noise
// substitution, mid/side and true gapless. The pre-2004 stream versions 4 to 6
// are refused by name, as the reference refuses them; so are more than two
// channels. Encoding is a non-goal.
//
// A frame is 32 subbands of 36 samples, 1152 PCM samples per channel. In SV7
// every frame carries its own bit length but the scalefactor deltas chain
// across frames forever, so no frame is a sync point on its own; in SV8 frames
// are grouped into blocks whose first frame is a key frame. The container
// therefore builds each packet as one frame (SV7) or one block (SV8) behind a
// small header this package defines (PutPacketHeader), optionally followed by
// the decoder state a seek landing needs.
package musepack

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// The committed .mpc fixtures are regenerated with the reference encoders; see
// fixturegen_test.go.
//go:generate go test -tags mpcfixtures -run ^TestGenerateFixtures$ -count=1

// Version is the decoder's cache-key version constant (ADR-0004): bump on any
// change that alters decoded samples.
const Version = "musepack-dec-1"

// Format constants.
const (
	// FrameLength is the PCM samples per channel one frame decodes to: 32
	// subbands times 36 subband samples.
	FrameLength = 1152
	// SynthDelay is the synthesis filter's priming, in samples. The reference
	// drops this many from the start of every stream; the container reports it
	// as the track's Delay.
	SynthDelay = 481
	// MaxBands is the subband count.
	MaxBands = 32
	// MaxBlockPwr is the largest frames-per-block exponent SV8 can state
	// (three bits, doubled), so a block holds at most 1 << MaxBlockPwr frames.
	MaxBlockPwr = 14
	// MaxChannels is this decoder's scope, and the reference's.
	MaxChannels = 2
)

// Rates maps the two-bit (SV7) or three-bit (SV8) rate index. The reference
// table has eight entries, the last four zero: an index there is refused.
var Rates = [8]int{44100, 48000, 37800, 32000}

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("musepack: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not, which is a thing a caller can act on. See
// [waxerr.Malformed] for the rule.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("musepack: ", format, args...)
}

// MatchNeed is how many bytes Match wants: the file magic.
const MatchNeed = 4

// Match reports whether b begins with a Musepack stream: "MPCK" (SV8) or
// "MP+" with a stream version nibble (SV7, and the SV4-6 headers that share
// the magic and are refused by name once the driver has the file). It is the
// format sniff-table entry; a leading ID3v2 tag is the registry's business.
func Match(b []byte) bool {
	if len(b) < MatchNeed {
		return false
	}
	if string(b[:4]) == "MPCK" {
		return true
	}
	return string(b[:3]) == "MP+" && b[3]&0x0F >= 4 && b[3]&0x0F <= 7
}

// Config is what a Decoder is built from: the coded stream's shape. Both
// stream versions share it; StreamVersion selects the frame syntax.
type Config struct {
	StreamVersion int
	Rate          int
	Channels      int
	// MaxBand is the highest coded subband, 0..31 (the header's max_band).
	MaxBand int
	// MS reports whether mid/side coding is enabled for the stream.
	MS bool
	// BlockPwr is the SV8 frames-per-block exponent (frames = 1 << BlockPwr);
	// 0 for SV7, whose packets are single frames.
	BlockPwr int
	// PNS is the encoder's noise-substitution declaration: 0 off, 1 on,
	// PNSUnknown when the stream does not say (SV8 without an EI packet).
	PNS uint8
	// TrueGapless and LastFrameSamples are the SV7 header's gapless fields,
	// carried for reporting; the container derives the length from them.
	TrueGapless      bool
	LastFrameSamples int
}

// PNSUnknown is the PNS value for a stream that does not declare it.
const PNSUnknown = 0xFF

// Format is the pipeline format the decoder emits: float32 in +-1.0, the
// reference's own output domain.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// FramesPerBlock is how many frames a full SV8 block holds, 1 for SV7.
func (c Config) FramesPerBlock() int {
	if c.StreamVersion != 8 {
		return 1
	}
	return 1 << c.BlockPwr
}

// Validate checks a Config against what this decoder covers.
func (c Config) Validate() error {
	switch {
	case c.StreamVersion >= 4 && c.StreamVersion <= 6:
		return unsupported("Musepack SV%d is not supported: only SV7 and SV8 are", c.StreamVersion)
	case c.StreamVersion != 7 && c.StreamVersion != 8:
		return malformed("stream version %d", c.StreamVersion)
	case c.Channels < 1 || c.Channels > MaxChannels:
		return unsupported("%d channels: only mono and stereo are supported", c.Channels)
	case c.StreamVersion == 7 && c.Channels != 2:
		return malformed("SV7 streams are always two channels, not %d", c.Channels)
	case !validRate(c.Rate):
		return malformed("sample rate %d is not one of 44100, 48000, 37800, 32000", c.Rate)
	case c.MaxBand < 1 || c.MaxBand > MaxBands-1:
		// The reference refuses max_band 0 and 32 alike (check_streaminfo).
		return malformed("max band %d outside 1..31", c.MaxBand)
	case c.BlockPwr < 0 || c.BlockPwr > MaxBlockPwr || c.BlockPwr%2 != 0:
		return malformed("block power %d is not an even number in 0..%d", c.BlockPwr, MaxBlockPwr)
	case c.StreamVersion == 7 && c.BlockPwr != 0:
		return malformed("SV7 has no blocks")
	case c.PNS > 1 && c.PNS != PNSUnknown:
		return malformed("PNS flag %d", c.PNS)
	case c.LastFrameSamples < 0 || c.LastFrameSamples > FrameLength:
		return malformed("last frame of %d samples exceeds %d", c.LastFrameSamples, FrameLength)
	}
	return nil
}

func validRate(rate int) bool {
	for _, r := range Rates {
		if r != 0 && r == rate {
			return true
		}
	}
	return false
}

// configVersion versions the marshaled Config layout.
const configVersion = 1

// MarshalBinary encodes the Config for Track.CodecConfig. Musepack carries no
// out-of-band config blob of its own, so this is our canonical form.
func (c Config) MarshalBinary() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	b[0] = configVersion
	b[1] = byte(c.StreamVersion)
	b[2] = byte(c.Channels)
	b[3] = byte(c.MaxBand)
	if c.MS {
		b[4] |= 1
	}
	if c.TrueGapless {
		b[4] |= 2
	}
	b[5] = byte(c.BlockPwr)
	b[6] = c.PNS
	binary.LittleEndian.PutUint32(b[8:], uint32(c.Rate))
	binary.LittleEndian.PutUint16(b[12:], uint16(c.LastFrameSamples))
	return b, nil
}

// ParseConfig decodes a Track.CodecConfig produced by MarshalBinary.
func ParseConfig(b []byte) (Config, error) {
	if len(b) != 16 || b[0] != configVersion || b[7] != 0 || b[14] != 0 || b[15] != 0 {
		return Config{}, malformed("malformed codec config")
	}
	c := Config{
		StreamVersion:    int(b[1]),
		Channels:         int(b[2]),
		MaxBand:          int(b[3]),
		MS:               b[4]&1 != 0,
		TrueGapless:      b[4]&2 != 0,
		BlockPwr:         int(b[5]),
		PNS:              b[6],
		Rate:             int(binary.LittleEndian.Uint32(b[8:])),
		LastFrameSamples: int(binary.LittleEndian.Uint16(b[12:])),
	}
	if b[4]&^3 != 0 {
		return Config{}, malformed("malformed codec config")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// PacketHeaderLen is the size of the header a packet carries ahead of its
// coded bits.
//
// An SV7 frame's bit length lives in a field the container consumes, and an
// SV8 block never states its own frame count (the reference stops by sample
// count), so the container writes both here. The optional state block is what
// makes a seek landing exact: SV7 has no key frames, so a frame decoded after
// a seek needs the scalefactors the frames before it left behind, and either
// version's noise generator is state the format never resets.
//
//	bytes 0     flags: bit0 hasState (a state block follows the header)
//	bytes 1     reserved
//	bytes 2:4   frames in this packet (u16 LE): SV7 always 1, SV8 the block's
//	bytes 4:8   SV7: the frame's bit length (u32 LE); SV8: 0
//	bytes 8..   the state block when flagged, then the payload
const PacketHeaderLen = 8

// State block sizes: the SV7 block is the last scalefactor of every band and
// channel (2 x 32 int32) plus the noise generator's two words; SV8 blocks start
// with a key frame, so only the generator is carried.
const (
	StateLenSV7 = 2*MaxBands*4 + 8
	StateLenSV8 = 8
)

// hasState is the packet flag marking a state block.
const hasState = 1

// PutPacketHeader writes the frame count and bit length into the first
// PacketHeaderLen bytes of b, and marks whether a state block follows.
func PutPacketHeader(b []byte, frames, bitLen int, state bool) {
	b[0], b[1] = 0, 0
	if state {
		b[0] = hasState
	}
	binary.LittleEndian.PutUint16(b[2:], uint16(frames))
	binary.LittleEndian.PutUint32(b[4:], uint32(bitLen))
}

// PacketHeader is what ParsePacketHeader reads back.
type PacketHeader struct {
	Frames   int
	BitLen   int
	HasState bool
}

// ParsePacketHeader reads a packet's header and returns what follows it: the
// state block (nil when none) and the payload.
func ParsePacketHeader(pkt []byte, cfg Config) (h PacketHeader, state, payload []byte, err error) {
	if len(pkt) < PacketHeaderLen {
		return h, nil, nil, malformed("packet header of %d bytes, want %d", len(pkt), PacketHeaderLen)
	}
	if pkt[0]&^hasState != 0 || pkt[1] != 0 {
		return h, nil, nil, malformed("packet flags %#x", pkt[0])
	}
	h.HasState = pkt[0]&hasState != 0
	h.Frames = int(binary.LittleEndian.Uint16(pkt[2:]))
	h.BitLen = int(binary.LittleEndian.Uint32(pkt[4:]))
	rest := pkt[PacketHeaderLen:]
	if h.HasState {
		n := StateLenSV8
		if cfg.StreamVersion == 7 {
			n = StateLenSV7
		}
		if len(rest) < n {
			return h, nil, nil, malformed("state block of %d bytes, want %d", len(rest), n)
		}
		state, rest = rest[:n], rest[n:]
	}
	switch {
	case h.Frames < 1 || h.Frames > cfg.FramesPerBlock():
		return h, nil, nil, malformed("packet of %d frames, want 1..%d", h.Frames, cfg.FramesPerBlock())
	case cfg.StreamVersion == 7 && (h.BitLen <= 0 || uint64(h.BitLen) > uint64(len(rest))*8):
		// Compared in 64 bits: on a 32-bit platform the field is wider than
		// int, so an addition here could wrap and pass a bound it fails.
		return h, nil, nil, malformed("frame of %d bits in a %d-byte payload", h.BitLen, len(rest))
	case cfg.StreamVersion == 8 && h.BitLen != 0:
		return h, nil, nil, malformed("SV8 packets state no bit length")
	}
	return h, state, rest, nil
}
