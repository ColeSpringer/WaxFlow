//go:build !wmaprotablesgen

package wmapro_test

// FuzzDecode asserts the hostile-input invariants on arbitrary packets under
// arbitrary configurations. This format has no sync word and no per-frame
// codec header, so every bound the decoder has comes from the WAVEFORMATEX;
// the fuzzer varies that too, since a crafted one is what a caller gets from
// a crafted file.
//
// The invariants: no panic, no unbounded production, and every emitted sample
// finite. The last one is not decorative. The per-band gain is 10 to a
// transmitted exponent, and a crafted step escape can name a gain no float32
// holds; a NaN or an Inf leaving here poisons the loudness meter and the
// resampler downstream, a long way from its cause.
//
// The second argument is a RUN of packets driven through one decoder, and
// that is the point of the shape: the cross-packet carry and its bit offset,
// the sequence number, the rolling buffer and the owed lead-in are all
// unreachable from a fuzzer that decodes once and resets.

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmapro"
)

func FuzzDecode(f *testing.F) {
	// Small packets and short frames, so the fuzzer spends its time on the
	// walk rather than on kilobytes of zeroes.
	small := config(16000, 2, 16, 0x03, 64, 0x00c0, 0)
	mono := config(22050, 1, 24, 0, 48, 0x00c0, 0)
	deep := config(44100, 2, 24, 0x03, 96, testFlags, 0)
	six := config(48000, 6, 24, 0x3f, 128, testFlags, 0)
	noDRC := config(16000, 2, 16, 0x03, 64, 0x0040, 0)
	twoSub := config(16000, 2, 16, 0x03, 64, 0x00c8, 0)
	for _, cfg := range [][]byte{small, mono, deep, six, noDRC, twoSub} {
		f.Add(cfg, uint16(0), make([]byte, 192))
		f.Add(cfg, uint16(0), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		f.Add(cfg, uint16(0x2), make([]byte, 128))
		f.Add(cfg, uint16(0xffff), make([]byte, 256))
	}
	f.Add(small, uint16(0), []byte{})
	f.Add([]byte{0x62, 0x01}, uint16(0), []byte{1, 2, 3})

	// Hand-built frames that reach the coefficient layer, which random
	// bytes seldom get past the tiling to.
	c := parse(f, smallConfig())
	var w bitWriter
	transmitFrame(f, c, &w, 0, nil)
	f.Add(smallConfig(), uint16(0), packet(f, c, 0, 0, frame(c, &w, false, 0), 0))

	// Real packets from the committed corpus, which is the only source of
	// material that gets deep into a frame at all.
	for _, cell := range committedCells() {
		if cell.refused {
			continue
		}
		track, pkts := demux(f, corpusPath(f, cell.name))
		var run []byte
		for i, p := range pkts {
			if i == 2 {
				break
			}
			run = append(run, p...)
		}
		f.Add(track.CodecConfig, uint16(0), run)
		f.Add(track.CodecConfig, uint16(0x4), run)
	}

	f.Fuzz(func(t *testing.T, cfgBytes []byte, resets uint16, stream []byte) {
		cfg, err := wmapro.ParseConfig(cfgBytes)
		if err != nil {
			return
		}
		// A large packet under a long frame is a slow input rather than an
		// interesting one; the walk it exercises is the same.
		if cfg.BlockAlign > 32<<10 || len(stream)/cfg.BlockAlign > 64 {
			return
		}
		dec, err := wmapro.NewDecoder(cfg, cfg.Format())
		if err != nil {
			return
		}
		defer dec.Release()

		// One Decode emits at most the frames one packet completes, which
		// the decoder caps; without that ceiling a packet is a decompression
		// bomb, since a frame in which no channel codes anything is a few
		// bits and still covers a whole frame of samples.
		limit := 256 * cfg.SamplesPerFrame() * cfg.Channels
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
				for i, v := range b.ChanF(ch)[:b.N] {
					if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
						t.Fatalf("channel %d sample %d is %v", ch, i, v)
					}
				}
			}
			return nil
		}

		// The run is walked as it lies: a refused packet is NOT reset away,
		// so the next Decode is one on a decoder that has just failed, which
		// is what a caller retrying after an error does.
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
