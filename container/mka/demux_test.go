package mka_test

// Differential tests for the Matroska/WebM demuxer against ffmpeg-generated
// fixtures and the ffmpeg decode oracle (testutil skips these when ffmpeg is
// absent; the CI differential job sets WAXFLOW_REQUIRE_FFMPEG=1). Lossless
// codecs (FLAC, PCM) must decode bit-for-bit; lossy codecs (Opus, Vorbis, AAC)
// must match within a loose margin, since the demuxer feeds the same packets
// the decoders were already validated on. The Opus fixtures pin the gapless
// contract: CodecDelay and DiscardPadding trims yield ffmpeg's exact count.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// spec describes one fixture the tests generate with ffmpeg.
type spec struct {
	name     string
	acodec   string
	srcRate  int // sine source rate ffmpeg encodes from
	outRate  int // expected decoded rate (Opus is always 48 kHz)
	channels int
	codec    codec.ID
	lossless bool
	extra    []string
}

var specs = []spec{
	{"opus.webm", "libopus", 48000, 48000, 2, codec.Opus, false, []string{"-b:a", "96k"}},
	{"opus.mka", "libopus", 48000, 48000, 1, codec.Opus, false, []string{"-b:a", "64k"}},
	{"vorbis.webm", "libvorbis", 48000, 48000, 2, codec.Vorbis, false, []string{"-q:a", "5"}},
	{"vorbis.mka", "libvorbis", 44100, 44100, 2, codec.Vorbis, false, []string{"-q:a", "4"}},
	{"flac.mka", "flac", 44100, 44100, 2, codec.FLAC, true, nil},
	{"flac-mono.mka", "flac", 48000, 48000, 1, codec.FLAC, true, nil},
	{"aac.mka", "aac", 44100, 44100, 2, codec.AACLC, false, []string{"-b:a", "128k"}},
	{"pcm16.mka", "pcm_s16le", 44100, 44100, 2, codec.PCM, true, nil},
	{"pcm16be.mka", "pcm_s16be", 44100, 44100, 2, codec.PCM, true, nil},
	{"pcm24.mka", "pcm_s24le", 48000, 48000, 2, codec.PCM, true, nil},
	{"pcmf32.mka", "pcm_f32le", 48000, 48000, 2, codec.PCM, true, nil},
}

// runFFmpeg executes ffmpeg, failing the test on error.
func runFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command(testutil.FFmpeg(t), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// gen encodes a two-second 440 Hz sine into a Matroska/WebM fixture, forcing
// short clusters so seeks and Cues cross cluster boundaries.
func gen(t *testing.T, dir string, s spec) string {
	t.Helper()
	path := filepath.Join(dir, s.name)
	args := []string{"-v", "error", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=2.0", s.srcRate),
		"-ac", strconv.Itoa(s.channels), "-c:a", s.acodec}
	args = append(args, s.extra...)
	args = append(args, "-cluster_time_limit", "500", path)
	runFFmpeg(t, args...)
	return path
}

// decoded is a fully decoded fixture, interleaved for oracle comparison.
type decoded struct {
	track container.Track
	i32   []int32   // populated for integer tracks (left-justified like ffmpeg s32)
	f32   []float32 // populated for float tracks
	count int       // frames
}

// decodeAll opens a fixture through format.Open and reads it to end.
func decodeAll(t *testing.T, path string, strict bool) decoded {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	med, err := format.Open(container.BytesSource(raw), "", &format.Options{Strict: strict})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer med.Close()
	tr := med.Info().Default()
	out := decoded{track: tr}
	tmp := audio.Get(tr.Fmt, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		out.count += tmp.N
		if tr.Fmt.Type == audio.Int {
			out.i32 = append(out.i32, testutil.Interleave(tmp)...)
		} else {
			out.f32 = append(out.f32, testutil.InterleaveF(tmp)...)
		}
	}
	return out
}

// TestDemuxProbe checks probe against ffprobe on the generated corpus.
func TestDemuxProbe(t *testing.T) {
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, s := range specs {
		t.Run(s.name, func(t *testing.T) {
			path := gen(t, dir, s)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := format.Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if info.Container != "mka" {
				t.Errorf("container = %q, want mka", info.Container)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings on a clean fixture: %v", info.Warnings)
			}
			d := info.Default()
			if d.Codec != s.codec {
				t.Errorf("codec = %q, want %q", d.Codec, s.codec)
			}
			if d.Fmt.Rate != s.outRate {
				t.Errorf("rate = %d, want %d", d.Fmt.Rate, s.outRate)
			}
			if d.Fmt.Channels != s.channels {
				t.Errorf("channels = %d, want %d", d.Fmt.Channels, s.channels)
			}
			ref := testutil.FFprobeFile(t, path)
			if d.Fmt.Rate != ref.SampleRate {
				t.Errorf("rate = %d, ffprobe says %d", d.Fmt.Rate, ref.SampleRate)
			}
			if d.Fmt.Channels != ref.Channels {
				t.Errorf("channels = %d, ffprobe says %d", d.Fmt.Channels, ref.Channels)
			}
		})
	}
}

// TestDemuxDecodeDifferential compares our full decode against ffmpeg's.
func TestDemuxDecodeDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, s := range specs {
		t.Run(s.name, func(t *testing.T) {
			path := gen(t, dir, s)
			got := decodeAll(t, path, false)
			switch {
			case s.lossless && got.track.Fmt.Type == audio.Int:
				want := testutil.FFmpegDecodeS32(t, path)
				if len(got.i32) != len(want) {
					t.Fatalf("sample count = %d, ffmpeg = %d", len(got.i32), len(want))
				}
				if idx := testutil.DiffI32(got.i32, want); idx != -1 {
					t.Errorf("lossless decode differs from ffmpeg at interleaved index %d", idx)
				}
			case s.lossless: // float PCM: bit-exact, both sides IEEE
				want := testutil.FFmpegDecodeF32(t, path)
				if len(got.f32) != len(want) {
					t.Fatalf("sample count = %d, ffmpeg = %d", len(got.f32), len(want))
				}
				if d := testutil.CompareF32(got.f32, want); d.MaxAbs != 0 {
					t.Errorf("float PCM decode differs from ffmpeg: %v", d)
				}
			default: // lossy
				// Only a gapless (CodecDelay) track has an authoritative length;
				// otherwise Samples is the advisory millisecond Duration and the
				// untrimmed decode legitimately runs a little past it.
				if got.track.SamplesExact && int64(got.count) != got.track.Samples {
					t.Errorf("decoded %d frames, track declares %d", got.count, got.track.Samples)
				}
				want := testutil.FFmpegDecodeF32(t, path)
				// ffmpeg's decode may or may not trim the track's front CodecDelay
				// (it trims Opus and Vorbis, leaves AAC's untrimmed with a negative
				// start time), so align our trimmed content at either offset.
				d := bestAlign(got.f32, want, s.channels, int(got.track.Delay))
				if tol := lossyTolerance(s.codec); d.RMS > tol {
					t.Errorf("lossy decode differs from ffmpeg: %v (tol %.3g)", d, tol)
				}
			}
		})
	}
}

// bestAlign compares our content against ffmpeg's output at both plausible
// front alignments (no trim, or a CodecDelay trim) and returns the closer fit.
func bestAlign(got, want []float32, channels, delay int) testutil.FloatDiff {
	best := testutil.FloatDiff{RMS: 1e9, MaxAbs: 1e9}
	for _, offFrames := range []int{0, delay} {
		off := offFrames * channels
		if off < 0 || off > len(want) {
			continue
		}
		n := min(len(got), len(want)-off)
		if n <= 0 {
			continue
		}
		if d := testutil.CompareF32(got[:n], want[off:off+n]); d.RMS < best.RMS {
			best = d
		}
	}
	return best
}

// lossyTolerance is the RMS ceiling for a lossy codec's decode-vs-ffmpeg
// differential. These are loose demuxer-level bounds: the decoders were
// validated to tight metrics in their own suites, so a correct demuxer
// lands far inside these, and only mis-framed packets blow past them.
func lossyTolerance(id codec.ID) float64 {
	switch id {
	case codec.Vorbis:
		return 1e-3
	case codec.AACLC:
		return 1e-2
	default: // Opus
		return 5e-2
	}
}

// TestGaplessOpus pins the Opus gapless contract: the decoded length equals
// ffmpeg's, which applies the same CodecDelay (front) and DiscardPadding (end)
// trims, and the track reports an exact length.
func TestGaplessOpus(t *testing.T) {
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, s := range specs {
		if s.codec != codec.Opus {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			path := gen(t, dir, s)
			got := decodeAll(t, path, false)
			if !got.track.SamplesExact {
				t.Errorf("Opus track is not marked SamplesExact")
			}
			if got.track.Delay <= 0 {
				t.Errorf("Opus track has no CodecDelay (Delay = %d)", got.track.Delay)
			}
			if int64(got.count) != got.track.Samples {
				t.Errorf("decoded %d frames, track declares %d", got.count, got.track.Samples)
			}
			want := len(testutil.FFmpegDecodeF32(t, path)) / s.channels
			if got.count != want {
				t.Errorf("decoded %d frames, ffmpeg %d (gapless mismatch)", got.count, want)
			}
		})
	}
}

// TestSeekLossless verifies seeking on the lossless fixtures: the frame-counted
// index lands sample-exact, and the decoded tail matches ffmpeg's from that
// offset bit-for-bit.
func TestSeekLossless(t *testing.T) {
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, s := range specs {
		if !s.lossless {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			path := gen(t, dir, s)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ref := decodeAll(t, path, false)
			isFloat := ref.track.Fmt.Type == audio.Float
			fullI := testutil.FFmpegDecodeS32(t, path)
			fullF := testutil.FFmpegDecodeF32(t, path)
			total := ref.count
			for _, frac := range []float64{0.25, 0.5, 0.8} {
				target := int64(float64(total) * frac)
				med, err := format.Open(container.BytesSource(raw), "", nil)
				if err != nil {
					t.Fatal(err)
				}
				landed, err := med.SeekSample(target)
				if err != nil {
					med.Close()
					t.Fatalf("seek to %d: %v", target, err)
				}
				if landed != target {
					t.Errorf("seek to %d landed at %d (not sample-exact)", target, landed)
				}
				gotI, gotF := readAll(t, med)
				med.Close()
				off := int(target) * s.channels
				if isFloat {
					want := fullF[off:]
					if len(gotF) != len(want) {
						t.Fatalf("after seek to %d: %d samples, want %d", target, len(gotF), len(want))
					}
					if d := testutil.CompareF32(gotF, want); d.MaxAbs != 0 {
						t.Errorf("after seek to %d: decode differs %v", target, d)
					}
				} else {
					want := fullI[off:]
					if len(gotI) != len(want) {
						t.Fatalf("after seek to %d: %d samples, want %d", target, len(gotI), len(want))
					}
					if idx := testutil.DiffI32(gotI, want); idx != -1 {
						t.Errorf("after seek to %d: decode differs at %d", target, idx)
					}
				}
			}
		})
	}
}

// readAll reads a Media to end, returning interleaved samples in whichever
// domain the track uses (the other slice stays nil).
func readAll(t *testing.T, med format.Media) (i32 []int32, f32 []float32) {
	t.Helper()
	tr := med.Info().Default()
	tmp := audio.Get(tr.Fmt, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		if tr.Fmt.Type == audio.Int {
			i32 = append(i32, testutil.Interleave(tmp)...)
		} else {
			f32 = append(f32, testutil.InterleaveF(tmp)...)
		}
	}
	return i32, f32
}

// TestSeekLossy exercises the seek path for the position-exact lossy codecs,
// where the per-codec pre-roll lets the decoder rebuild its state. It compares
// a seek-and-read against our own linear decode from the same offset (past a
// convergence margin), which isolates seek behavior from ffmpeg's codec-delay
// quirks. Opus packets are independently decodable and AAC frames are
// fixed-length, so a seek lands sample-exact once the pre-roll has warmed the
// decoder. Vorbis is excluded: its overlap-add packets re-prime after a
// mid-stream Reset, shifting the post-seek timeline by about a half block, so
// its MKV seek is approximate by construction (the pre-roll still reconverges
// the audio).
func TestSeekLossy(t *testing.T) {
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, s := range specs {
		if s.lossless || s.codec == codec.Vorbis {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			path := gen(t, dir, s)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ref := decodeAll(t, path, false) // our own linear decode
			total := ref.count
			target := int64(total / 2)
			med, err := format.Open(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			landed, err := med.SeekSample(target)
			if err != nil {
				med.Close()
				t.Fatalf("seek to %d: %v", target, err)
			}
			if landed != target {
				t.Errorf("seek to %d landed at %d", target, landed)
			}
			_, gotF := readAll(t, med)
			med.Close()
			// Compare the post-seek tail against our linear decode from target,
			// past a convergence margin.
			const skip = 4096
			lo := skip * s.channels
			from := int(target) * s.channels
			n := min(len(gotF), len(ref.f32)-from)
			if n <= lo {
				t.Fatalf("post-seek decode too short: %d samples", n)
			}
			d := testutil.CompareF32(gotF[lo:n], ref.f32[from+lo:from+n])
			if tol := lossyTolerance(s.codec); d.RMS > tol {
				t.Errorf("post-seek decode diverges from linear decode: %v (tol %.3g)", d, tol)
			}
		})
	}
}

// TestStrictCleanFixtures confirms the generated corpus parses in strict mode
// (no tolerated damage on well-formed ffmpeg output).
func TestStrictCleanFixtures(t *testing.T) {
	testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, s := range specs {
		t.Run(s.name, func(t *testing.T) {
			path := gen(t, dir, s)
			d := decodeAll(t, path, true)
			if d.count == 0 {
				t.Errorf("decoded no frames")
			}
		})
	}
}
