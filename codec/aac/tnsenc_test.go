package aac

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// TestTNSEngagesAndRoundTrips drives a signal with a strong temporal
// envelope (pitched clicks decaying inside long blocks, TNS's home
// case), asserts the filter actually engages, and checks the decode.
func TestTNSEngagesAndRoundTrips(t *testing.T) {
	const rate, n = 44100, 32768
	f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1),
		Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := make([]float32, n)
	// A dense pulse train with slow decay: broadband spectrum, sharp
	// temporal envelope, but no per-block attack jump once running (the
	// window switcher stays long, which is where TNS operates).
	for i := range src {
		phase := i % 441 // 100 Hz pulse train
		src[i] = float32(0.6 * math.Exp(-float64(phase)/40))
	}

	var pkts [][]byte
	tnsFrames := 0
	emit := func(p codec.Packet) error {
		pkts = append(pkts, append([]byte(nil), p.Data...))
		if enc.tns[0].present {
			tnsFrames++
		}
		return nil
	}
	for off := 0; off < n; off += 1024 {
		buf := audio.Get(f, 1024)
		buf.N = 1024
		copy(buf.ChanF(0), src[off:off+1024])
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
		audio.Put(buf)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if tnsFrames == 0 {
		t.Fatal("TNS never engaged on a strong temporal envelope")
	}
	t.Logf("TNS engaged on %d/%d frames", tnsFrames, len(pkts))

	out := decodeAll(t, enc.CodecConfig(), pkts)
	got := out[0][EncoderDelay : EncoderDelay+int(tr.Samples)]
	snr := snrDB(src, got)
	t.Logf("TNS round-trip SNR %.1f dB", snr)
	if snr < 15 {
		t.Fatalf("SNR %.1f dB below 15", snr)
	}
}
