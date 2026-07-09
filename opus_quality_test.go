package waxflow_test

import (
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpusEncoderQuality is the CELT/music encoder-quality gate
// (docs/quality-gates.md): encode the pinned 20-track corpus with
// our Opus encoder and with libopus at matched CBR targets, decode both with
// the reference libopus decoder, score each against the original with the
// reference opus_compare metric, and enforce the weighted-error ratio
// (ours / libopus): geometric mean <= 1.20 per bitrate, no track > 2.6, at
// 96/128/160 kbps stereo.
//
// The harness is deterministic and sample-exact by construction, with no
// cross-correlation alignment anywhere: our packets go through `opus_demo -d`
// (whose output leads the input by exactly the declared pre-skip) carrying
// per-packet Encoder.FinalRange values that the reference decoder
// cross-checks, and the libopus side runs an `opus_demo` round trip (whose
// output is lookahead-trimmed). Both sides therefore decode through the same
// reference decoder and score over the same sample range against the same
// original.
//
// Our encoder runs at complexity 10: the gate states what the encoder can
// do, and the complexity knob exists to trade quality for speed on
// constrained hosts, not to move the quality bar. libopus runs at its
// opus_demo default, also 10.
//
// The test self-skips without the fetched corpus (`make verify-vectors`) or
// the built reference tools (`make opus-tools`); the nightly encoder-quality
// job escalates both with WAXFLOW_REQUIRE_VECTORS=1 and
// WAXFLOW_REQUIRE_OPUS_TOOLS=1. WAXFLOW_QUALITY_REPORT writes the per-track
// HTML report published as a nightly artifact.
func TestOpusEncoderQuality(t *testing.T) {
	if raceEnabled {
		// Single-goroutine numeric encode over the whole corpus: many times
		// slower under the race detector for no concurrency coverage, like
		// the opus conformance test. The nightly encoder-quality job runs it
		// non-race and enforcing.
		t.Skip("encoder-quality gate skipped under -race (runs non-race in the encoder-quality job)")
	}
	opusDemo, opusCompare := testutil.OpusTools(t)
	corpus := loadOpusQualityCorpus(t)
	bitrates := []int{96, 128, 160}
	const complexity = 10
	const meanGate, trackGate = 1.20, 2.6
	const rate, channels = 48000, 2

	type row struct {
		name        string
		kbps        int
		qOurs, qLib float64
		errRatio    float64
	}
	var rows []row

	for _, kbps := range bitrates {
		logRatioSum, worst := 0.0, 0.0
		worstName := ""
		for _, track := range corpus {
			frames := len(track.pcm) / channels

			// Ours: encode, then decode with the reference decoder. The
			// decode-only output leads the input by the declared pre-skip
			// and covers at least the whole input (Finish pads the tail),
			// so dropping the pre-skip aligns it sample-exactly.
			packets, ranges := encodeOpusPackets(t, track.pcm, channels, kbps*1000, complexity)
			bitPath := filepath.Join(t.TempDir(), "ours.bit")
			if err := testutil.WriteOpusBitstream(bitPath, packets, ranges); err != nil {
				t.Fatal(err)
			}
			oursDec := testutil.OpusDemoDecode(t, opusDemo, bitPath, rate, channels)
			if len(oursDec) < opus.EncoderDelay*channels+len(track.pcm) {
				t.Fatalf("%s: reference decode of our stream is short: %d samples, want >= %d",
					track.name, len(oursDec)/channels, opus.EncoderDelay+frames)
			}
			ours := oursDec[opus.EncoderDelay*channels:]

			// libopus: reference round trip, output lookahead-trimmed and
			// aligned, shorter than the input by the encoder lookahead.
			lib := testutil.OpusDemoRoundTrip(t, opusDemo, track.pcm, rate, channels, kbps*1000, complexity)

			// Score both over the identical range against the identical
			// original.
			n := min(frames, min(len(ours)/channels, len(lib)/channels)) * channels
			errOurs, qOurs := testutil.OpusCompareTool(t, opusCompare, track.pcm[:n], ours[:n], channels)
			errLib, qLib := testutil.OpusCompareTool(t, opusCompare, track.pcm[:n], lib[:n], channels)

			ratio := errOurs / max(errLib, 1e-12)
			rows = append(rows, row{track.name, kbps, qOurs, qLib, ratio})
			logRatioSum += math.Log(ratio)
			if ratio > worst {
				worst, worstName = ratio, track.name
			}
			t.Logf("%dk %-12s err_ours=%.4f err_lib=%.4f ratio=%.3f (Q %6.1f vs %6.1f)",
				kbps, track.name, errOurs, errLib, ratio, qOurs, qLib)
			if ratio > trackGate {
				t.Errorf("%dk %s: error ratio %.3f exceeds the per-track gate of %.1f", kbps, track.name, ratio, trackGate)
			}
		}
		geoMean := math.Exp(logRatioSum / float64(len(corpus)))
		t.Logf("bitrate=%dk: geometric-mean error ratio %.3f over %d tracks, worst %.3f (%s); gate mean<=%.2f track<=%.1f",
			kbps, geoMean, len(corpus), worst, worstName, meanGate, trackGate)
		if geoMean > meanGate {
			t.Errorf("%dk: geometric-mean error ratio %.3f exceeds the corpus gate of %.2f", kbps, geoMean, meanGate)
		}
	}

	if report := os.Getenv("WAXFLOW_QUALITY_REPORT"); report != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "<h1>Opus (CELT) encoder-quality report</h1>\n")
		fmt.Fprintf(&b, "<p>opus_compare weighted-error ratio vs libopus at matched CBR, both decoded by the reference decoder, scored against the original (docs/quality-gates.md). Gate: geometric mean &le; %.2f, no track &gt; %.1f. Lower is better; 1.0 is parity.</p>\n", meanGate, trackGate)
		fmt.Fprintf(&b, "<table border=1 cellpadding=4><tr><th>track</th><th>kbps</th><th>error ratio</th><th>Q ours</th><th>Q libopus</th></tr>\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%d</td><td>%.3f</td><td>%.1f</td><td>%.1f</td></tr>\n",
				html.EscapeString(r.name), r.kbps, r.errRatio, r.qOurs, r.qLib)
		}
		fmt.Fprintf(&b, "</table>\n")
		if err := os.WriteFile(report, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing quality report: %v", err)
		}
		t.Logf("wrote opus quality report to %s", report)
	}
}

// opusQualityTrack is one corpus clip as interleaved 16-bit 48 kHz stereo,
// trimmed to a whole number of 20 ms frames.
type opusQualityTrack struct {
	name string
	pcm  []int16
}

// loadOpusQualityCorpus decodes the pinned 20-track corpus (48 kHz / 16-bit
// / stereo WAV) through our own riff/pcm path. It self-skips when the
// vectors have not been fetched.
func loadOpusQualityCorpus(t *testing.T) []opusQualityTrack {
	t.Helper()
	var corpus []opusQualityTrack
	for _, name := range testutil.OpusQualityCorpus() {
		path := testutil.VectorPath(t, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		med, err := format.Open(container.BytesSource(data), "wav", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f := med.Info().Default().Fmt
		if f.Rate != 48000 || f.Channels != 2 || f.BitDepth != 16 {
			t.Fatalf("%s: corpus clip is %v, want 48 kHz 16-bit stereo", name, f)
		}
		var interleaved []int16
		buf := audio.Get(f, audio.StandardChunk)
		for {
			err := med.ReadChunk(buf)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < buf.N; i++ {
				for c := 0; c < f.Channels; c++ {
					interleaved = append(interleaved, int16(buf.ChanI(c)[i]))
				}
			}
		}
		audio.Put(buf)
		med.Close()
		// A whole number of 20 ms frames keeps every length in the harness
		// exact frame arithmetic.
		frames := len(interleaved) / f.Channels / 960 * 960
		corpus = append(corpus, opusQualityTrack{
			name: strings.TrimSuffix(filepath.Base(name), ".wav"),
			pcm:  interleaved[:frames*f.Channels],
		})
	}
	return corpus
}

// encodeOpusPackets runs our encoder over interleaved 16-bit PCM and returns
// the raw Opus packets with their range coder final states.
func encodeOpusPackets(t *testing.T, pcm []int16, channels, bitrate, complexity int) ([][]byte, []uint32) {
	t.Helper()
	f := audio.Format{Rate: 48000, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Float, BitDepth: 32}
	enc, err := opus.NewEncoder(f, &opus.EncoderOptions{Bitrate: bitrate, Complexity: complexity})
	if err != nil {
		t.Fatal(err)
	}
	// The same nominal [-1, 1) mapping the engine's convert stage applies.
	n := len(pcm) / channels
	flat := make([]float32, channels*n)
	for i := 0; i < n; i++ {
		for c := 0; c < channels; c++ {
			flat[c*n+i] = float32(pcm[i*channels+c]) / 32768
		}
	}
	var packets [][]byte
	var ranges []uint32
	emit := func(p codec.Packet) error {
		packets = append(packets, p.Data)
		ranges = append(ranges, enc.FinalRange())
		return nil
	}
	buf := &audio.Buffer{Fmt: f, F: flat, Stride: n, N: n}
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	return packets, ranges
}
