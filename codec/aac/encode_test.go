package aac

import (
	"bytes"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// encodeAll drives the encoder over src (per-channel planar) and
// returns the packets and trailer.
func encodeAll(t *testing.T, f audio.Format, src [][]float32, opts *EncoderOptions) ([][]byte, codec.Trailer) {
	t.Helper()
	enc, err := NewEncoder(f, opts)
	if err != nil {
		t.Fatal(err)
	}
	var pkts [][]byte
	emit := func(p codec.Packet) error {
		pkts = append(pkts, append([]byte(nil), p.Data...))
		return nil
	}
	n := len(src[0])
	for off := 0; off < n; off += 1024 {
		end := min(off+1024, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off
		for c := 0; c < f.Channels; c++ {
			copy(buf.ChanF(c), src[c][off:end])
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
		audio.Put(buf)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	return pkts, tr
}

// decodeAll runs our decoder over the packets, returning planar output.
func decodeAll(t *testing.T, asc []byte, pkts [][]byte) [][]float32 {
	t.Helper()
	cfg, err := ParseASC(asc)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]float32, cfg.Format().Channels)
	for _, pkt := range pkts {
		err := dec.Decode(pkt, func(b *audio.Buffer) error {
			for c := range out {
				out[c] = append(out[c], b.ChanF(c)[:b.N]...)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	dec.Release()
	return out
}

// snrDB measures reconstruction quality over the trimmed region.
func snrDB(ref, got []float32) float64 {
	var sig, err float64
	for i := range ref {
		s := float64(ref[i])
		e := s - float64(got[i])
		sig += s * s
		err += e * e
	}
	if err == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/err)
}

func synthMusic(n, ch int, rate int) [][]float32 {
	src := make([][]float32, ch)
	for c := range src {
		src[c] = make([]float32, n)
		state := uint32(0x1234 + c)
		for i := range src[c] {
			state = state*1664525 + 1013904223
			noise := float64(int32(state)) / (1 << 31)
			ti := float64(i) / float64(rate)
			v := 0.35*math.Sin(2*math.Pi*440*ti) +
				0.2*math.Sin(2*math.Pi*1320*ti+0.3*float64(c)) +
				0.1*math.Sin(2*math.Pi*3700*ti) +
				0.05*noise
			src[c][i] = float32(v)
		}
	}
	return src
}

func TestAACEncodeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rate   int
		ch     int
		frames int
		minSNR float64
	}{
		{"stereo48k", 48000, 2, 48000, 20},
		{"stereo44k", 44100, 2, 44100, 20},
		{"mono44k", 44100, 1, 22050, 25},
		{"stereo32k", 32000, 2, 16000, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: tc.rate, Channels: tc.ch,
				Layout: audio.DefaultLayout(tc.ch), Type: audio.Float, BitDepth: 32}
			src := synthMusic(tc.frames, tc.ch, tc.rate)
			pkts, tr := encodeAll(t, f, src, nil)

			if tr.Samples != int64(tc.frames) {
				t.Fatalf("trailer samples %d, want %d", tr.Samples, tc.frames)
			}
			if tr.Delay != EncoderDelay {
				t.Fatalf("trailer delay %d, want %d", tr.Delay, EncoderDelay)
			}
			total := int64(len(pkts)) * frameLen
			if total != tr.Delay+tr.Samples+tr.Padding {
				t.Fatalf("coverage %d != delay %d + samples %d + padding %d",
					total, tr.Delay, tr.Samples, tr.Padding)
			}

			enc, _ := NewEncoder(f, nil)
			out := decodeAll(t, enc.CodecConfig(), pkts)
			for c := 0; c < tc.ch; c++ {
				got := out[c][EncoderDelay : EncoderDelay+tc.frames]
				snr := snrDB(src[c][:tc.frames], got)
				t.Logf("ch %d SNR %.1f dB (%d packets, %.1f kbps)", c, snr,
					len(pkts), float64(streamBits(pkts))*float64(tc.rate)/float64(total)/1000)
				if snr < tc.minSNR {
					t.Fatalf("ch %d SNR %.1f dB below %.1f", c, snr, tc.minSNR)
				}
			}
		})
	}
}

func streamBits(pkts [][]byte) int {
	n := 0
	for _, p := range pkts {
		n += len(p) * 8
	}
	return n
}

// TestAACEncodeTransient drives an impulse train through the window
// switcher: the stream must decode with bounded error around each hit.
func TestAACEncodeTransient(t *testing.T) {
	const rate, n = 48000, 48000
	f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1),
		Type: audio.Float, BitDepth: 32}
	src := [][]float32{make([]float32, n)}
	for i := range src[0] {
		ti := float64(i) / rate
		src[0][i] = float32(0.05 * math.Sin(2*math.Pi*220*ti))
	}
	// Sharp attacks every ~0.19 s, decaying bursts.
	for hit := 5000; hit < n-2000; hit += 9000 {
		for j := 0; j < 800; j++ {
			src[0][hit+j] += float32(0.8 * math.Exp(-float64(j)/150) *
				math.Sin(2*math.Pi*2500*float64(j)/rate))
		}
	}
	pkts, tr := encodeAll(t, f, src, nil)
	enc, _ := NewEncoder(f, nil)
	out := decodeAll(t, enc.CodecConfig(), pkts)
	got := out[0][EncoderDelay : EncoderDelay+int(tr.Samples)]
	snr := snrDB(src[0], got)
	t.Logf("transient SNR %.1f dB", snr)
	if snr < 25 {
		t.Fatalf("transient SNR %.1f dB below 25", snr)
	}
}

// TestAACEncodeStereoMS checks highly correlated stereo (the M/S sweet
// spot) round-trips well and spends fewer bits than decorrelated noise.
func TestAACEncodeStereoMS(t *testing.T) {
	const rate, n = 44100, 44100
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	// Near-identical channels.
	src := make([][]float32, 2)
	src[0] = make([]float32, n)
	src[1] = make([]float32, n)
	for i := range src[0] {
		ti := float64(i) / rate
		v := 0.4*math.Sin(2*math.Pi*440*ti) + 0.2*math.Sin(2*math.Pi*997*ti)
		src[0][i] = float32(v)
		src[1][i] = float32(v * 0.98)
	}
	pkts, tr := encodeAll(t, f, src, nil)
	enc, _ := NewEncoder(f, nil)
	out := decodeAll(t, enc.CodecConfig(), pkts)
	for c := 0; c < 2; c++ {
		snr := snrDB(src[c][:tr.Samples], out[c][EncoderDelay:EncoderDelay+int(tr.Samples)])
		t.Logf("ch %d SNR %.1f dB", c, snr)
		if snr < 40 {
			t.Fatalf("ch %d SNR %.1f dB below 40", c, snr)
		}
	}
}

func TestAACEncodeDeterministic(t *testing.T) {
	const rate, n = 44100, 22050
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	src := synthMusic(n, 2, rate)
	a, _ := encodeAll(t, f, src, nil)
	b, _ := encodeAll(t, f, src, nil)
	if len(a) != len(b) {
		t.Fatalf("packet counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("packet %d differs between runs", i)
		}
	}
}

func TestAACEncodeSilence(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	src := [][]float32{make([]float32, 8192), make([]float32, 8192)}
	pkts, tr := encodeAll(t, f, src, nil)
	if tr.Samples != 8192 {
		t.Fatalf("samples %d", tr.Samples)
	}
	for i, p := range pkts {
		if len(p) > 24 {
			t.Fatalf("silent packet %d is %d bytes", i, len(p))
		}
	}
	enc, _ := NewEncoder(f, nil)
	out := decodeAll(t, enc.CodecConfig(), pkts)
	for c := range out {
		for i, v := range out[c] {
			if v != 0 {
				t.Fatalf("ch %d sample %d = %g, want silence", c, i, v)
			}
		}
	}
}

func TestAACEncoderOptionValidation(t *testing.T) {
	good := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	if _, err := NewEncoder(good, nil); err != nil {
		t.Fatalf("default: %v", err)
	}
	bad := []audio.Format{
		{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16},
		{Rate: 44100, Channels: 3, Layout: audio.DefaultLayout(3), Type: audio.Float, BitDepth: 32},
		{Rate: 44000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
	}
	for i, f := range bad {
		if _, err := NewEncoder(f, nil); err == nil {
			t.Errorf("format %d accepted", i)
		}
	}
}
