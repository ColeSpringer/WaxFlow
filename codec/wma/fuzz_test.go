//go:build !wmatablesgen

package wma_test

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wma"
)

// FuzzDecode asserts the hostile-input invariants on arbitrary packets under
// arbitrary configurations. WMA has no sync word, no frame header and no
// per-frame length, so every bound the decoder has comes from the
// WAVEFORMATEX: the fuzzer varies that too, since a crafted one is what a
// caller actually gets from a crafted file.
//
// The invariants: no panic, no unbounded production, and every emitted sample
// finite. That last one is not decorative. The block gain is a 127-escape
// ladder, the noise gains accumulate deltas, and both feed multipliers that a
// crafted stream can drive toward infinity; a NaN or an Inf leaving here
// poisons the loudness meter and the resampler downstream, where it would be a
// long way from its cause.
//
// The second argument is a RUN of packets driven through one decoder, not a
// single packet, and that is the point of the shape. Everything this decoder
// carries across a packet boundary -- the block-length walk, the exponent
// curve a later block reuses, the reservoir carry, the noise index, the
// latched failure -- is unreachable from a fuzzer that decodes once and
// resets. A single-packet fuzzer certifies the single-packet path and says
// nothing about the state machine, which is where every seam in this codec is.
func FuzzDecode(f *testing.F) {
	// Two configurations that between them cover both versions, both channel
	// counts, and the bit-reservoir header.
	v2 := cfgBytes(0x161, 2, 44100, 16000, 743, 10, 1)
	v1 := cfgBytes(0x160, 1, 22050, 4000, 185, 4, 1)
	res := cfgBytes(0x161, 1, 44100, 8000, 371, 10, 3)
	lsp := cfgBytes(0x161, 2, 44100, 16000, 743, 10, 0)
	varblk := cfgBytes(0x161, 1, 44100, 8000, 371, 10, 1|4)

	// A seed is one or more length-prefixed packets; run lays them out.
	for _, cfg := range [][]byte{v2, v1, res, lsp, varblk} {
		f.Add(cfg, run(make([]byte, 64)))
		f.Add(cfg, run([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}))
		f.Add(cfg, run([]byte{0}))
		// Several packets through one decoder, and the same again across a
		// Reset: the two orders a seam is reached from.
		f.Add(cfg, run(make([]byte, 64), make([]byte, 64), []byte{0x0c}))
		f.Add(cfg, append(run(make([]byte, 64)), reset(run(make([]byte, 64)))...))
	}
	f.Add(v2, []byte{})
	f.Add([]byte{0x61, 0x01}, run([]byte{1, 2, 3}))

	f.Fuzz(func(t *testing.T, cfgBytes, stream []byte) {
		cfg, err := wma.ParseConfig(cfgBytes)
		if err != nil {
			return
		}
		dec, err := wma.NewDecoder(cfg, cfg.Format())
		if err != nil {
			return
		}
		defer dec.Release()
		// One Decode emits at most fifteen frames: a non-reservoir packet is
		// one frame, and a reservoir packet states its count in four bits.
		// Drain adds the accumulator's last half. Without that ceiling a
		// packet would be a decompression bomb -- a block with no coded
		// channel is one bit and still covers a whole block of samples, so
		// bits alone would let three bytes produce two dozen frames.
		limit := 16 * cfg.FrameLen() * cfg.Channels
		got, pktLen := 0, 0
		emit := func(b *audio.Buffer) error {
			got += b.N * b.Fmt.Channels
			if got > limit {
				t.Fatalf("%d samples from a %d-byte packet, limit %d", got, pktLen, limit)
			}
			for ch := range b.Fmt.Channels {
				for i, v := range b.ChanF(ch) {
					if f := float64(v); math.IsNaN(f) || math.IsInf(f, 0) {
						t.Fatalf("channel %d sample %d is %v", ch, i, f)
					}
				}
			}
			return nil
		}
		// The run is walked as it lies: a refused packet is NOT reset away, so
		// the next Decode is one on a decoder that has already failed, which
		// is what a caller retrying after an error does and where the walk's
		// half-updated state used to be read as a stream's.
		for at := 0; at+2 <= len(stream); {
			n := int(stream[at])<<8 | int(stream[at+1])
			at += 2
			if n&resetBit != 0 {
				dec.Reset()
			}
			n = min(n&^resetBit, len(stream)-at)
			pkt := stream[at : at+n]
			at += n
			got, pktLen = 0, n
			if err := dec.Decode(pkt, emit); err != nil {
				continue
			}
			if err := dec.Drain(emit); err != nil {
				t.Fatalf("drain after a clean decode: %v", err)
			}
			// Drain is one-shot, so the run continues past it only after a
			// Reset; that is the seek path and the fuzzer reaches it through
			// the marker bit rather than on every packet.
			dec.Reset()
		}
	})
}

// resetBit marks a packet in a fuzz run that the decoder is Reset before,
// which is what a seek landing on it does.
const resetBit = 0x8000

// run lays packets out as the fuzz body reads them: a two-byte big-endian
// length, then that many bytes.
func run(pkts ...[]byte) []byte {
	var out []byte
	for _, p := range pkts {
		out = append(out, byte(len(p)>>8), byte(len(p)))
		out = append(out, p...)
	}
	return out
}

// reset marks the first packet of a run as one to Reset before.
func reset(b []byte) []byte {
	if len(b) > 0 {
		b[0] |= resetBit >> 8
	}
	return b
}

// cfgBytes builds a WAVEFORMATEX plus extra bytes, with flags2 at the offset
// the version puts it.
func cfgBytes(tag uint16, channels, rate, bytesPerSec, blockAlign, extra int, flags2 uint16) []byte {
	b := make([]byte, 18+extra)
	binary.LittleEndian.PutUint16(b, tag)
	binary.LittleEndian.PutUint16(b[2:], uint16(channels))
	binary.LittleEndian.PutUint32(b[4:], uint32(rate))
	binary.LittleEndian.PutUint32(b[8:], uint32(bytesPerSec))
	binary.LittleEndian.PutUint16(b[12:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(b[14:], 16)
	binary.LittleEndian.PutUint16(b[16:], uint16(extra))
	at := 2
	if tag == 0x161 {
		at = 4
	}
	if extra >= at+2 {
		binary.LittleEndian.PutUint16(b[18+at:], flags2)
	}
	return b
}
