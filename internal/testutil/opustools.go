package testutil

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// opusToolsVersion is the libopus release the reference tools are built
// from, matching the pinned source tarball in Vectors.
const opusToolsVersion = "opus-1.6.1"

// OpusTools locates the reference libopus tools opus_demo and opus_compare,
// the robust encoder-quality oracle: opus_demo decodes bitstreams with the
// reference decoder at deterministic sample positions (no cross-correlation
// alignment), and opus_compare is the RFC 6716 section 6 metric. They are
// built from the pinned libopus source by `make opus-tools` into
// testdata/tools. WAXFLOW_OPUS_TOOLS overrides the directory; tests
// self-skip when the tools are absent and WAXFLOW_REQUIRE_OPUS_TOOLS=1
// escalates absence to failure.
func OpusTools(t testing.TB) (opusDemo, opusCompare string) {
	t.Helper()
	dirs := []string{
		os.Getenv("WAXFLOW_OPUS_TOOLS"),
		filepath.Join(VectorsDir(), "..", "tools", opusToolsVersion),
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		demo := filepath.Join(dir, "opus_demo")
		compare := filepath.Join(dir, "opus_compare")
		if fileExecutable(demo) && fileExecutable(compare) {
			return demo, compare
		}
	}
	if demo, err := exec.LookPath("opus_demo"); err == nil {
		if compare, err := exec.LookPath("opus_compare"); err == nil {
			return demo, compare
		}
	}
	if os.Getenv("WAXFLOW_REQUIRE_OPUS_TOOLS") == "1" {
		t.Fatalf("opus_demo/opus_compare required by WAXFLOW_REQUIRE_OPUS_TOOLS=1 but not found (run `make opus-tools`)")
	}
	t.Skipf("opus_demo/opus_compare not found (run `make opus-tools`); skipping")
	return "", ""
}

func fileExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// WriteOpusBitstream writes packets in the opus_demo bitstream form: a
// 4-byte big-endian payload length, the 4-byte big-endian range coder final
// state, then the payload. ranges carries each packet's Encoder.FinalRange;
// opus_demo verifies the reference decoder reaches the same state on every
// packet and hard-fails the decode on a mismatch, so the file format itself
// carries a cross-implementation integrity check.
func WriteOpusBitstream(path string, packets [][]byte, ranges []uint32) error {
	if len(packets) != len(ranges) {
		return fmt.Errorf("opus bitstream: %d packets but %d final ranges", len(packets), len(ranges))
	}
	var buf []byte
	var hdr [8]byte
	for i, pkt := range packets {
		binary.BigEndian.PutUint32(hdr[0:], uint32(len(pkt)))
		binary.BigEndian.PutUint32(hdr[4:], ranges[i])
		buf = append(buf, hdr[:]...)
		buf = append(buf, pkt...)
	}
	return os.WriteFile(path, buf, 0o644)
}

// OpusDemoDecode decodes an opus_demo bitstream file through the reference
// libopus decoder and returns the interleaved 16-bit output. Decode-only
// opus_demo applies no pre-skip trimming: the output timeline starts at the
// first decoded sample, so the caller trims its encoder's declared pre-skip
// for sample-exact alignment.
func OpusDemoDecode(t testing.TB, opusDemo, bitPath string, rate, channels int) []int16 {
	t.Helper()
	// Not t.TempDir(): that defers cleanup to test end, and a corpus-sweep
	// test makes hundreds of these calls (gigabytes on a tmpfs /tmp).
	dir := mkTempDir(t)
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "dec.sw")
	cmd := exec.Command(opusDemo, "-d", strconv.Itoa(rate), strconv.Itoa(channels), bitPath, out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opus_demo -d: %v\n%s", err, msg)
	}
	return readSW(t, out)
}

// OpusDemoRoundTrip encodes and decodes raw PCM through the reference
// libopus encoder and decoder in one opus_demo run at the given CBR bit
// rate and complexity. Combined-mode opus_demo trims the encoder lookahead
// from the decoded output, so the result is sample-aligned with pcm (and
// shorter by the lookahead at the tail).
func OpusDemoRoundTrip(t testing.TB, opusDemo string, pcm []int16, rate, channels, bps, complexity int) []int16 {
	t.Helper()
	dir := mkTempDir(t)
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.sw")
	out := filepath.Join(dir, "out.sw")
	writeSW(t, in, pcm)
	cmd := exec.Command(opusDemo, "audio", strconv.Itoa(rate), strconv.Itoa(channels),
		strconv.Itoa(bps), "-cbr", "-complexity", strconv.Itoa(complexity), in, out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opus_demo round trip: %v\n%s", err, msg)
	}
	return readSW(t, out)
}

// OpusDemoEncode encodes raw PCM with the reference libopus encoder alone
// (opus_demo -e) at the given CBR bit rate and complexity, returning the raw
// Opus packets. The TOC byte of each packet carries libopus's mode and
// bandwidth decisions, which the speech gate compares against ours.
func OpusDemoEncode(t testing.TB, opusDemo string, pcm []int16, rate, channels, bps, complexity int) [][]byte {
	t.Helper()
	dir := mkTempDir(t)
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.sw")
	out := filepath.Join(dir, "out.bit")
	writeSW(t, in, pcm)
	cmd := exec.Command(opusDemo, "-e", "audio", strconv.Itoa(rate), strconv.Itoa(channels),
		strconv.Itoa(bps), "-cbr", "-complexity", strconv.Itoa(complexity), in, out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opus_demo -e: %v\n%s", err, msg)
	}
	buf, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	var packets [][]byte
	for len(buf) >= 8 {
		n := int(binary.BigEndian.Uint32(buf[0:]))
		buf = buf[8:]
		if n < 0 || n > len(buf) {
			t.Fatalf("opus_demo bitstream: packet length %d exceeds remaining %d bytes", n, len(buf))
		}
		packets = append(packets, append([]byte(nil), buf[:n]...))
		buf = buf[n:]
	}
	return packets
}

var opusCompareErrRE = regexp.MustCompile(`(?i)internal weighted error is ([0-9.eE+-]+)`)

// OpusCompareTool scores test against ref with the reference opus_compare
// binary (the RFC 6716 section 6 quality metric; the validated Go port
// OpusCompare serves the decoder conformance test) and returns the internal
// weighted error plus the quality score Q derived from it. Both inputs are
// interleaved 16-bit PCM at 48 kHz and must hold the same sample count.
// The weighted error is the gate unit (docs/quality-gates.md): Q-point
// deltas do not compare across error depths, and opus_compare prints Q only
// when it is positive anyway, while both the pass and fail paths report the
// error.
func OpusCompareTool(t testing.TB, opusCompare string, ref, test []int16, channels int) (werr, q float64) {
	t.Helper()
	if len(ref) != len(test) {
		t.Fatalf("opus_compare inputs disagree: %d vs %d samples", len(ref), len(test))
	}
	dir := mkTempDir(t)
	defer os.RemoveAll(dir)
	refPath := filepath.Join(dir, "ref.sw")
	testPath := filepath.Join(dir, "test.sw")
	if channels == 1 {
		// opus_compare always reads the reference as stereo pairs and, in
		// mono mode, averages each pair (that is how the RFC ships mono
		// references). Duplicating each sample makes the average the
		// original sample.
		stereoRef := make([]int16, 2*len(ref))
		for i, s := range ref {
			stereoRef[2*i] = s
			stereoRef[2*i+1] = s
		}
		writeSW(t, refPath, stereoRef)
	} else {
		writeSW(t, refPath, ref)
	}
	writeSW(t, testPath, test)
	args := []string{}
	if channels == 2 {
		args = append(args, "-s")
	}
	args = append(args, refPath, testPath)
	// A negative Q exits nonzero by design; only unparseable output is an
	// error.
	msg, _ := exec.Command(opusCompare, args...).CombinedOutput()
	m := opusCompareErrRE.FindSubmatch(msg)
	if m == nil {
		t.Fatalf("opus_compare output not understood:\n%s", msg)
	}
	errVal, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		t.Fatalf("opus_compare error value %q: %v", m[1], err)
	}
	return errVal, 100 * (1 - 0.5*math.Log(1+errVal)/math.Log(1.13))
}

// mkTempDir is a per-call temp dir the caller removes as soon as its files
// are consumed, unlike t.TempDir() whose cleanup waits for test end.
func mkTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "opustools-")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	return dir
}

func writeSW(t testing.TB, path string, pcm []int16) {
	t.Helper()
	buf := make([]byte, 2*len(pcm))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func readSW(t testing.TB, path string) []int16 {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	pcm := make([]int16, len(buf)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
	}
	return pcm
}
