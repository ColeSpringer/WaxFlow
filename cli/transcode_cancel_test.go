package cli

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/posixfs"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/internal/tofile"
)

// sineWAV writes a one-second sine WAV, long enough that the encode
// reports progress more than once.
func sineWAV(t *testing.T, dir string) string {
	t.Helper()
	const rate = 44100
	s := make([]float32, rate)
	for i := range s {
		s[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/rate))
	}
	return writeFixture(t, dir, "in.wav", testutil.PCM16WAVBytes(t, rate, [][]float32{s}))
}

// runCanceled runs the CLI under a context hook may cancel, given each
// transcode's request before it runs.
func runCanceled(t *testing.T, hook func(req *tofile.Request, cancel context.CancelFunc), args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transcodeHook = func(req *tofile.Request) { hook(req, cancel) }
	defer func() { transcodeHook = nil }()
	var out, errOut strings.Builder
	code = execute(ctx, "test", args, &out, &errOut, Flavor{})
	return code, out.String(), errOut.String()
}

// wantNothingLeft checks dir holds only the files named, none of them a temp,
// and that a target which existed is byte-identical.
func wantNothingLeft(t *testing.T, dir string, keep map[string][]byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		want, ok := keep[e.Name()]
		if !ok {
			t.Errorf("%s left behind (temp: %v)", e.Name(), posixfs.IsTemp(e.Name()))
			continue
		}
		if want == nil {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s changed", e.Name())
		}
	}
}

// cancelCases are the two ways a transcode meets its target: a new path, and
// an existing file under --force.
var cancelCases = []struct {
	name  string
	force bool
}{{"new output", false}, {"existing output under --force", true}}

// TestTranscodeCancelRemovesTheOutput: a cancel mid-encode exits 6 and leaves
// no output and no temp; under --force the old target is untouched.
func TestTranscodeCancelRemovesTheOutput(t *testing.T) {
	for _, tc := range cancelCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := sineWAV(t, dir)
			out := filepath.Join(dir, "out.flac")
			keep := map[string][]byte{"in.wav": nil}
			args := []string{"transcode", in, out}
			if tc.force {
				old := []byte("the old target")
				if err := os.WriteFile(out, old, 0o644); err != nil {
					t.Fatal(err)
				}
				keep["out.flac"] = old
				args = append(args, "--force")
			}
			var midEncode, called bool
			code, stdout, stderr := runCanceled(t, func(req *tofile.Request, cancel context.CancelFunc) {
				req.Progress = func(phase string, done, total int64) {
					if phase == "transcode" && !called {
						called, midEncode = true, done < total
						cancel()
					}
				}
			}, args...)
			if !called || !midEncode {
				t.Fatalf("the cancel did not land mid-encode (called %v); exit %d, stderr: %s", called, code, stderr)
			}
			if code != 6 {
				t.Errorf("exit = %d, want 6 (canceled); stderr: %s", code, stderr)
			}
			if strings.Contains(stdout, "wrote ") {
				t.Errorf("stdout = %q after a cancel", stdout)
			}
			wantNothingLeft(t, dir, keep)
		})
	}
}

// cancelingMapper cancels the run as the post-pass starts, then tags.
type cancelingMapper struct {
	meta.Mapper
	cancel  context.CancelFunc
	applied *bool
}

func (m cancelingMapper) Apply(ctx context.Context, path string, info *meta.Info, extra []container.Tag) error {
	*m.applied = true
	m.cancel()
	return m.Mapper.Apply(ctx, path, info, extra)
}

// TestCanceledPostPassIsNotSuccess: a cancel that lands in the post-pass is a
// cancel. The output may be untagged, so nothing is published or reported
// written, and the exit is 6.
func TestCanceledPostPassIsNotSuccess(t *testing.T) {
	for _, tc := range cancelCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := sineWAV(t, dir)
			out := filepath.Join(dir, "out.flac")
			keep := map[string][]byte{"in.wav": nil}
			args := []string{"transcode", in, out}
			if tc.force {
				old := []byte("the old target")
				if err := os.WriteFile(out, old, 0o644); err != nil {
					t.Fatal(err)
				}
				keep["out.flac"] = old
				args = append(args, "--force")
			}
			var applied bool
			code, stdout, stderr := runCanceled(t, func(req *tofile.Request, cancel context.CancelFunc) {
				req.Mapper = cancelingMapper{Mapper: req.Mapper, cancel: cancel, applied: &applied}
			}, args...)
			if !applied {
				t.Fatalf("the post-pass never ran; exit %d, stderr: %s", code, stderr)
			}
			if code != 6 {
				t.Errorf("exit = %d, want 6 (canceled); stderr: %s", code, stderr)
			}
			if strings.Contains(stdout, "wrote ") {
				t.Errorf("stdout = %q after a cancel", stdout)
			}
			wantNothingLeft(t, dir, keep)
		})
	}
}

// TestTranscodeRefusesATargetThatAppeared: a file another writer puts at the
// target during the encode is kept, and the command refuses as it would
// have at the start, exit 2, leaving no temp.
func TestTranscodeRefusesATargetThatAppeared(t *testing.T) {
	dir := t.TempDir()
	in := sineWAV(t, dir)
	out := filepath.Join(dir, "out.flac")
	theirs := []byte("another writer's file")
	var wrote bool
	code, stdout, stderr := runCanceled(t, func(req *tofile.Request, _ context.CancelFunc) {
		req.Progress = func(phase string, done, total int64) {
			if phase == "transcode" && !wrote {
				wrote = true
				if err := os.WriteFile(out, theirs, 0o644); err != nil {
					t.Error(err)
				}
			}
		}
	}, "transcode", in, out)
	if !wrote {
		t.Fatalf("the encode never reported; exit %d, stderr: %s", code, stderr)
	}
	if code != 2 || !strings.Contains(stderr, "output exists (use --force to overwrite)") {
		t.Errorf("exit %d, stderr %q; want 2 and output exists", code, stderr)
	}
	if strings.Contains(stdout, "wrote ") {
		t.Errorf("stdout = %q after a refusal", stdout)
	}
	wantNothingLeft(t, dir, map[string][]byte{"in.wav": nil, "out.flac": theirs})
}
