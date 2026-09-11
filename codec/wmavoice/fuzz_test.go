//go:build !wmavoicetablesgen

package wmavoice_test

// FuzzDecode asserts the hostile-input invariants on arbitrary packets under
// arbitrary configurations. This format has no sync word and no per-frame
// codec header, so every bound the decoder has comes from the WAVEFORMATEX and
// its 46 extra bytes; the fuzzer varies those too, since a crafted header is
// what a caller gets from a crafted file.
//
// The invariants: no panic, no unbounded production, and every emitted sample
// inside MaxMagnitude, which a NaN is not. The last one is not decorative
// here. Two recursions can run away, a sustained adaptive codebook gain above
// one and a crafted LSF set whose float32 LPC neither synthesis filter holds
// stable, and the denoise filter takes base-10 logarithms of an LPC power
// spectrum and divides by that spectrum's range. Any of them would otherwise
// put an infinity into every downstream sample, a long way from its cause,
// and the fuzzer found the LSF route (testdata/fuzz/FuzzDecode).
//
// The second argument is a RUN of packets driven through one decoder, and that
// is the point of the shape: the cross-packet carry and its bit offset, the
// pitch state, the gain predictor, the frame counter and the postfilter's four
// memories are all unreachable from a fuzzer that decodes once and resets.

import (
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
)

func FuzzDecode(f *testing.F) {
	// Configurations across both LSP orders, both LSP modes, the postfilter on
	// and off, the denoise tilt, and a small nBlockAlign so the fuzzer spends
	// its time on the walk rather than on kilobytes of zeroes.
	seeds := [][]byte{
		fuzzConfig(f, 8000, 64, 0x031980a7),
		fuzzConfig(f, 11025, 96, 0x0959e115),
		fuzzConfig(f, 16000, 80, 0x0b1991a7),
		fuzzConfig(f, 22050, 128, 0x0f99f205),
		fuzzConfig(f, 16000, 80, noPostfilter),
		fuzzConfig(f, 8000, 64, 0x031980e7), // the denoise tilt set
	}
	for _, cfg := range seeds {
		f.Add(cfg, make([]byte, 64))
		f.Add(cfg, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		f.Add(cfg, []byte{})
	}
	// A hand-built stream, which random bytes rarely reach the block layer of.
	s := newStream(f, 16000, 80, noPostfilter, defaultClasses())
	f.Add(s.config(), append(s.packet(0, 1), s.packet(0, 2)...))
	// Real packets, the only material that gets deep into a frame at all.
	for _, c := range cells {
		track, pkts := demux(f, corpusPath(f, c.name))
		var run []byte
		for _, p := range pkts[:min(len(pkts), 3)] {
			run = append(run, p...)
		}
		f.Add(track.CodecConfig, run)
	}

	f.Fuzz(func(t *testing.T, cfgRaw, run []byte) {
		cfg, err := wmavoice.ParseConfig(cfgRaw)
		if err != nil {
			return
		}
		dec, err := wmavoice.NewDecoder(cfg, cfg.Format())
		if err != nil {
			return
		}
		defer dec.Release()

		// The production bound: a packet cannot yield more than its own bits
		// allow, so a run of them cannot yield more than the whole input does
		// at the format's floor of a superframe's worth of bits. Generous by a
		// wide margin and still finite, which is the property under test.
		limit := int64(len(run)+1) * 8 * wmavoice.SuperframeSamples

		// The packetisation is the run's last byte's to choose, so the fuzzer
		// reaches what a container hands over besides exact nBlockAlign
		// packets: one byte over on every third packet (refused, with a carry
		// pending), an empty packet after each (the flush), and one byte
		// short (accepted, its tail missing).
		scheme := 0
		if len(run) > 0 {
			scheme = int(run[len(run)-1]) & 3
		}
		pass := func() []float32 {
			var out []float32
			emit := func(b *audio.Buffer) error {
				for i, v := range b.ChanF(0)[:b.N] {
					if !(math.Abs(float64(v)) <= wmavoice.MaxMagnitude) {
						t.Fatalf("sample %d of a buffer is %v", i, v)
					}
				}
				out = append(out, b.ChanF(0)[:b.N]...)
				if int64(len(out)) > limit {
					t.Fatalf("produced %d samples from %d bytes", len(out), len(run))
				}
				return nil
			}
			// A refusal is a fine answer anywhere below; what matters is that
			// the decoder is still usable afterwards, since nothing latches.
			for i, at := 0, 0; at < len(run); i++ {
				n := cfg.BlockAlign
				switch scheme {
				case 1:
					if i%3 == 2 {
						n++
					}
				case 3:
					n = max(n-1, 1)
				}
				end := min(at+n, len(run))
				_ = dec.Decode(run[at:end], emit)
				at = end
				if scheme == 2 {
					_ = dec.Decode(nil, emit)
				}
			}
			_ = dec.Drain(emit)
			return out
		}

		// Run it, then Reset and run it again: Reset must put back every field
		// a decode reads before it writes, and a refused packet's leftovers
		// must not reach the second run, so the two runs are the same samples.
		first := pass()
		dec.Reset()
		dec.SetPosition(0)
		second := pass()
		if !slices.Equal(first, second) {
			t.Fatalf("a run after Reset differs from the first: %d against %d samples", len(second), len(first))
		}
	})
}

// fuzzConfig is a WAVEFORMATEX plus extra bytes with the default frame type
// tree, which is what a seed needs to reach the frame layer at all.
func fuzzConfig(t testing.TB, rate, align int, flags uint32) []byte {
	t.Helper()
	return newStream(t, rate, align, flags, defaultClasses()).config()
}
