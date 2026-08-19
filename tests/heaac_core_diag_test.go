package waxflow_test

// Core-vs-fdk isolation harness: encodes click material through the
// half-rate AAC-LC core at its HE operating point (~22 kb/s a channel
// at 22050 Hz) and through fdk's LC, and localizes the error in time
// (folded over the click period) and frequency. This harness measured
// the closed HE ledger's transient deficit and its fix (the deferred
// window decision); the click-phase test doubles as its ODG-level
// canary. Not a gate; runs only under WAXFLOW_HE_DIAG=1 with a
// WAXFLOW_FDKENC route.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

func heDiagGate(t *testing.T) {
	if os.Getenv("WAXFLOW_HE_DIAG") != "1" {
		t.Skip("diagnostic; set WAXFLOW_HE_DIAG=1")
	}
	testutil.FFmpeg(t)
}

// fdkEncLC drives the WAXFLOW_FDKENC tool directly at aot 2 (AAC-LC),
// which FDKEncodeADTS does not expose.
func fdkEncLC(t *testing.T, dir string, samples []float32, rate, ch, bps int) string {
	t.Helper()
	tool := os.Getenv("WAXFLOW_FDKENC")
	if tool == "" {
		t.Skip("needs WAXFLOW_FDKENC")
	}
	raw := filepath.Join(dir, "in.raw")
	var b bytes.Buffer
	for _, v := range samples {
		s := int16(math.Round(float64(v) * 32767))
		if v >= 1 {
			s = 32767
		} else if v <= -1 {
			s = -32768
		}
		binary.Write(&b, binary.LittleEndian, s)
	}
	if err := os.WriteFile(raw, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "fdk_lc.aac")
	cmd := exec.Command(tool, strconv.Itoa(rate), strconv.Itoa(ch), strconv.Itoa(bps), "2", raw, out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fdkenc: %v: %s", err, o)
	}
	return out
}

// alignTo finds the lag of t against r minimizing error over a window
// past the head, and returns t shifted onto r.
func alignTo(r, t []float32) []float32 {
	skip := 4000
	win := 12000
	if skip+win > len(r) {
		skip = 0
		win = len(r)
	}
	best, bestLag := math.Inf(1), 0
	for lag := 0; lag < 9000 && lag+skip+win <= len(t); lag++ {
		var e float64
		for i := skip; i < skip+win; i++ {
			d := float64(r[i]) - float64(t[lag+i])
			e += d * d
		}
		if e < best*0.95 {
			best, bestLag = e, lag
		}
	}
	return t[bestLag:]
}

// foldedProfile reports RMS (dB) of x folded over the click period in
// nBins phase bins.
func foldedProfile(x []float32, period, nBins int) []float64 {
	sums := make([]float64, nBins)
	counts := make([]int, nBins)
	for i, v := range x {
		b := (i % period) * nBins / period
		sums[b] += float64(v) * float64(v)
		counts[b]++
	}
	out := make([]float64, nBins)
	for i := range sums {
		if counts[i] > 0 {
			out[i] = 10 * math.Log10(sums[i]/float64(counts[i])+1e-20)
		}
	}
	return out
}

func profileString(p []float64) string {
	var sb strings.Builder
	for _, v := range p {
		fmt.Fprintf(&sb, "%6.1f", v)
	}
	return sb.String()
}

// TestHECoreClickPhase pins the window-switch position hypothesis: a
// click train with period exactly 3 blocks, so every click lands at one
// chosen in-block offset. Offset ~600 is inside the short windows'
// coverage; offset ~100 is in the hole the preceding LONG_START window
// codes at full weight. fdk's own lookahead should handle both alike.
func TestHECoreClickPhase(t *testing.T) {
	heDiagGate(t)

	const rate = 22050
	const bps = 22000
	frames := 3 * rate
	period := 3 * 1024

	gen := func(shift int) []float32 {
		out := make([]float32, frames)
		for i := shift; i < frames; i++ {
			phase := (i - shift) % period
			env := math.Exp(-float64(phase) / float64(rate) * 60)
			out[i] = float32(env * math.Sin(2*math.Pi*2000*float64(phase)/float64(rate)) * 0.6)
		}
		return out
	}

	for _, tc := range []struct {
		name  string
		shift int
	}{{"offset600-covered", 600}, {"offset100-hole", 100}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ref := gen(tc.shift)
			f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
			wav := synthWAVFromSamples(t, f, ref)

			var out bytes.Buffer
			transcodeAAC(t, wav, &out, waxflow.TranscodeOptions{AACBitrate: bps, Container: "adts"})
			ourPath := filepath.Join(dir, "ours.aac")
			if err := os.WriteFile(ourPath, out.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			ourODG := testutil.ODGProxy(ref, testutil.FFmpegDecodeF32(t, ourPath), rate, 1)
			fdkODG := testutil.ODGProxy(ref, testutil.FFmpegDecodeF32(t, fdkEncLC(t, dir, ref, rate, 1, bps)), rate, 1)
			t.Logf("ODG ours=%.3f fdk=%.3f (delta %+.3f)", ourODG, fdkODG, ourODG-fdkODG)
		})
	}
}

// TestHECoreTransientDiag encodes the transient corpus item at the HE
// core's own operating point (22050 Hz, mono) through our LC and fdk's
// LC, and prints ODG plus time- and frequency-localized error.
func TestHECoreTransientDiag(t *testing.T) {
	heDiagGate(t)

	const rate = 22050
	frames := 3 * rate
	ref := genTransient(rate, frames, 1)
	period := rate / 8

	for _, bps := range []int{16000, 22000, 32000} {
		t.Run(fmt.Sprintf("%dbps", bps), func(t *testing.T) {
			dir := t.TempDir()
			f := audio.Format{Rate: rate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
			wav := synthWAVFromSamples(t, f, ref)

			var out bytes.Buffer
			transcodeAAC(t, wav, &out, waxflow.TranscodeOptions{AACBitrate: bps, Container: "adts"})
			ourPath := filepath.Join(dir, "ours.aac")
			if err := os.WriteFile(ourPath, out.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			ourDec := testutil.FFmpegDecodeF32(t, ourPath)

			fdkPath := fdkEncLC(t, dir, ref, rate, 1, bps)
			fdkDec := testutil.FFmpegDecodeF32(t, fdkPath)

			ourODG := testutil.ODGProxy(ref, ourDec, rate, 1)
			fdkODG := testutil.ODGProxy(ref, fdkDec, rate, 1)
			t.Logf("ODG ours=%.3f fdk=%.3f (delta %+.3f)", ourODG, fdkODG, ourODG-fdkODG)

			// Time profile: error folded over the click period.
			our := alignTo(ref, ourDec)
			fdk := alignTo(ref, fdkDec)
			n := min(len(ref), min(len(our), len(fdk)))
			// Skip the first two periods (codec warm-up).
			lo := 2 * period
			errOur := make([]float32, 0, n-lo)
			errFdk := make([]float32, 0, n-lo)
			sig := make([]float32, 0, n-lo)
			for i := lo; i < n; i++ {
				errOur = append(errOur, ref[i]-our[i])
				errFdk = append(errFdk, ref[i]-fdk[i])
				sig = append(sig, ref[i])
			}
			const nBins = 24
			t.Logf("phase bins over %d-sample period (bin 0 = click attack), dB:", period)
			t.Logf("signal: %s", profileString(foldedProfile(sig, period, nBins)))
			t.Logf("ourErr: %s", profileString(foldedProfile(errOur, period, nBins)))
			t.Logf("fdkErr: %s", profileString(foldedProfile(errFdk, period, nBins)))

			// Frequency profile.
			for _, leg := range []struct {
				name string
				dec  []float32
			}{{"ours", ourDec}, {"fdk", fdkDec}} {
				stats := testutil.ODGBandNMR(ref, leg.dec, rate, 1)
				var sb strings.Builder
				for _, s := range stats {
					if s.SignalFracDB > -60 || s.NoiseToSigDB > -60 {
						fmt.Fprintf(&sb, " %4.0f-%4.0fHz sig%6.1f n/s%6.1f |", s.LoHz, s.HiHz, s.SignalFracDB, s.NoiseToSigDB)
					}
				}
				t.Logf("%s bands:%s", leg.name, sb.String())
			}
		})
	}
}
