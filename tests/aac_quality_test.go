package waxflow_test

import (
	"bytes"
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

// TestAACEncoderQuality is the AAC half of the encoder-quality harness. It
// encodes the shared synthesized corpus with our AAC-LC encoder and with
// ffmpeg's native aac encoder (the gate's reference: a realistic bar, not
// Apple's), scores both against the source with the ODG-proxy, and enforces
// the docs/quality-gates.md AAC-LC gate: our corpus mean is at least
// ffmpeg-aac's minus 0.2, and no track falls more than 0.5 below it. It
// self-skips without ffmpeg; the nightly job sets WAXFLOW_REQUIRE_FFMPEG=1
// and WAXFLOW_QUALITY_REPORT to publish the HTML report.
//
// Both outputs decode through ffmpeg. Ours travels as ADTS (the elementary
// stream, so the comparison is codec against codec; the metric's alignment
// search absorbs the unsignaled priming), the reference as its native M4A.
func TestAACEncoderQuality(t *testing.T) {
	testutil.EncoderQualityGate(t) // not part of the default loop; `make encoder-quality`
	testutil.FFmpeg(t)             // skip early without the oracle

	const rate, kbps = 44100, 128
	corpus := qualityCorpus(rate, 3*rate)

	type row struct {
		name             string
		ours, ref, delta float64
	}
	var rows []row
	var sumOurs, sumRef float64
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
		var out bytes.Buffer
		transcodeAAC(t, wav, &out, waxflow.TranscodeOptions{AACBitrate: kbps * 1000, Container: "adts"})
		ourPath := filepath.Join(t.TempDir(), item.name+".ours.aac")
		if err := os.WriteFile(ourPath, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		ourDec := testutil.FFmpegDecodeF32(t, ourPath)
		ourODG := testutil.ODGProxy(ref, ourDec, rate, item.ch)

		// ffmpeg's native aac encoder, decoded by ffmpeg.
		refPath := testutil.FFmpegAACEncodeFile(t, wavPath, kbps)
		refDec := testutil.FFmpegDecodeF32(t, refPath)
		refODG := testutil.ODGProxy(ref, refDec, rate, item.ch)

		delta := ourODG - refODG
		rows = append(rows, row{item.name, ourODG, refODG, delta})
		sumOurs += ourODG
		sumRef += refODG
		worst = math.Min(worst, delta)
		t.Logf("%-20s ours=%.3f ffmpeg-aac=%.3f delta=%+.3f", item.name, ourODG, refODG, delta)
	}

	meanOurs := sumOurs / float64(len(rows))
	meanRef := sumRef / float64(len(rows))
	t.Logf("corpus mean: ours=%.3f ffmpeg-aac=%.3f (delta %+.3f); worst per-track delta %+.3f",
		meanOurs, meanRef, meanOurs-meanRef, worst)

	if path := os.Getenv("WAXFLOW_QUALITY_REPORT"); path != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "<h1>AAC-LC encoder-quality report</h1>\n")
		fmt.Fprintf(&b, "<p>ODG-proxy at %d kbit/s. Higher is better (0 imperceptible, -4 very annoying). Gate: mean &ge; ffmpeg-aac &minus; 0.2, no track &gt; 0.5 below.</p>\n", kbps)
		fmt.Fprintf(&b, "<table border=1 cellpadding=4><tr><th>track</th><th>WaxFlow</th><th>ffmpeg-aac</th><th>delta</th></tr>\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%.3f</td><td>%.3f</td><td>%+.3f</td></tr>\n",
				html.EscapeString(r.name), r.ours, r.ref, r.delta)
		}
		fmt.Fprintf(&b, "<tr><th>mean</th><th>%.3f</th><th>%.3f</th><th>%+.3f</th></tr>\n", meanOurs, meanRef, meanOurs-meanRef)
		fmt.Fprintf(&b, "</table>\n")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing quality report: %v", err)
		}
		t.Logf("wrote quality report to %s", path)
	}

	if meanOurs < meanRef-0.2 {
		t.Errorf("corpus mean ODG %.3f below ffmpeg-aac mean %.3f - 0.2 (gate)", meanOurs, meanRef)
	}
	if worst < -0.5 {
		t.Errorf("worst per-track ODG delta %.3f exceeds the 0.5 allowance below ffmpeg-aac", worst)
	}
}
