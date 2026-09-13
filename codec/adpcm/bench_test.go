package adpcm_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/adpcm"
)

// BenchmarkDecode records the realtime floors docs/quality-gates.md carries,
// one cell per layout at the shape each is found in: the two WAV layouts at
// the 1024-byte block every encoder writes, and QuickTime at its fixed 34
// bytes per channel, where the per-block header work is heaviest.
func BenchmarkDecode(b *testing.B) {
	cases := []struct {
		name string
		cfg  adpcm.Config
		rate int
	}{
		{"ima-wav-stereo", adpcm.Config{Layout: adpcm.IMAWav, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1017}, 44100},
		{"ima-quicktime-stereo", adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: 2, BlockAlign: 68, SamplesPerBlock: 64}, 44100},
		{"ms-stereo", adpcm.Config{Layout: adpcm.MS, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1012, Coefs: adpcm.DefaultCoefs}, 44100},
	}
	for _, tt := range cases {
		b.Run(tt.name, func(b *testing.B) {
			// One second of audio, rounded up to whole blocks.
			blocks := (tt.rate + tt.cfg.SamplesPerBlock - 1) / tt.cfg.SamplesPerBlock
			pkt := make([]byte, blocks*tt.cfg.BlockBytes())
			for i := range pkt {
				pkt[i] = byte(i * 11)
			}
			// Headers a decoder accepts: the pattern above is nibble data,
			// and a block's first bytes are not.
			for off := 0; off < len(pkt); off += tt.cfg.BlockBytes() {
				blk := pkt[off:]
				for c := range tt.cfg.Channels {
					switch tt.cfg.Layout {
					case adpcm.IMAQuickTime:
						blk[34*c+1] &= 0x3F // a step index inside the table
					case adpcm.MS:
						blk[c] = byte(c) // a coefficient pair inside the table
					default:
						blk[4*c+2] &= 0x3F
					}
				}
			}
			dec, err := adpcm.NewDecoder(tt.cfg, tt.cfg.Format(tt.rate, audio.DefaultLayout(tt.cfg.Channels)))
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Release()
			emit := func(*audio.Buffer) error { return nil }
			frames := blocks * tt.cfg.SamplesPerBlock
			b.SetBytes(int64(len(pkt)))
			b.ResetTimer()
			for b.Loop() {
				if err := dec.Decode(pkt, emit); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(frames)*float64(b.N)/b.Elapsed().Seconds()/float64(tt.rate), "x-realtime")
		})
	}
}
