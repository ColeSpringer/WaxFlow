//go:build unix

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/posixfs"
)

// gateTimeout bounds every wait on a child, so a broken gate fails the test
// instead of hanging it.
const gateTimeout = 30 * time.Second

// heldLookup is a pid lookup host that holds each request until released,
// reporting it as it arrives.
type heldLookup struct {
	*httptest.Server
	asked   chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	dropped bool // a request ended by its client before the release
}

func newHeldLookup(t *testing.T) *heldLookup {
	t.Helper()
	h := &heldLookup{asked: make(chan struct{}, 8), release: make(chan struct{})}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.asked <- struct{}{}
		select {
		case <-h.release:
		case <-r.Context().Done():
			h.mu.Lock()
			h.dropped = true
			h.mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ref":"lib/track.wav"}`)
	}))
	t.Cleanup(func() {
		h.unblock()
		h.Close()
	})
	return h
}

func (h *heldLookup) unblock() { h.once.Do(func() { close(h.release) }) }

func (h *heldLookup) wasDropped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// waitAsked waits for the lookup to report a request.
func (h *heldLookup) waitAsked(t *testing.T) {
	t.Helper()
	select {
	case <-h.asked:
	case <-time.After(gateTimeout):
		t.Fatal("the lookup never saw the request")
	}
}

// helperCmd is the test binary as a CLI child, with the settings env holds;
// a later value of a variable wins, so env may set the helper's mode.
func helperCmd(t *testing.T, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), "WAXFLOW_CLI_HELPER=1"), env...)
	return cmd
}

// waitExit waits for the child and returns its exit code, failing the test
// when a signal ended it.
func waitExit(t *testing.T, cmd *exec.Cmd, done <-chan error) int {
	t.Helper()
	select {
	case <-done:
	case <-time.After(gateTimeout):
		cmd.Process.Kill()
		<-done
		t.Fatal("the child did not exit")
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		t.Fatalf("the child was killed by %v", ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// TestInterruptCancelsACommand: SIGINT during a command cancels it. The
// child is held in a pid lookup; the interrupt cancels the lookup, the
// command exits 6, and nothing is left where the output would go.
func TestInterruptCancelsACommand(t *testing.T) {
	lib := t.TempDir()
	if err := os.WriteFile(filepath.Join(lib, "track.wav"), []byte("unread"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	lookup := newHeldLookup(t)
	var stderr strings.Builder
	cmd := helperCmd(t, []string{
		"WAXFLOW_ROOTS=lib=" + lib,
		"WAXFLOW_DATA_DIR=" + t.TempDir(),
		"WAXFLOW_PID_LOOKUP_URL=" + lookup.URL + "/pid",
	}, "transcode", "pid:"+lookupPID, filepath.Join(outDir, "out.flac"))
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	lookup.waitAsked(t)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := waitExit(t, cmd, done); code != 6 {
		t.Errorf("exit = %d, want 6 (canceled); stderr: %s", code, stderr.String())
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s left behind", e.Name())
	}
}

var listeningAddr = regexp.MustCompile(`"waxflow daemon listening" addr=(\S+)`)

// TestDaemonDrainSwallowsASecondSignal: the daemon keeps its own signal
// registration while it drains, so a second SIGINT does not kill it. A
// stream held in a pid lookup keeps the drain from finishing until the test
// releases it.
func TestDaemonDrainSwallowsASecondSignal(t *testing.T) {
	lib := t.TempDir()
	if err := os.WriteFile(filepath.Join(lib, "track.wav"), []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	lookup := newHeldLookup(t)
	cmd := helperCmd(t, []string{
		"WAXFLOW_ADDR=127.0.0.1:0",
		"WAXFLOW_API_KEYS=k1",
		"WAXFLOW_ROOTS=lib=" + lib,
		"WAXFLOW_DATA_DIR=" + t.TempDir(),
		"WAXFLOW_CACHE_DIR=" + t.TempDir(),
		"WAXFLOW_SCRATCH_DIR=" + t.TempDir(),
		"WAXFLOW_PID_LOOKUP_URL=" + lookup.URL + "/pid",
	}, "server")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The daemon logs to stdout: the listening line names the port, and the
	// stopped line ends the drain.
	addrc := make(chan string, 1)
	var mu sync.Mutex
	var log strings.Builder
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			mu.Lock()
			log.WriteString(sc.Text())
			log.WriteByte('\n')
			mu.Unlock()
			if m := listeningAddr.FindStringSubmatch(sc.Text()); m != nil {
				addrc <- m[1]
			}
		}
		if err := sc.Err(); err != nil {
			mu.Lock()
			fmt.Fprintf(&log, "reading stdout: %v\n", err)
			mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		<-scanned // Wait closes the pipe; read it to the end first
		done <- cmd.Wait()
	}()
	var addr string
	select {
	case addr = <-addrc:
	case <-time.After(gateTimeout):
		cmd.Process.Kill()
		<-done
		t.Fatalf("the daemon never listened; stdout: %s stderr: %s", log.String(), stderr.String())
	}

	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/stream?src=pid:"+lookupPID+"&format=flac", nil)
		req.Header.Set("X-API-Key", "k1")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	lookup.waitAsked(t)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	// A refused dial shows the first signal began the drain, so the second
	// cannot merge with it.
	deadline := time.Now().Add(gateTimeout)
	for {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the daemon kept listening after SIGINT")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("the second SIGINT found no daemon: %v", err)
	}
	lookup.unblock()
	<-streamed

	code := waitExit(t, cmd, done)
	mu.Lock()
	out := log.String()
	mu.Unlock()
	if code != 0 {
		t.Errorf("exit = %d, want 0; stdout: %s stderr: %s", code, out, stderr.String())
	}
	if !strings.Contains(out, "waxflow daemon stopped") {
		t.Errorf("no stopped line; stdout: %s", out)
	}
	if lookup.wasDropped() {
		t.Error("the held lookup ended before its release, so nothing held the drain")
	}
}

// TestSecondInterruptKillsACommand: the first SIGINT cancels a command and
// releases the CLI's registration, so a second one takes the default action
// even while the command is held in a step that ignores cancellation.
func TestSecondInterruptKillsACommand(t *testing.T) {
	cmd := helperCmd(t, []string{"WAXFLOW_CLI_HELPER=hold"}, "probe", "pid:"+lookupPID)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 4)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		if err := sc.Err(); err != nil {
			lines <- "reading stdout: " + err.Error()
		}
	}()
	done := make(chan error, 1)
	go func() {
		<-scanned
		done <- cmd.Wait()
	}()
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			<-done
		}
	}()
	wantLine := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("child said %q, want %q", got, want)
			}
		case <-time.After(gateTimeout):
			t.Fatalf("the child never said %q", want)
		}
	}

	wantLine("held")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	wantLine("canceled")
	// The release runs on its own goroutine after the cancel, which the
	// child cannot report, so the second signal is resent until it lands.
	// A registration still held swallows every one.
	deadline := time.After(gateTimeout)
	for exited := false; !exited; {
		resend := time.After(50 * time.Millisecond)
		// ErrProcessDone: Wait reaped the child after the last send, so
		// done is due and nothing is resent.
		if err := cmd.Process.Signal(os.Interrupt); errors.Is(err, os.ErrProcessDone) {
			resend = nil
		} else if err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			exited = true
		case <-resend:
		case <-deadline:
			t.Fatal("a second SIGINT did not kill the command")
		}
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Errorf("child ended %v, want death by SIGINT", cmd.ProcessState)
	}
}

// TestKilledTranscodeLeavesNoPartialOutput: a transcode killed mid-encode
// leaves at most a temp beside its target, never a partial target, and the
// same command run again writes the output.
func TestKilledTranscodeLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	in := sineWAV(t, dir)
	out := filepath.Join(dir, "out.flac")
	cmd := helperCmd(t, []string{"WAXFLOW_CLI_HELPER=hold-encode"}, "transcode", in, out)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	held := make(chan bool, 1)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(stdout)
		ok := sc.Scan() && sc.Text() == "encoding"
		held <- ok
		for sc.Scan() {
		}
	}()
	done := make(chan error, 1)
	go func() {
		<-scanned
		done <- cmd.Wait()
	}()
	select {
	case ok := <-held:
		if !ok {
			cmd.Process.Kill()
			<-done
			t.Fatalf("the child never held mid-encode; stderr: %s", stderr.String())
		}
	case <-time.After(gateTimeout):
		cmd.Process.Kill()
		<-done
		t.Fatal("the child never reached the encode")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "in.wav" && !posixfs.IsTemp(e.Name()) {
			t.Errorf("the killed transcode left %s, which is not a temp", e.Name())
		}
	}

	var rerunOut, rerunErr strings.Builder
	if code := execute(t.Context(), "test", []string{"transcode", in, out}, &rerunOut, &rerunErr, Flavor{}); code != 0 {
		t.Fatalf("the rerun exited %d; stderr: %s", code, rerunErr.String())
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Errorf("the rerun wrote no output: %v", err)
	}
}
