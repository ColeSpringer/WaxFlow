package waxflow_test

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/internal/testutil"
)

var updateGoldens = flag.Bool("update", false, "rewrite golden files (make goldens)")

func pcmTrack(f audio.Format, frames int) container.Track {
	return container.Track{Codec: codec.PCM, Fmt: f, Samples: int64(frames), Default: true}
}

// TestGoldenSegments pins the HLS init headers and media segments byte
// for byte: the M13 exit criterion that deterministic-mode segments stay
// byte-identical across regeneration, plus a tripwire for anything that
// would silently change the wire format cached segments stand for
// (encoder bytes, box layout, boundary arithmetic). Regenerate with
// `make goldens` and review the diff.
func TestGoldenSegments(t *testing.T) {
	const frames = 50000 // ~1.04 s at 48 kHz: one full segment plus a tail
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 41)
	e := waxflow.New()

	cases := []struct {
		name       string
		opts       waxflow.TranscodeOptions
		segSamples int
	}{
		{"opus", waxflow.TranscodeOptions{Format: "opus"}, 48000},
		{"flac", waxflow.TranscodeOptions{Format: "flac"}, 45056},
		{"aac", waxflow.TranscodeOptions{Format: "aac"}, 48128},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := e.PlanSegments(pcmTrack(src.Fmt, frames), tt.opts, float64(tt.segSamples)/48000)
			if err != nil {
				t.Fatal(err)
			}
			if plan.SegmentSamples != tt.segSamples {
				t.Fatalf("plan snapped to %d samples, fixture expects %d", plan.SegmentSamples, tt.segSamples)
			}
			init, err := e.InitSegment(plan, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, fmt.Sprintf("hls-%s-init.mp4", tt.name), init)
			segs, _ := collectSegments(t, e, raw, tt.opts, tt.segSamples, 0)
			if int64(len(segs)) != plan.Segments {
				t.Fatalf("%d segments, plan promised %d", len(segs), plan.Segments)
			}
			for _, s := range segs {
				checkGolden(t, fmt.Sprintf("hls-%s-seg-%d.m4s", tt.name, s.Index), s.Data)
			}
		})
	}
}

func checkGolden(t *testing.T, name string, data []byte) {
	t.Helper()
	path := repoPath("testdata", "golden", "hls", name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run `make goldens`): %v", path, err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("output differs from %s (%d vs %d bytes); if intentional, `make goldens` and review", path, len(data), len(want))
	}
}

// TestSegmentsFFprobe validates the produced init header and segments
// against ffmpeg: the concatenation of init plus segments in order is one
// well-formed fMP4 stream ffprobe identifies and ffmpeg decodes, each
// single segment appended to the init is independently decodable, and
// the FLAC path round-trips bit-exact through ffmpeg. Self-skipping;
// WAXFLOW_REQUIRE_FFMPEG=1 escalates in the differential CI job.
func TestSegmentsFFprobe(t *testing.T) {
	testutil.FFmpeg(t)
	const frames = 100000
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 42)
	e := waxflow.New()

	t.Run("opus", func(t *testing.T) {
		opts := waxflow.TranscodeOptions{Format: "opus"}
		plan, err := e.PlanSegments(pcmTrack(src.Fmt, frames), opts, 1)
		if err != nil {
			t.Fatal(err)
		}
		init, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatal(err)
		}
		segs, _ := collectSegments(t, e, raw, opts, plan.SegmentSamples, 0)

		path := writeTemp(t, "stream-opus.mp4", concatSegments(init, segs...))
		info := testutil.FFprobeFile(t, path)
		if info.CodecName != "opus" || info.SampleRate != 48000 || info.Channels != 2 {
			t.Fatalf("ffprobe = %+v", info)
		}
		got := testutil.FFmpegDecodeF32(t, path)
		// The edit list carries the delay and the known length, so the
		// presentation ffmpeg reconstructs is the trimmed one; some ffmpeg
		// versions keep the flushed tail frame, so allow up to one frame
		// beyond.
		if n := len(got) / 2; n < frames || n > frames+960 {
			t.Fatalf("ffmpeg decoded %d frames, want %d..%d", n, frames, frames+960)
		}

		// Every segment is independently decodable after the init header.
		for _, s := range segs {
			segPath := writeTemp(t, "seg-opus.mp4", concatSegments(init, s))
			if got := testutil.FFmpegDecodeF32(t, segPath); len(got) == 0 {
				t.Fatalf("segment %d: ffmpeg decoded nothing", s.Index)
			}
		}
	})

	t.Run("flac", func(t *testing.T) {
		opts := waxflow.TranscodeOptions{Format: "flac"}
		plan, err := e.PlanSegments(pcmTrack(src.Fmt, frames), opts, 1)
		if err != nil {
			t.Fatal(err)
		}
		init, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatal(err)
		}
		segs, _ := collectSegments(t, e, raw, opts, plan.SegmentSamples, 0)
		path := writeTemp(t, "stream-flac.mp4", concatSegments(init, segs...))
		info := testutil.FFprobeFile(t, path)
		if info.CodecName != "flac" || info.SampleRate != 48000 {
			t.Fatalf("ffprobe = %+v", info)
		}
		ref := testutil.FFmpegDecodeS32(t, path)
		if idx := testutil.DiffI32(testutil.Interleave(src), ref); idx != -1 {
			t.Fatalf("ffmpeg decode differs from the source at interleaved sample %d", idx)
		}
	})

	t.Run("aac", func(t *testing.T) {
		opts := waxflow.TranscodeOptions{Format: "aac"}
		plan, err := e.PlanSegments(pcmTrack(src.Fmt, frames), opts, 1)
		if err != nil {
			t.Fatal(err)
		}
		init, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatal(err)
		}
		segs, _ := collectSegments(t, e, raw, opts, plan.SegmentSamples, 0)
		path := writeTemp(t, "stream-aac.mp4", concatSegments(init, segs...))
		info := testutil.FFprobeFile(t, path)
		if info.CodecName != "aac" || info.SampleRate != 48000 || info.Channels != 2 {
			t.Fatalf("ffprobe = %+v", info)
		}
		// The edit list's front trim applies; the tail edit is beyond
		// ffmpeg's fragmented reader, so the decoded run is the trimmed
		// start through the final padded frame.
		got := testutil.FFmpegDecodeF32(t, path)
		if n := len(got) / 2; n < frames || n > frames+1024 {
			t.Fatalf("ffmpeg decoded %d frames, want %d..%d", n, frames, frames+1024)
		}
		// Every segment is independently decodable after the init header.
		for _, s := range segs {
			segPath := writeTemp(t, "seg-aac.mp4", concatSegments(init, s))
			if got := testutil.FFmpegDecodeF32(t, segPath); len(got) == 0 {
				t.Fatalf("segment %d: ffmpeg decoded nothing", s.Index)
			}
		}
	})
}

func concatSegments(init []byte, segs ...mp4.Segment) []byte {
	out := bytes.Clone(init)
	for _, s := range segs {
		out = append(out, s.Data...)
	}
	return out
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
