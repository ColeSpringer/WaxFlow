package mix

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
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
// supported conversion: no output row has power gain above 1. Every default
// layout is a target now that widening exists, and a pair the positions rule
// refuses is skipped rather than excluded by hand: the invariant is about
// the matrices that get built.
func TestRowEnergyBound(t *testing.T) {
	var targets []audio.ChannelMask
	for ch := 1; ch <= audio.MaxChannels; ch++ {
		targets = append(targets, audio.DefaultLayout(ch))
	}
	for ch := 1; ch <= audio.MaxChannels; ch++ {
		src := audio.DefaultLayout(ch)
		for _, dst := range targets {
			// Skipped by the positions rule alone, computed here rather than
			// read off the error: matching on ErrUnsupportedFormat would also
			// swallow "no downmix gain for position", so a hole in the
			// stereoGain table would stop failing this.
			if src == dst || (src&^dst != 0 && dst.Count() > 2) {
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
		// The positions rule: 5.1's rear is a back pair and the 6.1 default's
		// is BC plus a side pair, so the widening has nowhere to put BL/BR.
		// This normalizes channel counts, not speaker assignments.
		{"unplaceable position", audio.DefaultLayout(6), audio.DefaultLayout(7)},
	}
	for _, c := range cases {
		if _, err := For(c.src, c.dst); err == nil {
			t.Errorf("%s: want error", c.name)
		}
	}
}

// TestWidenPlacesAndZeroFills is the widening arm's whole contract: source
// positions land on their own, everything else is silent, and nothing is
// scaled. A member that joins a wider queue keeps its own levels, which is
// what makes it measure the same loudness alone and inside the envelope.
func TestWidenPlacesAndZeroFills(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst audio.ChannelMask
		// want is the target's channels in order, each naming the source
		// channel index it takes at unity, or -1 for a silent one.
		want []int
	}{
		{"stereo into 5.1", audio.DefaultLayout(2), audio.DefaultLayout(6), []int{0, 1, -1, -1, -1, -1}},
		{"5.1 into 7.1", audio.DefaultLayout(6), audio.DefaultLayout(8), []int{0, 1, 2, 3, 4, 5, -1, -1}},
		{"mono into 5.1 lands on the center", audio.FrontCenter, audio.DefaultLayout(6), []int{-1, -1, 0, -1, -1, -1}},
		{"an FL-masked mono lands there too", audio.FrontLeft, audio.DefaultLayout(6), []int{-1, -1, 0, -1, -1, -1}},
		{"mono into quad duplicates the front pair", audio.FrontCenter, audio.DefaultLayout(4), []int{0, 0, -1, -1}},
		{"3.0 into 5.1", audio.DefaultLayout(3), audio.DefaultLayout(6), []int{0, 1, 2, -1, -1, -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := For(tc.src, tc.dst)
			if err != nil {
				t.Fatalf("For(%v, %v): %v", tc.src, tc.dst, err)
			}
			if m.Out() != len(tc.want) || m.In() != tc.src.Count() {
				t.Fatalf("matrix is %dx%d, want %dx%d", m.Out(), m.In(), len(tc.want), tc.src.Count())
			}
			for o, from := range tc.want {
				for i, g := range m.coef[o] {
					want := float32(0)
					if i == from {
						want = 1
					}
					if g != want {
						t.Errorf("coef[%d][%d] = %v, want %v", o, i, g, want)
					}
				}
			}
			if got := m.MaxGain(); got != 1 {
				t.Errorf("MaxGain = %v, want 1: nothing is scaled, so no limiter engages", got)
			}
		})
	}
}

// TestWidenRefusesAnUnplaceablePosition names what it cannot do rather than
// substituting a nearby speaker, which is what swresample would do: a back
// pair folded onto a side pair is a different mix, and silently making one is
// worse than refusing.
func TestWidenRefusesAnUnplaceablePosition(t *testing.T) {
	for _, tc := range []struct{ src, dst audio.ChannelMask }{
		{audio.DefaultLayout(6), audio.DefaultLayout(7)}, // BL/BR into a side pair
		{audio.DefaultLayout(7), audio.DefaultLayout(8)}, // BC into 7.1
	} {
		_, err := For(tc.src, tc.dst)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Fatalf("For(%v, %v) = %v, want unsupported", tc.src, tc.dst, err)
		}
		missing := (tc.src &^ tc.dst).String()
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name the positions with no place (%s)", err, missing)
		}
	}
}
