package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// The oracle's release number decides whether codec/wmavoice skips the three
// superframes FFmpeg 7.0 and older decode with a narrowed denoise index, so
// the banners every packager writes have to parse. A banner with no release in
// it must report ok false, which is what makes an unknown build take the tight
// gate rather than the widened one.
func TestParseFFmpegRelease(t *testing.T) {
	for _, c := range []struct {
		banner       string
		major, minor int
		ok           bool
	}{
		{"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers", 6, 1, true},
		{"ffmpeg version 8.0.1-3ubuntu2 Copyright (c) 2000-2025 the FFmpeg developers", 8, 0, true},
		{"ffmpeg version 7.1 Copyright (c) 2000-2024 the FFmpeg developers", 7, 1, true},
		{"ffmpeg version n7.1.1-6-g123456 Copyright (c) 2000-2024 the FFmpeg developers", 7, 1, true},
		{"ffmpeg version 4.4.2-0ubuntu0.22.04.1 Copyright (c) 2000-2021 the FFmpeg developers", 4, 4, true},
		{"ffmpeg version 9.0-full_build-www.gyan.dev Copyright (c) 2000-2025 the FFmpeg developers", 9, 0, true},
		{"ffmpeg version N-119999-g1234567 Copyright (c) 2000-2026 the FFmpeg developers", 0, 0, false},
		{"ffmpeg version", 0, 0, false},
	} {
		major, minor, ok := parseFFmpegRelease(c.banner)
		if major != c.major || minor != c.minor || ok != c.ok {
			t.Errorf("%q parsed as %d.%d (ok %v), want %d.%d (ok %v)",
				c.banner, major, minor, ok, c.major, c.minor, c.ok)
		}
	}
}

// TestFetchRetriesTransientFailures pins what a pinned host dropping a
// connection mid-body costs: a repeated attempt, not the whole run. A
// failure a second attempt cannot fix is settled on the first.
func TestFetchRetriesTransientFailures(t *testing.T) {
	defer func(d time.Duration) { fetchRetryDelay = d }(fetchRetryDelay)
	fetchRetryDelay = time.Millisecond

	payload := []byte("vector payload bytes")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	var mu sync.Mutex
	attempts := map[string]int{}
	count := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return attempts[path]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts[r.URL.Path]++
		n := attempts[r.URL.Path]
		mu.Unlock()
		switch r.URL.Path {
		case "/busy":
			http.Error(w, "slow down", http.StatusServiceUnavailable)
		case "/gone":
			http.NotFound(w, r)
		case "/unimplemented":
			http.Error(w, "no", http.StatusNotImplemented)
		case "/flaky":
			if n <= 2 {
				// A header, a few bytes, then the connection goes away:
				// what a host that resets mid-body looks like from here.
				conn, bufrw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(payload))
				bufrw.Write(payload[:5])
				bufrw.Flush()
				conn.Close()
				return
			}
			w.Write(payload)
		default:
			w.Write(payload)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	var log strings.Builder
	flaky := Vector{Name: "suite/flaky.bin", URL: srv.URL + "/flaky", SHA256: digest}
	if err := Fetch(&log, dir, []Vector{flaky}); err != nil {
		t.Fatalf("Fetch through two dropped connections: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "suite", "flaky.bin")); err != nil || string(got) != string(payload) {
		t.Fatalf("fetched payload = %q, %v", got, err)
	}
	if got := count("/flaky"); got != 3 {
		t.Errorf("dropped connection took %d attempts, want 3", got)
	}
	if !strings.Contains(log.String(), "retry") {
		t.Errorf("a repeated attempt must be reported:\n%s", log.String())
	}

	gone := Vector{Name: "suite/gone.bin", URL: srv.URL + "/gone", SHA256: digest}
	if err := Fetch(&log, dir, []Vector{gone}); err == nil {
		t.Error("404 must fail")
	}
	if got := count("/gone"); got != 1 {
		t.Errorf("404 took %d attempts, want 1", got)
	}

	stuck := Vector{Name: "suite/stuck.bin", URL: srv.URL + "/unimplemented", SHA256: digest}
	if err := Fetch(&log, dir, []Vector{stuck}); err == nil {
		t.Error("501 must fail")
	}
	if got := count("/unimplemented"); got != 1 {
		t.Errorf("501 took %d attempts, want 1", got)
	}

	wrong := Vector{Name: "suite/wrong.bin", URL: srv.URL + "/ok", SHA256: strings.Repeat("0", 64)}
	if err := Fetch(&log, dir, []Vector{wrong}); err == nil {
		t.Error("digest mismatch must fail")
	}
	if got := count("/ok"); got != 1 {
		t.Errorf("digest mismatch took %d attempts, want 1", got)
	}

	busy := Vector{Name: "suite/busy.bin", URL: srv.URL + "/busy", SHA256: digest}
	if err := Fetch(&log, dir, []Vector{busy}); err == nil {
		t.Error("a host that stays unavailable must fail")
	}
	if got := count("/busy"); got != fetchAttempts {
		t.Errorf("503 took %d attempts, want %d", got, fetchAttempts)
	}
}

// TestFetchBudgetCoversEveryAttempt pins that the deadline belongs to the
// vector and not to the request: a host slow enough to spend it is not asked
// again, since four fresh tries would outlast the CI job the retry exists to
// keep alive.
func TestFetchBudgetCoversEveryAttempt(t *testing.T) {
	defer func(b time.Duration) { fetchBudget = b }(fetchBudget)
	fetchBudget = 500 * time.Millisecond

	var mu sync.Mutex
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		<-r.Context().Done() // outlast the budget; the client hanging up ends this
	}))
	defer srv.Close()

	slow := Vector{Name: "suite/slow.bin", URL: srv.URL + "/slow", SHA256: strings.Repeat("0", 64)}
	var log strings.Builder
	start := time.Now()
	if err := Fetch(&log, t.TempDir(), []Vector{slow}); err == nil {
		t.Fatal("a host that spends the whole budget must fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("fetch took %s, want the spent budget to end it near %s", elapsed, fetchBudget)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 1 {
		t.Errorf("slow host asked %d times, want 1", asked)
	}
}

// TestFetchHonorsRetryAfter pins that a host naming its own wait is heard,
// and only so far. raw.githubusercontent.com serves 86 of the pinned vectors
// and rate-limits in minutes, which the backoff alone would never wait out;
// a wait past the cap is one the run is better off failing than sitting for.
func TestFetchHonorsRetryAfter(t *testing.T) {
	defer func(d, c time.Duration) { fetchRetryDelay, fetchRetryCap = d, c }(fetchRetryDelay, fetchRetryCap)
	fetchRetryDelay, fetchRetryCap = time.Millisecond, 200*time.Millisecond

	payload := []byte("vector payload bytes")
	sum := sha256.Sum256(payload)

	var mu sync.Mutex
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		n := asked
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	limited := Vector{Name: "suite/limited.bin", URL: srv.URL + "/limited", SHA256: hex.EncodeToString(sum[:])}
	var log strings.Builder
	start := time.Now()
	if err := Fetch(&log, t.TempDir(), []Vector{limited}); err != nil {
		t.Fatalf("Fetch through a rate limit: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < fetchRetryCap {
		t.Errorf("second attempt came after %s, want the host's own wait (capped at %s)", elapsed, fetchRetryCap)
	}
	if elapsed > 5*time.Second {
		t.Errorf("second attempt came after %s, want the 60s Retry-After capped at %s", elapsed, fetchRetryCap)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 2 {
		t.Errorf("rate-limited host asked %d times, want 2", asked)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"soon", 0},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := retryAfter(h); got != c.want {
			t.Errorf("retryAfter(%q) = %s, want %s", c.header, got, c.want)
		}
	}
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	if got := retryAfter(h); got < 59*time.Minute || got > time.Hour {
		t.Errorf("retryAfter(an hour out) = %s, want about an hour", got)
	}
}
