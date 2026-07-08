// Package vorbis decodes Vorbis I audio (the Xiph "Vorbis I specification").
// It is a clean-room implementation from the specification, cross-checked
// against the permissive references stb_vorbis (public domain) and
// jfreymuth/oggvorbis (MIT) for structure only.
//
// The decoder consumes whole Vorbis packets (as the Ogg demuxer reassembles
// them) and emits planar float32 buffers. Vorbis is inherently float, so the
// track format is always Float/32; positions, gapless trimming, and seeking
// are format.Media's job. A Vorbis packet's output length depends on both its
// own block size and the previous packet's (the overlap-add is between
// neighbours), so the first packet after a Reset primes the overlap and emits
// nothing, exactly as the reference decoders do.
package vorbis

import (
	"errors"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

// errEndOfPacket signals a read past the end of a packet. Vorbis treats this
// as a legal early termination in several places (a floor that runs out of
// bits produces a silent frame), so the decoder catches it rather than
// failing the stream.
var errEndOfPacket = errors.New("vorbis: end of packet")

// Hostile-input caps. Vorbis setup data is attacker-controlled, so every
// count is bounded before allocation (ADR-0005 invariants).
const (
	maxChannels    = audio.MaxChannels
	maxCodebooks   = 1 << 16
	maxCodewordLen = 32
	maxFloors      = 1 << 6
	maxResidues    = 1 << 6
	maxMappings    = 1 << 6
	maxModes       = 1 << 6
	maxSubmaps     = 16
	maxFloor1Parts = 31
	maxFloor1Class = 65
	maxFloor1Xs    = 65
	// maxBlockSize bounds a single transform: the spec caps blocksize_1 at
	// 8192 (log2 == 13).
	maxBlockSize = 8192
	minBlockLog  = 6
	maxBlockLog  = 13
)

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "vorbis: "+fmt.Sprintf(format, args...))
}

// bitReader reads Vorbis's little-endian bit packing: the first bit of a
// field is the least-significant bit of the byte, and multi-bit fields fill
// from the low bit up. Reads past the end return zero bits and latch eof,
// which is legal at end of packet (the spec ends several loops that way).
type bitReader struct {
	data  []byte
	bytex int
	bitx  uint // bits already consumed from data[bytex], 0..7
	eof   bool
}

func newBitReader(data []byte) *bitReader { return &bitReader{data: data} }

// bit reads a single bit.
func (r *bitReader) bit() uint32 {
	if r.bytex >= len(r.data) {
		r.eof = true
		return 0
	}
	b := (uint32(r.data[r.bytex]) >> r.bitx) & 1
	r.bitx++
	if r.bitx == 8 {
		r.bitx = 0
		r.bytex++
	}
	return b
}

// read reads n bits (0..32) LSB-first.
func (r *bitReader) read(n int) uint32 {
	var result uint32
	got := 0
	for got < n {
		if r.bytex >= len(r.data) {
			r.eof = true
			return result
		}
		avail := 8 - int(r.bitx)
		take := n - got
		if take > avail {
			take = avail
		}
		chunk := (uint32(r.data[r.bytex]) >> r.bitx) & ((1 << take) - 1)
		result |= chunk << got
		got += take
		r.bitx += uint(take)
		if r.bitx == 8 {
			r.bitx = 0
			r.bytex++
		}
	}
	return result
}

// Config is the decoder configuration carried out of band by the container
// (the concatenated identification and setup headers). The Ogg mapping fills
// it from the three Vorbis header packets.
type Config struct {
	Version  uint32
	Channels int
	Rate     int
	Bitrate  int // nominal, informational only

	blockSizes [2]int // [short, long], in samples

	codebooks []codebook
	floors    []floor
	residues  []residue
	mappings  []mapping
	modes     []mode
}

// Format returns the PCM format this stream decodes to. Vorbis is always
// float; the layout is the conventional one for the channel count, because
// the decoder reorders its native Vorbis channel order (spec 4.3.9) into
// WAVE order on emit so downstream mix matrices see a standard layout.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     c.Rate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// LongBlock returns the long transform size, used by the Ogg mapping to size a
// seek pre-roll.
func (c Config) LongBlock() int { return c.blockSizes[1] }

// ModeBits returns the number of bits a packet's mode number occupies, which
// the Ogg mapping needs to read a packet's block size without a full decode.
func ModeBits(c Config) int { return ilog(len(c.modes) - 1) }

// PacketBlockSize reads an audio packet's block size (its mode's transform
// length) without decoding it. modeBits comes from ModeBits. ok is false for a
// non-audio packet or an out-of-range mode.
func PacketBlockSize(c Config, modeBits int, pkt []byte) (block int, ok bool) {
	if len(pkt) == 0 || pkt[0]&1 != 0 {
		return 0, false
	}
	r := newBitReader(pkt)
	if r.bit() != 0 {
		return 0, false
	}
	modeNum := int(r.read(modeBits))
	if modeNum >= len(c.modes) {
		return 0, false
	}
	if c.modes[modeNum].blockflag {
		return c.blockSizes[1], true
	}
	return c.blockSizes[0], true
}

// waveFromVorbis maps a WAVE-order output channel index to the Vorbis-native
// source channel index for a stream of n channels (spec section 4.3.9). Mono
// and stereo are identity; 3+ reorder (Vorbis puts center second, LFE last).
func waveFromVorbis(n int) []int {
	switch n {
	case 3: // Vorbis L,C,R -> WAVE FL,FR,FC
		return []int{0, 2, 1}
	case 4: // FL,FR,BL,BR -> identity
		return []int{0, 1, 2, 3}
	case 5: // Vorbis FL,C,FR,BL,BR -> WAVE FL,FR,FC,BL,BR
		return []int{0, 2, 1, 3, 4}
	case 6: // Vorbis FL,C,FR,BL,BR,LFE -> WAVE FL,FR,FC,LFE,BL,BR
		return []int{0, 2, 1, 5, 3, 4}
	case 7: // Vorbis FL,C,FR,SL,SR,BC,LFE -> WAVE FL,FR,FC,LFE,BC,SL,SR
		return []int{0, 2, 1, 6, 5, 3, 4}
	case 8: // Vorbis FL,C,FR,SL,SR,BL,BR,LFE -> WAVE FL,FR,FC,LFE,BL,BR,SL,SR
		return []int{0, 2, 1, 7, 5, 6, 3, 4}
	default: // 1, 2, and anything unconventional: identity
		id := make([]int, n)
		for i := range id {
			id[i] = i
		}
		return id
	}
}
