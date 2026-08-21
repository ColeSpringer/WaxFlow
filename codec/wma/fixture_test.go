//go:build !wmatablesgen

package wma_test

// The committed fixture, decoded with no tools installed.
//
// Every other differential in this package funnels through testutil.FFmpeg and
// skips without it, so on a machine with no ffmpeg the only WMA decoding that
// ran was over hand-built streams that have no oracle -- while a real
// ffmpeg-written file sat committed in testdata, used by nothing but a
// FuzzProbe seed. That is the gap docs/quality-gates.md describes the
// committed fixtures as closing: a regression in a band row, a coefficient
// book pair, coefsEnd, the noise ladder or the frame-length rule moves these
// numbers, and nothing else here would catch it toolless.

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/internal/testutil"
)

func fixture(t testing.TB, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCommittedFixtureDecodes is the toolless decode. The digest is a golden:
// it is what makes this catch a change rather than merely an outage, since
// finite-and-not-silent survives most of the ways a decode can go wrong. The
// statistics beside it are there so a failure says what moved.
func TestCommittedFixtureDecodes(t *testing.T) {
	track, pkts := demux(t, fixture(t, "sine-s16.wma"))
	cfg, err := wma.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if !cfg.V2 || cfg.Rate != 44100 || cfg.Channels != 2 {
		t.Fatalf("fixture is %+v; the numbers below are for v2 44.1 kHz stereo", cfg)
	}
	got := decodeAll(t, track, pkts)
	if len(got) == 0 {
		t.Fatal("nothing decoded")
	}

	var sum float64
	var peak float64
	for i, v := range got {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			t.Fatalf("sample %d is %v", i, f)
		}
		sum += f * f
		peak = max(peak, math.Abs(f))
	}
	rms := math.Sqrt(sum / float64(len(got)))

	const (
		wantSamples = 45056
		wantRMS     = 0.06043
		wantPeak    = 0.08918
		wantDigest  = "100f2d220ba98ed9bf247cca867267252625b68ffde855683d9627b58f07a64d"
	)
	if len(got) != wantSamples {
		t.Errorf("%d samples, want %d", len(got), wantSamples)
	}
	if math.Abs(rms-wantRMS) > 1e-4 || math.Abs(peak-wantPeak) > 1e-4 {
		t.Errorf("rms %.5f peak %.5f, want %.5f and %.5f", rms, peak, wantRMS, wantPeak)
	}
	h := sha256.New()
	var b [4]byte
	for _, v := range got {
		bits := math.Float32bits(v)
		b[0], b[1], b[2], b[3] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
		h.Write(b[:])
	}
	if d := hex.EncodeToString(h.Sum(nil)); d != wantDigest {
		t.Errorf("decode digest %s, want %s", d, wantDigest)
	}

	// And the golden is anchored rather than merely recorded: where ffmpeg is
	// installed the same bytes are scored against it here, at the same gate
	// the generated corpus uses. Without that this would pin whatever the
	// decoder happened to produce the day it was written.
	if !testutil.HaveFFmpeg(t) {
		return
	}
	want := testutil.FFmpegDecodeF32NoSIMD(t, fixture(t, "sine-s16.wma"))
	lead := cfg.FrameLen() * cfg.Channels
	if len(got) < lead {
		t.Fatalf("decoded %d samples, fewer than the %d-sample head lead", len(got), lead)
	}
	ours := got[lead:]
	n := min(len(ours), len(want))
	if d := testutil.CompareF32(ours[:n], want[:n]); d.RMS > gateRMS || d.MaxAbs > gateMax {
		t.Errorf("the committed fixture differs from ffmpeg: %v", d)
	}
}
