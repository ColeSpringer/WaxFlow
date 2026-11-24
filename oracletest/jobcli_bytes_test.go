package oracletest

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxflow/cli"
	"github.com/colespringer/waxflow/internal/jobs"
)

// TestJobAndCLIWriteTheSameBytes: a transcode job and `waxflow transcode`
// run one file pipeline, so the same source and options under loudness
// analyze write the same file through either front end.
func TestJobAndCLIWriteTheSameBytes(t *testing.T) {
	env, _ := newJobsEnv(t)
	// The ramp is loud enough to measure, so a real gain is applied.
	src := filepath.Join(env.root, "tagged.wav")
	raw, err := os.ReadFile(filepath.Join(env.root, "ramp.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	tagFixture(t, src, map[tag.Key]string{tag.Title: "Same Title", tag.Artist: "Same Artist"}, tinyPNG(t), "")

	for _, tc := range []struct {
		name, format, ext string
		rate              int // 0 keeps the source's
	}{
		{"flac", "flac", "flac", 0},
		{"aac", "aac", "m4a", 0}, // a file output is flat MP4
		{"wavpack", "wavpack", "wv", 0},
		// The ramp is 48 kHz, so this one runs the resampler on both sides.
		{"flac at 44100", "flac", "flac", 44100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"type": "transcode", "src": "lib/tagged.wav", "format": tc.format, "loudness": "analyze",
			}
			args := []string{"transcode", "--loudness", "analyze"}
			if tc.rate != 0 {
				body["rate"] = tc.rate
				args = append(args, "--rate", strconv.Itoa(tc.rate))
			}
			j := decodeJob(t, env.postJSON(t, "/jobs", body), http.StatusCreated)
			done := waitJob(t, env, j.ID)
			if done.State != jobs.StateDone {
				t.Fatalf("job ended %s: %+v", done.State, done.Error)
			}
			resp := env.get(t, "/jobs/"+j.ID+"/result", nil)
			job := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("result: %d", resp.StatusCode)
			}

			out := filepath.Join(t.TempDir(), "out."+tc.ext)
			var stdout, stderr strings.Builder
			if code := cli.Execute("test", append(args, src, out), &stdout, &stderr); code != 0 {
				t.Fatalf("cli exit = %d: %s", code, stderr.String())
			}
			mine, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			// Equal bytes prove little if the pipeline wrote neither tags nor
			// ReplayGain.
			doc := parseTags(t, mine)
			if got := doc.Fields().Title; got != "Same Title" {
				t.Errorf("title = %q", got)
			}
			if g, _ := doc.Get(tag.ReplayGainTrackGain); len(g) != 1 {
				t.Errorf("ReplayGain gain = %v, want one", g)
			}
			if !strings.Contains(stderr.String(), "applying ") || strings.Contains(stderr.String(), "applying +0.00 dB") {
				t.Errorf("stderr = %q, want a gain applied", stderr.String())
			}
			if !bytes.Equal(job, mine) {
				t.Errorf("job wrote %d bytes, the CLI %d, and they differ at byte %d",
					len(job), len(mine), firstDiff(job, mine))
			}
		})
	}
}

// firstDiff is the offset of the first byte a and b disagree on.
func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
