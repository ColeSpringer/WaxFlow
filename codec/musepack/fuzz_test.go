package musepack_test

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/musepack"
)

// FuzzDecode asserts the hostile-input invariants on arbitrary packets under
// arbitrary configurations: no panic, no unbounded production, every emitted
// sample finite, the emitted format the config's.
//
// The second argument is a RUN of packets driven through one decoder with no
// Reset between them, and that is the point of the shape: the scalefactor
// chain, the key-frame flags, the noise generator and the latched failure all
// live across packets, and a fuzzer that decodes once and resets never
// reaches them. A marker bit in a packet's length Resets before it, which is
// the seek path.
func FuzzDecode(f *testing.F) {
	sv7, err := musepack.Config{StreamVersion: 7, Rate: 44100, Channels: 2, MaxBand: 28, MS: true, PNS: 1, TrueGapless: true, LastFrameSamples: 700}.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	sv8, err := musepack.Config{StreamVersion: 8, Rate: 48000, Channels: 1, MaxBand: 31, BlockPwr: 2, PNS: 1}.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	sv8s, err := musepack.Config{StreamVersion: 8, Rate: 44100, Channels: 2, MaxBand: 25, MS: true, BlockPwr: 0, PNS: 0}.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	for _, name := range []string{"noise-s16.mpc", "sine-s16.mpc"} {
		cfg, _, pkts := demuxAll(f, readFile(f, repoPath("testdata", name)))
		blob, _ := cfg.MarshalBinary()
		f.Add(blob, run(pkts[:min(3, len(pkts))]...))
		f.Add(blob, append(run(pkts[0]), reset(run(pkts[len(pkts)-1]))...))
		damaged := append([]byte(nil), pkts[0]...)
		damaged[len(damaged)/2] ^= 0xFF
		f.Add(blob, run(damaged, pkts[0]))
	}
	for _, cfg := range [][]byte{sv7, sv8, sv8s} {
		f.Add(cfg, run(make([]byte, 64)))
		f.Add(cfg, run([]byte{0, 0, 1, 0, 0xff, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}))
		f.Add(cfg, run([]byte{1, 0, 1, 0, 0, 0, 0, 0}))
		f.Add(cfg, append(run(make([]byte, 32)), reset(run(make([]byte, 300)))...))
	}
	f.Add(sv7, []byte{})
	f.Add([]byte{1, 2}, run([]byte{1, 2, 3}))

	f.Fuzz(func(t *testing.T, cfgBytes, stream []byte) {
		cfg, err := musepack.ParseConfig(cfgBytes)
		if err != nil {
			return
		}
		dec, err := musepack.NewDecoder(cfg, cfg.Format())
		if err != nil {
			return
		}
		defer dec.Release()
		emitted, declared := 0, 0
		emit := func(b *audio.Buffer) error {
			emitted += b.N
			if b.Fmt != cfg.Format() {
				t.Fatalf("emitted %v, want %v", b.Fmt, cfg.Format())
			}
			if emitted > declared*musepack.FrameLength {
				t.Fatalf("emitted %d samples for a packet declaring %d frames", emitted, declared)
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
		for at := 0; at+2 <= len(stream); {
			n := int(stream[at])<<8 | int(stream[at+1])
			at += 2
			if n&resetBit != 0 {
				dec.Reset()
			}
			n = min(n&^resetBit, len(stream)-at)
			pkt := stream[at : at+n]
			at += n
			emitted, declared = 0, 0
			if h, _, _, err := musepack.ParsePacketHeader(pkt, cfg); err == nil {
				declared = h.Frames
			}
			// A refused packet is NOT reset away: the next Decode is one on a
			// decoder that has already failed, which is what a caller retrying
			// after an error does.
			_ = dec.Decode(pkt, emit)
		}
	})
}

// resetBit marks a packet in a fuzz run that the decoder is Reset before.
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

// FuzzParseConfig asserts the config parser on arbitrary bytes: no panic, and
// an accepted config describes a stream a decoder can be built for and
// round-trips.
func FuzzParseConfig(f *testing.F) {
	cfg, _, _ := demuxAll(f, readFile(f, repoPath("testdata", "sine-s16.mpc")))
	blob, _ := cfg.MarshalBinary()
	f.Add(blob)
	f.Add(blob[:8])
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := musepack.ParseConfig(data)
		if err != nil {
			return
		}
		if err := c.Format().Valid(); err != nil {
			t.Fatalf("accepted a config whose format is invalid: %v", err)
		}
		if _, err := musepack.NewDecoder(c, c.Format()); err != nil {
			t.Fatalf("accepted a config no decoder can be built for: %v", err)
		}
		again, err := c.MarshalBinary()
		if err != nil || string(again) != string(data) {
			t.Fatalf("config does not round-trip: %v", err)
		}
	})
}
