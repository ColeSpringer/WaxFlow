package wmalossless_test

// FuzzDecode asserts the hostile-input invariants on arbitrary packets under
// arbitrary configurations. This format has no sync word, no frame header and
// no per-frame length, so every bound the decoder has comes from the
// WAVEFORMATEX; the fuzzer varies that too, since a crafted one is what a
// caller gets from a crafted file.
//
// The invariants: no panic, no unbounded production, and every emitted sample
// inside the depth the format declares. The last one is not decorative. A
// crafted stream can drive the filters past the range, and a sample outside
// the declared depth leaving here is a value the buffer's own contract says
// cannot exist, a long way from its cause.
//
// The second argument is a RUN of packets driven through one decoder, and that
// is the point of the shape. Everything this decoder carries across a packet
// boundary is unreachable from a fuzzer that decodes once and resets: the
// cross-packet carry and its bit offset, the sequence number, the filter state
// that is restated only at a seekable tile, and the latched failure. A
// single-packet fuzzer certifies the single-packet path and says nothing about
// the state machine, which for a codec whose frames span packets is the whole
// of it.

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmalossless"
)

func FuzzDecode(f *testing.F) {
	// Small packets and short frames, so the fuzzer spends its time on the
	// walk rather than on megabytes of zeroes.
	small := config(16000, 2, 16, 0x03, 64, testFlags)
	mono := config(22050, 1, 24, 0, 48, testFlags)
	deep := config(44100, 2, 24, 0x03, 96, 0x01a1)
	noDRC := config(16000, 2, 16, 0x03, 64, 0x0121)
	prefixed := config(16000, 2, 16, 0x03, 64, 0x01e1)
	for _, cfg := range [][]byte{small, mono, deep, noDRC, prefixed} {
		f.Add(cfg, uint16(0), make([]byte, 192))
		f.Add(cfg, uint16(0), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		f.Add(cfg, uint16(0x2), make([]byte, 128))
		f.Add(cfg, uint16(0xffff), make([]byte, 256))
	}
	f.Add(small, uint16(0), []byte{})
	f.Add([]byte{0x63, 0x01}, uint16(0), []byte{1, 2, 3})

	// Real packets from the committed corpus, which is the only source of
	// material that gets past the first subframe at all.
	for _, c := range cells {
		if !c.committed {
			continue
		}
		track, pkts := demux(f, corpusPath(f, c.name))
		var run []byte
		for i, p := range pkts {
			if i == 3 {
				break
			}
			run = append(run, p...)
		}
		f.Add(track.CodecConfig, uint16(0), run)
		f.Add(track.CodecConfig, uint16(0x4), run)
	}

	f.Fuzz(func(t *testing.T, cfgBytes []byte, resets uint16, stream []byte) {
		cfg, err := wmalossless.ParseConfig(cfgBytes)
		if err != nil {
			return
		}
		// A large packet under a long frame is a slow input rather than an
		// interesting one; the walk it exercises is the same.
		if cfg.BlockAlign > 16<<10 || len(stream)/cfg.BlockAlign > 64 {
			return
		}
		dec, err := wmalossless.NewDecoder(cfg, cfg.Format())
		if err != nil {
			return
		}
		defer dec.Release()

		// One Decode emits the frames that began in the previous packet, which
		// the decoder caps; without that ceiling a packet is a decompression
		// bomb, since a frame whose channels are all uncoded is a handful of
		// bits and still covers a whole frame of samples.
		limit := 1024 * cfg.SamplesPerFrame() * cfg.Channels
		lo := int32(-1) << (cfg.BitsPerSample - 1)
		hi := -(lo + 1)
		got := 0
		emit := func(b *audio.Buffer) error {
			if b.N > cfg.SamplesPerFrame() {
				t.Fatalf("a buffer of %d samples, longer than the %d-sample frame",
					b.N, cfg.SamplesPerFrame())
			}
			got += b.N * b.Fmt.Channels
			if got > limit {
				t.Fatalf("%d samples from one packet, limit %d", got, limit)
			}
			for ch := 0; ch < b.Fmt.Channels; ch++ {
				for i, v := range b.ChanI(ch)[:b.N] {
					if v < lo || v > hi {
						t.Fatalf("channel %d sample %d is %d, outside %d..%d at %d bits",
							ch, i, v, lo, hi, cfg.BitsPerSample)
					}
				}
			}
			return nil
		}

		// The run is walked as it lies: a refused packet is NOT reset away, so
		// the next Decode is one on a decoder that has already failed, which is
		// what a caller retrying after an error does.
		for i, at := 0, 0; at+cfg.BlockAlign <= len(stream); i, at = i+1, at+cfg.BlockAlign {
			if resets>>(uint(i)&15)&1 != 0 {
				dec.Reset()
			}
			got = 0
			if err := dec.Decode(stream[at:at+cfg.BlockAlign], emit); err != nil {
				continue
			}
		}
		got = 0
		_ = dec.Drain(emit)
	})
}
