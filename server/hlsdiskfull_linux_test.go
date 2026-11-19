package server_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fileSizeLimited marks the child process that runs under the limit.
const fileSizeLimited = "WAXFLOW_TEST_FILE_SIZE_LIMITED"

// A volume that takes a file's first bytes and then no more, as a full disk
// does: the variant's directory, its meta and its init header are written,
// each segment's write fails part way, and the listener still hears the
// stream a healthy daemon serves. The limit is the process's, so the body
// runs in a child of the test binary.
func TestHLSPlaysThroughAVolumeThatTakesNoMoreBytes(t *testing.T) {
	if os.Getenv(fileSizeLimited) == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		// The child gives up by itself, and is killed if it does not.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		child := exec.CommandContext(ctx, exe, "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.timeout=1m")
		child.Env = append(os.Environ(), fileSizeLimited+"=1")
		child.WaitDelay = 10 * time.Second
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("under a file-size limit: %v\n%s", err, out)
		}
		return
	}
	twin := newTestEnv(t, nil)
	twinInit, twinSegs := hlsURIs(t, twin, rampSegments)
	healthy := playHLS(t, twin, twinInit, twinSegs)

	env, rec := recordingEnv(t, nil)
	initURI, segURIs := hlsURIs(t, env, rampSegments)

	// No file of this process grows past the limit from here on: a write
	// that would cross it is cut short and the next fails.
	const limit = 4096
	for _, seg := range healthy.segs[:len(healthy.segs)-1] {
		if len(seg) <= limit {
			t.Fatalf("a %d-byte segment fits under the %d-byte limit", len(seg), limit)
		}
	}
	// The soft limit alone, lifted again at cleanup: what the process
	// writes after the test (a coverage report) is not this test's to cut.
	var before syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: before.Max}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &before); err != nil {
			t.Errorf("lifting the file-size limit: %v", err)
		}
		signal.Reset(syscall.SIGXFSZ)
	})

	got := playHLS(t, env, initURI, segURIs)
	if !bytes.Equal(got.whole(), healthy.whole()) {
		t.Error("the stream differs from a healthy daemon's")
	}
	waitForIdle(t, env)
	var held []string
	variants, _ := filepath.Glob(filepath.Join(env.cache, "v1", "*", "*", "*"))
	for _, p := range variants {
		held = append(held, filepath.Base(p))
	}
	slices.Sort(held)
	if want := []string{"init.mp4", "meta.json"}; !slices.Equal(held, want) {
		t.Errorf("the variant holds %v, want %v and no part of a segment", held, want)
	}
	failed := 0
	for _, r := range rec.Records() {
		if r.Msg == "hls worker failed" && strings.Contains(r.Attrs, "file too large") {
			failed++
		}
	}
	if failed == 0 {
		t.Errorf("no worker failed on the limit; the log holds %v", rec.Warnings())
	}
}
