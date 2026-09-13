package adpcm_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/adpcm"
)

// FuzzDecode asserts the hostile-input invariants on arbitrary blocks under
// arbitrary geometries: no panic, exactly the frames the geometry promises,
// every sample inside the 16-bit domain both predictors reconstruct in, and
// the emitted format the config's.
//
// The geometry is fuzzed alongside the payload because it is the interesting
// half. The nibble arithmetic is table-bounded; what a crafted file gets to
// choose is a block size, a channel count and (for MS) the coefficient table
// the predictor multiplies by, and those are what put the block walk and the
// step adaptation outside the range a real stream keeps them in.
//
// The stream is a RUN of blocks through one decoder with no Reset between
// them, since the QuickTime layout's predictor carries across blocks and a
// fuzzer that decodes one block never reaches that.
func FuzzDecode(f *testing.F) {
	add := func(cfg adpcm.Config, blocks int, fill byte) {
		blob, err := cfg.MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		data := make([]byte, blocks*cfg.BlockBytes())
		for i := range data {
			data[i] = fill + byte(i*7)
		}
		f.Add(blob, data)
	}
	add(adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 1024, SamplesPerBlock: 2041}, 2, 0x11)
	add(adpcm.Config{Layout: adpcm.IMAWav, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1017}, 2, 0x5A)
	add(adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: 2, BlockAlign: 68, SamplesPerBlock: 64}, 8, 0x33)
	add(adpcm.Config{Layout: adpcm.MS, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1012, Coefs: adpcm.DefaultCoefs}, 2, 0x88)
	add(adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 64, SamplesPerBlock: 116, Coefs: adpcm.DefaultCoefs}, 1, 0xFF)
	f.Add([]byte{1, 0, 1, 0, 4, 0, 0}, []byte{})
	f.Add([]byte{1, 2}, []byte{1, 2, 3})

	f.Fuzz(func(t *testing.T, cfgBytes, stream []byte) {
		cfg, err := adpcm.ParseConfig(cfgBytes)
		if err != nil {
			return
		}
		want := cfg.Format(8000, audio.DefaultLayout(cfg.Channels))
		dec, err := adpcm.NewDecoder(cfg, want)
		if err != nil {
			return
		}
		defer dec.Release()
		var frames int
		emit := func(b *audio.Buffer) error {
			if b.Fmt != want {
				t.Fatalf("emitted %v, want %v", b.Fmt, want)
			}
			if b.N < 0 || b.N > audio.StandardChunk {
				t.Fatalf("emitted %d frames", b.N)
			}
			for c := range cfg.Channels {
				for _, v := range b.ChanI(c)[:b.N] {
					if v < -32768 || v > 32767 {
						t.Fatalf("sample %d left the 16-bit domain", v)
					}
				}
			}
			frames += b.N
			return nil
		}
		if err := dec.Decode(stream, emit); err != nil {
			return
		}
		if blocks := len(stream) / cfg.BlockBytes(); frames != blocks*cfg.SamplesPerBlock {
			t.Fatalf("emitted %d frames from %d blocks of %d", frames, blocks, cfg.SamplesPerBlock)
		}
		if err := dec.Drain(emit); err != nil {
			t.Fatalf("drain: %v", err)
		}
	})
}
