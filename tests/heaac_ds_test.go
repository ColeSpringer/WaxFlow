package waxflow_test

// Downsampled (single-rate) SBR, pinned against the committed fdk fixture
// (codec/aac/testdata/fdk_he_ds.m4a: explicit AOT-5 with the extension
// rate equal to the 48000 core rate, stereo, delay 2529, 144000 samples,
// 1024 samples per AU). The chain runs in the dual-rate QMF domain and
// only the synthesis decimates, so the differential is against ffmpeg's
// own downsampled path.
//
// The fixture's tones carry a slight vibrato on purpose. A pure tone
// parked in one QMF source band makes the SBR transposer's covariance
// matrix near-singular, and the solved prediction filter there is
// precision-DEFINED: ffmpeg's own SIMD and scalar builds disagree with
// each other by ~5e-4 on such content (measured), so no decoder can hold
// this suite's gate against it. Real music modulates; a fixture must too.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

const dsFixtureSamples = 144000

func dsFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "codec", "aac", "testdata", "fdk_he_ds.m4a"))
	if err != nil {
		t.Fatalf("reading the committed downsampled fixture: %v", err)
	}
	return raw
}

// TestHEAACDownsampledIdentity pins the open: he-aac at the single 48000
// rate (not doubled), stereo, exact trims, and no band-limit warning: the
// warned base-layer path this config used to take is gone.
func TestHEAACDownsampledIdentity(t *testing.T) {
	raw := dsFixture(t)
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if tr.Codec != codec.HEAAC {
		t.Errorf("codec = %q, want %q", tr.Codec, codec.HEAAC)
	}
	if tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
		t.Errorf("format = %d Hz %d ch, want 48000/2 (the single rate)", tr.Fmt.Rate, tr.Fmt.Channels)
	}
	if tr.Delay != 2529 || tr.Samples != dsFixtureSamples {
		t.Errorf("gapless = delay %d samples %d, want 2529/%d", tr.Delay, tr.Samples, dsFixtureSamples)
	}
	if wr, ok := demux.(container.Warner); ok {
		for _, w := range wr.Warnings() {
			t.Errorf("unexpected warning on a downsampled-SBR file: %q", w.Msg)
		}
	}
}

// TestHEAACDownsampledDifferential compares our decimated synthesis
// against ffmpeg's downsampled decode within the AAC RMS gate.
func TestHEAACDownsampledDifferential(t *testing.T) {
	raw := dsFixture(t)
	got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer audio.Put(got)
	if got.N != dsFixtureSamples {
		t.Errorf("decoded %d samples, want the gapless %d", got.N, dsFixtureSamples)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ds.m4a")
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	rms, off := alignedRMS(testutil.InterleaveF(got), ref, 2)
	t.Logf("downsampled differential: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("RMS %g exceeds gate %g (offset %d)", rms, aacRMSGate, off)
	}
}

// TestHEAACDownsampledADTSRefusal pins the one HE shape ADTS cannot
// carry: implicit signalling implies the dual rate, so re-framing a
// single-rate stream's units would change their legal meaning. The remux
// rung declines at PLAN time (so the ladder falls through to a transcode
// instead of committing to a rung that cannot serve), and the muxer's
// Begin refuses by name as the backstop for callers that skip planning.
func TestHEAACDownsampledADTSRefusal(t *testing.T) {
	raw := dsFixture(t)
	_, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()

	e := waxflow.New()
	rp, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: "aac", Container: "adts"})
	if err != nil {
		t.Fatalf("the decline must be silent (nil, nil), got error: %v", err)
	}
	if rp != nil {
		t.Fatal("PlanRemux must decline a downsampled-SBR source for adts at plan time")
	}
	var out bytes.Buffer
	if _, err := e.Remux(context.Background(), container.BytesSource(raw), "m4a", &out,
		waxflow.TranscodeOptions{Format: "aac", Container: "adts"}); err == nil {
		t.Fatal("a forced remux of a downsampled-SBR source to adts must not serve")
	}

	mux := adts.NewMuxer(&out)
	err = mux.Begin([]container.Track{track})
	if err == nil {
		t.Fatal("the adts muxer must refuse a downsampled-SBR track")
	}
	if !strings.Contains(err.Error(), "downsampled") {
		t.Errorf("the backstop refusal does not name the cause: %v", err)
	}
}
