package loudness

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// sine synthesizes amp*sin(2*pi*freq*t + phase) at the given rate.
func sine(rate, frames int, freq, amp, phase float64) []float32 {
	s := make([]float32, frames)
	w := 2 * math.Pi * freq / float64(rate)
	for i := range s {
		s[i] = float32(amp * math.Sin(w*float64(i)+phase))
	}
	return s
}

// silence returns frames of zeros.
func silence(frames int) []float32 {
	return make([]float32, frames)
}

// measure feeds whole channels through a fresh meter in chunks and
// flushes, returning the meter for its result methods.
func measure(t *testing.T, rate int, layout audio.ChannelMask, chunk int, chans ...[]float32) *Meter {
	t.Helper()
	m, err := NewMeter(rate, len(chans), layout)
	if err != nil {
		t.Fatalf("NewMeter(%d, %d, %v): %v", rate, len(chans), layout, err)
	}
	n := len(chans[0])
	for off := 0; off < n; off += chunk {
		end := min(off+chunk, n)
		seg := make([][]float32, len(chans))
		for c := range chans {
			seg[c] = chans[c][off:end]
		}
		if err := m.Process(seg); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	m.Flush()
	return m
}

func TestKWeightingCoefficients48k(t *testing.T) {
	shelf, hp := kWeighting(48000)
	cases := []struct {
		name      string
		got, want float64
	}{
		{"shelf b0", shelf.b0, 1.53512485958697},
		{"shelf b1", shelf.b1, -2.69169618940638},
		{"shelf b2", shelf.b2, 1.19839281085285},
		{"shelf a1", shelf.a1, -1.69065929318241},
		{"shelf a2", shelf.a2, 0.73248077421585},
		{"hp b0", hp.b0, 1},
		{"hp b1", hp.b1, -2},
		{"hp b2", hp.b2, 1},
		{"hp a1", hp.a1, -1.99004745483398},
		{"hp a2", hp.a2, 0.99007225036621},
	}
	for _, c := range cases {
		if math.Abs(c.got-c.want) > 1e-6 {
			t.Errorf("%s = %.14f, want %.14f", c.name, c.got, c.want)
		}
	}
}

// TestIntegratedAnchors checks the BS.1770 compliance anchors at three
// rates: a 0 dBFS 997 Hz sine in one channel of a stereo meter reads
// -3.01 LKFS, and a -18 dBFS sine in both channels reads -18.0 (the
// channel doubling and the calibration offset cancel exactly). The
// derived filters must agree across rates within 0.05 LU.
func TestIntegratedAnchors(t *testing.T) {
	amp18 := math.Pow(10, -18.0/20)
	var single, dual [3]float64
	rates := []int{44100, 48000, 96000}
	for i, rate := range rates {
		secs := 10
		if rate == 48000 {
			secs = 20 // the ITU test signal duration
		}
		n := secs * rate
		ms := measure(t, rate, 0, 4096, sine(rate, n, 997, 1, 0), silence(n))
		single[i] = ms.Integrated()
		if math.Abs(single[i]-(-3.0103)) > 0.05 {
			t.Errorf("rate %d: single-channel 0 dBFS 997 Hz integrated = %.4f LUFS, want -3.01 +-0.05", rate, single[i])
		}
		md := measure(t, rate, 0, 4096, sine(rate, n, 997, amp18, 0), sine(rate, n, 997, amp18, 0))
		dual[i] = md.Integrated()
		if math.Abs(dual[i]-(-18.0)) > 0.05 {
			t.Errorf("rate %d: dual-channel -18 dBFS 997 Hz integrated = %.4f LUFS, want -18.0 +-0.05", rate, dual[i])
		}
	}
	for i, rate := range rates[1:] {
		if d := math.Abs(single[i+1] - single[0]); d > 0.05 {
			t.Errorf("single anchor at %d vs %d differs by %.4f LU", rate, rates[0], d)
		}
		if d := math.Abs(dual[i+1] - dual[0]); d > 0.05 {
			t.Errorf("dual anchor at %d vs %d differs by %.4f LU", rate, rates[0], d)
		}
	}
}

// TestDualChannelGain checks the relative anchor: duplicating a signal
// into the second channel doubles the channel-sum power, raising the
// integrated loudness by exactly 3.01 dB.
func TestDualChannelGain(t *testing.T) {
	const rate = 48000
	n := 10 * rate
	s := sine(rate, n, 997, 1, 0)
	one := measure(t, rate, 0, 4096, s, silence(n)).Integrated()
	two := measure(t, rate, 0, 4096, s, s).Integrated()
	if d := two - one; math.Abs(d-3.0103) > 0.02 {
		t.Errorf("adding an identical channel raised integrated by %.4f dB, want 3.01", d)
	}
}

// TestGatingSilenceGap checks the absolute gate: inserting silence into
// a steady signal must not move the integrated value, because the
// silent blocks fall below -70 LUFS and are gated out.
func TestGatingSilenceGap(t *testing.T) {
	const rate = 48000
	amp := math.Pow(10, -23.0/20)
	tone := func(secs int) []float32 { return sine(rate, secs*rate, 997, amp, 0) }

	solid := measure(t, rate, 0, 4096, tone(18), tone(18)).Integrated()

	var l, r []float32
	l = append(append(append(l, tone(8)...), silence(2*rate)...), tone(10)...)
	r = append(append(append(r, tone(8)...), silence(2*rate)...), tone(10)...)
	gapped := measure(t, rate, 0, 4096, l, r).Integrated()

	if d := math.Abs(solid - gapped); d > 0.1 {
		t.Errorf("2 s silence gap moved integrated from %.4f to %.4f (%.4f LU), want < 0.1", solid, gapped, d)
	}
}

// TestChunkInvariance checks that measurement does not depend on how
// the stream is chunked, down to single-sample Process calls.
func TestChunkInvariance(t *testing.T) {
	const rate = 48000
	n := rate + rate/5
	l := sine(rate, n, 997, 0.3, 0)
	r := sine(rate, n, 211, 0.2, 0.7)
	ref := measure(t, rate, 0, n, l, r)
	for _, chunk := range []int{1, 17, 480, 4801} {
		m := measure(t, rate, 0, chunk, l, r)
		if got, want := m.Integrated(), ref.Integrated(); math.Abs(got-want) > 1e-9 {
			t.Errorf("chunk %d: integrated %.12f, want %.12f", chunk, got, want)
		}
		if got, want := m.TruePeak(), ref.TruePeak(); math.Abs(got-want) > 1e-9 {
			t.Errorf("chunk %d: true peak %.12f, want %.12f", chunk, got, want)
		}
	}
}

func TestTruePeak(t *testing.T) {
	t.Run("sine 997 at 44100", func(t *testing.T) {
		const rate, amp = 44100, 0.5
		m := measure(t, rate, 0, 4096, sine(rate, 2*rate, 997, amp, 0))
		want := 20 * math.Log10(amp)
		if got := m.TruePeak(); math.Abs(got-want) > 0.1 {
			t.Errorf("true peak = %.3f dBTP, want %.3f +-0.1", got, want)
		}
	})
	// A sine at fs/4 with phase pi/4 only ever hits +-A/sqrt(2) on the
	// sample grid while its true peak is A; the 4x grid lands exactly on
	// the peak, so this isolates the interpolator's accuracy.
	t.Run("inter-sample peak at fs over 4", func(t *testing.T) {
		const rate, amp = 48000, 0.5
		m := measure(t, rate, 0, 4096, sine(rate, rate, float64(rate)/4, amp, math.Pi/4))
		wantSP := 20 * math.Log10(amp/math.Sqrt2)
		if got := m.SamplePeak(); math.Abs(got-wantSP) > 0.05 {
			t.Errorf("sample peak = %.3f dBFS, want %.3f +-0.05", got, wantSP)
		}
		wantTP := 20 * math.Log10(amp)
		if got := m.TruePeak(); math.Abs(got-wantTP) > 0.2 {
			t.Errorf("true peak = %.3f dBTP, want %.3f +-0.2", got, wantTP)
		}
	})
}

// TestChannelWeights checks the layout-driven weighting on a 5.1 mask:
// LFE content must not move the reading at all, and content in the back
// pair must read 10 log10(1.41) = 1.49 dB hotter than the same content
// in the front pair.
func TestChannelWeights(t *testing.T) {
	const rate = 48000
	layout := audio.FrontLeft | audio.FrontRight | audio.FrontCenter |
		audio.LowFrequency | audio.BackLeft | audio.BackRight
	n := 5 * rate
	tone := sine(rate, n, 997, 0.1, 0)
	quiet := func() [][]float32 {
		chans := make([][]float32, 6)
		for c := range chans {
			chans[c] = silence(n)
		}
		return chans
	}

	t.Run("lfe excluded", func(t *testing.T) {
		a := quiet()
		a[0] = tone
		b := quiet()
		b[0] = tone
		b[3] = sine(rate, n, 60, 1, 0) // full-scale LFE rumble
		ia := measure(t, rate, layout, 4096, a...).Integrated()
		ib := measure(t, rate, layout, 4096, b...).Integrated()
		if math.Abs(ia-ib) > 1e-9 {
			t.Errorf("LFE content moved integrated from %.9f to %.9f", ia, ib)
		}
	})

	t.Run("surround weight", func(t *testing.T) {
		front := quiet()
		front[0], front[1] = tone, tone
		back := quiet()
		back[4], back[5] = tone, tone
		fi := measure(t, rate, layout, 4096, front...).Integrated()
		bi := measure(t, rate, layout, 4096, back...).Integrated()
		want := 10 * math.Log10(1.41)
		if d := bi - fi; math.Abs(d-want) > 0.02 {
			t.Errorf("back pair reads %.4f dB above front pair, want %.4f", d, want)
		}
	})
}

func TestSilence(t *testing.T) {
	m := measure(t, 48000, 0, 4096, silence(48000), silence(48000))
	if got := m.Integrated(); !math.IsInf(got, -1) {
		t.Errorf("Integrated() = %v, want -Inf", got)
	}
	if got := m.Range(); got != 0 {
		t.Errorf("Range() = %v, want 0", got)
	}
	if got := m.TruePeak(); !math.IsInf(got, -1) {
		t.Errorf("TruePeak() = %v, want -Inf", got)
	}
	if got := m.SamplePeak(); !math.IsInf(got, -1) {
		t.Errorf("SamplePeak() = %v, want -Inf", got)
	}
}

func TestErrors(t *testing.T) {
	newCases := []struct {
		name           string
		rate, channels int
	}{
		{"zero rate", 0, 2},
		{"negative rate", -48000, 2},
		{"zero channels", 48000, 0},
		{"nine channels", 48000, 9},
	}
	for _, c := range newCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewMeter(c.rate, c.channels, 0); err == nil {
				t.Errorf("NewMeter(%d, %d, 0) accepted", c.rate, c.channels)
			}
		})
	}

	m, err := NewMeter(48000, 2, 0)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	if err := m.Process([][]float32{make([]float32, 8)}); err == nil {
		t.Error("Process accepted a chunk with the wrong channel count")
	}
	if err := m.Process([][]float32{make([]float32, 8), make([]float32, 7)}); err == nil {
		t.Error("Process accepted channel slices of different lengths")
	}
	if err := m.Process([][]float32{make([]float32, 8), make([]float32, 8)}); err != nil {
		t.Errorf("Process on a valid chunk: %v", err)
	}
	m.Flush()
	m.Flush() // idempotent
	if err := m.Process([][]float32{make([]float32, 8), make([]float32, 8)}); err == nil {
		t.Error("Process accepted a chunk after Flush")
	}
}
