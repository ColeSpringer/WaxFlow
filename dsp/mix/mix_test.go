package mix

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

const eps = 1e-6

func TestFiveOneToStereo(t *testing.T) {
	m, err := For(audio.DefaultLayout(6), audio.DefaultLayout(2))
	if err != nil {
		t.Fatal(err)
	}
	// Raw ITU row is FL 1, FC 1/sqrt2, BL 1/sqrt2 (LFE dropped): energy 2,
	// so normalization scales by 1/sqrt2.
	want := [][]float64{
		// in order FL FR FC LFE BL BR
		{db3, 0, 0.5, 0, 0.5, 0},
		{0, db3, 0.5, 0, 0, 0.5},
	}
	for o := range want {
		for i := range want[o] {
			if got := float64(m.coef[o][i]); math.Abs(got-want[o][i]) > eps {
				t.Errorf("coef[%d][%d] = %.6f, want %.6f", o, i, got, want[o][i])
			}
		}
	}
	if g := m.MaxGain(); math.Abs(g-(db3+0.5+0.5)) > eps {
		t.Errorf("MaxGain = %.6f, want %.6f", g, db3+0.5+0.5)
	}
	if g := m.MaxGain(); g <= 1 {
		t.Errorf("5.1 downmix MaxGain %.3f should exceed unity (limiter must engage)", g)
	}
}

func TestStereoToMono(t *testing.T) {
	m, err := For(audio.DefaultLayout(2), audio.FrontCenter)
	if err != nil {
		t.Fatal(err)
	}
	// db3*(L+R): row energy is exactly 1, no rescale.
	for i := 0; i < 2; i++ {
		if got := float64(m.coef[0][i]); math.Abs(got-db3) > eps {
			t.Errorf("coef[0][%d] = %.6f, want %.6f", i, got, db3)
		}
	}
	if g := m.MaxGain(); g <= 1 {
		t.Errorf("stereo to mono MaxGain %.3f should exceed unity for correlated content", g)
	}
}

func TestMonoToStereoUnity(t *testing.T) {
	m, err := For(audio.FrontCenter, audio.DefaultLayout(2))
	if err != nil {
		t.Fatal(err)
	}
	for o := 0; o < 2; o++ {
		if got := float64(m.coef[o][0]); got != 1 {
			t.Errorf("coef[%d][0] = %.6f, want exactly 1", o, got)
		}
	}
	if g := m.MaxGain(); g != 1 {
		t.Errorf("mono duplication MaxGain = %.6f, want 1 (no limiter)", g)
	}
}

// TestRowEnergyBound asserts the normalization invariant on every
// supported conversion: no output row has power gain above 1.
func TestRowEnergyBound(t *testing.T) {
	targets := []audio.ChannelMask{audio.FrontCenter, audio.DefaultLayout(2)}
	for ch := 1; ch <= audio.MaxChannels; ch++ {
		src := audio.DefaultLayout(ch)
		for _, dst := range targets {
			if src == dst {
				continue
			}
			m, err := For(src, dst)
			if err != nil {
				t.Fatalf("For(%v, %v): %v", src, dst, err)
			}
			for o, row := range m.coef {
				var energy float64
				for _, g := range row {
					energy += float64(g) * float64(g)
				}
				if energy > 1+eps {
					t.Errorf("%v -> %v row %d energy %.6f > 1", src, dst, o, energy)
				}
			}
		}
	}
}

func TestApply(t *testing.T) {
	m, err := For(audio.DefaultLayout(6), audio.DefaultLayout(2))
	if err != nil {
		t.Fatal(err)
	}
	const n = 64
	src := make([][]float32, 6)
	for c := range src {
		src[c] = make([]float32, n)
		for i := range src[c] {
			src[c][i] = float32(c+1) * 0.01
		}
	}
	dst := [][]float32{make([]float32, n), make([]float32, n)}
	m.Apply(dst, src, n)
	for o := range dst {
		var want float64
		for i := range src {
			want += float64(m.coef[o][i]) * float64(i+1) * 0.01
		}
		for j := range dst[o] {
			if math.Abs(float64(dst[o][j])-want) > 1e-6 {
				t.Fatalf("out[%d][%d] = %.7f, want %.7f", o, j, dst[o][j], want)
			}
		}
	}
}

func TestForErrors(t *testing.T) {
	stereo := audio.DefaultLayout(2)
	cases := []struct {
		name     string
		src, dst audio.ChannelMask
	}{
		{"zero src", 0, stereo},
		{"zero dst", stereo, 0},
		{"equal", stereo, stereo},
		{"multichannel target", stereo, audio.DefaultLayout(6)},
	}
	for _, c := range cases {
		if _, err := For(c.src, c.dst); err == nil {
			t.Errorf("%s: want error", c.name)
		}
	}
}
