package testutil

import (
	"fmt"
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
// For WMA Lossless the same argument is not a comparison but the whole story:
// FFmpeg decodes that format and cannot write it, so scripts/wmfenc/wmfll.ps1
// is the only encoder in reach and without it the codec has no fixtures at
// all. It is a separate script because Media Foundation cannot write the
// format either; the header there says why.
//
// The same script decodes (WMFDecode), which puts Windows' own DECODER in
// reach as well. That one is not a fixture generator but a second oracle: two
// implementations that agree on a file neither of them wrote is a much
// stronger statement than agreeing with the program that made it.
//
// Windows only, and Windows PowerShell 5.1 only (the WinRT projections the
// script needs are absent from pwsh). Absence escalates under
// WAXFLOW_REQUIRE_WMFENC=1 and never under WAXFLOW_REQUIRE_FFMPEG, which no
// Linux CI could satisfy.

// HaveWMFEnc reports whether Windows' WMA encoder can be driven here.
func HaveWMFEnc(t testing.TB) bool { return haveWMF(t, "wmfenc.ps1") }

// HaveWMFLossless reports whether Windows' WMA Lossless encoder can be driven
// here. It is a separate script and so a separate check: nothing else can
// write that format at all, so a build without it loses the whole codec's
// generated corpus rather than a few cells of a wider one.
//
// The script's presence is not the whole question, since the lossless codec is
// a Windows component that a given build may not carry; that one cannot be
// answered without running it, and the script says so by name when it is
// missing ("this Windows build has no WMA Lossless encoder").
func HaveWMFLossless(t testing.TB) bool { return haveWMF(t, "wmfll.ps1") }

func haveWMF(t testing.TB, script string) bool {
	t.Helper()
	why := ""
	switch {
	case runtime.GOOS != "windows":
		why = "needs Windows"
	case wmfScript(script) == "":
		why = "scripts/wmfenc/" + script + " is missing from the tree"
	default:
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			why = "powershell.exe is not on PATH (Windows PowerShell 5.1, not pwsh)"
		}
	}
	if why == "" {
		return true
	}
	if os.Getenv("WAXFLOW_REQUIRE_WMFENC") == "1" {
		// Named by the script rather than by "Media Foundation": wmfll.ps1
		// documents at length that Media Foundation cannot write its format
		// and goes through the Format SDK instead, so naming it here would
		// point a reader at the wrong thing to install.
		t.Fatalf("Windows' own WMA encoder (scripts/wmfenc/%s) required by "+
			"WAXFLOW_REQUIRE_WMFENC=1 but unavailable: %s", script, why)
	}
	return false
}

// wmfScript locates an encoder script from this source file rather than from
// the test's working directory. Walking up a fixed number of levels finds it
// only for packages within that many of the root and never from a nested
// module, and the failure is silent: the corpus skips green and takes with it
// the only coverage of the reservoir, variable blocks, LSP exponents, noise
// fill and one-sided mid/side.
func wmfScript(name string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "wmfenc", name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// runWMF drives one of the encoder scripts and holds them to the same output
// contract. The exit status alone is not enough: PowerShell can report success
// for a run whose encode never wrote anything, and a caller that only checked
// the status would then decode an empty file as a corpus cell.
func runWMF(t testing.TB, script, what, dst string, args ...string) {
	t.Helper()
	path := wmfScript(script)
	if path == "" {
		t.Fatal("scripts/wmfenc/" + script + " is missing from the tree")
	}
	argv := append([]string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", path}, args...)
	b, err := exec.Command("powershell.exe", argv...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", script, what, err, b)
	}
	if !strings.Contains(string(b), "ok "+dst) {
		t.Fatalf("%s %s did not report success:\n%s", script, what, b)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("%s %s wrote no file: %v", script, what, err)
	}
	if fi.Size() == 0 {
		t.Fatalf("%s %s wrote an empty file", script, what)
	}
}

// Media Foundation subtypes for the WMA family, as scripts/wmfenc/wmfenc.ps1
// takes them: a MediaEncodingSubtypes name where one exists, a {GUID} where it
// does not. Voice has no name, which is the whole reason the script accepts a
// GUID at all.
const (
	SubtypeWMAV2  = "Wma8"
	SubtypeWMAPro = "Wma9"
)

// WMFEncode encodes a WAV to WMA Standard v2 with Windows' own encoder. The
// encoder chooses its own bit rate near the request and ignores it entirely at
// low sample rates, so callers read back what they actually got rather than
// asserting the request.
func WMFEncode(t testing.TB, wav, out string, rate, channels, bitRate int) {
	t.Helper()
	WMFEncodeSubtype(t, wav, out, rate, channels, bitRate, 16, SubtypeWMAV2)
}

// WMFEncodeSubtype is WMFEncode for the rest of the family: SubtypeWMAPro, or
// a {GUID} for a codec Media Foundation names no constant for.
//
// bits is the depth the encoder codes at, and it selects among the formats
// Media Foundation offers rather than describing the source. It has to be
// passed because MediaTranscoder RENEGOTIATES instead of refusing: every WMA
// Pro format above 48 kHz is 24-bit, so a 96 kHz request at 16 bits comes back
// as a 48 kHz file that looks like a successful encode. Callers read back what
// they actually got.
func WMFEncodeSubtype(t testing.TB, wav, out string, rate, channels, bitRate, bits int, subtype string) {
	t.Helper()
	in, dst := wmfPaths(t, wav, out)
	runWMF(t, "wmfenc.ps1", fmt.Sprintf("%s %dHz %dch %d %dbit", subtype, rate, channels, bitRate, bits), dst,
		"-In", in, "-Out", dst,
		"-Rate", strconv.Itoa(rate), "-Channels", strconv.Itoa(channels),
		"-BitRate", strconv.Itoa(bitRate), "-Bits", strconv.Itoa(bits), "-Subtype", subtype)
}

// WMFDecode decodes a .wma to a PCM WAV with WINDOWS' OWN decoder, which is a
// second oracle beside FFmpeg's and an independent one: where the two agree on
// a file neither of them wrote, a disagreement with this decoder is this
// decoder's.
//
// The output keeps the source's rate and channel count, so what comes back is
// the decoder's own answer and not a resampler's. bits is the PCM width; 0
// takes the script's default of 16.
func WMFDecode(t testing.TB, wma, out string, bits int) {
	t.Helper()
	in, dst := wmfPaths(t, wma, out)
	args := []string{"-Decode", "-In", in, "-Out", dst}
	if bits != 0 {
		args = append(args, "-Bits", strconv.Itoa(bits))
	}
	runWMF(t, "wmfenc.ps1", "decode "+filepath.Base(wma), dst, args...)
}

// WMFEncodeLossless encodes a WAV to WMA Lossless (wFormatTag 0x0163) with
// Windows' own encoder, the only one there is: FFmpeg decodes the format and
// cannot write it.
//
// The stream's shape is the WAV's, because for a lossless codec the output IS
// the input, and that is what makes the source PCM the oracle. Windows offers
// only eight shapes, and the script says which; a WAV outside them fails here
// rather than being resampled into range.
func WMFEncodeLossless(t testing.TB, wav, out string) {
	t.Helper()
	in, dst := wmfPaths(t, wav, out)
	runWMF(t, "wmfll.ps1", filepath.Base(wav), dst, "-In", in, "-Out", dst)
}

func wmfPaths(t testing.TB, wav, out string) (string, string) {
	t.Helper()
	in, err := filepath.Abs(wav)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := filepath.Abs(out)
	if err != nil {
		t.Fatal(err)
	}
	return in, dst
}
