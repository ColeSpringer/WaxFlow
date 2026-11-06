package testutil

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// fatalTB turns a Fatalf into a panic the test can recover, so a refusal in
// runWMF is observable instead of ending the test.
type fatalTB struct{ testing.TB }

func (f *fatalTB) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }
func (f *fatalTB) Fatal(args ...any)                 { panic(fmt.Sprint(args...)) }

// fakeWMF stands in for powershell.exe: it fails the first n launches with
// out and then behaves as a successful script run, copying in to dst.
func fakeWMF(t *testing.T, failures int, out string, in, dst string) (*int, func([]string) ([]byte, error)) {
	t.Helper()
	calls := 0
	return &calls, func(argv []string) ([]byte, error) {
		calls++
		if calls <= failures {
			return []byte(out), errors.New("exit status 1")
		}
		b, err := os.ReadFile(in)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return nil, err
		}
		return []byte("ok " + wmfArg(t, dst) + "\n"), nil
	}
}

func wavPair(t *testing.T) (in, dst string) {
	t.Helper()
	dir := t.TempDir()
	in = filepath.Join(dir, "in.wav")
	samps := make([]int32, 8000)
	for i := range samps {
		samps[i] = int32(8000 * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	WriteWAV(t, in, audio.Format{Rate: 8000, Channels: 1, Type: audio.Int, BitDepth: 16}, samps)
	return in, filepath.Join(dir, "out.wma")
}

// TestRunWMFRetriesAnInteropLaunchFailure: WSL's interop relay can fail to
// start powershell.exe at all when many start at once, and says so in its
// own words; that launch is tried again, and the script's result is read
// from the run that happened.
func TestRunWMFRetriesAnInteropLaunchFailure(t *testing.T) {
	in, dst := wavPair(t)
	calls, fake := fakeWMF(t, 2, "<3>WSL (3033193 - ) ERROR: UtilAcceptVsock:273: accept4 failed 110\n", in, dst)
	defer swapWMFExec(fake)()
	defer swapWMFRetryDelay(0)()
	runWMF(t, "wmfenc.ps1", "retry probe", in, dst)
	if *calls != 3 {
		t.Fatalf("powershell.exe launched %d times, want 3 (two relay failures, then the run)", *calls)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("no output after the retried run: %v", err)
	}
}

// TestRunWMFDoesNotRetryTheScript: a failure the script itself reports is a
// defect to find, so one launch is all it gets, and the refusal carries it.
func TestRunWMFDoesNotRetryTheScript(t *testing.T) {
	in, dst := wavPair(t)
	calls, fake := fakeWMF(t, 1, "encoder refused the format\n", in, dst)
	defer swapWMFExec(fake)()
	defer swapWMFRetryDelay(0)()
	ft := &fatalTB{TB: t}
	func() {
		defer func() {
			msg, _ := recover().(string)
			if !strings.Contains(msg, "encoder refused the format") {
				t.Fatalf("runWMF = %q, want the script's own refusal", msg)
			}
		}()
		runWMF(ft, "wmfenc.ps1", "no retry probe", in, dst)
	}()
	if *calls != 1 {
		t.Fatalf("powershell.exe launched %d times, want 1", *calls)
	}
}
