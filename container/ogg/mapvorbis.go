package ogg

import (
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/container"
)

// vorbisMapping decodes the Ogg-Vorbis mapping (Vorbis I spec section A.2).
// Vorbis accumulates: a packet's output length is (previous block size + this
// block size)/4, so timing is stateful and seeks anchor to page granules.
type vorbisMapping struct {
	id, comment, setup []byte
	cfg                vorbis.Config
	haveCfg            bool
	modeBits           int
	prevBlock          int // previous packet's block size, 0 before the first
	firstBlock         int // first audio packet's block size, for granuleShift
}

func (m *vorbisMapping) codecID() codec.ID { return codec.Vorbis }

// parseID stashes and lightly validates the identification header; the full
// parse happens once all three headers are in hand (finalizeTrack).
func (m *vorbisMapping) parseID(pkt []byte) (int, error) {
	if len(pkt) < 7 || pkt[0] != 0x01 || string(pkt[1:7]) != "vorbis" {
		return 0, malformed("vorbis identification header malformed")
	}
	m.id = append([]byte(nil), pkt...)
	return 2, nil // comment + setup headers follow
}

func (m *vorbisMapping) parseHeader(pkt []byte) error {
	switch {
	case m.comment == nil:
		m.comment = append([]byte(nil), pkt...)
	case m.setup == nil:
		m.setup = append([]byte(nil), pkt...)
		cfg, err := vorbis.ParseHeaders(m.id, m.comment, m.setup)
		if err != nil {
			return err
		}
		m.cfg = cfg
		m.haveCfg = true
		m.modeBits = vorbis.ModeBits(cfg)
	default:
		return malformed("unexpected fourth vorbis header packet")
	}
	return nil
}

// isAudio reports an audio packet: the packet type bit (LSB of the first byte)
// is 0, whereas header packets are odd-typed (1, 3, 5).
func (m *vorbisMapping) isAudio(pkt []byte) bool {
	return len(pkt) > 0 && pkt[0]&1 == 0
}

func (m *vorbisMapping) finalizeTrack(lastGranule func() int64) (container.Track, error) {
	if !m.haveCfg {
		return container.Track{}, malformed("vorbis stream missing setup header")
	}
	f := m.cfg.Format()
	if err := f.Valid(); err != nil {
		return container.Track{}, err
	}
	// The granule timeline leads the decoder output by firstBlock/2 (the
	// priming half-block the decoder never emits), so the playable length is
	// the final granule minus that shift.
	samples := int64(-1)
	if lg := lastGranule(); lg >= 0 {
		samples = lg - m.granuleShift()
		if samples < 0 {
			samples = 0
		}
	}
	return container.Track{
		Codec:        codec.Vorbis,
		CodecConfig:  vorbis.PackHeaders(m.id, m.comment, m.setup),
		Fmt:          f,
		Samples:      samples,
		SamplesExact: samples >= 0,
		Default:      true,
	}, nil
}

// packetTiming returns the packet's output length from its block size. The
// first packet primes the overlap and emits nothing (duration 0), matching the
// decoder; from the second on, output is (prevBlock + block)/4.
func (m *vorbisMapping) packetTiming(pkt []byte, running int64) (pts, dur int64, sync, ok bool) {
	if !m.haveCfg || len(pkt) == 0 || pkt[0]&1 != 0 {
		return 0, 0, false, false
	}
	block, valid := vorbis.PacketBlockSize(m.cfg, m.modeBits, pkt)
	if !valid {
		return 0, 0, false, false
	}
	if m.firstBlock == 0 {
		m.firstBlock = block
	}
	if m.prevBlock != 0 {
		dur = int64(m.prevBlock+block) / 4
	}
	m.prevBlock = block
	return running, dur, true, true
}

func (m *vorbisMapping) selfTiming() bool { return false }

// preroll lands a long block before the target so the decoder primes its
// overlap before the first delivered sample.
func (m *vorbisMapping) preroll() int64 { return int64(m.cfg.LongBlock()) }

func (m *vorbisMapping) granuleShift() int64 { return int64(m.firstBlock / 2) }

// resetTiming clears the block-size accumulator for a seek restart; firstBlock
// is a stream property set once at parse and is left intact.
func (m *vorbisMapping) resetTiming() { m.prevBlock = 0 }
