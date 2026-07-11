package server_test

// The M19 hardening harnesses: a mixed-traffic load test, a long
// streaming soak with a goroutine/heap leak watch, and the TTFA/seek
// latency percentiles recorded in the README.
//
// All three run short in the default suite (seconds, a smoke that the
// harness itself works) and escalate through the environment for the
// dedicated nightly soak job and perf boxes:
//
//	WAXFLOW_LOAD_DURATION / WAXFLOW_LOAD_WORKERS   load shape
//	WAXFLOW_SOAK_DURATION                          soak length (e.g. 30m)
//	WAXFLOW_PERF_ITERS                             latency sample count
//	WAXFLOW_PERF=1                                 enforce the p95 targets
//	                                               (300 ms warm / 800 ms cold)

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
)

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d
		}
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// drainStatus performs a keyed GET, drains the body, and returns the
// status code plus the start of the body for error reporting. Errors
// come back as status 0 rather than t.Fatal: this runs on worker
// goroutines, where FailNow is off-contract (its Goexit would only
// stop the worker anyway), and the callers already classify status 0
// as a failure with the text attached.
func drainStatus(t *testing.T, env *testEnv, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, env.ts.URL+path, nil)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("X-API-Key", testKey)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	head := make([]byte, 256)
	n, _ := io.ReadFull(resp.Body, head)
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, string(head[:n])
}

// TestLoadMixedTraffic hammers a live daemon with concurrent mixed
// traffic (live transcodes, per-seek cache entries, probes, direct
// play, HLS) and requires every response to be well-formed: success or
// an honest 503 overloaded envelope, never a 5xx crash or a hung
// request.
func TestLoadMixedTraffic(t *testing.T) {
	env := newTestEnv(t, nil)
	duration := envDuration("WAXFLOW_LOAD_DURATION", 3*time.Second)
	workers := envInt("WAXFLOW_LOAD_WORKERS", 8)

	master := "/hls/master.m3u8?src=lib%2Framp.wav&format=opus"
	paths := []string{
		"/stream?src=lib%2Framp.wav&format=opus",
		"/stream?src=lib%2Framp.wav&format=flac",
		"/stream?src=lib%2Framp.wav&format=opus&t=1.5",
		"/stream?src=lib%2Falbum%2Ftrack.flac&format=auto", // direct play
		"/probe?src=lib%2Falbum%2Ftrack.flac",
		"/caps",
		master,
	}

	var total, overloaded atomic.Int64
	var mu sync.Mutex
	badBodies := map[string]string{} // "status path" -> body head

	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for time.Now().Before(deadline) {
				path := paths[rng.Intn(len(paths))]
				if strings.Contains(path, "t=") {
					// Distinct offsets are distinct cache entries; a
					// small set exercises both cold and warm seeks.
					path = fmt.Sprintf("/stream?src=lib%%2Framp.wav&format=opus&t=%.1f", 0.5*float64(rng.Intn(6)))
				}
				status, head := drainStatus(t, env, path)
				total.Add(1)
				switch {
				case status == http.StatusOK:
				case status == http.StatusServiceUnavailable:
					overloaded.Add(1)
					if !strings.Contains(head, "overloaded") {
						mu.Lock()
						badBodies[fmt.Sprintf("503 %s (no overloaded envelope)", path)] = head
						mu.Unlock()
					}
				default:
					mu.Lock()
					if len(badBodies) < 10 {
						badBodies[fmt.Sprintf("%d %s", status, path)] = head
					}
					mu.Unlock()
				}
			}
		}(int64(w) + 1)
	}
	wg.Wait()

	if len(badBodies) > 0 {
		for k, v := range badBodies {
			t.Errorf("unexpected response %s: %s", k, v)
		}
	}
	t.Logf("load: %d requests over %s with %d workers (%.0f req/s), %d honest 503s",
		total.Load(), duration, workers,
		float64(total.Load())/duration.Seconds(), overloaded.Load())
}

// TestStreamingSoak runs continuous streaming traffic (full reads,
// mid-body client abandons, seeks, HLS segment fetches) for the
// configured duration while watching goroutine and heap trajectories,
// then requires the daemon to return to its idle baseline: leaked
// goroutines and monotonic heap growth are the two production failure
// modes codec correctness tests cannot catch.
func TestStreamingSoak(t *testing.T) {
	env := newTestEnv(t, nil)
	duration := envDuration("WAXFLOW_SOAK_DURATION", 5*time.Second)

	// Warm up one full request so lazily initialized machinery (plan
	// cache, cache index) is in the baseline.
	if status, head := drainStatus(t, env, "/stream?src=lib%2Framp.wav&format=opus"); status != http.StatusOK {
		t.Fatalf("warmup = %d: %s", status, head)
	}
	waitForIdle(t, env)
	env.ts.Client().CloseIdleConnections() // symmetric with the final count
	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	baselineHeap := mem.HeapAlloc

	stop := make(chan struct{})
	time.AfterFunc(duration, func() { close(stop) })
	var requests atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-stop:
					return
				default:
				}
				requests.Add(1)
				switch rng.Intn(4) {
				case 0: // full read
					drainStatus(t, env, "/stream?src=lib%2Framp.wav&format=opus")
				case 1: // client abandons mid-body (the disconnect path)
					req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/stream?src=lib%2Framp.wav&format=flac", nil)
					req.Header.Set("X-API-Key", testKey)
					if resp, err := env.ts.Client().Do(req); err == nil {
						io.ReadFull(resp.Body, make([]byte, 512))
						resp.Body.Close()
					}
				case 2: // seek entry
					drainStatus(t, env, fmt.Sprintf("/stream?src=lib%%2Framp.wav&format=opus&t=%.1f", 0.5*float64(rng.Intn(6))))
				case 3: // HLS master + first media playlist
					drainStatus(t, env, "/hls/master.m3u8?src=lib%2Framp.wav&format=opus")
				}
			}
		}(int64(w) + 100)
	}

	// Sample trajectories while the load runs.
	type sample struct {
		goroutines int
		heap       uint64
	}
	var samples []sample
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
sampling:
	for {
		select {
		case <-stop:
			break sampling
		case <-ticker.C:
			runtime.ReadMemStats(&mem)
			samples = append(samples, sample{runtime.NumGoroutine(), mem.HeapAlloc})
		}
	}
	wg.Wait()
	waitForIdle(t, env)
	// Drop the client transport's idle connections before counting:
	// each idle conn keeps two client goroutines and a server handler
	// alive for the transport's idle timeout, and the leak watch is
	// about the server, not the test client's pool.
	env.ts.Client().CloseIdleConnections()

	// Goroutines must return to baseline (small slack for runtime and
	// http idle-conn machinery). Poll: session teardown is asynchronous.
	deadline := time.Now().Add(15 * time.Second)
	var now int
	for {
		runtime.GC()
		now = runtime.NumGoroutine()
		if now <= baselineGoroutines+8 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if now > baselineGoroutines+8 {
		var buf strings.Builder
		pprof.Lookup("goroutine").WriteTo(&buf, 1)
		t.Errorf("goroutines leaked: baseline %d, after soak %d\n%s", baselineGoroutines, now, buf.String())
	}

	runtime.ReadMemStats(&mem)
	finalHeap := mem.HeapAlloc
	bound := max(3*baselineHeap, baselineHeap+64<<20)
	if finalHeap > bound {
		t.Errorf("heap grew from %d to %d bytes over the soak (bound %d)", baselineHeap, finalHeap, bound)
	}
	t.Logf("soak: %s, %d requests; goroutines %d -> %d (baseline %d); heap %.1f -> %.1f MiB; %d samples",
		duration, requests.Load(), baselineGoroutines, now, baselineGoroutines,
		float64(baselineHeap)/(1<<20), float64(finalHeap)/(1<<20), len(samples))
}

// ttfb measures request start to first body byte.
func ttfb(t *testing.T, env *testEnv, path string) time.Duration {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, env.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testKey)
	start := time.Now()
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, one); err != nil {
		t.Fatalf("reading first byte of %s: %v", path, err)
	}
	d := time.Since(start)
	io.Copy(io.Discard, resp.Body)
	return d
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	idx := int(p*float64(len(sorted))+0.5) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// TestTTFAPercentiles measures time-to-first-audio on the three paths
// the §8 targets name: cold (a fresh transcode, which is also what a
// t= seek costs, since every seek offset is its own cache entry) and
// warm (a completed cache entry served with ranges). Results are
// logged; the README records them per release. WAXFLOW_PERF=1 turns
// the targets (p95 warm <300 ms, cold <800 ms) into hard assertions
// for a dedicated perf run.
func TestTTFAPercentiles(t *testing.T) {
	env := newTestEnv(t, nil)
	iters := envInt("WAXFLOW_PERF_ITERS", 5)

	// Cold and seek: each distinct t= is a distinct cache entry, so
	// unique offsets keep every request a fresh pipeline.
	cold := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		cold = append(cold, ttfb(t, env, fmt.Sprintf("/stream?src=lib%%2Framp.wav&format=opus&t=%.3f", 0.001*float64(i+1))))
	}

	// Warm: prime one URL to completion, then re-fetch it.
	warmPath := "/stream?src=lib%2Framp.wav&format=opus"
	if status, head := drainStatus(t, env, warmPath); status != http.StatusOK {
		t.Fatalf("warm prime = %d: %s", status, head)
	}
	waitForIdle(t, env)
	warm := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		warm = append(warm, ttfb(t, env, warmPath))
	}

	// Seek into a long compressed source: FLAC bisection seek plus
	// pipeline start is the realistic worst case a scrubbing player
	// pays, unlike the O(1) WAV seeks above. The fixture is generated
	// here (60 s) so only this test carries its cost.
	longFLAC := filepath.Join(env.root, "long.flac")
	f, err := os.Create(longFLAC)
	if err != nil {
		t.Fatal(err)
	}
	wav := rampWAV(t, 48000, 2, 60*48000)
	if _, err := waxflow.New().Transcode(context.Background(), bytes.NewReader(wav), "wav", f,
		waxflow.TranscodeOptions{Format: "flac"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	seek := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		// Unique fractional offsets: every request is a cold pipeline.
		tt := 5 + 50*rng.Float64()
		seek = append(seek, ttfb(t, env, fmt.Sprintf("/stream?src=lib%%2Flong.flac&format=opus&t=%.3f", tt)))
	}

	coldP50, coldP95 := percentile(cold, 0.50), percentile(cold, 0.95)
	warmP50, warmP95 := percentile(warm, 0.50), percentile(warm, 0.95)
	seekP50, seekP95 := percentile(seek, 0.50), percentile(seek, 0.95)
	t.Logf("TTFA cold: p50 %s p95 %s (n=%d); warm: p50 %s p95 %s (n=%d); seek into 60s flac: p50 %s p95 %s (n=%d)",
		coldP50, coldP95, len(cold), warmP50, warmP95, len(warm), seekP50, seekP95, len(seek))

	if os.Getenv("WAXFLOW_PERF") == "1" {
		if warmP95 >= 300*time.Millisecond {
			t.Errorf("warm TTFA p95 = %s, target <300ms", warmP95)
		}
		if coldP95 >= 800*time.Millisecond {
			t.Errorf("cold TTFA p95 = %s, target <800ms", coldP95)
		}
		if seekP95 >= 800*time.Millisecond {
			t.Errorf("seek TTFA p95 = %s, target <800ms (cold budget)", seekP95)
		}
	}
}
