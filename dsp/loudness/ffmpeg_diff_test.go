package loudness

import (
	"bytes"
	"math"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// TestFFmpegDifferential measures synthesized signals with both this
// meter and ffmpeg's ebur128 filter and compares the summaries. ffmpeg
// prints one decimal, so its values carry up to 0.05 quantization; the
// tolerances absorb that plus genuine implementation differences (LRA
// percentile convention, the true-peak interpolator design).
//
// The 0.5 LU on range is an empirical bound over the signals below, not
// a property of either meter, and it is not the correctness gate: that
// is TestEBUTech3342Vectors, which measures the four EBU Tech 3342 cases
// against their specified results at the document's own +-1 LU. Real
// material can separate the two implementations by more than the numbers
// here -- around 0.6 LU has been reported -- for the structural reasons
// percentileIndex describes, and that is conformant on both sides. So a
// failure here means one of them moved, not that either is wrong; widen
// this only with a signal in hand that shows why.
func TestFFmpegDifferential(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)

	cases := []struct {
		name  string
		rate  int
		chans [][]float32
	}{
		{"am noise stereo 48k", 48000, [][]float32{
			amNoise(48000, 12*48000, 1),
			amNoise(48000, 12*48000, 2),
		}},
		{"sine mix mono 44k", 44100, [][]float32{
			sineMix(44100, 10*44100),
		}},
		{"noise and sine stereo 44k", 44100, [][]float32{
			amNoise(44100, 12*44100, 7),
			toneOverNoise(44100, 12*44100, 9),
		}},
		// A wide, smooth loudness distribution, and roughly twice the
		// range of the cases above. It was added expecting the opposite
		// of what it found: a smooth distribution makes the percentile
		// ranks *insensitive*, since a one-rank shift barely moves a
		// value on a smooth CDF, and this case agrees more closely than
		// any other here. It stays as the wide-range regression guard,
		// and as the evidence that the delta does not simply grow with
		// the range.
		{"wide sweep stereo 48k", 48000, [][]float32{
			sweepNoise(48000, 30*48000, 11),
			sweepNoise(48000, 30*48000, 13),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := NewMeter(c.rate, len(c.chans), 0)
			if err != nil {
				t.Fatalf("NewMeter: %v", err)
			}
			n := len(c.chans[0])
			for off := 0; off < n; off += 4096 {
				end := min(off+4096, n)
				seg := make([][]float32, len(c.chans))
				for ch := range c.chans {
					seg[ch] = c.chans[ch][off:end]
				}
				if err := m.Process(seg); err != nil {
					t.Fatalf("Process: %v", err)
				}
			}
			m.Flush()

			wav := filepath.Join(t.TempDir(), "in.wav")
			testutil.WriteFloatWAV(t, wav, c.rate, c.chans)
			ref := ffmpegEbur128(t, ffmpeg, wav)

			i, lra, tp := m.Integrated(), m.Range(), m.TruePeak()
			t.Logf("integrated %.2f LUFS (ffmpeg %.1f, delta %+.3f)", i, ref.i, i-ref.i)
			t.Logf("range      %.2f LU   (ffmpeg %.1f, delta %+.3f)", lra, ref.lra, lra-ref.lra)
			t.Logf("true peak  %.2f dBTP (ffmpeg %.1f, delta %+.3f)", tp, ref.peak, tp-ref.peak)
			if math.Abs(i-ref.i) > 0.15 {
				t.Errorf("integrated %.2f LUFS, ffmpeg %.1f, delta %+.3f exceeds 0.15", i, ref.i, i-ref.i)
			}
			if math.Abs(lra-ref.lra) > 0.5 {
				t.Errorf("range %.2f LU, ffmpeg %.1f, delta %+.3f exceeds 0.5", lra, ref.lra, lra-ref.lra)
			}
			if math.Abs(tp-ref.peak) > 0.3 {
				t.Errorf("true peak %.2f dBTP, ffmpeg %.1f, delta %+.3f exceeds 0.3", tp, ref.peak, tp-ref.peak)
			}
		})
	}
}

// amNoise is low-passed white noise under a slow amplitude sweep, a
// deterministic music-like signal with a meaningful loudness range. The
// one-pole smoothing tames content near Nyquist, where true-peak
// interpolator designs legitimately differ the most.
func amNoise(rate, frames int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float32, frames)
	var lp float64
	for i := range out {
		w := rng.Float64()*2 - 1
		lp += 0.25 * (w - lp)
		tsec := float64(i) / float64(rate)
		amp := 0.05 + 0.28*(0.5+0.5*math.Sin(2*math.Pi*0.13*tsec))
		out[i] = float32(3 * lp * amp)
	}
	return out
}

// sineMix is a three-tone mix under a slow amplitude sweep.
func sineMix(rate, frames int) []float32 {
	out := make([]float32, frames)
	for i := range out {
		tsec := float64(i) / float64(rate)
		env := 0.1 + 0.4*(0.5+0.5*math.Sin(2*math.Pi*0.11*tsec))
		v := 0.5*math.Sin(2*math.Pi*997*tsec) +
			0.3*math.Sin(2*math.Pi*3001*tsec) +
			0.2*math.Sin(2*math.Pi*211*tsec)
		out[i] = float32(env * v)
	}
	return out
}

// sweepNoise is low-passed noise whose level walks continuously across
// roughly 35 LU, with a slow drift and a faster ripple so no level is
// held long enough to build a plateau.
func sweepNoise(rate, frames int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float32, frames)
	var lp float64
	for i := range out {
		w := rng.Float64()*2 - 1
		lp += 0.25 * (w - lp)
		tsec := float64(i) / float64(rate)
		// A drift over the whole take plus a ripple, in dB, so the level
		// moves through every value rather than settling on a few.
		db := -34 + 16*math.Sin(2*math.Pi*0.008*tsec) +
			5*math.Sin(2*math.Pi*0.037*tsec+2)
		out[i] = float32(3 * lp * math.Pow(10, db/20))
	}
	return out
}

// toneOverNoise is a swept-level tone over a quiet noise floor.
func toneOverNoise(rate, frames int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float32, frames)
	var lp float64
	for i := range out {
		w := rng.Float64()*2 - 1
		lp += 0.25 * (w - lp)
		tsec := float64(i) / float64(rate)
		env := 0.08 + 0.3*(0.5+0.5*math.Sin(2*math.Pi*0.09*tsec+1))
		out[i] = float32(env*math.Sin(2*math.Pi*1202*tsec) + 0.15*lp)
	}
	return out
}

// ebur128Summary is the ffmpeg summary subset the differential compares.
type ebur128Summary struct {
	i, lra, peak float64
}

// ffmpegEbur128 runs ffmpeg's ebur128 filter over a file and parses the
// Summary block from stderr ("I:", "LRA:", and true-peak "Peak:" lines).
func ffmpegEbur128(t *testing.T, ffmpeg, path string) ebur128Summary {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-nostdin", "-nostats", "-hide_banner",
		"-i", path, "-filter_complex", "ebur128=peak=true", "-f", "null", "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg ebur128: %v\n%s", err, out.String())
	}
	var s ebur128Summary
	var haveI, haveLRA, havePeak bool
	inSummary := false
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Summary:") {
			inSummary = true
			continue
		}
		if !inSummary {
			continue
		}
		if v, ok := summaryValue(line, "I:"); ok {
			s.i, haveI = v, true
		}
		if v, ok := summaryValue(line, "LRA:"); ok {
			s.lra, haveLRA = v, true
		}
		if v, ok := summaryValue(line, "Peak:"); ok {
			s.peak, havePeak = v, true
		}
	}
	if !haveI || !haveLRA || !havePeak {
		t.Fatalf("ffmpeg summary missing fields (I %v, LRA %v, Peak %v):\n%s",
			haveI, haveLRA, havePeak, out.String())
	}
	return s
}

// summaryValue parses "<prefix>  <number> <unit>" summary lines.
func summaryValue(line, prefix string) (float64, bool) {
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	fields := strings.Fields(strings.TrimPrefix(line, prefix))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
