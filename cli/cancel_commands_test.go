package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanceledCommandsExit6: a command whose context is canceled exits 6,
// whatever it was waiting on: a daemon's answer, doctor's self-bench, a
// metadata read or a probe that only tunes the answer.
func TestCanceledCommandsExit6(t *testing.T) {
	dir := t.TempDir()
	wav := writeFixture(t, dir, "in.wav", rampWAVBytes(t, 44100, 2, 10_000))
	cfg := writeConfig(t, healthyConfig(t))
	for name, args := range map[string][]string{
		"ping":        {"ping", "--addr", "127.0.0.1:1"},
		"cache stats": {"cache", "stats", "--addr", "127.0.0.1:1"},
		"cache gc":    {"cache", "gc", "--addr", "127.0.0.1:1"},
		"doctor":      {"doctor", "--config", cfg},
		"probe":       {"probe", wav},
		"probe json":  {"probe", "--json", wav},
		"sign":        {"sign", "--config", cfg, "--src", "lib/a.wav"},
		"transcode":   {"transcode", wav, filepath.Join(dir, "out.flac")},
		"split":       {"split", wav, filepath.Join(dir, "pieces"), "--at", "5000"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var out, errOut strings.Builder
			if code := execute(ctx, "test", args, &out, &errOut, Flavor{}); code != 6 {
				t.Errorf("exit = %d, want 6 (canceled); stdout: %s stderr: %s", code, out.String(), errOut.String())
			}
		})
	}
}

// TestDoctorCanceledMidBenchPrintsNoReport: a cancel that lands while doctor
// benches exits 6 and prints nothing, since the report would be partial.
func TestDoctorCanceledMidBenchPrintsNoReport(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var benched []string
	benchHook = func(formatName string) {
		benched = append(benched, formatName)
		if len(benched) == 2 {
			cancel()
		}
	}
	defer func() { benchHook = nil }()
	var out, errOut strings.Builder
	code := execute(ctx, "test", []string{"doctor", "--config", writeConfig(t, healthyConfig(t))}, &out, &errOut, Flavor{})
	if len(benched) != 2 {
		t.Fatalf("benched %q, want both formats", benched)
	}
	if code != 6 || out.Len() != 0 {
		t.Errorf("exit = %d, stdout %q; want 6 and no report; stderr: %s", code, out.String(), errOut.String())
	}
}
