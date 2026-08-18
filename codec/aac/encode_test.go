package aac

import (
	"bytes"
	"fmt"
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
	f, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]float32, f.Channels)
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

// TestAACEncodeAUCeiling holds the rate loop to the spec's hard limit: an
// access unit may not exceed 6144 bits per channel, whatever the reservoir,
// the bit rate, or how hard the frame is.
//
// The loop is where this can go wrong, and it is not obvious from reading it.
// rateSearch fits a candidate to the budget at one amplification state, and the
// loop then remembers the best-scoring candidate across rounds while band
// amplification keeps raising costs. Assembling a candidate from an earlier
// round against the final amplification state emits a frame nobody sized: the
// case below overshot by 475 bits, enough to break the ceiling and to make a
// conforming decoder's input buffer underrun.
//
// The signal is deliberately awkward: a three-byte pattern at a high mono bit
// rate, which is what the encode fuzzer found. Ordinary program material stays
// far enough under the budget that the stale candidate never shows.
func TestAACEncodeAUCeiling(t *testing.T) {
	pattern := []byte{0xE2, 0x00, 0x0D}
	// The block count matters and is not incidental: the overshoot needs the
	// reservoir the first three frames leave behind, which lets the fourth (the
	// Finish flush) ask for the full 93%-of-ceiling budget. A longer run spends
	// the reservoir down and never gets close.
	for _, blocks := range []int{3, 6} {
		for _, ch := range []int{1, 2} {
			bitrate := 184000
			name := fmt.Sprintf("ch%d/%dblk", ch, blocks)
			t.Run(name, func(t *testing.T) {
				fm := audio.Format{Rate: 44100, Channels: ch, Layout: audio.DefaultLayout(ch),
					Type: audio.Float, BitDepth: 32}
				e, err := NewEncoder(fm, &EncoderOptions{Bitrate: bitrate})
				if err != nil {
					t.Fatal(err)
				}
				ceiling := 6144 * ch
				worst := 0
				check := func(p codec.Packet) error {
					bits := len(p.Data) * 8
					worst = max(worst, bits)
					if bits > ceiling {
						t.Fatalf("access unit of %d bits exceeds the %d-bit ceiling", bits, ceiling)
					}
					return nil
				}
				buf := audio.Get(fm, frameLen)
				defer audio.Put(buf)
				for blk := 0; blk < blocks; blk++ {
					buf.N = frameLen
					for c := 0; c < ch; c++ {
						dst := buf.ChanF(c)
						for i := range dst {
							b := pattern[(blk*frameLen+i*ch+c)%len(pattern)]
							dst[i] = (float32(b) - 127.5) / 127.5
						}
					}
					if err := e.Encode(buf, check); err != nil {
						t.Fatalf("Encode: %v", err)
					}
				}
				if _, err := e.Finish(check); err != nil {
					t.Fatalf("Finish: %v", err)
				}
				// A run that never approached the ceiling would pass whatever the
				// loop did, so say how close it got.
				t.Logf("worst access unit %d of %d bits", worst, ceiling)
			})
		}
	}
}

// TestEncodeSilentChannelDPCM pins the assemble-stage DPCM repair: a
// digitally silent channel beside a very quiet one at a generous
// per-channel budget spreads coded scalefactors past the +-60 codeword
// range, the clamp moves a band to a scalefactor where it re-quantizes
// to zero, and the pre-fix writer then anchored the next delta on a band
// it never wrote (panic: index out of range in writeSFDelta). Every AU
// must also still decode.
func TestEncodeSilentChannelDPCM(t *testing.T) {
	for _, bitrate := range []int{64000, 320000, 1000000} {
		fm := audio.Format{Rate: 24000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
		e, err := NewEncoder(fm, &EncoderOptions{Bitrate: bitrate})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseASC(e.CodecConfig())
		if err != nil {
			t.Fatal(err)
		}
		df, err := cfg.Format()
		if err != nil {
			t.Fatal(err)
		}
		d, err := NewDecoder(cfg, df)
		if err != nil {
			t.Fatal(err)
		}
		check := func(p codec.Packet) error {
			return d.Decode(p.Data, func(*audio.Buffer) error { return nil })
		}
		buf := audio.Get(fm, 1024)
		for blk := 0; blk < 8; blk++ {
			buf.N = 1024
			clear(buf.ChanF(0)[:1024])
			for i := range buf.ChanF(1)[:1024] {
				buf.ChanF(1)[i] = float32(0.01 * math.Sin(2*math.Pi*3000*float64(blk*1024+i)/24000))
			}
			if err := e.Encode(buf, check); err != nil {
				t.Fatalf("bitrate %d: Encode: %v", bitrate, err)
			}
		}
		if _, err := e.Finish(check); err != nil {
			t.Fatalf("bitrate %d: Finish: %v", bitrate, err)
		}
		audio.Put(buf)
		d.Release()
	}
}
