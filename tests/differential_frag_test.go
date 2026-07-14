package waxflow_test

// Fragmented-MP4 demux differential: our demuxer must read the
// fragmented (CMAF) MP4 a third-party muxer writes, not just its own output.
// ffmpeg generates fragmented ALAC and AAC (ftyp+moov(mvex)+moof+mdat); we read
// each through the production facade and compare the decoded PCM to ffmpeg's
// own decode of the same file. ALAC is lossless so the match is bit-exact;
// AAC is lossy so a small tolerance applies. Skips without ffmpeg unless
// WAXFLOW_REQUIRE_FFMPEG=1.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// decodeFacadeS32 decodes a container file through the engine facade to
// interleaved little-endian-domain int32 samples (16-bit sources left-justified
// to match ffmpeg's s32 output).
func decodeFacadeS32(t *testing.T, e *waxflow.Engine, raw []byte) []int32 {
	t.Helper()
	med, err := e.OpenStream(container.BytesSource(raw), "")
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	var out []int32
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		for i := 0; i < tmp.N; i++ {
			for c := 0; c < f.Channels; c++ {
				if f.Type == audio.Int {
					// Left-justify to the int32 domain ffmpeg decodes into.
					out = append(out, tmp.ChanI(c)[i]<<(32-f.BitDepth))
				} else {
					out = append(out, int32(tmp.ChanF(c)[i]*(1<<31)))
				}
			}
		}
	}
	return out
}

func TestFragmentedMP4DemuxDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const fragFlags = "+frag_keyframe+empty_moov+default_base_moof"

	t.Run("alac lossless", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "frag.mp4")
		testutil.FFmpegGenerate(t, path, 44100, 2, "alac", "-movflags", fragFlags)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ours := decodeFacadeS32(t, e, raw)
		ref := testutil.FFmpegDecodeS32(t, path)
		if len(ours) != len(ref) {
			t.Fatalf("decoded %d samples, ffmpeg %d", len(ours), len(ref))
		}
		if idx := testutil.DiffI32(ref, ours); idx != -1 {
			t.Errorf("our fragmented-ALAC decode differs from ffmpeg at %d", idx)
		}
	})

	t.Run("aac lossy", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "frag.mp4")
		testutil.FFmpegGenerate(t, path, 44100, 2, "aac", "-movflags", fragFlags)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// The demuxer parsed the fragments if we decode a comparable count of
		// samples to ffmpeg (the two AAC decoders differ only slightly, and the
		// gapless trims may differ by a frame).
		ours := decodeFacadeS32(t, e, raw)
		ref := testutil.FFmpegDecodeS32(t, path)
		if d := len(ours) - len(ref); d < -4096 || d > 4096 {
			t.Errorf("decoded %d samples, ffmpeg %d (demuxer likely mis-parsed fragments)", len(ours), len(ref))
		}
		if len(ours) == 0 {
			t.Error("decoded no samples from ffmpeg's fragmented AAC")
		}
	})
}
