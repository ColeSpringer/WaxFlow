package waxflow_test

import (
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestMP3EncoderQuality is the nightly encoder-quality harness, standing up
// with the first lossy encoder (M7). It encodes a corpus with our MP3 encoder
// and with Shine (the baseline the gate names), scores both against the source
// with the ODG-proxy, and enforces the M14 quality-phase gate: our corpus
// mean beats Shine's by at least 0.3, and no track falls more than 0.1 below
// Shine (docs/quality-gates.md). A LAME (libmp3lame) column joins the report
// when the local ffmpeg carries it: informational, never blocking. It
// self-skips without ffmpeg+libshine; the nightly job sets
// WAXFLOW_REQUIRE_SHINE=1 and WAXFLOW_QUALITY_REPORT to publish the HTML
// report.
//
// The corpus is synthesized deterministically here (broadband, tonal,
// transient, and noise classes). The pinned 20-item real-audio corpus in
// internal/testutil/vectors.go supersedes it once the vectors are chosen; the
// harness, metric, and gate are what land now.
func TestMP3EncoderQuality(t *testing.T) {
	testutil.EncoderQualityGate(t) // not part of the default loop; `make encoder-quality`
	testutil.Shine(t)              // skip early if libshine is unavailable
	haveLame := testutil.HaveLAME(t)

	const rate, kbps = 44100, 128
	corpus := qualityCorpus(rate, 3*rate)

	type row struct {
		name               string
		ours, shine, delta float64
		lame               float64 // NaN when unavailable
	}
	var rows []row
	var sumOurs, sumShine float64
	worst := math.Inf(1)

	for _, item := range corpus {
		f := audio.Format{Rate: rate, Channels: item.ch, Layout: audio.DefaultLayout(item.ch), Type: audio.Float, BitDepth: 32}
		wav := synthWAVFromSamples(t, f, item.samples)
		wavPath := filepath.Join(t.TempDir(), item.name+".wav")
		if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
			t.Fatal(err)
		}
		ref := item.samples

		// Our encoder through the engine, decoded by ffmpeg.
		ours := transcodeMP3(t, wav, waxflow.TranscodeOptions{MP3Bitrate: kbps * 1000})
		ourPath := filepath.Join(t.TempDir(), item.name+".ours.mp3")
		if err := os.WriteFile(ourPath, ours, 0o644); err != nil {
			t.Fatal(err)
		}
		ourDec := testutil.FFmpegDecodeF32(t, ourPath)
		ourODG := testutil.ODGProxy(ref, ourDec, rate, item.ch)

		// Shine, decoded by ffmpeg.
		shinePath := testutil.ShineEncodeFile(t, wavPath, kbps)
		shineDec := testutil.FFmpegDecodeF32(t, shinePath)
		shineODG := testutil.ODGProxy(ref, shineDec, rate, item.ch)

		lameODG := math.NaN()
		if haveLame {
			lamePath := testutil.LAMEEncodeFile(t, wavPath, kbps)
			lameDec := testutil.FFmpegDecodeF32(t, lamePath)
			lameODG = testutil.ODGProxy(ref, lameDec, rate, item.ch)
		}

		delta := ourODG - shineODG
		rows = append(rows, row{item.name, ourODG, shineODG, delta, lameODG})
		sumOurs += ourODG
		sumShine += shineODG
		worst = math.Min(worst, delta)
		t.Logf("%-20s ours=%.3f shine=%.3f delta=%+.3f lame=%.3f", item.name, ourODG, shineODG, delta, lameODG)
	}

	meanOurs := sumOurs / float64(len(rows))
	meanShine := sumShine / float64(len(rows))
	t.Logf("corpus mean: ours=%.3f shine=%.3f (delta %+.3f); worst per-track delta %+.3f",
		meanOurs, meanShine, meanOurs-meanShine, worst)

	if path := os.Getenv("WAXFLOW_QUALITY_REPORT"); path != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "<h1>MP3 encoder-quality report</h1>\n")
		fmt.Fprintf(&b, "<p>ODG-proxy at %d kbit/s CBR. Higher is better (0 imperceptible, -4 very annoying). Gate: mean &ge; Shine + 0.3, no track &gt; 0.1 below Shine. The LAME column is informational.</p>\n", kbps)
		fmt.Fprintf(&b, "<table border=1 cellpadding=4><tr><th>track</th><th>WaxFlow</th><th>Shine</th><th>delta</th><th>LAME</th></tr>\n")
		for _, r := range rows {
			lame := "&mdash;"
			if !math.IsNaN(r.lame) {
				lame = fmt.Sprintf("%.3f", r.lame)
			}
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%.3f</td><td>%.3f</td><td>%+.3f</td><td>%s</td></tr>\n",
				html.EscapeString(r.name), r.ours, r.shine, r.delta, lame)
		}
		fmt.Fprintf(&b, "<tr><th>mean</th><th>%.3f</th><th>%.3f</th><th>%+.3f</th><th></th></tr>\n", meanOurs, meanShine, meanOurs-meanShine)
		fmt.Fprintf(&b, "</table>\n")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing quality report: %v", err)
		}
		t.Logf("wrote quality report to %s", path)
	}

	if meanOurs < meanShine+0.3 {
		t.Errorf("corpus mean ODG %.3f below Shine mean %.3f + 0.3 (quality-phase gate)", meanOurs, meanShine)
	}
	if worst < -0.1 {
		t.Errorf("worst per-track ODG delta %.3f exceeds the 0.1 allowance below Shine", worst)
	}
}

// qualityItem is one corpus track: a deterministic interleaved signal.
type qualityItem struct {
	name    string
	ch      int
	samples []float32
}

// qualityCorpus synthesizes the four gate classes at the given length.
func qualityCorpus(rate, frames int) []qualityItem {
	return []qualityItem{
		{"broadband-a", 2, genBroadband(rate, frames, 2, 1)},
		{"broadband-b", 2, genBroadband(rate, frames, 2, 7)},
		{"tonal", 2, genTonal(rate, frames, 2)},
		{"transient", 2, genTransient(rate, frames, 2)},
		{"noise", 2, genNoise(frames, 2, 3)},
		{"mono-broadband", 1, genBroadband(rate, frames, 1, 5)},
	}
}

func genBroadband(rate, frames, ch int, seed uint64) []float32 {
	out := make([]float32, frames*ch)
	freqs := []float64{110, 220, 440, 880, 1500, 3000, 6000, 9000}
	s := seed
	for i := 0; i < frames; i++ {
		var v float64
		for j, fq := range freqs {
			v += 0.12 * math.Sin(2*math.Pi*fq*float64(i)/float64(rate)+float64(j))
		}
		s = s*6364136223846793005 + 1442695040888963407
		v += 0.05 * (2*float64(int64(s>>11))/float64(1<<53) - 1)
		for c := 0; c < ch; c++ {
			out[i*ch+c] = float32(v)
		}
	}
	return out
}

func genTonal(rate, frames, ch int) []float32 {
	out := make([]float32, frames*ch)
	for i := 0; i < frames; i++ {
		v := 0.4*math.Sin(2*math.Pi*523.25*float64(i)/float64(rate)) +
			0.25*math.Sin(2*math.Pi*1046.5*float64(i)/float64(rate)) +
			0.15*math.Sin(2*math.Pi*1568*float64(i)/float64(rate))
		for c := 0; c < ch; c++ {
			out[i*ch+c] = float32(v)
		}
	}
	return out
}

func genTransient(rate, frames, ch int) []float32 {
	out := make([]float32, frames*ch)
	period := rate / 8
	for i := 0; i < frames; i++ {
		phase := i % period
		env := math.Exp(-float64(phase) / float64(rate) * 60)
		v := env * math.Sin(2*math.Pi*2000*float64(phase)/float64(rate)) * 0.6
		for c := 0; c < ch; c++ {
			out[i*ch+c] = float32(v)
		}
	}
	return out
}

func genNoise(frames, ch int, seed uint64) []float32 {
	out := make([]float32, frames*ch)
	s := seed
	var lp float64
	for i := 0; i < frames; i++ {
		s = s*6364136223846793005 + 1442695040888963407
		w := 2*float64(int64(s>>11))/float64(1<<53) - 1
		lp = 0.97*lp + 0.03*w // pink-ish
		for c := 0; c < ch; c++ {
			out[i*ch+c] = float32(0.5 * lp)
		}
	}
	return out
}

// synthWAVFromSamples wraps interleaved float samples in a WAV of the given
// format (reusing the PCM/riff write path).
func synthWAVFromSamples(t *testing.T, f audio.Format, interleaved []float32) []byte {
	t.Helper()
	frames := len(interleaved) / f.Channels
	buf := audio.Get(f, frames)
	buf.N = frames
	for ch := 0; ch < f.Channels; ch++ {
		dst := buf.ChanF(ch)
		for i := 0; i < frames; i++ {
			dst[i] = interleaved[i*f.Channels+ch]
		}
	}
	out := wavFromBuffer(t, f, buf)
	audio.Put(buf)
	return out
}
