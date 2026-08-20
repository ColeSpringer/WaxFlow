package testutil

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// The reference `mac` console tool is the APE decoder's fixture generator and
// its second oracle beside ffmpeg. It is the only APE encoder there is:
// ffmpeg decodes the format but does not write it, and no conformance corpus
// exists, so a decoder is verified against files the reference produced and
// samples we know went in.
//
// Same policy as the flac and wavpack tools: tests self-skip when the binary
// is missing, and WAXFLOW_REQUIRE_MAC=1 (the CI differential job) escalates
// absence to a failure so the suite cannot silently thin out.

// apeToolsVersion is the Monkey's Audio SDK release the console tool is built
// from, matching the pinned source zip in Vectors.
const apeToolsVersion = "mac-13.25"

// APETool locates the reference `mac` console tool. No distribution packages
// it, so `make ape-tools` builds it from the pinned SDK source into
// testdata/tools, the same way the libopus tools are built.
// WAXFLOW_APE_TOOLS overrides the directory; tests self-skip when the binary
// is absent and WAXFLOW_REQUIRE_MAC=1 escalates absence to failure.
func APETool(t testing.TB) string {
	t.Helper()
	for _, dir := range []string{
		os.Getenv("WAXFLOW_APE_TOOLS"),
		filepath.Join(VectorsDir(), "..", "tools", apeToolsVersion),
	} {
		if dir == "" {
			continue
		}
		if path, ok := toolIn(dir, "mac"); ok {
			return path
		}
	}
	if path, err := exec.LookPath("mac"); err == nil {
		return path
	}
	if os.Getenv("WAXFLOW_REQUIRE_MAC") == "1" {
		t.Fatal("mac required by WAXFLOW_REQUIRE_MAC=1 but not found (run `make ape-tools`)")
	}
	t.Skip("mac (Monkey's Audio console) not found (run `make ape-tools`); skipping reference-tool test")
	return ""
}

// HaveAPETool reports whether the reference tool is available, for a test that
// has something to check without it and more to check with it. It follows the
// same policy as APETool: WAXFLOW_REQUIRE_MAC=1 turns absence into a failure
// rather than a quiet loss of coverage.
func HaveAPETool(t testing.TB) bool {
	t.Helper()
	for _, dir := range []string{
		os.Getenv("WAXFLOW_APE_TOOLS"),
		filepath.Join(VectorsDir(), "..", "tools", apeToolsVersion),
	} {
		if dir == "" {
			continue
		}
		if _, ok := toolIn(dir, "mac"); ok {
			return true
		}
	}
	if _, err := exec.LookPath("mac"); err == nil {
		return true
	}
	if os.Getenv("WAXFLOW_REQUIRE_MAC") == "1" {
		t.Fatal("mac required by WAXFLOW_REQUIRE_MAC=1 but not found (run `make ape-tools`)")
	}
	return false
}

// APEEncodeFile runs the reference encoder on a WAV input at the given
// compression level (1000 fast to 5000 insane) and returns the .ape path. The
// output lands beside the input, named for the cell so one source can feed
// several.
func APEEncodeFile(t testing.TB, wavPath, name string, level int) string {
	t.Helper()
	out := filepath.Join(filepath.Dir(wavPath), name)
	t.Cleanup(func() { os.Remove(out) })
	var errOut bytes.Buffer
	cmd := exec.Command(APETool(t), wavPath, out, "-c"+strconv.Itoa(level))
	cmd.Stderr = &errOut
	// The tool writes its progress to stdout and says nothing useful on
	// failure beyond its exit code, so both streams are worth reporting.
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("mac -c%d %s: %v\n%s%s", level, wavPath, err, errOut.String(), stdout)
	}
	return out
}

// APEVerifyFile runs the reference decoder's own verification of a stream,
// which checks every frame against the CRC the encoder stored.
func APEVerifyFile(t testing.TB, path string) {
	t.Helper()
	var errOut bytes.Buffer
	cmd := exec.Command(APETool(t), path, "auto", "-v")
	cmd.Stderr = &errOut
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("mac -v %s: %v\n%s%s", path, err, errOut.String(), stdout)
	}
}

// APEDecodeFile runs the reference decoder on a .ape and returns the WAV it
// writes. It is the second half of the reference-tool gate: mac -v checks a
// stream against its stored MD5 and frame CRCs, which says the bytes are
// intact, while this one says they decode to the audio we meant.
func APEDecodeFile(t testing.TB, apePath string) []byte {
	t.Helper()
	out := apePath + ".decoded.wav"
	t.Cleanup(func() { os.Remove(out) })
	var errOut bytes.Buffer
	cmd := exec.Command(APETool(t), apePath, out, "-d")
	cmd.Stderr = &errOut
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("mac -d %s: %v\n%s%s", apePath, err, errOut.String(), stdout)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// WriteWAV writes interleaved int samples as a canonical PCM WAV, the input
// form every reference encoder here takes. 8-bit rides unsigned, as WAV
// requires; wider depths are little-endian signed.
func WriteWAV(t testing.TB, path string, f audio.Format, samps []int32) {
	t.Helper()
	if f.Type != audio.Int {
		t.Fatalf("WriteWAV: %v is not an int format", f)
	}
	bps := f.BitDepth / 8
	if bps*8 != f.BitDepth || bps < 1 || bps > 4 {
		t.Fatalf("WriteWAV: bit depth %d is not a whole number of bytes", f.BitDepth)
	}
	body := make([]byte, 0, 44+len(samps)*bps)
	body = append(body, make([]byte, 44)...)
	for _, v := range samps {
		if f.BitDepth == 8 {
			body = append(body, byte(v+128))
			continue
		}
		for b := range bps {
			body = append(body, byte(v>>(8*b)))
		}
	}
	data := len(samps) * bps
	copy(body, "RIFF")
	binary.LittleEndian.PutUint32(body[4:], uint32(36+data))
	copy(body[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(body[16:], 16)
	binary.LittleEndian.PutUint16(body[20:], 1)
	binary.LittleEndian.PutUint16(body[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(body[24:], uint32(f.Rate))
	binary.LittleEndian.PutUint32(body[28:], uint32(f.Rate*f.Channels*bps))
	binary.LittleEndian.PutUint16(body[32:], uint16(f.Channels*bps))
	binary.LittleEndian.PutUint16(body[34:], uint16(f.BitDepth))
	copy(body[36:], "data")
	binary.LittleEndian.PutUint32(body[40:], uint32(data))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
