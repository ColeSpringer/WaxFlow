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

// heQualityCorpus is the shared corpus plus bright items: the stock
// classes top out near 9 kHz, which barely reaches the SBR band, so the
// HE gate adds content whose upper octaves actually exercise the tool
// (including a transient class with high-band attacks, where a
// FIXFIX-only grid loses to fdk by more than the allowance).
func heQualityCorpus(rate, frames int) []qualityItem {
	items := qualityCorpus(rate, frames)
	bright := func(seed uint64, tones []float64, burst bool) []float32 {
		out := make([]float32, frames*2)
		s := seed
		for i := 0; i < frames; i++ {
			var v float64
			for j, fq := range tones {
				v += 0.4 / float64(j+1) * math.Sin(2*math.Pi*fq*float64(i)/float64(rate)+float64(j)*1.3)
			}
			s = s*6364136223846793005 + 1442695040888963407
			n := 2*float64(int64(s>>11))/float64(1<<53) - 1
			if burst {
				// Hi-hat-like eighth-note noise bursts with a fast decay.
				period := rate / 4
				ph := i % period
				env := math.Exp(-8 * float64(ph) / float64(period))
				v += 0.5 * n * env
			} else {
				v += 0.04 * n
			}
			out[2*i] = float32(v)
			out[2*i+1] = float32(0.85 * v)
		}
		return out
	}
	items = append(items,
		qualityItem{"bright-tonal", 2, bright(3, []float64{523.25, 3140, 8000, 11500, 14700}, false)},
		qualityItem{"bright-hihat", 2, bright(11, []float64{220, 1100}, true)},
	)
	return items
}

// TestHEAACEncoderQuality is the HE-AAC leg of the encoder-quality
// harness (docs/quality-gates.md). Two references, by availability:
//
//   - libfdk_aac (best-in-class, a harder bar than the LC gate's
//     ffmpeg-native reference): our corpus mean must be at least fdk's
//     minus 0.4, and no track more than 0.7 below fdk, judged at each of
//     48 and 64 kb/s separately (a win at one bitrate must not offset a
//     loss at another: nobody streaming 48k hears the 64k encode). The
//     known deficits that exceed the allowance are named per (track,
//     bitrate) in heaacKnownDeficits below, symmetry-allowlist style:
//     each carries its own measured bound and the test fails when one
//     has clearly closed, so an entry is a debt on record, not a
//     concealment. This leg self-skips without a libfdk route (ffmpeg's
//     wrapper or the scripts/fdkenc tool via WAXFLOW_FDKENC; the
//     encoder-quality make target runs this test either way, and
//     WAXFLOW_REQUIRE_FDK=1 turns a missing route into a failure where
//     one is expected to exist).
//   - our own AAC-LC at the same bitrate (always available): a loose
//     catastrophe net, not parity. The NMR-based proxy structurally
//     punishes parametric replication (band-center sines shift tones by
//     up to half a band, patched noise is phase-random), so a healthy
//     SBR layer measures a few tenths under LC at 64 kb/s even when it
//     sounds better (both bitrates run this leg; without it the 48k
//     subtest judged nothing on a machine with no libfdk route); fdk
//     faces the same metric, which is why the real
//     bar is fdk-relative. What this leg catches offline is a grossly
//     wrong high band; a merely missing or level-wrong one already fails
//     the codec-level QMF band-energy round trip.
//
// Both outputs travel as ADTS and decode through ffmpeg, so the
// comparison is codec against codec under one reference decoder.
func TestHEAACEncoderQuality(t *testing.T) {
	testutil.EncoderQualityGate(t)
	testutil.FFmpeg(t)

	const rate = 44100
	corpus := heQualityCorpus(rate, 3*rate)
	haveFDK := testutil.HaveFDKEncoder(t)

	decodeOurs := func(t *testing.T, item qualityItem, path string) []float32 {
		dec := testutil.FFmpegDecodeF32(t, path)
		if item.ch == 1 && testutil.FFprobeFile(t, path).Channels == 2 {
			// ffmpeg widens mono HE-AAC to an implicit-PS-ready pair;
			// fold back to the mono reference's shape.
			mono := make([]float32, len(dec)/2)
			for i := range mono {
				mono[i] = dec[2*i]
			}
			dec = mono
		}
		return dec
	}

	// heaacKnownDeficits is the burn-down list: (track, kbps) cells whose
	// measured delta below fdk exceeds the standard 0.7 allowance, each
	// with its own bound and the work that deletes it. An entry whose
	// measured delta comes back clearly inside the standard allowance
	// fails as stale (delete it with the change that closed it); the
	// clear margin keeps a value hovering at the boundary from flapping.
	type deficitKey struct {
		name string
		kbps int
	}
	heaacKnownDeficits := map[deficitKey]float64{
		// Transient reproduction at the lowest gated rate: measured -0.93
		// under fdk (2026-08-18, after the metric's alignment fixes; the
		// same cell reads -0.08 at 64k). SBR-side tuning did not move it
		// (crossover moves were A/B-neutral, a short-window bit loan was
		// worse), which points at the half-rate core's click handling at
		// ~22 kb/s a channel; closing it is core-encoder transient work,
		// LC-affecting with its own version bump.
		{"transient", 48}: 1.3,
	}

	for _, kbps := range []int{48, 64} {
		t.Run(fmt.Sprintf("%dk", kbps), func(t *testing.T) {
			type row struct {
				name          string
				ours, fdk, lc float64
				fdkDelta      float64
			}
			var rows []row
			var sumOurs, sumFDK, sumLC float64

			for _, item := range corpus {
				f := audio.Format{Rate: rate, Channels: item.ch, Layout: audio.DefaultLayout(item.ch), Type: audio.Float, BitDepth: 32}
				wav := synthWAVFromSamples(t, f, item.samples)
				wavPath := filepath.Join(t.TempDir(), item.name+".wav")
				if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
					t.Fatal(err)
				}

				var out bytes.Buffer
				transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{AACBitrate: kbps * 1000, Container: "adts"})
				ourPath := filepath.Join(t.TempDir(), item.name+".ours.aac")
				if err := os.WriteFile(ourPath, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				ourODG := testutil.ODGProxy(item.samples, decodeOurs(t, item, ourPath), rate, item.ch)

				r := row{name: item.name, ours: ourODG, fdk: math.NaN(), lc: math.NaN()}
				sumOurs += ourODG

				if haveFDK {
					fdkPath := testutil.FDKEncodeADTS(t, t.TempDir(), wavPath, rate, item.ch, kbps, "aac_he")
					fdkODG := testutil.ODGProxy(item.samples, decodeOurs(t, item, fdkPath), rate, item.ch)
					r.fdk = fdkODG
					r.fdkDelta = ourODG - fdkODG
					sumFDK += fdkODG
					allow, known := heaacKnownDeficits[deficitKey{item.name, kbps}]
					switch {
					case !known && r.fdkDelta < -0.7:
						t.Errorf("%s at %dk sits %.3f below fdk (allowance 0.7)", item.name, kbps, -r.fdkDelta)
					case known && r.fdkDelta < -allow:
						t.Errorf("%s at %dk sits %.3f below fdk, past even its recorded %.1f deficit bound", item.name, kbps, -r.fdkDelta, allow)
					case known && r.fdkDelta > -0.5:
						t.Errorf("%s at %dk measures %+.3f, clearly inside the standard allowance: its known-deficit entry has closed, delete it", item.name, kbps, r.fdkDelta)
					}
				}
				// The LC reference runs at BOTH bitrates: without it the 48k
				// subtest asserted nothing on a machine without libfdk, and
				// 48k is where the recorded deficit lives.
				var lcOut bytes.Buffer
				transcodeAAC(t, wav, &lcOut, waxflow.TranscodeOptions{AACBitrate: kbps * 1000, Container: "adts"})
				lcPath := filepath.Join(t.TempDir(), item.name+".lc.aac")
				if err := os.WriteFile(lcPath, lcOut.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				r.lc = testutil.ODGProxy(item.samples, testutil.FFmpegDecodeF32(t, lcPath), rate, item.ch)
				sumLC += r.lc
				rows = append(rows, r)
				t.Logf("%-16s ours=%.3f fdk=%.3f lc=%.3f", item.name, r.ours, r.fdk, r.lc)
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
			meanLC := sumLC / n
			t.Logf("offline reference: our AAC-LC at %dk mean=%.3f (HE delta %+.3f)", kbps, meanLC, meanOurs-meanLC)
			if meanOurs < meanLC-0.7 {
				t.Errorf("HE mean ODG %.3f below our own LC-at-%dk mean %.3f - 0.7: the high band is grossly wrong, not merely parametric", meanOurs, kbps, meanLC)
			}

			if path := os.Getenv("WAXFLOW_QUALITY_REPORT"); path != "" && kbps == 64 {
				var b strings.Builder
				fmt.Fprintf(&b, "<h1>HE-AAC v1 encoder-quality report</h1>\n")
				fmt.Fprintf(&b, "<p>ODG-proxy at %d kbit/s. Gate: mean &ge; fdk &minus; 0.4 and no track &gt; 0.7 below, per bitrate; recorded deficits carry their own bounds.</p>\n", kbps)
				fmt.Fprintf(&b, "<table border=1 cellpadding=4><tr><th>track</th><th>WaxFlow HE</th><th>fdk HE</th><th>WaxFlow LC</th></tr>\n")
				for _, r := range rows {
					fmt.Fprintf(&b, "<tr><td>%s</td><td>%.3f</td><td>%.3f</td><td>%.3f</td></tr>\n",
						html.EscapeString(r.name), r.ours, r.fdk, r.lc)
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
