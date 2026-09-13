package g711_test

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/g711"
)

// BenchmarkDecode records the realtime floor docs/quality-gates.md carries.
// One cell per law at the rate the codecs exist for: G.711 is telephony, so
// 8 kHz mono is the shape every real stream has, and the two laws differ only
// in which 256-entry table the loop reads.
func BenchmarkDecode(b *testing.B) {
	for _, law := range []g711.Law{g711.ALaw, g711.MuLaw} {
		b.Run(law.String(), func(b *testing.B) {
			const frames = 8000 // one second
			f := g711.Format(8000, 1, audio.DefaultLayout(1))
			pkt := make([]byte, frames)
			for i := range pkt {
				pkt[i] = byte(i * 7)
			}
			dec, err := g711.NewDecoder(law, f)
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Release()
			emit := func(*audio.Buffer) error { return nil }
			b.SetBytes(int64(len(pkt)))
			b.ResetTimer()
			for b.Loop() {
				if err := dec.Decode(pkt, emit); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(frames)*float64(b.N)/b.Elapsed().Seconds()/8000, "x-realtime")
		})
	}
}
