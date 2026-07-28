package waxflow_test

// Coupled-stereo gate. Vorbis is the only output row that codes a stereo pair
// jointly (square-polar coupling), so it is the only one where a defect can land
// on one channel and leave the other exact. Every other Vorbis corpus in the
// suite is near-dual-mono, which leaves coupling essentially unexercised: the
// encoder-quality corpus detunes the two channels by 0.1% and the interop corpus
// shares a component, so both keep the angle residue small. This gate runs
// decorrelated, anti-phase and swept content through the encoder and reads it
// back through every Vorbis decoder available, because the WaxTap v3.0 report's
// F1 was a stream our own decoder and libvorbis reconstructed correctly while
// ffmpeg's native decoder destroyed the right channel: a gate that decodes only
// through our own decoder would have stayed green through all of it.
//
// The legs are scored two ways. Each is scored against the source, which catches
// a channel that stopped tracking its input; and the decoders are scored against
// each other, which is what actually pins F1, since the defect was a
// disagreement between correct decoders rather than a stream nobody could read.
// The ffmpeg legs run both with and without SIMD dispatch for the same reason:
// the defect lived only in the vectorized inverse coupling, so a leg that only
// ever reached the C fallback would go green on a stream the field mangles.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// couplingRate and couplingSeconds size the gate's signals: long enough for the
// correlation search to be meaningful, short enough to keep the case table times
// four decoder legs inside the default suite's budget.
const (
	couplingRate    = 48000
	couplingSeconds = 2
)

// tone sums sine partials at amplitude amp each.
func tone(amp float64, freqs ...float64) func(t float64) float64 {
	return func(t float64) float64 {
		var v float64
		for _, f := range freqs {
			v += amp * math.Sin(2*math.Pi*f*t)
		}
		return v
	}
}

// logSweep sweeps from f0 to f1 exponentially over the gate's duration. Opposite
// sweep directions in the two channels is the report's worst case: the channels
// decorrelate completely while both stay tonal, so the angle residue is large
// everywhere.
func logSweep(amp, f0, f1 float64) func(t float64) float64 {
	if f0 == f1 {
		panic("logSweep: f0 == f1 has no exponential sweep (the rate is log(f1/f0)); use tone")
	}
	dur := float64(couplingSeconds)
	k := math.Log(f1 / f0)
	return func(t float64) float64 {
		// Phase is the integral of the instantaneous frequency f0*(f1/f0)^(t/dur).
		phase := 2 * math.Pi * f0 * dur / k * (math.Exp(k*t/dur) - 1)
		return amp * math.Sin(phase)
	}
}

// TestVorbisCoupledStereo is the F1 regression gate. It encodes stereo material
// whose channels differ (the report's trigger matrix, plus the anti-phase rows
// the sign-preserving nudge exists for) and asserts, per channel and through
// every available decoder, that the reconstruction still tracks its own source
// channel, does not overshoot its peak, and agrees with what the other decoders
// read from the same bytes.
func TestVorbisCoupledStereo(t *testing.T) {
	cases := []struct {
		name    string
		quality float64
		l, r    func(float64) float64
	}{
		// The report's trigger matrix, at the quality it was reported against.
		{"both-440", 6, tone(0.4, 440), tone(0.4, 440)},
		{"both-440+3k", 6, tone(0.3, 440, 3000), tone(0.3, 440, 3000)},
		{"L-440+3k/R-1k", 6, tone(0.3, 440, 3000), tone(0.4, 1000)},
		{"L-440/R-1k", 6, tone(0.4, 440), tone(0.4, 1000)},
		{"L-1k/R-440+3k", 6, tone(0.4, 1000), tone(0.3, 440, 3000)},
		{"opposite-sweeps", 6, logSweep(0.4, 200, 8000), logSweep(0.4, 8000, 200)},
		// Anti-phase is the case the sign-preserving nudge exists for.
		{"anti-phase", 6, tone(0.4, 440), tone(-0.4, 440)},
		{"near-anti-phase", 6, tone(0.4, 440), tone(-0.4, 441)},
		// Low quality is where the nudge actually has to work. qualityToOffsetDB
		// shifts demand by (q-3)*2 dB, so q6 lifts audible partitions onto the
		// med/fine/super classes, all of which carry refinement passes that can
		// walk a pass-0 zero back. The noise and coarse classes have a single pass
		// and no such recovery, and they are what a low-quality encode allocates,
		// so these rows are the ones a regression in the nudge would show up in.
		{"q1-anti-phase", 1, tone(0.4, 440), tone(-0.4, 440)},
		{"q1-decorrelated", 1, tone(0.4, 1000), tone(0.3, 440, 3000)},
		{"q1-opposite-sweeps", 1, logSweep(0.4, 200, 8000), logSweep(0.4, 8000, 200)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const frames = couplingRate * couplingSeconds
			f := audio.Format{Rate: couplingRate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
			src := make([]float32, frames*2)
			for i := 0; i < frames; i++ {
				tt := float64(i) / couplingRate
				src[i*2] = float32(c.l(tt))
				src[i*2+1] = float32(c.r(tt))
			}
			wav := synthWAVFromSamples(t, f, src)

			e := waxflow.New()
			path := filepath.Join(t.TempDir(), "out.ogg")
			out, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out,
				waxflow.TranscodeOptions{Format: "vorbis", VorbisQuality: c.quality}); err != nil {
				out.Close()
				t.Fatalf("transcode: %v", err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			type leg struct {
				name string
				pcm  []float32
			}
			// Our own decoder always runs, so the gate has teeth with no ffmpeg.
			decoded := readAll(t, e, raw, frames)
			defer audio.Put(decoded)
			legs := []leg{{"waxflow", testutil.InterleaveF(decoded)}}

			if testutil.HaveFFmpeg(t) {
				if testutil.HaveLibVorbisDecoder(t) {
					legs = append(legs, leg{"libvorbis", testutil.FFmpegDecodeF32Codec(t, path, "libvorbis")})
				} else {
					t.Log("ffmpeg has no libvorbis decoder: that leg skipped")
				}
				// ffmpeg's native decoder is the one the field actually runs: it is
				// what Chromium-family browsers, mpv, VLC and every other
				// libavcodec-based player decode Vorbis with, and two of the four
				// client families in docs/client-matrix.md are on it. It carries no
				// authority on spec conformance, but it decides what a listener on
				// those clients actually hears.
				legs = append(legs,
					leg{"ffmpeg-native", testutil.FFmpegDecodeF32(t, path)},
					leg{"ffmpeg-native-nosimd", testutil.FFmpegDecodeF32NoSIMD(t, path)})
			} else {
				t.Log("ffmpeg absent: the libvorbis and ffmpeg-native legs are skipped")
			}

			for _, l := range legs {
				checkCoupledChannels(t, l.name, l.pcm, src)
			}
			// Cross-decoder agreement. F1 was not a stream that failed to decode; it
			// was correct decoders reading different audio out of the same bytes, so
			// the disagreement is the thing to assert. In particular the two ffmpeg
			// legs differ only in SIMD dispatch, so their agreement is what proves
			// the vectorized inverse coupling was exercised and is happy.
			for i := 1; i < len(legs); i++ {
				checkDecodersAgree(t, legs[0], legs[i])
			}
		})
	}
}

// checkCoupledChannels scores each channel of an interleaved stereo decode
// against its own source channel: best-lag correlation (which absorbs the
// codec's priming lead without hiding a channel that no longer tracks its
// source) and peak overshoot. A coupling defect shows up as one channel's
// correlation collapsing while the other stays near 1, so the two are asserted
// separately rather than as a combined error.
func checkCoupledChannels(t *testing.T, decoder string, dec, src []float32) {
	t.Helper()
	if len(dec) == 0 {
		t.Errorf("%s: decoded no samples", decoder)
		return
	}
	for ch := 0; ch < 2; ch++ {
		d := deinterleave(dec, 2, ch)
		s := deinterleave(src, 2, ch)
		corr := bestLagCorrelation(d, s)
		// 0.90 sits well below what a healthy encode reaches (>0.99 on every row
		// here) and well above the collapse the report measured (0.295 on the
		// sweep row), so it fails on a broken channel without tracking encoder
		// tuning.
		if corr < 0.90 {
			t.Errorf("%s ch%d: best-lag correlation %.5f against its source channel, want >= 0.90", decoder, ch, corr)
		}
		// A lossy encode overshoots its source peak a little; the report's
		// signature was the right channel clipping to full scale from a source
		// that peaked at 0.4 (+8 dB). 1.5x (+3.5 dB) separates the two.
		if dp, sp := peakOf(d), peakOf(s); sp > 0 && dp > 1.5*sp {
			t.Errorf("%s ch%d: peak %.3f overshoots the source peak %.3f by more than 1.5x", decoder, ch, dp, sp)
		}
	}
}

// checkDecodersAgree asserts two decoders read the same audio out of one file.
// The bar is much higher than the source comparison above, since both sides are
// the same samples rather than a lossy encode of them. It stays a correlation
// rather than a sample compare because the legs trim their priming
// independently, and a vectorized kernel is entitled to differ from its C twin
// in the last ulp.
func checkDecodersAgree(t *testing.T, a, b struct {
	name string
	pcm  []float32
},
) {
	t.Helper()
	if len(a.pcm) == 0 || len(b.pcm) == 0 {
		return // already reported by checkCoupledChannels
	}
	for ch := 0; ch < 2; ch++ {
		if corr := bestLagCorrelation(deinterleave(b.pcm, 2, ch), deinterleave(a.pcm, 2, ch)); corr < 0.999 {
			t.Errorf("%s and %s ch%d disagree on the same file: correlation %.5f, want >= 0.999",
				a.name, b.name, ch, corr)
		}
	}
}

func deinterleave(x []float32, channels, ch int) []float32 {
	out := make([]float32, len(x)/channels)
	for i := range out {
		out[i] = x[i*channels+ch]
	}
	return out
}

func peakOf(x []float32) float64 {
	var p float64
	for _, v := range x {
		if a := math.Abs(float64(v)); a > p {
			p = a
		}
	}
	return p
}

// Bounds on the correlation search. The Vorbis output is gapless-trimmed on
// every leg (the engine trims from the trailer, ffmpeg from the granulepos), so
// the measured alignment is lag 0 on aperiodic material; corrMaxLag is the long
// block size, twice the largest priming lead a trim edge could leave, and exists
// only so a residual edge cannot be read as a broken channel. The search runs
// both directions because a leg that over-trims sits ahead of its reference, not
// behind it, and on aperiodic material (the sweep rows) a one-sided search would
// score that as a destroyed channel. corrWindow caps the scored span:
// correlation over a third of a second already resolves these signals far past
// the 0.90 threshold, and the product of the two bounds is what keeps this
// gate's arithmetic small next to its encodes. Scoring the full two seconds
// across 4096 one-sided lags cost 14 s of a 17 s run for no added sensitivity.
const (
	corrMaxLag = 2048
	corrWindow = 1 << 14
)

// bestLagCorrelation returns the highest normalized cross-correlation between
// got and want over lags in [-corrMaxLag, corrMaxLag], which absorbs the codec's
// priming lead in either direction. At most corrWindow samples are scored.
func bestLagCorrelation(got, want []float32) float64 {
	best := -1.0
	for lag := -corrMaxLag; lag <= corrMaxLag; lag++ {
		// A negative lag means got runs ahead of want, so the overlap starts at
		// want[-lag] against got[0].
		gi, wi := lag, 0
		if lag < 0 {
			gi, wi = 0, -lag
		}
		n := min(len(got)-gi, len(want)-wi, corrWindow)
		if n <= 0 {
			continue
		}
		var xy, xx, yy float64
		for i := 0; i < n; i++ {
			a, b := float64(got[gi+i]), float64(want[wi+i])
			xy += a * b
			xx += a * a
			yy += b * b
		}
		if xx == 0 || yy == 0 {
			continue
		}
		if c := xy / math.Sqrt(xx*yy); c > best {
			best = c
		}
	}
	return best
}
