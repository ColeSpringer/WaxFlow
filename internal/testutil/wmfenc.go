package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Windows' own WMA encoder, reached through Media Foundation's MediaTranscoder
// by scripts/wmfenc/wmfenc.ps1.
//
// It is here because FFmpeg's WMA encoder is a poor oracle for this format and
// the gap is measured rather than suspected: across every corpus cell and on
// tone, low-passed, high-passed, two-tone and near-silent sources, it emits a
// FLAT exponent curve (0 of 1216 exponent decodes had two different band
// values), never sets a noise-fill flag (0 of 2317 high bands), always codes
// both channels (0 one-sided mid/side blocks, even on digital silence), and
// never sets a flags2 bit past the first. Microsoft's encoder sets the bit
// reservoir and variable block lengths on everything, uses LSP-coded exponents
// below 22 kHz, noise-fills, and codes one channel of a mid/side block in most
// blocks -- between them, nearly every path an ffmpeg corpus cannot reach.
//
// Windows only, and Windows PowerShell 5.1 only (the WinRT projections the
// script needs are absent from pwsh). Absence escalates under
// WAXFLOW_REQUIRE_WMFENC=1 and never under WAXFLOW_REQUIRE_FFMPEG, which no
// Linux CI could satisfy.

// HaveWMFEnc reports whether Windows' WMA encoder can be driven here.
func HaveWMFEnc(t testing.TB) bool {
	t.Helper()
	why := ""
	switch {
	case runtime.GOOS != "windows":
		why = "needs Windows"
	case wmfScript() == "":
		why = "scripts/wmfenc/wmfenc.ps1 is missing from the tree"
	default:
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			why = "powershell.exe is not on PATH (Windows PowerShell 5.1, not pwsh)"
		}
	}
	if why == "" {
		return true
	}
	if os.Getenv("WAXFLOW_REQUIRE_WMFENC") == "1" {
		t.Fatalf("Windows' Media Foundation WMA encoder required by WAXFLOW_REQUIRE_WMFENC=1 "+
			"but unavailable: %s", why)
	}
	return false
}

// wmfScript locates the encoder script from this source file rather than from
// the test's working directory. Walking up a fixed number of levels finds it
// only for packages within that many of the root and never from a nested
// module, and the failure is silent: the corpus skips green and takes with it
// the only coverage of the reservoir, variable blocks, LSP exponents, noise
// fill and one-sided mid/side.
func wmfScript() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "wmfenc", "wmfenc.ps1")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// WMFEncode encodes a WAV to WMA Standard v2 with Windows' own encoder. The
// encoder chooses its own bit rate near the request and ignores it entirely at
// low sample rates, so callers read back what they actually got rather than
// asserting the request.
func WMFEncode(t testing.TB, wav, out string, rate, channels, bitRate int) {
	t.Helper()
	script := wmfScript()
	if script == "" {
		t.Fatal("scripts/wmfenc/wmfenc.ps1 is missing from the tree")
	}
	in, err := filepath.Abs(wav)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := filepath.Abs(out)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", script, "-In", in, "-Out", dst,
		"-Rate", strconv.Itoa(rate), "-Channels", strconv.Itoa(channels),
		"-BitRate", strconv.Itoa(bitRate))
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wmfenc %dHz %dch %d: %v\n%s", rate, channels, bitRate, err, b)
	}
	// The exit status alone is not enough: PowerShell can report success for a
	// run whose transcode never wrote anything, and a caller that only checked
	// the status would then decode an empty file as a corpus cell.
	if !strings.Contains(string(b), "ok "+dst) {
		t.Fatalf("wmfenc %dHz %dch %d did not report success:\n%s", rate, channels, bitRate, b)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("wmfenc %dHz %dch %d wrote no file: %v", rate, channels, bitRate, err)
	}
	if fi.Size() == 0 {
		t.Fatalf("wmfenc %dHz %dch %d wrote an empty file", rate, channels, bitRate)
	}
}
