package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

func TestDiffI32(t *testing.T) {
	if got := DiffI32([]int32{1, 2, 3}, []int32{1, 2, 3}); got != -1 {
		t.Errorf("equal slices diff at %d, want -1", got)
	}
	if got := DiffI32([]int32{1, 2, 3}, []int32{1, 9, 3}); got != 1 {
		t.Errorf("diff at %d, want 1", got)
	}
	if got := DiffI32([]int32{1, 2}, []int32{1, 2, 3}); got != 2 {
		t.Errorf("length mismatch diff at %d, want 2", got)
	}
}

func TestCompareF32(t *testing.T) {
	d := CompareF32([]float32{0, 1, -1}, []float32{0, 1, -1})
	if d.RMS != 0 || d.MaxAbs != 0 {
		t.Errorf("identical slices: %v", d)
	}
	d = CompareF32([]float32{0, 0}, []float32{0, 0.5})
	if d.MaxAbs != 0.5 || d.MaxAt != 1 {
		t.Errorf("diff = %v, want max 0.5 at 1", d)
	}
	if d := CompareF32([]float32{0}, []float32{0, 0}); d.N != -1 {
		t.Errorf("length mismatch must poison the diff: %v", d)
	}
}

func TestSynthDeterministic(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	a := Noise(f, 256, 42)
	b := Noise(f, 256, 42)
	c := Noise(f, 256, 43)
	defer audio.Put(a)
	defer audio.Put(b)
	defer audio.Put(c)
	if DiffI32(a.ChanI(0), b.ChanI(0)) != -1 {
		t.Error("same seed must synthesize identical noise")
	}
	if DiffI32(a.ChanI(0), c.ChanI(0)) == -1 {
		t.Error("different seeds must differ")
	}
	if a.ChanI(0)[0] != -32768 || a.ChanI(0)[1] != 32767 {
		t.Errorf("noise must pin range extremes, got %d, %d", a.ChanI(0)[0], a.ChanI(0)[1])
	}

	s := Sine(f, 128, 440, 0.9)
	defer audio.Put(s)
	if DiffI32(s.ChanI(0), s.ChanI(1)) == -1 {
		t.Error("sine channels must be phase-offset, not identical")
	}

	r := Ramp(f, 128)
	defer audio.Put(r)
	for i, v := range r.ChanI(1) {
		if v != RampAtI(f, 1, int64(i)) {
			t.Fatalf("ramp[%d] = %d, closed form says %d", i, v, RampAtI(f, 1, int64(i)))
		}
	}
}

func TestInterleaveShiftsLikeFFmpeg(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	b := audio.Get(f, 2)
	defer audio.Put(b)
	b.N = 2
	copy(b.ChanI(0), []int32{1, 2})
	copy(b.ChanI(1), []int32{3, 4})
	got := Interleave(b)
	want := []int32{1 << 16, 3 << 16, 2 << 16, 4 << 16}
	if DiffI32(got, want) != -1 {
		t.Errorf("Interleave = %v, want %v", got, want)
	}
}

func TestFetchVerifiesDigests(t *testing.T) {
	payload := []byte("vector payload bytes")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	good := Vector{Name: "suite/v1.bin", URL: srv.URL + "/v1", SHA256: hex.EncodeToString(sum[:])}

	var log strings.Builder
	if err := Fetch(&log, dir, []Vector{good}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "suite", "v1.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("fetched payload = %q, %v", got, err)
	}
	// Second run hits the cache.
	log.Reset()
	if err := Fetch(&log, dir, []Vector{good}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "cached") {
		t.Errorf("second fetch must report a cache hit:\n%s", log.String())
	}

	bad := good
	bad.Name = "suite/v2.bin"
	bad.SHA256 = strings.Repeat("0", 64)
	if err := Fetch(&log, dir, []Vector{bad}); err == nil {
		t.Error("digest mismatch must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "suite", "v2.bin")); err == nil {
		t.Error("mismatched download must not be kept")
	}

	gone := good
	gone.Name = "suite/v3.bin"
	gone.URL = srv.URL + "/missing"
	if err := Fetch(&log, dir, []Vector{gone}); err == nil {
		t.Error("404 must fail")
	}
}

// TestOracleAgainstKnownFile exercises the ffmpeg helpers themselves:
// ffmpeg synthesizes a WAV, and decode plus probe of that same file must
// be self-consistent. Skips without ffmpeg; the differential CI job
// requires it.
func TestOracleAgainstKnownFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oracle.wav")
	FFmpegGenerate(t, path, 44100, 2, "pcm_s16le")
	info := FFprobeFile(t, path)
	if info.CodecName != "pcm_s16le" || info.SampleRate != 44100 || info.Channels != 2 || info.BitsPerSample != 16 {
		t.Errorf("ffprobe info = %+v", info)
	}
	samples := FFmpegDecodeS32(t, path)
	if info.Samples > 0 && int64(len(samples)) != info.Samples*2 {
		t.Errorf("decoded %d samples, ffprobe says %d frames x 2", len(samples), info.Samples)
	}
	if len(samples) == 0 {
		t.Error("decoded no samples")
	}
}
