package mp3

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// encodeSignal runs samples (planar per channel) through the encoder and
// returns every emitted frame packet.
func encodeSignal(t *testing.T, e *Encoder, chans [][]float32, chunk int) [][]byte {
	t.Helper()
	var pkts [][]byte
	emit := func(p codec.Packet) error {
		b := make([]byte, len(p.Data))
		copy(b, p.Data)
		pkts = append(pkts, b)
		return nil
	}
	n := len(chans[0])
	for off := 0; off < n; off += chunk {
		end := min(off+chunk, n)
		buf := audio.Get(e.fmt, end-off)
		buf.N = end - off
		for ch := range chans {
			copy(buf.ChanF(ch)[:buf.N], chans[ch][off:end])
		}
		if err := e.Encode(buf, emit); err != nil {
			t.Fatalf("Encode: %v", err)
		}
		audio.Put(buf)
	}
	if _, err := e.Finish(emit); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return pkts
}

// decodeFrames decodes every packet with our decoder into planar PCM.
func decodeFrames(t *testing.T, f audio.Format, pkts [][]byte) [][]float32 {
	t.Helper()
	d, err := NewDecoder(f)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	out := make([][]float32, f.Channels)
	emit := func(b *audio.Buffer) error {
		for ch := 0; ch < f.Channels; ch++ {
			out[ch] = append(out[ch], b.ChanF(ch)[:b.N]...)
		}
		return nil
	}
	for _, p := range pkts {
		if err := d.Decode(p, emit); err != nil {
			t.Fatalf("Decode: %v", err)
		}
	}
	return out
}

// bestLagSNR aligns out to in over a lag window and returns the lag and the
// signal-to-noise ratio in dB at the best alignment.
func bestLagSNR(in, out []float32, maxLag int) (lag int, snrDB float64) {
	bestLag, bestErr := 0, math.Inf(1)
	var sig float64
	for _, v := range in {
		sig += float64(v) * float64(v)
	}
	for l := 0; l < maxLag; l++ {
		var e float64
		cnt := 0
		for i := 2000; i+l < len(out) && i < len(in); i++ {
			d := float64(out[i+l]) - float64(in[i])
			e += d * d
			cnt++
		}
		if cnt > 0 && e/float64(cnt) < bestErr {
			bestErr = e / float64(cnt)
			bestLag = l
		}
	}
	// SNR over the aligned region.
	var s, n float64
	for i := 2000; i+bestLag < len(out) && i < len(in); i++ {
		s += float64(in[i]) * float64(in[i])
		d := float64(out[i+bestLag]) - float64(in[i])
		n += d * d
	}
	return bestLag, 10 * math.Log10(s/n)
}

// TestBitrateResolution checks that the encoder rejects malformed bit rates
// and clamps out-of-range ones to a layer-legal value (never free format).
func TestBitrateResolution(t *testing.T) {
	f24 := audio.Format{Rate: 24000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	f44 := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}

	// q=high (192) on the MPEG-2 layer clamps to its 160 maximum, not an error.
	if e, err := NewEncoder(f24, &EncoderOptions{Bitrate: 192000}); err != nil {
		t.Errorf("192k on 24k should clamp, not error: %v", err)
	} else if e.Bitrate() != 160000 {
		t.Errorf("192k on 24k = %d, want 160000", e.Bitrate())
	}
	// 192 is legal on MPEG-1.
	if e, err := NewEncoder(f44, &EncoderOptions{Bitrate: 192000}); err != nil || e.Bitrate() != 192000 {
		t.Errorf("192k on 44.1k = %v/%d, want 192000", err, bitrateOf(e))
	}
	// Below the layer minimum clamps up to it.
	if e, err := NewEncoder(f24, &EncoderOptions{Bitrate: 4000}); err != nil || e.Bitrate() != 8000 {
		t.Errorf("4k on 24k = %v/%d, want 8000", err, bitrateOf(e))
	}
	// Non-round is rejected.
	if _, err := NewEncoder(f44, &EncoderOptions{Bitrate: 128500}); err == nil {
		t.Error("128500 (non-round) should be rejected")
	}
	// A round non-standard rate clamps to the nearest legal one below it.
	if e, err := NewEncoder(f44, &EncoderOptions{Bitrate: 100000}); err != nil || e.Bitrate() != 96000 {
		t.Errorf("100k on 44.1k = %v/%d, want 96000", err, bitrateOf(e))
	}
}

func bitrateOf(e *Encoder) int {
	if e == nil {
		return 0
	}
	return e.Bitrate()
}

// TestCBRPaddingRate checks the CBR padding accumulator: frame sizes must
// track the exact fractional byte size, so a stream pads a fraction of its
// frames (never all of them) and the average bit rate lands on the target.
// A running accumulator that adds a whole frame's worth of bits instead of
// the remainder pads every frame and overflows a 32-bit int on a long stream.
func TestCBRPaddingRate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rate, bitrate int
		wantPadFracLo float64
		wantPadFracHi float64
	}{
		// 128k/44.1k: exact size 417.96 bytes -> ~0.96 of frames padded.
		{"44k-128", 44100, 128000, 0.90, 1.00},
		// 128k/48k: exact size 384.0 bytes -> essentially no padding.
		{"48k-128", 48000, 128000, 0.00, 0.05},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: tc.rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
			const n = 90000 // ~2 s, dozens of frames past a 32-bit overflow point
			chans := [][]float32{make([]float32, n), make([]float32, n)}
			for c := range chans {
				for i := range chans[c] {
					x := float64(i)
					chans[c][i] = float32(0.3 * math.Sin(2*math.Pi*440*x/float64(tc.rate)))
				}
			}
			e, err := NewEncoder(f, &EncoderOptions{Bitrate: tc.bitrate})
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			pkts := encodeSignal(t, e, chans, 1152)

			var padded, total, bytes int
			for _, p := range pkts {
				h, err := ParseHeader(p)
				if err != nil {
					t.Fatalf("bad frame: %v", err)
				}
				if h.Padding {
					padded++
				}
				total++
				bytes += len(p)
			}
			frac := float64(padded) / float64(total)
			t.Logf("%s: %d frames, %.3f padded, avg %.2f bytes/frame", tc.name, total, frac, float64(bytes)/float64(total))
			if frac < tc.wantPadFracLo || frac > tc.wantPadFracHi {
				t.Errorf("padded fraction %.3f outside [%.2f, %.2f]", frac, tc.wantPadFracLo, tc.wantPadFracHi)
			}
			// Average frame size must match the exact CBR size within a byte.
			exact := 1152.0 / 8 * float64(tc.bitrate) / float64(tc.rate)
			if avg := float64(bytes) / float64(total); math.Abs(avg-exact) > 1.0 {
				t.Errorf("avg frame %.2f bytes, exact CBR is %.2f", avg, exact)
			}
		})
	}
}

// TestEncodeDecodeSNR encodes tones and verifies the decoded output tracks
// the input at a reasonable signal-to-noise ratio: proof the whole chain
// (analysis, quantization, Huffman, reservoir, frame assembly) is coherent.
func TestEncodeDecodeSNR(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rate     int
		channels int
		bitrate  int
		minSNR   float64
	}{
		{"44k-mono-128", 44100, 1, 128000, 20},
		{"44k-stereo-128", 44100, 2, 128000, 18},
		{"48k-stereo-192", 48000, 2, 192000, 20},
		{"32k-stereo-128", 32000, 2, 128000, 18},
		{"24k-stereo-96-mpeg2", 24000, 2, 96000, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: tc.rate, Channels: tc.channels, Layout: audio.DefaultLayout(tc.channels), Type: audio.Float, BitDepth: 32}
			const n = 30000
			chans := make([][]float32, tc.channels)
			for ch := range chans {
				chans[ch] = make([]float32, n)
				for i := range chans[ch] {
					x := float64(i)
					chans[ch][i] = float32(0.3*math.Sin(2*math.Pi*(440+float64(ch)*110)*x/float64(tc.rate)) +
						0.2*math.Sin(2*math.Pi*2500*x/float64(tc.rate)))
				}
			}
			e, err := NewEncoder(f, &EncoderOptions{Bitrate: tc.bitrate})
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			pkts := encodeSignal(t, e, chans, 1152)
			if len(pkts) == 0 {
				t.Fatal("no frames emitted")
			}
			// Every packet must parse as a Layer III frame of the right shape.
			for i, p := range pkts {
				h, err := ParseHeader(p)
				if err != nil {
					t.Fatalf("packet %d bad header: %v", i, err)
				}
				if h.Rate != tc.rate || h.Channels != tc.channels || h.Bitrate != tc.bitrate {
					t.Fatalf("packet %d header %+v disagrees with config", i, h)
				}
				if h.Size() != len(p) {
					t.Fatalf("packet %d size %d, header says %d", i, len(p), h.Size())
				}
			}
			out := decodeFrames(t, f, pkts)
			lag, snr := bestLagSNR(chans[0], out[0], 1600)
			t.Logf("%s: %d frames, lag=%d, SNR=%.1f dB", tc.name, len(pkts), lag, snr)
			if snr < tc.minSNR {
				t.Errorf("SNR %.1f dB below floor %.1f dB", snr, tc.minSNR)
			}
		})
	}
}
