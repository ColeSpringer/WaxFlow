package gain

import (
	"math"
	"testing"
)

func TestFromDB(t *testing.T) {
	cases := []struct{ db, want float64 }{
		{0, 1},
		{-6.0205999132796239, 0.5},
		{6.0205999132796239, 2},
		{-1, 0.8912509381337456},
	}
	for _, c := range cases {
		if got := FromDB(c.db); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("FromDB(%g) = %.12f, want %.12f", c.db, got, c.want)
		}
	}
}

func TestApply(t *testing.T) {
	x := []float32{-1, -0.5, 0, 0.25, 1}
	Apply(x, 0.5)
	want := []float32{-0.5, -0.25, 0, 0.125, 0.5}
	for i := range x {
		if x[i] != want[i] {
			t.Errorf("x[%d] = %g, want %g", i, x[i], want[i])
		}
	}
}

// limitAll pushes a whole signal through l with the given chunk size and
// drains, returning the complete output.
func limitAll(t *testing.T, l *Limiter, in [][]float32, chunk int) [][]float32 {
	t.Helper()
	channels := len(in)
	out := make([][]float32, channels)
	dst := make([][]float32, channels)
	for c := range out {
		dst[c] = make([]float32, chunk)
	}
	pos := 0
	for pos < len(in[0]) {
		end := min(pos+chunk, len(in[0]))
		src := make([][]float32, channels)
		for c := range src {
			src[c] = in[c][pos:end]
		}
		for len(src[0]) > 0 {
			produced, consumed := l.Process(dst, src)
			for c := range out {
				out[c] = append(out[c], dst[c][:produced]...)
			}
			for c := range src {
				src[c] = src[c][consumed:]
			}
		}
		pos = end
	}
	for {
		produced := l.Drain(dst)
		if produced == 0 {
			break
		}
		for c := range out {
			out[c] = append(out[c], dst[c][:produced]...)
		}
	}
	return out
}

func sineAt(rate int, freq, phase float64, frames int, amp float64) []float32 {
	s := make([]float32, frames)
	for i := range s {
		s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)+phase))
	}
	return s
}

// TestLimiterTransparent: a signal that never approaches the ceiling
// passes through bit-exactly, at full length, latency compensated.
func TestLimiterTransparent(t *testing.T) {
	l, err := NewLimiter(44100, 2, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	in := [][]float32{
		sineAt(44100, 997, 0, 20000, 0.5),
		sineAt(44100, 1499, 0.4, 20000, 0.5),
	}
	out := limitAll(t, l, in, 4096)
	for c := range in {
		if len(out[c]) != len(in[c]) {
			t.Fatalf("channel %d: %d frames out, want %d", c, len(out[c]), len(in[c]))
		}
		for i := range in[c] {
			if out[c][i] != in[c][i] {
				t.Fatalf("channel %d sample %d: %g != %g (must be bit-exact when idle)",
					c, i, out[c][i], in[c][i])
			}
		}
	}
}

// TestLimiterCeiling: an over-ceiling signal comes out at or below the
// ceiling, samplewise, with the steady-state level close to it.
func TestLimiterCeiling(t *testing.T) {
	const rate = 44100
	l, err := NewLimiter(rate, 1, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	ceil := FromDB(DefaultCeilingDB)
	in := [][]float32{sineAt(rate, 200, 0, rate, 1.4)}
	out := limitAll(t, l, in, 4096)
	if len(out[0]) != len(in[0]) {
		t.Fatalf("%d frames out, want %d", len(out[0]), len(in[0]))
	}
	var peak float64
	for _, v := range out[0] {
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	if peak > ceil {
		t.Errorf("output peak %.6f above ceiling %.6f", peak, ceil)
	}
	// Steady state (skip the first release-length): the limiter should
	// use the ceiling, not crush far below it.
	var steadyPeak float64
	for _, v := range out[0][rate/2:] {
		steadyPeak = math.Max(steadyPeak, math.Abs(float64(v)))
	}
	if steadyPeak < ceil*0.90 {
		t.Errorf("steady-state peak %.6f is more than 1 dB below ceiling %.6f", steadyPeak, ceil)
	}
}

// TestLimiterTruePeak: samples that stay under the ceiling but whose
// inter-sample peaks exceed it must still be attenuated. A quarter-rate
// sine at 45 degrees phase has every sample at amp/sqrt(2) while its
// true peak is amp.
func TestLimiterTruePeak(t *testing.T) {
	const rate = 48000
	l, err := NewLimiter(rate, 1, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	in := [][]float32{sineAt(rate, rate/4, math.Pi/4, rate/2, 1.0)}
	// Sample peaks: 1/sqrt(2) = 0.707, below the 0.891 ceiling. True
	// peak: 1.0, above it. Without oversampled detection this signal
	// passes untouched.
	out := limitAll(t, l, in, 4096)
	mid := out[0][len(out[0])/4 : len(out[0])/2]
	var samplePeak float64
	for _, v := range mid {
		samplePeak = math.Max(samplePeak, math.Abs(float64(v)))
	}
	wantMax := math.Sqrt(0.5) * FromDB(DefaultCeilingDB) // fully limited
	if samplePeak > wantMax*1.05 {
		t.Errorf("sample peak %.4f: true-peak limiting not engaged (want <= %.4f)",
			samplePeak, wantMax*1.05)
	}
	if samplePeak < wantMax*0.90 {
		t.Errorf("sample peak %.4f: over-attenuated (want >= %.4f)", samplePeak, wantMax*0.90)
	}
}

// TestLimiterLatency: an isolated impulse below the ceiling lands at
// exactly its input position.
func TestLimiterLatency(t *testing.T) {
	l, err := NewLimiter(48000, 1, DefaultCeilingDB)
	if err != nil {
		t.Fatal(err)
	}
	const pos = 12345
	in := make([]float32, 30000)
	in[pos] = 0.5
	out := limitAll(t, l, [][]float32{in}, 4096)
	if len(out[0]) != len(in) {
		t.Fatalf("%d frames out, want %d", len(out[0]), len(in))
	}
	for i, v := range out[0] {
		if (v != 0) != (i == pos) {
			t.Fatalf("sample %d = %g; impulse must sit at %d only", i, v, pos)
		}
	}
	if out[0][pos] != 0.5 {
		t.Errorf("impulse value %g, want 0.5", out[0][pos])
	}
}

// TestLimiterChunkingInvariance: identical output no matter how the
// stream is chunked.
func TestLimiterChunkingInvariance(t *testing.T) {
	const rate = 44100
	in := [][]float32{sineAt(rate, 300, 0, 15000, 1.3)}
	ref := func() [][]float32 {
		l, err := NewLimiter(rate, 1, DefaultCeilingDB)
		if err != nil {
			t.Fatal(err)
		}
		return limitAll(t, l, in, len(in[0]))
	}()
	for _, chunk := range []int{1, 7, 100, 4096} {
		l, err := NewLimiter(rate, 1, DefaultCeilingDB)
		if err != nil {
			t.Fatal(err)
		}
		got := limitAll(t, l, in, chunk)
		if len(got[0]) != len(ref[0]) {
			t.Fatalf("chunk %d: %d frames, want %d", chunk, len(got[0]), len(ref[0]))
		}
		for i := range got[0] {
			if got[0][i] != ref[0][i] {
				t.Fatalf("chunk %d: sample %d differs", chunk, i)
			}
		}
	}
}

// BenchmarkLimiter reports x-realtime for a stereo 48 kHz stream with
// the limiter engaging (peak detection always runs; this measures it).
func BenchmarkLimiter(b *testing.B) {
	const rate, chunk = 48000, 4096
	l, err := NewLimiter(rate, 2, DefaultCeilingDB)
	if err != nil {
		b.Fatal(err)
	}
	in := [][]float32{
		sineAt(rate, 997, 0, chunk, 1.2),
		sineAt(rate, 1499, 0.4, chunk, 1.2),
	}
	dst := [][]float32{make([]float32, chunk), make([]float32, chunk)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src := [][]float32{in[0], in[1]}
		for len(src[0]) > 0 {
			_, consumed := l.Process(dst, src)
			src[0], src[1] = src[0][consumed:], src[1][consumed:]
		}
	}
	b.StopTimer()
	seconds := float64(b.N) * chunk / float64(rate)
	b.ReportMetric(seconds/b.Elapsed().Seconds(), "x-realtime")
}

func TestNewLimiterValidation(t *testing.T) {
	if _, err := NewLimiter(0, 2, -1); err == nil {
		t.Error("zero rate: want error")
	}
	if _, err := NewLimiter(44100, 0, -1); err == nil {
		t.Error("zero channels: want error")
	}
	if _, err := NewLimiter(44100, 2, 0.5); err == nil {
		t.Error("ceiling above full scale: want error")
	}
}
