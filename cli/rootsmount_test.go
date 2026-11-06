package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// syncLog is a log sink safe to read while the sweep goroutine writes it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// mountConfig writes a reload config naming roots and loads it the way the
// daemon does, with the ambient env kept out.
func mountConfig(t *testing.T, base string, roots []config.Root) (config.Config, string) {
	t.Helper()
	unsetenv(t, "WAXFLOW_ROOTS")
	unsetenv(t, "WAXFLOW_CATALOG_DB")
	path := writeReloadConfig(t, base, roots)
	cfg, err := config.Load(path, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

// TestBuildServerConfigToleratesMissingRoot pins the start-up half of the
// tolerant roots: the daemon builds with every root it can open, reports the
// rest through RootsStatus, and logs each one at the level its cause earns.
func TestBuildServerConfigToleratesMissingRoot(t *testing.T) {
	base := t.TempDir()
	dirA := makeRoot(t, base, "A", "a.wav")
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, path := mountConfig(t, base, []config.Root{
		{Name: "A", Path: dirA}, {Name: "gone", Path: filepath.Join(base, "gone")}, {Name: "file", Path: file},
		{Name: "empty", Path: empty},
	})
	var logs syncLog
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", slog.New(slog.NewTextHandler(&logs, nil)), Flavor{})
	if err != nil {
		t.Fatalf("buildServerConfig with a missing root = %v, want success", err)
	}
	defer cleanup()

	ctx := context.Background()
	if f, err := srvCfg.Resolver.Resolve(ctx, "A/a.wav"); err != nil {
		t.Fatalf("A resolve beside the missing root = %v", err)
	} else {
		f.Close()
	}
	if _, err := srvCfg.Resolver.Resolve(ctx, "gone/a.wav"); waxerr.CodeOf(err) != waxerr.CodeRootUnavailable {
		t.Fatalf("gone resolve = %v, want root-unavailable", err)
	}
	if srvCfg.RootsStatus == nil {
		t.Fatal("RootsStatus not wired")
	}
	if st := srvCfg.RootsStatus(); len(st) != 4 || !st[0].Mounted || st[1].Mounted || st[2].Mounted || st[3].Mounted {
		t.Fatalf("RootsStatus() = %+v, want A mounted, gone, file and empty not", st)
	}

	out := logs.String()
	for _, want := range []string{
		`level=WARN msg="root not mounted" root=gone`,  // a missing folder: an unplugged drive
		`level=WARN msg="root not mounted" root=empty`, // an empty one: a mount point not yet mounted
		`level=ERROR msg="root not mounted" root=file`, // a file in the way: a mistake
	} {
		if !strings.Contains(out, want) {
			t.Errorf("start-up log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "root=A") || strings.Contains(out, "no library root") {
		t.Errorf("start-up log reports a mounted root or no roots:\n%s", out)
	}
}

// TestBuildServerConfigLogsNoRootMounted pins the one start-up state worth an
// Error on its own: a daemon with roots configured and none of them open.
func TestBuildServerConfigLogsNoRootMounted(t *testing.T) {
	base := t.TempDir()
	cfg, path := mountConfig(t, base, []config.Root{{Name: "gone", Path: filepath.Join(base, "gone")}})
	var logs syncLog
	_, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", slog.New(slog.NewTextHandler(&logs, nil)), Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if out := logs.String(); !strings.Contains(out, `level=ERROR msg="no library root is mounted"`) {
		t.Errorf("start-up log missing the no-root Error:\n%s", out)
	}
}

// TestReloadLogsUnmountedRoots pins the reload half: a root the edited config
// adds at a missing path is reported, not refused, and logged.
func TestReloadLogsUnmountedRoots(t *testing.T) {
	base := t.TempDir()
	dirA := makeRoot(t, base, "A", "a.wav")
	cfg, path := mountConfig(t, base, []config.Root{{Name: "A", Path: dirA}})
	var logs syncLog
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", slog.New(slog.NewTextHandler(&logs, nil)), Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	writeReloadConfig(t, base, []config.Root{{Name: "A", Path: dirA}, {Name: "gone", Path: filepath.Join(base, "gone")}})
	res, err := srvCfg.ReloadRoots()
	if err != nil {
		t.Fatalf("reload adding a missing root = %v, want success", err)
	}
	if len(res.Added) != 1 || len(res.Unmounted) != 1 || res.Unmounted[0] != "gone" {
		t.Fatalf("reload = %+v, want gone added and unmounted", res)
	}
	if out := logs.String(); !strings.Contains(out, `level=WARN msg="root not mounted" root=gone`) {
		t.Errorf("reload log missing the unmounted root:\n%s", out)
	}
}

// waitFor polls until cond holds or a second passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRemountSweep pins the daemon's sweep: a root whose folder appears is
// mounted with nobody asking for it and logged at Info, one whose folder goes
// is demoted and logged like a missing root at start, and stop ends it.
func TestRemountSweep(t *testing.T) {
	later := filepath.Join(t.TempDir(), "later")
	roots, err := source.OpenRoots([]source.Root{{Name: "later", Path: later}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	var logs syncLog
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	watchRoots(roots, logger)
	stop := startRemount(func() { roots.Remount() }, logger, 5*time.Millisecond, time.Second)
	defer stop()

	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "a.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sweep to mount later", func() bool { return roots.Status()[0].Mounted })
	waitFor(t, "the mount's Info line", func() bool {
		return strings.Contains(logs.String(), `level=INFO msg="root mounted" root=later`)
	})

	if runtime.GOOS != "windows" { // a held directory cannot be removed there
		if err := os.RemoveAll(later); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the sweep to demote later", func() bool { return !roots.Status()[0].Mounted })
		waitFor(t, "the demotion's Warn line", func() bool {
			return strings.Contains(logs.String(), `level=WARN msg="root not mounted" root=later`)
		})
		waitFor(t, "the Error for no root mounted", func() bool {
			return strings.Contains(logs.String(), `level=ERROR msg="no library root is mounted"`)
		})
	}

	stop()
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(later, "a.wav"), []byte("RIFFdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // ten intervals
	if roots.Status()[0].Mounted != (runtime.GOOS == "windows") {
		t.Fatal("the sweep ran after stop")
	}
}

// TestDoctorReportsEachRoot pins doctor's per-root rows: a mounted root is
// checked as before, and an unmounted one fails under its own name with its
// reason, the root-unavailable exit class.
func TestDoctorReportsEachRoot(t *testing.T) {
	rootDir := doctorEnv(t)
	gone := filepath.Join(t.TempDir(), "gone")
	t.Setenv("WAXFLOW_ROOTS", "lib="+rootDir+",gone="+gone)
	code, out, _ := run(t, "doctor", "--json")
	if code != waxerr.CodeRootUnavailable.ExitCode() {
		t.Fatalf("exit = %d, want %d (io)\n%s", code, waxerr.CodeRootUnavailable.ExitCode(), out)
	}
	var report struct {
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s", err, out)
	}
	rows := map[string]string{}
	for _, c := range report.Checks {
		rows[c.Name] = c.Status + " " + c.Detail
	}
	if got := rows["root:lib"]; !strings.HasPrefix(got, "ok ") {
		t.Errorf("root:lib = %q, want ok", got)
	}
	if got := rows["root:gone"]; !strings.HasPrefix(got, "fail ") || !strings.Contains(got, gone) {
		t.Errorf("root:gone = %q, want a fail with the open error on its path", got)
	}
	if _, set := rows["roots"]; set {
		t.Errorf("a set-level roots row beside the per-root rows: %v", rows)
	}
}

// TestRemountStopIsBounded pins the shutdown against a hung library mount:
// stop waits out a pass in progress, but not for ever, and says why.
func TestRemountStopIsBounded(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	hung := func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	var logs syncLog
	stop := startRemount(hung, slog.New(slog.NewTextHandler(&logs, nil)), time.Millisecond, 20*time.Millisecond)
	<-entered
	start := time.Now()
	stop()
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("stop waited %v on a hung pass", waited)
	}
	if out := logs.String(); !strings.Contains(out, `level=WARN msg="remount sweep did not stop`) {
		t.Errorf("no Warn for the abandoned pass:\n%s", out)
	}
}

// TestBuildServerConfigCleanupStopsTheSweep pins the daemon's teardown order:
// the cleanup buildServerConfig returns ends the sweep it started.
func TestBuildServerConfigCleanupStopsTheSweep(t *testing.T) {
	base := t.TempDir()
	cfg, path := mountConfig(t, base, []config.Root{{Name: "A", Path: makeRoot(t, base, "A", "a.wav")}})
	var exited <-chan struct{}
	sweepStarted = func(ch <-chan struct{}) { exited = ch }
	t.Cleanup(func() { sweepStarted = nil })
	_, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", slog.New(slog.DiscardHandler), Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	if exited == nil {
		cleanup()
		t.Fatal("buildServerConfig started no remount sweep")
	}
	select {
	case <-exited:
		cleanup()
		t.Fatal("the sweep ended before cleanup")
	default:
	}
	cleanup()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("cleanup left the sweep running")
	}
}

// TestReloadDoesNotRelogADownRoot pins the alert noise a long-down root would
// make: it is logged when it goes down, and a reload that leaves it down for
// the same reason logs nothing about it.
func TestReloadDoesNotRelogADownRoot(t *testing.T) {
	base := t.TempDir()
	dirA := makeRoot(t, base, "A", "a.wav")
	roots := []config.Root{{Name: "A", Path: dirA}, {Name: "gone", Path: filepath.Join(base, "gone")}}
	cfg, path := mountConfig(t, base, roots)
	var logs syncLog
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", slog.New(slog.NewTextHandler(&logs, nil)), Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for range 3 {
		if _, err := srvCfg.ReloadRoots(); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), `msg="root not mounted" root=gone`); n != 1 {
		t.Fatalf("gone logged %d times over start and three reloads, want once:\n%s", n, logs.String())
	}
}

// TestRequestMountIsLogged pins that a mount a request finds first is logged
// like the sweep's: the daemon's log is the record of every change.
func TestRequestMountIsLogged(t *testing.T) {
	base := t.TempDir()
	later := filepath.Join(base, "later")
	cfg, path := mountConfig(t, base, []config.Root{{Name: "later", Path: later}})
	var logs syncLog
	srvCfg, cleanup, err := buildServerConfig(context.Background(), cfg, path, "test", slog.New(slog.NewTextHandler(&logs, nil)), Flavor{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	makeRoot(t, base, "later", "a.wav")
	f, err := srvCfg.Resolver.Resolve(context.Background(), "later/a.wav")
	if err != nil {
		t.Fatalf("resolve once the folder exists = %v", err)
	}
	f.Close()
	if out := logs.String(); !strings.Contains(out, `level=INFO msg="root mounted" root=later`) {
		t.Fatalf("the request's mount was not logged:\n%s", out)
	}
}

// TestSignNamesAnUnmountedRootsPath pins what a terminal adds over the
// daemon's 503: root-unavailable names the root alone, since a client must not
// see the path, so sign appends the configured path an operator needs.
func TestSignNamesAnUnmountedRootsPath(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	t.Setenv("WAXFLOW_ROOTS", "lib="+gone)
	t.Setenv("WAXFLOW_DATA_DIR", t.TempDir())
	unsetenv(t, "WAXFLOW_CATALOG_DB")
	code, _, stderr := run(t, "sign", "--src", "lib/a.flac")
	if want := waxerr.CodeRootUnavailable.ExitCode(); code != want {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, want, stderr)
	}
	if !strings.Contains(stderr, gone) {
		t.Errorf("stderr %q does not name the configured path", stderr)
	}
}

// TestSignNamesAnUnmountedRootsPathForAPID: the hint survives a pid
// reference, whose root name is the lookup host's answer, not the ref.
func TestSignNamesAnUnmountedRootsPathForAPID(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	t.Setenv("WAXFLOW_ROOTS", "lib="+gone)
	t.Setenv("WAXFLOW_DATA_DIR", t.TempDir())
	pidHostEnv(t, map[string]string{lookupPID: "lib/a.flac"})
	code, _, stderr := run(t, "sign", "--src", "pid:"+lookupPID)
	if want := waxerr.CodeRootUnavailable.ExitCode(); code != want {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, want, stderr)
	}
	if !strings.Contains(stderr, gone) {
		t.Errorf("stderr %q does not name the configured path", stderr)
	}
}
