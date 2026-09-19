package waxflow_test

// Fragmented-MP4 demux differential: our demuxer must read the
// fragmented (CMAF) MP4 a third-party muxer writes, not just its own output.
// ffmpeg generates fragmented ALAC and AAC (ftyp+moov(mvex)+moof+mdat); we read
// each through the production facade and compare the decoded PCM to ffmpeg's
// own decode of the same file. ALAC is lossless so the match is bit-exact;
// AAC is lossy so a small tolerance applies. Skips without ffmpeg unless
// WAXFLOW_REQUIRE_FFMPEG=1.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
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

// fragProbe opens a fragmented file through the facade and reports its default
// track plus what the open cost, so a length cell also pins that reading the
// headers did not read the file.
func fragProbe(t *testing.T, e *waxflow.Engine, path string) (container.Track, *testutil.CountingSource, int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	info, err := e.Probe(src, "", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return info.Default(), src, int64(len(raw))
}

// TestFragmentedMP4LengthFromHeaders is the length half of the fragmented
// differential: what a real writer states about its own length, and what we
// read back from it.
//
// The shapes are ffmpeg's, measured. Its default fragmented output carries no
// edit list and zeroes both header durations, so the length is genuinely absent
// and must read as such. +global_sidx writes one index covering every fragment
// and then an mfra, so the index is complete and its sum is the length ffprobe
// prints. +delay_moov adds a priming edit, and the index's sum shrinks by
// exactly that delay, which is what "a sidx is timed on the presentation
// timeline" means: the delay stays in Delay and is never subtracted twice.
// +dash writes one index per fragment, so the first covers a prefix and must
// be ignored rather than believed.
func TestFragmentedMP4LengthFromHeaders(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const rate = 44100
	const base = "+frag_keyframe+empty_moov+default_base_moof"

	for _, tc := range []struct {
		name        string
		flags       string
		wantDelay   int64
		wantSamples int64 // -1 for "no length"; 0 means "round(ffprobe duration)"
	}{
		{"the default fragmented shape states nothing", base, 0, -1},
		{"a global sidx is the length", base + "+global_sidx", 0, 0},
		{"a priming edit beside a global sidx", base + "+delay_moov+global_sidx", 1024, 0},
		{"a per-fragment sidx covers a prefix", "+dash", 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "frag.mp4")
			testutil.FFmpegGenerateDuration(t, path, 6, rate, 1, "aac",
				"-b:a", "128k", "-frag_duration", "1000000", "-movflags", tc.flags)

			track, src, size := fragProbe(t, e, path)
			want := tc.wantSamples
			if want == 0 {
				want = int64(math.Round(testutil.FFprobeFormatDuration(t, path) * rate))
			}
			if track.Samples != want {
				t.Errorf("Samples = %d, want %d", track.Samples, want)
			}
			if track.Delay != tc.wantDelay {
				t.Errorf("Delay = %d, want %d", track.Delay, tc.wantDelay)
			}
			if track.SamplesExact || track.SamplesAdvisory {
				t.Errorf("exact=%v advisory=%v; a fragmented header count is declared, not measured",
					track.SamplesExact, track.SamplesAdvisory)
			}
			if src.Bytes >= size {
				t.Errorf("opening read %d bytes of a %d-byte file; the open reads the head", src.Bytes, size)
			}
		})
	}
}

// TestHybridFragmentedMP4DecodesWhole: `-movflags frag_keyframe` without
// `empty_moov` writes a movie that carries both, and it is one ffmpeg flag
// away from the default. The samples written before the first fragment live in
// the moov's sample table (44 AUs on the AAC fixture here) and the rest live in
// moofs; skipping the table for any movie carrying mvex dropped those samples
// with no warning, which is 45056 frames of a 266240-frame file.
func TestHybridFragmentedMP4DecodesWhole(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()

	t.Run("alac lossless", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hybrid.mp4")
		testutil.FFmpegGenerateDuration(t, path, 2, 44100, 2, "alac",
			"-frag_duration", "500000", "-movflags", "+frag_keyframe")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ours := decodeFacadeS32(t, e, raw)
		ref := testutil.FFmpegDecodeS32(t, path)
		if len(ours) != len(ref) {
			t.Fatalf("decoded %d samples, ffmpeg %d: the moov's own samples are part of the track", len(ours), len(ref))
		}
		if idx := testutil.DiffI32(ref, ours); idx != -1 {
			t.Errorf("our hybrid-ALAC decode differs from ffmpeg at %d", idx)
		}
	})

	t.Run("aac lossy", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hybrid.mp4")
		testutil.FFmpegGenerateDuration(t, path, 6, 44100, 1, "aac",
			"-b:a", "128k", "-frag_duration", "1000000", "-movflags", "+frag_keyframe")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ours := decodeFacadeS32(t, e, raw)
		ref := testutil.FFmpegDecodeS32(t, path)
		if len(ours) != len(ref) {
			t.Errorf("decoded %d samples, ffmpeg %d", len(ours), len(ref))
		}
	})
}

// TestFragmentedLengthWalkedAndNot is a decision stated as a test, not a
// surprise found in one. A fragmented AAC file whose head declares a length
// gives two numbers, and both are right by the length contract:
//
//   - Walked, it delivers the presentation the container describes, which is
//     the truns' sum and what ffprobe prints. That is what a cold /stream, a
//     transcode to a pipe and HLS all get, because each walks first.
//   - Un-walked, it delivers whole access units, which over-produce past the
//     last trun's short duration. A declared count does not cap a decode
//     (format.rawEndFor's rule for a sample table), so a plain open-and-read
//     or a transcode to a seekable file gets that instead.
//
// ffmpeg writes the final AAC sample's trun duration as the remaining
// presentation time rather than a whole AU, which is where the difference
// comes from; the progressive sample table has the same oddity by the same
// rule. Closing it means the reader stating a per-packet trim, which puts mp4
// in front of the copy rungs' mid-trim rules; that is a separate design.
func TestFragmentedLengthWalkedAndNot(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const rate = 44100
	path := filepath.Join(t.TempDir(), "frag.mp4")
	testutil.FFmpegGenerateDuration(t, path, 6, rate, 1, "aac", "-b:a", "128k",
		"-frag_duration", "1000000", "-movflags",
		"+frag_keyframe+empty_moov+default_base_moof+global_sidx")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	read := func(walk bool) int64 {
		t.Helper()
		med, err := e.OpenStream(container.BytesSource(raw), "")
		if err != nil {
			t.Fatal(err)
		}
		defer med.Close()
		if walk {
			if err := format.WalkMedia(med); err != nil {
				t.Fatal(err)
			}
		}
		buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
		defer audio.Put(buf)
		var n int64
		for {
			err := med.ReadChunk(buf)
			if errors.Is(err, io.EOF) {
				return n
			}
			if err != nil {
				t.Fatal(err)
			}
			n += int64(buf.N)
		}
	}

	walked, plain := read(true), read(false)
	presentation := int64(math.Round(testutil.FFprobeFormatDuration(t, path) * rate))
	if walked != presentation {
		t.Errorf("a walked read delivered %d samples, ffprobe's duration is %d", walked, presentation)
	}
	if plain <= walked {
		t.Errorf("an un-walked read delivered %d, not more than the walked %d; this file's last access "+
			"unit over-produces past its trun duration and the two numbers are the point", plain, walked)
	}
	if plain%1024 != 0 {
		t.Errorf("an un-walked read delivered %d samples, not a whole number of 1024-sample AUs", plain)
	}
}

// TestFragmentedTranscodeToAPipeCommitsTheWalkedCount: a destination the muxer
// cannot patch makes the output's header a commitment, so the run confirms the
// source's declared length first. A fragmented movie now answers that question
// (confirmableLength returns its walk), where before it had no length to
// confirm and the WAV carried a streaming placeholder.
func TestFragmentedTranscodeToAPipeCommitsTheWalkedCount(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const rate = 44100
	path := filepath.Join(t.TempDir(), "frag.mp4")
	testutil.FFmpegGenerateDuration(t, path, 6, rate, 1, "aac", "-b:a", "128k",
		"-frag_duration", "1000000", "-movflags",
		"+frag_keyframe+empty_moov+default_base_moof+global_sidx")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	med, err := e.OpenStream(container.BytesSource(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()

	var out bytes.Buffer // a plain Writer: no seek, so the header commits
	res, err := e.TranscodeMedia(context.Background(), med, &out,
		waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatalf("transcode to a pipe: %v", err)
	}
	presentation := int64(math.Round(testutil.FFprobeFormatDuration(t, path) * rate))
	if res.Samples != presentation {
		t.Errorf("wrote %d samples, the walk settles at %d", res.Samples, presentation)
	}
	info, err := e.Probe(container.BytesSource(out.Bytes()), "wav", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Default().Samples; got != presentation {
		t.Errorf("the WAV's header declares %d samples, the run wrote %d", got, presentation)
	}
}
