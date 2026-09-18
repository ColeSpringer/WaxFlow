package waxflow_test

// MP3 decode-stack gates (docs/quality-gates.md): differential vs ffmpeg
// under the RMS 1e-4 and max-abs 1e-3 full-scale thresholds, the LAME
// gapless sample-count invariant, and sample-exact seeking at random
// offsets in VBR streams. Committed fixtures carry the no-oracle tests;
// ffmpeg generates the long streams.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// mp3Fixtures pins every committed MP3 fixture: all LAME-tagged ones
// carry the gapless trims, so samples is the exact source length; the
// untagged stream has no signaled length (-1) and decodes to whole
// frames including the encoder's delay and flush.
var mp3Fixtures = []struct {
	name     string
	rate     int
	channels int
	samples  int64
	rawLen   int64 // expected decoded length; equals samples when trimmed
}{
	{"sine-cbr128.mp3", 44100, 2, 22050, 22050},
	{"sine-vbr.mp3", 44100, 2, 22050, 22050},
	{"sine-mono-cbr64.mp3", 44100, 1, 22050, 22050},
	{"sine-22050-vbr.mp3", 22050, 2, 11025, 11025},
	{"sine-8000-cbr16.mp3", 8000, 1, 4000, 4000},
	{"sine-untagged.mp3", 44100, 2, -1, 24192},
	{"sine-id3.mp3", 44100, 2, 22050, 22050},
	// Broadband at a pretab-engaging bitrate: the scalefactor-sharing
	// regression fixture (preflag with scfsi, energy in the high bands).
	{"noise-cbr64.mp3", 44100, 2, 44100, 44100},
	{"noise-cbr320.mp3", 44100, 2, 22050, 22050},
}

func TestMP3FixturesProbe(t *testing.T) {
	for _, tt := range mp3Fixtures {
		t.Run(tt.name, func(t *testing.T) {
			info, err := waxflow.New().Probe(fixtureSource(t, tt.name), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if info.Container != "mp3" {
				t.Errorf("container = %q, want mp3", info.Container)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings on a clean fixture: %v", info.Warnings)
			}
			d := info.Default()
			if d.Codec != codec.MP3 {
				t.Errorf("codec = %q, want %q", d.Codec, codec.MP3)
			}
			if d.Fmt.Rate != tt.rate || d.Fmt.Channels != tt.channels ||
				d.Fmt.Type != audio.Float || d.Fmt.BitDepth != 32 {
				t.Errorf("format = %v, want %d Hz %d ch float32", d.Fmt, tt.rate, tt.channels)
			}
			if d.Samples != tt.samples {
				t.Errorf("samples = %d, want %d", d.Samples, tt.samples)
			}
		})
	}
}

// TestEveryMP3FixtureWalksWhole is the gate on the encoders that produced
// them: a strict probe walks the run and compares it against the count the
// metadata frame states, so a fixture whose tag is off by one surfaces here
// as a Note rather than as a length nobody checked. Our own muxer counts
// audio frames only, which is the convention the walk expects.
func TestEveryMP3FixtureWalksWhole(t *testing.T) {
	var files []string
	for _, tt := range mp3Fixtures {
		files = append(files, repoPath("testdata", tt.name))
	}
	more, err := filepath.Glob(repoPath("container", "mpa", "testdata", "*.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", &waxflow.ProbeOptions{Strict: true})
			if err != nil {
				t.Fatalf("strict probe: %v", err)
			}
			if d := info.Default(); !d.SamplesExact || d.Samples < 0 {
				t.Errorf("strict probe reports %d samples (exact %v), want a measured length",
					d.Samples, d.SamplesExact)
			}
			if len(info.Notes) != 0 {
				t.Errorf("notes = %v; the encoder's frame count disagrees with the run", info.Notes)
			}
		})
	}
}

// TestMP3DecodeDifferential holds the decoder to the quality gates
// against ffmpeg's float decode, and to the exact output length: for the
// LAME-tagged fixtures both sides apply the same gapless trims, so a
// length mismatch is a trim bug, not a tolerance matter.
func TestMP3DecodeDifferential(t *testing.T) {
	for _, tt := range mp3Fixtures {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeAllDynamic(t, fixtureSource(t, tt.name), "")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(got)
			if int64(got.N) != tt.rawLen {
				t.Fatalf("decoded %d samples, want %d", got.N, tt.rawLen)
			}
			want := testutil.FFmpegDecodeF32(t, repoPath("testdata", tt.name))
			d := testutil.CompareF32(testutil.InterleaveF(got), want)
			if d.N < 0 {
				t.Fatalf("length mismatch vs ffmpeg: ours %d, ffmpeg %d floats", got.N*got.Fmt.Channels, len(want))
			}
			t.Logf("diff: %v", d)
			if d.RMS > 1e-4 {
				t.Errorf("RMS %g exceeds 1e-4", d.RMS)
			}
			if d.MaxAbs > 1e-3 {
				t.Errorf("max abs %g exceeds 1e-3", d.MaxAbs)
			}
		})
	}
}

// TestMP3ProbeAgreesWithFFprobe compares the shape fields only: ffprobe
// reports MP3 duration in the demuxer timebase, not samples, so length
// agreement is pinned against ffmpeg's decoded output above instead.
func TestMP3ProbeAgreesWithFFprobe(t *testing.T) {
	for _, tt := range mp3Fixtures {
		t.Run(tt.name, func(t *testing.T) {
			ref := testutil.FFprobeFile(t, repoPath("testdata", tt.name))
			if ref.CodecName != "mp3" {
				t.Fatalf("ffprobe codec = %q", ref.CodecName)
			}
			if ref.SampleRate != tt.rate || ref.Channels != tt.channels {
				t.Errorf("ffprobe says %d Hz %d ch, fixture pins %d Hz %d ch",
					ref.SampleRate, ref.Channels, tt.rate, tt.channels)
			}
		})
	}
}

// TestMP3LAMEGaplessInvariant is the gapless gate: for every LAME-tagged
// fixture, output_samples == source_samples_after_trim, end to end
// through the engine, and ffmpeg agrees on the count.
func TestMP3LAMEGaplessInvariant(t *testing.T) {
	for _, tt := range mp3Fixtures {
		if tt.samples < 0 {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeAllDynamic(t, fixtureSource(t, tt.name), "")
			if err != nil {
				t.Fatal(err)
			}
			n := int64(got.N)
			audio.Put(got)
			if n != tt.samples {
				t.Errorf("decoded %d samples, invariant wants %d", n, tt.samples)
			}
			ff := testutil.FFmpegDecodeF32(t, repoPath("testdata", tt.name))
			if ffN := int64(len(ff) / tt.channels); ffN != tt.samples {
				t.Errorf("ffmpeg decodes %d samples, invariant wants %d (fixture pin is wrong?)", ffN, tt.samples)
			}
		})
	}
}

// TestMP3FixtureSeekSampleExact runs the shared seek harness over the
// committed fixtures: post-seek chunks bit-identical to a linear decode.
func TestMP3FixtureSeekSampleExact(t *testing.T) {
	for _, tt := range mp3Fixtures {
		t.Run(tt.name, func(t *testing.T) {
			src := fixtureSource(t, tt.name)
			ref, err := decodeAllDynamic(t, src, "")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(src, "")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			seekMatchesReference(t, med, ref, 25, 5)
		})
	}
}

// TestMP3VBRSeek100Offsets is the seek gate: a VBR stream long enough
// that frame sizes vary widely, 100 pseudo-random offsets, each seek
// sample-exact against the linear reference.
func TestMP3VBRSeek100Offsets(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "long-vbr.mp3")
	out, err := exec.Command(ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "anoisesrc=color=pink:sample_rate=44100:duration=30:seed=7",
		"-ac", "2", "-c:a", "libmp3lame", "-q:a", "2", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generating long vbr: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)
	med, err := waxflow.New().OpenStream(src, "")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	seekMatchesReference(t, med, ref, 100, 13)
}

// TestMP3ToleratedDamage exercises the tolerant-parse policy on mangled
// fixtures: junk survives with warnings, strict mode turns them into
// errors, and the decode still runs.
func TestMP3ToleratedDamage(t *testing.T) {
	clean, err := os.ReadFile(repoPath("testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("leading junk", func(t *testing.T) {
		mangled := append([]byte("this is not audio at all, sorry!"), clean...)
		info, err := waxflow.New().Probe(container.BytesSource(mangled), "mp3", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Warnings) == 0 || !strings.Contains(info.Warnings[0], "before the first frame") {
			t.Errorf("warnings = %v, want a leading-junk note", info.Warnings)
		}
		if _, err := waxflow.New().Probe(container.BytesSource(mangled), "mp3", &waxflow.ProbeOptions{Strict: true}); err == nil {
			t.Error("strict mode accepted leading junk")
		}
	})

	t.Run("trailing tag", func(t *testing.T) {
		tag := make([]byte, 128)
		copy(tag, "TAG")
		src := container.BytesSource(append(append([]byte(nil), clean...), tag...))
		got, err := decodeAllDynamic(t, src, "mp3")
		if err != nil {
			t.Fatal(err)
		}
		n := got.N
		audio.Put(got)
		if int64(n) != 24192 {
			t.Errorf("decoded %d samples with an ID3v1 trailer, want 24192", n)
		}
	})

	t.Run("truncated tail", func(t *testing.T) {
		src := container.BytesSource(clean[:len(clean)-100])
		med, err := waxflow.New().OpenStream(src, "mp3")
		if err != nil {
			t.Fatal(err)
		}
		defer med.Close()
		got, err := decodeAllDynamic(t, src, "mp3")
		if err != nil {
			t.Fatal(err)
		}
		n := got.N
		audio.Put(got)
		if n <= 0 || int64(n) >= 24192 {
			t.Errorf("truncated stream decoded %d samples, want a shorter whole-frame count", n)
		}
		if n%1152 != 0 {
			t.Errorf("truncated stream decoded %d samples, not whole frames", n)
		}
	})

	t.Run("mid-stream junk", func(t *testing.T) {
		// Zero a stretch in the middle, which takes at least one frame
		// header with it; the walk warns and resyncs at the next frame.
		// The head is clean, so a tolerant probe sees nothing; a strict
		// probe walks the run and refuses; and an opened media's Info
		// reports the finding only once a read has reached it.
		mangled := testutil.ZeroMiddle(clean, 2048)
		e := waxflow.New()
		info, err := e.Probe(container.BytesSource(mangled), "mp3", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("a tolerant probe reports %v; it reads the head, which is clean", info.Warnings)
		}
		if _, err := e.Probe(container.BytesSource(mangled), "mp3", &waxflow.ProbeOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
			t.Errorf("strict probe error = %v, want malformed", err)
		}
		med, err := e.OpenStream(container.BytesSource(mangled), "mp3")
		if err != nil {
			t.Fatal(err)
		}
		defer med.Close()
		live := med.Info()
		if len(live.Warnings) != 0 {
			t.Errorf("warnings at open = %v, want none", live.Warnings)
		}
		got := drainMedia(t, med, 30000)
		n := got.N
		audio.Put(got)
		if n <= 0 {
			t.Error("mid-stream damage killed the decode entirely")
		}
		// Asked again after the read: the lists are refolded on each Info
		// call, into the same *Info the caller already holds.
		if again := med.Info(); again != live {
			t.Fatal("Info handed out a different *Info after the read")
		}
		if !slices.ContainsFunc(live.Warnings, func(s string) bool { return strings.Contains(s, "unparsable bytes skipped") }) {
			t.Errorf("warnings after the read = %v, want the skipped bytes", live.Warnings)
		}
		// Both write rungs carry the read's verdict on the result: the
		// decode rung through the media, the copy rung through the demuxer
		// it walked.
		for _, rung := range []struct {
			name string
			run  func(dst io.Writer) (*waxflow.TranscodeResult, error)
		}{
			{"decode", func(dst io.Writer) (*waxflow.TranscodeResult, error) {
				return e.Transcode(context.Background(), container.BytesSource(mangled), "mp3", dst, waxflow.TranscodeOptions{Format: "wav"})
			}},
			{"copy", func(dst io.Writer) (*waxflow.TranscodeResult, error) {
				return e.Remux(context.Background(), container.BytesSource(mangled), "mp3", dst, waxflow.TranscodeOptions{Format: "mp3"})
			}},
		} {
			var out bytes.Buffer
			res, err := rung.run(&out)
			if err != nil {
				t.Fatalf("%s rung: %v", rung.name, err)
			}
			if !slices.ContainsFunc(res.InputWarnings, func(s string) bool { return strings.Contains(s, "unparsable bytes skipped") }) {
				t.Errorf("%s rung: input warnings = %v, want the skipped bytes", rung.name, res.InputWarnings)
			}
		}
	})
}

// TestMP3TranscodeToFLAC drives the whole write path from an MP3 source:
// the float track quantizes to 24-bit by the flac row's default and the
// sample count survives.
func TestMP3TranscodeToFLAC(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := waxflow.New().Transcode(t.Context(), container.BytesSource(raw), "", &out, waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 22050 {
		t.Errorf("transcode wrote %d samples, want 22050", res.Samples)
	}
	got, err := decodeAllDynamic(t, container.BytesSource(out.Bytes()), "flac")
	if err != nil {
		t.Fatalf("decoding our own flac: %v", err)
	}
	n := got.N
	audio.Put(got)
	if int64(n) != 22050 {
		t.Errorf("flac round trip has %d samples, want 22050", n)
	}
}

// captureIndexCache is a waxflow.IndexCache that records traffic.
type captureIndexCache struct {
	blobs map[string][]byte
	loads int
	saves int
}

func (c *captureIndexCache) key(src container.Source) string {
	return fmt.Sprint(src.Size()) // enough for a single-source test
}

func (c *captureIndexCache) Load(src container.Source) []byte {
	c.loads++
	return c.blobs[c.key(src)]
}

func (c *captureIndexCache) Save(src container.Source, blob []byte) {
	c.saves++
	c.blobs[c.key(src)] = blob
}

func (c *captureIndexCache) Drop(src container.Source) {
	delete(c.blobs, c.key(src))
}

// TestMP3IndexSidecar drives the sidecar through the engine: the first
// session's seek builds and saves the frame index, the second session
// restores it and seeks to the same sample-exact landing.
func TestMP3IndexSidecar(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := mp3.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	stream := bytes.Repeat(raw[:h.Size()], 5000)
	cache := &captureIndexCache{blobs: map[string][]byte{}}
	eng := waxflow.New(waxflow.WithIndexCache(cache))

	readAt := func(med format.Media, target int64) *audio.Buffer {
		t.Helper()
		if landed, err := med.SeekSample(target); err != nil || landed != target {
			t.Fatalf("seek to %d: landed %d, err %v", target, landed, err)
		}
		dst := audio.Get(med.Info().Default().Fmt, 512)
		if err := med.ReadChunk(dst); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	med, err := eng.OpenStream(container.BytesSource(stream), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := med.(container.Indexer); !ok {
		t.Fatal("engine media hides container.Indexer")
	}
	if _, ok := med.(format.Walker); !ok {
		t.Fatal("engine media hides format.Walker")
	}
	first := readAt(med, 5_000_000)
	defer audio.Put(first)
	med.Close()
	med.Close() // Close is idempotent: no second save
	if cache.saves != 1 {
		t.Fatalf("saves = %d, want 1", cache.saves)
	}

	med2, err := eng.OpenStream(container.BytesSource(stream), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer med2.Close()
	second := readAt(med2, 5_000_000)
	defer audio.Put(second)
	if cache.loads < 2 {
		t.Fatalf("loads = %d, want one per open", cache.loads)
	}
	for c := 0; c < first.Fmt.Channels; c++ {
		a, b := first.ChanF(c), second.ChanF(c)
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("restored-index decode differs at channel %d frame %d", c, i)
			}
		}
	}
	// The second session grew nothing, so it saves nothing new.
	if cache.saves != 1 {
		t.Errorf("saves = %d after a restore-only session, want 1", cache.saves)
	}
}

// TestMP3RejectsFreeFormat pins the documented deviation: free-format
// streams (bit rate index 0) are refused as unsupported, not misparsed.
func TestMP3RejectsFreeFormat(t *testing.T) {
	clean, err := os.ReadFile(repoPath("testdata", "sine-untagged.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	// Clear the bit rate index of every frame, walking the clean stream
	// by header sizes first.
	mangled := append([]byte(nil), clean...)
	for off := 0; off+mp3.HeaderLen <= len(clean); {
		h, err := mp3.ParseHeader(clean[off:])
		if err != nil || h.Size() == 0 {
			break
		}
		mangled[off+2] &= 0x0F
		off += h.Size()
	}
	_, err = waxflow.New().Probe(container.BytesSource(mangled), "mp3", nil)
	if err == nil {
		t.Fatal("free-format stream was accepted")
	}
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Errorf("error = %v, want unsupported-format", err)
	}
}
