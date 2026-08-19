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

// heV2QualityCorpus is the v2 gate's corpus: the shared HE corpus's
// stereo items plus two items with a real stereo image. The shared
// items are all R = k*L (ICC 1.0 in every band, IID a constant), which
// exercises none of the parameter layer: without width, the ps_data
// stays a few dozen bits, the escaped extension size never serializes,
// and breaking the rotation, the ICC sign, or the decorrelator moves
// no gate number. The wide items put per-band IID/ICC variation, real
// side-info cost, and the phase-aligned downmix itself under the gate.
func heV2QualityCorpus(rate, frames int) []qualityItem {
	var corpus []qualityItem
	for _, item := range heQualityCorpus(rate, frames) {
		if item.ch == 2 {
			corpus = append(corpus, item)
		}
	}

	// wide-pan: tones alternately panned hard left and right with a slow
	// pan drift and light independent beds; IID varies per band and per
	// frame, ICC stays high.
	widePan := make([]float32, frames*2)
	s0, s1 := uint64(41), uint64(1223)
	tones := []float64{196, 440, 880, 1760, 3100, 5200, 8200, 11800}
	for i := 0; i < frames; i++ {
		drift := 0.5 + 0.45*math.Sin(2*math.Pi*0.7*float64(i)/float64(rate))
		var l, r float64
		for j, fq := range tones {
			v := 0.16 / float64(j/2+1) * math.Sin(2*math.Pi*fq*float64(i)/float64(rate)+1.1*float64(j))
			if j%2 == 0 {
				l += v
				r += v * (1 - drift)
			} else {
				r += v
				l += v * drift
			}
		}
		s0 = s0*6364136223846793005 + 1442695040888963407
		s1 = s1*6364136223846793005 + 1442695040888963407
		widePan[2*i] = float32(l + 0.03*(2*float64(s0>>11)/float64(1<<53)-1))
		widePan[2*i+1] = float32(r + 0.03*(2*float64(s1>>11)/float64(1<<53)-1))
	}

	// wide-noise: a shared tonal bed under independent per-channel noise,
	// ICC well below 1 across the band so the coded coherence and the
	// decoder's decorrelator carry real signal.
	wideNoise := make([]float32, frames*2)
	s0, s1 = uint64(907), uint64(2027)
	for i := 0; i < frames; i++ {
		bed := 0.22*math.Sin(2*math.Pi*330*float64(i)/float64(rate)) +
			0.14*math.Sin(2*math.Pi*2400*float64(i)/float64(rate)) +
			0.08*math.Sin(2*math.Pi*7900*float64(i)/float64(rate))
		s0 = s0*6364136223846793005 + 1442695040888963407
		s1 = s1*6364136223846793005 + 1442695040888963407
		wideNoise[2*i] = float32(bed + 0.2*(2*float64(s0>>11)/float64(1<<53)-1))
		wideNoise[2*i+1] = float32(bed + 0.2*(2*float64(s1>>11)/float64(1<<53)-1))
	}

	return append(corpus,
		qualityItem{"wide-pan", 2, widePan},
		qualityItem{"wide-noise", 2, wideNoise},
	)
}

// TestHEAACv2EncoderQuality is the HE-AAC v2 leg of the encoder-quality
// harness, the v1 gate's shape at v2's operating points (24 and 32
// kb/s, judged separately; a win at one bitrate must not offset a loss
// at another). Stereo corpus items only: v2 is parametric stereo, and
// mono has no v2 form.
//
//   - libfdk_aac aac_he_v2 (best-in-class): corpus mean at least fdk's
//     minus 0.4 and no track more than 0.7 below, with known deficits
//     named per (track, bitrate) in heaacV2KnownDeficits, each carrying
//     its own measured bound and failing as stale when it closes.
//     Self-skips without a libfdk route; WAXFLOW_REQUIRE_FDK=1
//     escalates.
//   - our own HE-AAC v1 at the same bitrate (always available): the
//     offline catastrophe net. The proxy downmixes to mono, so this leg
//     scores the mono core plus SBR against v1's coupled stereo at the
//     same total budget; v2 exists because that trade wins at low
//     rates, so v2 falling clearly under v1 here means the downmix or
//     the parameter layer is broken, not merely parametric. The stereo
//     image itself is gated codec-side (TestHEEncoderV2StereoImage),
//     which the mono proxy cannot see.
func TestHEAACv2EncoderQuality(t *testing.T) {
	testutil.EncoderQualityGate(t)
	testutil.FFmpeg(t)

	const rate = 44100
	corpus := heV2QualityCorpus(rate, 3*rate)
	haveFDK := testutil.HaveFDKEncoder(t)

	type deficitKey struct {
		name string
		kbps int
	}
	// The burn-down ledger, the v1 gate's shape: cells past the standard
	// allowance carry their own bounds and fail as stale when they close.
	// Empty since the core's deferred window decision (aac-enc-2) landed:
	// the two transient cells (-1.30 at 24k, -0.84 at 32k, both proven
	// core-bound by the mono A/B recorded at hePlanParams) measure -0.19
	// and +0.20 now; all three ledger cells closed together with the v1
	// entry, as its note predicted.
	heaacV2KnownDeficits := map[deficitKey]float64{}

	for _, kbps := range []int{24, 32} {
		t.Run(fmt.Sprintf("%dk", kbps), func(t *testing.T) {
			type row struct {
				name          string
				ours, fdk, v1 float64
				fdkDelta      float64
			}
			var rows []row
			var sumOurs, sumFDK, sumV1 float64

			for _, item := range corpus {
				f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
				wav := synthWAVFromSamples(t, f, item.samples)
				wavPath := filepath.Join(t.TempDir(), item.name+".wav")
				if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
					t.Fatal(err)
				}

				var out bytes.Buffer
				transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{HEAACv2: true, AACBitrate: kbps * 1000, Container: "adts"})
				ourPath := filepath.Join(t.TempDir(), item.name+".ours.aac")
				if err := os.WriteFile(ourPath, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				ourODG := testutil.ODGProxy(item.samples, testutil.FFmpegDecodeF32(t, ourPath), rate, 2)

				r := row{name: item.name, ours: ourODG, fdk: math.NaN(), v1: math.NaN()}
				sumOurs += ourODG

				if haveFDK {
					fdkPath := testutil.FDKEncodeADTS(t, t.TempDir(), wavPath, rate, 2, kbps, "aac_he_v2")
					fdkODG := testutil.ODGProxy(item.samples, testutil.FFmpegDecodeF32(t, fdkPath), rate, 2)
					r.fdk = fdkODG
					r.fdkDelta = ourODG - fdkODG
					sumFDK += fdkODG
					allow, known := heaacV2KnownDeficits[deficitKey{item.name, kbps}]
					switch {
					case !known && r.fdkDelta < -0.7:
						t.Errorf("%s at %dk sits %.3f below fdk (allowance 0.7)", item.name, kbps, -r.fdkDelta)
					case known && r.fdkDelta < -allow:
						t.Errorf("%s at %dk sits %.3f below fdk, past even its recorded %.1f deficit bound", item.name, kbps, -r.fdkDelta, allow)
					case known && r.fdkDelta > -0.5:
						t.Errorf("%s at %dk measures %+.3f, clearly inside the standard allowance: its known-deficit entry has closed, delete it", item.name, kbps, r.fdkDelta)
					}
				}

				var v1Out bytes.Buffer
				transcodeHEAAC(t, wav, &v1Out, waxflow.TranscodeOptions{AACBitrate: kbps * 1000, Container: "adts"})
				v1Path := filepath.Join(t.TempDir(), item.name+".v1.aac")
				if err := os.WriteFile(v1Path, v1Out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				r.v1 = testutil.ODGProxy(item.samples, testutil.FFmpegDecodeF32(t, v1Path), rate, 2)
				sumV1 += r.v1
				rows = append(rows, r)
				t.Logf("%-16s ours=%.3f fdk=%.3f v1=%.3f", item.name, r.ours, r.fdk, r.v1)
			}

			n := float64(len(rows))
			meanOurs := sumOurs / n
			if haveFDK {
				meanFDK := sumFDK / n
				t.Logf("corpus mean: ours=%.3f fdk=%.3f (delta %+.3f)", meanOurs, meanFDK, meanOurs-meanFDK)
				if meanOurs < meanFDK-0.4 {
					t.Errorf("corpus mean ODG %.3f below fdk mean %.3f - 0.4 (gate)", meanOurs, meanFDK)
				}
			} else {
				t.Logf("corpus mean: ours=%.3f (no libfdk_aac route; fdk gate not judged)", meanOurs)
			}
			meanV1 := sumV1 / n
			t.Logf("offline reference: our HE-AAC v1 at %dk mean=%.3f (v2 delta %+.3f)", kbps, meanV1, meanOurs-meanV1)
			if meanOurs < meanV1-0.4 {
				t.Errorf("v2 mean ODG %.3f clearly below our own v1-at-%dk mean %.3f: the downmix or parameter layer is broken, not merely parametric", meanOurs, kbps, meanV1)
			}

			// The nightly artifact, the v1 leg's shape: without it a gate
			// failure or a deficit burn-down leaves numbers only in the job
			// log, and re-measuring a ledger bound means a local run with an
			// fdk route, the thing the reports exist to avoid.
			if path := os.Getenv("WAXFLOW_QUALITY_REPORT"); path != "" && kbps == 32 {
				var b strings.Builder
				fmt.Fprintf(&b, "<h1>HE-AAC v2 encoder-quality report</h1>\n")
				fmt.Fprintf(&b, "<p>ODG-proxy at %d kbit/s. Gate: mean &ge; fdk (aac_he_v2) &minus; 0.4 and no track &gt; 0.7 below, per bitrate; recorded deficits carry their own bounds (see heaacV2KnownDeficits).</p>\n", kbps)
				fmt.Fprintf(&b, "<table border=1 cellpadding=4><tr><th>track</th><th>WaxFlow v2</th><th>fdk v2</th><th>WaxFlow v1</th></tr>\n")
				for _, r := range rows {
					fmt.Fprintf(&b, "<tr><td>%s</td><td>%.3f</td><td>%.3f</td><td>%.3f</td></tr>\n",
						html.EscapeString(r.name), r.ours, r.fdk, r.v1)
				}
				fmt.Fprintf(&b, "</table>\n")
				if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
					t.Fatalf("writing quality report: %v", err)
				}
				t.Logf("wrote quality report to %s", path)
			}
		})
	}
}
