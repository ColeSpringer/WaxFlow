package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/internal/metrics"
	"github.com/colespringer/waxflow/server"
)

// recordingEnv is a test server whose logger records into the returned
// recorder.
func recordingEnv(t *testing.T, mutate func(*server.Config)) (*testEnv, *server.LogRecorder) {
	t.Helper()
	rec := &server.LogRecorder{}
	env := newTestEnv(t, func(c *server.Config) {
		c.Logger = slog.New(rec)
		if mutate != nil {
			mutate(c)
		}
	})
	return env, rec
}

// gatePipelines holds every progressive pipeline after its first write until
// the returned open runs. It also opens at cleanup, so a test that fails first
// cannot leave a pipeline gated and the server's Close waiting on it.
func gatePipelines(t *testing.T, env *testEnv) (open func()) {
	t.Helper()
	ch := make(chan struct{})
	open = sync.OnceFunc(func() { close(ch) })
	t.Cleanup(open)
	env.srv.GatePipelineDst(func(w io.Writer) io.Writer { return &gatedWriter{w: w, open: ch} })
	return open
}

// gatedWriter passes its first Write and holds every later one until open
// closes. The pipeline is its only writer.
type gatedWriter struct {
	w      io.Writer
	open   <-chan struct{}
	passed bool
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	if g.passed {
		<-g.open
	}
	g.passed = true
	return g.w.Write(p)
}

// writeLongRamp writes a ramp of the given seconds into the library root, long
// enough at segDur=1 for a fetch to land outside a worker's lookahead.
func writeLongRamp(t *testing.T, env *testEnv, name string, seconds int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.root, name), rampWAV(t, 48000, 2, seconds*48000), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hlsSegmentURIs mints a master for params and returns its first variant's
// segment URIs.
func hlsSegmentURIs(t *testing.T, env *testEnv, params map[string]string) []string {
	t.Helper()
	master := readBody(t, keyless(t, env, mintHLS(t, env, params)))
	mediaRef := variantURIs(t, master)[0]
	resp := keyless(t, env, "/hls/"+mediaRef)
	media := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("media playlist = %d: %s", resp.StatusCode, media)
	}
	_, segs, _ := mediaURIs(t, media)
	return segs
}

// TestClientLeavingMidStreamIsNotAWarning: a listener closing the player
// mid-track is routine. The handler is held in its read when the disconnect
// arrives, since the gate stops the pipeline after its first write.
func TestClientLeavingMidStreamIsNotAWarning(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	open := gatePipelines(t, env)

	resp := env.get(t, "/stream?src=lib/album/track.flac&format=mp3", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Close waits for the handler, so its log line is in before the gate opens.
	env.ts.Close()
	open()
	waitForIdle(t, env)

	if w := rec.Warnings(); len(w) != 0 {
		t.Errorf("a client leaving mid-stream logged %v", w)
	}
	if !rec.Has(slog.LevelDebug, "client left mid-stream") {
		t.Error("no Debug record says the client left")
	}
}

// leaveAfterOneByte starts stream, reads one byte of it and leaves, returning
// once the handler has seen its client go. Under gatePipelines the encode is
// still running then.
func leaveAfterOneByte(t *testing.T, env *testEnv, rec *server.LogRecorder, stream string) {
	t.Helper()
	resp := env.get(t, stream, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, "the handler to see its client leave", func() bool { return rec.Has(slog.LevelDebug, "client left mid-stream") })
}

// waitFor polls until done reports true, failing after 10 s.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantCached fails unless stream answers from a completed cache entry.
func wantCached(t *testing.T, env *testEnv, stream string) []byte {
	t.Helper()
	resp := env.get(t, stream, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Accept-Ranges") != "bytes" || resp.ContentLength != int64(len(body)) {
		t.Fatalf("stream = %d, Accept-Ranges %q, Content-Length %d of %d bytes: the encode did not finish into the cache",
			resp.StatusCode, resp.Header.Get("Accept-Ranges"), resp.ContentLength, len(body))
	}
	return body
}

// TestStreamFinishesAfterItsClientLeaves: a live encode runs on after its only
// client leaves and caches what it wrote (read-behind), so the next request is
// a cache hit. It does with no grace, and inside one. The gate holds the
// pipeline until the handler has seen its client go.
func TestStreamFinishesAfterItsClientLeaves(t *testing.T) {
	for _, grace := range []time.Duration{0, time.Minute} {
		t.Run(fmt.Sprintf("grace %v", grace), func(t *testing.T) {
			env, rec := recordingEnv(t, func(c *server.Config) { c.LiveIdleGrace = grace })
			open := gatePipelines(t, env)
			const stream = "/stream?src=lib/ramp.wav&format=mp3"
			leaveAfterOneByte(t, env, rec, stream)
			open()
			waitForIdle(t, env)
			wantCached(t, env, stream)
		})
	}
}

// TestStreamAbandonFreesSlot: an encode whose only client has been gone for
// the grace stops at its next write, on every rung that runs one: its live
// slot comes back, nothing is cached, and it counts and logs as abandoned
// rather than failed. The rung counters say which rung ran.
func TestStreamAbandonFreesSlot(t *testing.T) {
	const grace = 50 * time.Millisecond
	for _, tc := range []struct {
		name   string
		stream func(*testing.T, *testEnv) string
		ran    func(*metrics.Metrics) bool
	}{
		{"transcode", func(*testing.T, *testEnv) string { return "/stream?src=lib/ramp.wav&format=mp3" },
			func(m *metrics.Metrics) bool { return m.Remuxes.Load() == 0 && m.Cuts.Load() == 0 }},
		{"remux", func(*testing.T, *testEnv) string { return "/stream?src=lib/album/track.flac&format=flac&container=mka" },
			func(m *metrics.Metrics) bool { return m.Remuxes.Load() == 1 }},
		{"cut", func(t *testing.T, env *testEnv) string {
			return "/stream?src=" + writeOpusFixture(t, env, "ramp.wav", "ramp.opus") + "&from=48000&to=144000&format=opus"
		}, func(m *metrics.Metrics) bool { return m.Cuts.Load() == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, rec := recordingEnv(t, func(c *server.Config) {
				c.LiveSlots = 1
				c.LiveIdleGrace = grace
			})
			open := gatePipelines(t, env)
			leaveAfterOneByte(t, env, rec, tc.stream(t, env))
			time.Sleep(2 * grace)
			open()
			waitFor(t, "the slot to come back", func() bool {
				release, ok := env.srv.HoldLiveSlot()
				if ok {
					release()
				}
				return ok && env.srv.Metrics().SessionsActive.Load() == 0
			})

			met := env.srv.Metrics()
			if !tc.ran(met) {
				t.Fatalf("the stream did not run on the %s rung (remuxes %d, cuts %d)", tc.name, met.Remuxes.Load(), met.Cuts.Load())
			}
			if n := met.AbandonedLive.Load(); n != 1 {
				t.Errorf("abandoned live sessions = %d, want 1", n)
			}
			if st := cacheStats(t, env); st.Entries != 0 {
				t.Errorf("the abandoned encode left %d cache entries", st.Entries)
			}
			if !rec.Has(slog.LevelInfo, "pipeline abandoned") {
				t.Error("no Info record says the pipeline was abandoned")
			}
			if w := rec.Warnings(); len(w) != 0 {
				t.Errorf("an abandoned encode logged %v", w)
			}
		})
	}
}

// TestStreamReattachWithinGrace: a listener coming back inside the grace joins
// the encode still running, which then finishes into the cache.
func TestStreamReattachWithinGrace(t *testing.T) {
	env, rec := recordingEnv(t, func(c *server.Config) { c.LiveIdleGrace = time.Minute })
	open := gatePipelines(t, env)
	const stream = "/stream?src=lib/ramp.wav&format=wav&rate=24000"
	want := engineReference(t, filepath.Join(env.root, "ramp.wav"), waxflow.TranscodeOptions{Format: "wav", Rate: 24000})
	leaveAfterOneByte(t, env, rec, stream)

	again := env.get(t, stream, nil)
	if again.StatusCode != 200 {
		t.Fatalf("the returning request = %d: %s", again.StatusCode, readBody(t, again))
	}
	open()
	if body := readBody(t, again); !bytes.Equal(body, want) {
		t.Errorf("the returning request read %d bytes, want the %d of the encode", len(body), len(want))
	}
	waitForIdle(t, env)
	if body := wantCached(t, env, stream); !bytes.Equal(body, want) {
		t.Errorf("the cache holds %d bytes, want the %d of the encode", len(body), len(want))
	}
	if n := env.srv.Metrics().SessionsLive.Load(); n != 1 {
		t.Errorf("%d pipelines ran, want 1: the returning request has to join the first", n)
	}
}

// TestStreamRetriesAnAbandonedJoin: a request whose flight found an encode that
// was abandoned before the request attached starts a fresh encode, rather than
// answering the abandonment as an error, and at one live slot: the abandoned
// encode is held in its gate, never unwinding, so only a slot given back at the
// abandon lets the retry run. The after-flight gate holds the request across
// the abandon.
func TestStreamRetriesAnAbandonedJoin(t *testing.T) {
	const grace = time.Second
	env, rec := recordingEnv(t, func(c *server.Config) {
		c.LiveSlots = 1
		c.LiveIdleGrace = grace
	})
	var armed, held atomic.Bool
	gate := make(chan struct{})
	attach := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(attach) // before the server's cleanup, which waits on the request
	env.srv.GateAfterFlight(func() {
		if armed.Load() && held.CompareAndSwap(false, true) {
			<-gate
		}
	})
	// Only the first pipeline is held; the retry's runs free.
	stalled := make(chan struct{})
	open := sync.OnceFunc(func() { close(stalled) })
	t.Cleanup(open)
	var pipelines atomic.Int32
	env.srv.GatePipelineDst(func(w io.Writer) io.Writer {
		if pipelines.Add(1) == 1 {
			return &gatedWriter{w: w, open: stalled}
		}
		return w
	})
	const stream = "/stream?src=lib/ramp.wav&format=wav&rate=24000"
	want := engineReference(t, filepath.Join(env.root, "ramp.wav"), waxflow.TranscodeOptions{Format: "wav", Rate: 24000})
	leaveAfterOneByte(t, env, rec, stream)
	left := time.Now()

	armed.Store(true)
	type result struct {
		status int
		body   []byte
		err    error
	}
	got := make(chan result, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, env.ts.URL+stream, nil)
		if err != nil {
			got <- result{err: err}
			return
		}
		req.Header.Set("X-API-Key", testKey)
		resp, err := env.ts.Client().Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		got <- result{resp.StatusCode, body, err}
	}()
	waitFor(t, "the second request to reach the gate", held.Load)
	if time.Since(left) > grace/2 {
		t.Fatalf("the second request reached its gate %v after the first left, too late to find the encode before its grace ends", time.Since(left))
	}
	time.Sleep(time.Until(left.Add(2 * grace)))
	attach()

	r := <-got
	if r.err != nil || r.status != 200 || !bytes.Equal(r.body, want) {
		t.Fatalf("the second request = %d, %d bytes (%v); want 200 and the %d of the encode: %.200s",
			r.status, len(r.body), r.err, len(want), r.body)
	}
	if n := env.srv.Metrics().SessionsLive.Load(); n != 2 {
		t.Errorf("%d pipelines ran, want 2: the abandoned one and the retry", n)
	}
	open()
	waitForIdle(t, env)
	if n := env.srv.Metrics().AbandonedLive.Load(); n != 1 {
		t.Errorf("abandoned live sessions = %d, want 1", n)
	}
}

// gateEveryWrite holds every progressive pipeline before its first write until
// the returned open runs, and opens at cleanup.
func gateEveryWrite(t *testing.T, env *testEnv) (open func()) {
	t.Helper()
	ch := make(chan struct{})
	open = sync.OnceFunc(func() { close(ch) })
	t.Cleanup(open)
	env.srv.GatePipelineDst(func(w io.Writer) io.Writer { return &gatedWriter{w: w, open: ch, passed: true} })
	return open
}

// TestStreamStalledPipelineGivesItsSlotBack: a pipeline whose reader has gone
// gives its live slot back a grace later even while it is stalled, before its
// first write or between writes (a hung source, a long seek walk), and is
// counted once it unwinds.
func TestStreamStalledPipelineGivesItsSlotBack(t *testing.T) {
	const grace = 50 * time.Millisecond
	for _, tc := range []struct {
		name  string
		leave func(*testing.T, *testEnv, *server.LogRecorder) (open func())
	}{
		{"before its first write", func(t *testing.T, env *testEnv, _ *server.LogRecorder) func() {
			open := gateEveryWrite(t, env)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/stream?src=lib/ramp.wav&format=mp3", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-API-Key", testKey)
			if resp, err := env.ts.Client().Do(req); err == nil {
				resp.Body.Close()
				t.Fatalf("a stream whose every write is held answered %d", resp.StatusCode)
			}
			return open
		}},
		{"between writes", func(t *testing.T, env *testEnv, rec *server.LogRecorder) func() {
			open := gatePipelines(t, env)
			leaveAfterOneByte(t, env, rec, "/stream?src=lib/ramp.wav&format=mp3")
			return open
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, rec := recordingEnv(t, func(c *server.Config) {
				c.LiveSlots = 1
				c.LiveIdleGrace = grace
			})
			open := tc.leave(t, env, rec)
			waitFor(t, "the slot to come back", func() bool {
				release, ok := env.srv.HoldLiveSlot()
				if ok {
					release()
				}
				return ok
			})
			if n := env.srv.Metrics().SessionsActive.Load(); n != 1 {
				t.Fatalf("%d sessions active, want the stalled pipeline still running", n)
			}
			open()
			waitForIdle(t, env)
			if n := env.srv.Metrics().AbandonedLive.Load(); n != 1 {
				t.Errorf("abandoned live sessions = %d, want 1", n)
			}
			if w := rec.Warnings(); len(w) != 0 {
				t.Errorf("a stalled abandoned pipeline logged %v", w)
			}
		})
	}
}

// TestStreamJoinsTheEncodeInFlight: a request for an encode that is already
// running reads that run from its start rather than starting another. The gate
// holds the first pipeline, so the second request cannot arrive in its flight.
func TestStreamJoinsTheEncodeInFlight(t *testing.T) {
	env := newTestEnv(t, nil)
	open := gatePipelines(t, env)
	const stream = "/stream?src=lib/ramp.wav&format=wav&rate=24000"
	want := engineReference(t, filepath.Join(env.root, "ramp.wav"), waxflow.TranscodeOptions{Format: "wav", Rate: 24000})

	first := env.get(t, stream, nil)
	head := make([]byte, 1)
	if _, err := io.ReadFull(first.Body, head); err != nil {
		t.Fatal(err)
	}
	second := env.get(t, stream, nil)
	if second.StatusCode != 200 {
		t.Fatalf("second request = %d: %s", second.StatusCode, readBody(t, second))
	}
	open()
	bodies := [][]byte{append(head, readBody(t, first)...), readBody(t, second)}
	for i, b := range bodies {
		if !bytes.Equal(b, want) {
			t.Errorf("request %d read %d bytes, want the %d of the encode", i, len(b), len(want))
		}
	}
	if n := env.srv.Metrics().SessionsLive.Load(); n != 1 {
		t.Errorf("%d pipelines ran, want 1: the second request has to join the first", n)
	}
}

// holdWorkersBefore holds every variant worker before it publishes segment n,
// until the worker is canceled.
func holdWorkersBefore(env *testEnv, n int64) {
	env.srv.GateHLSPublish(func(ctx context.Context, i int64) {
		if i == n {
			<-ctx.Done()
		}
	})
}

// restartFarAhead fetches segment 0 of a 12 s ramp and then segment 10, far
// outside the first worker's lookahead. The first worker waits before segment
// 1 until it is canceled, so it is still running, and in place, when the far
// fetch arrives; it returns the number of workers that ran.
func restartFarAhead(t *testing.T, env *testEnv) uint64 {
	t.Helper()
	holdWorkersBefore(env, 1)
	writeLongRamp(t, env, "long.wav", 12)
	segs := hlsSegmentURIs(t, env, map[string]string{"src": "lib/long.wav", "format": "opus", "segDur": "1"})
	for _, n := range []int{0, 10} {
		resp := keyless(t, env, "/hls/"+segs[n])
		if body := readBody(t, resp); resp.StatusCode != 200 {
			t.Fatalf("segment %d = %d: %s", n, resp.StatusCode, body)
		}
	}
	waitForIdle(t, env)
	return env.srv.Metrics().SessionsHLS.Load()
}

// TestHLSRestartIsNotAWarning: a far seek cancels the running variant worker
// and starts another, which is the restart working, not a worker failing.
func TestHLSRestartIsNotAWarning(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	if got := restartFarAhead(t, env); got != 2 {
		t.Fatalf("%d workers ran, want 2: the far fetch has to restart the first", got)
	}
	if w := rec.Warnings(); len(w) != 0 {
		t.Errorf("a seek restart logged %v", w)
	}
	if !rec.Has(slog.LevelDebug, "hls worker canceled") {
		t.Error("no Debug record says the first worker was canceled")
	}
}

// TestHLSFarSeekServesAtOneSlot: at one live slot a far seek restarts the
// worker and serves, never a 503. The release-before-exit order that keeps it
// so is a race this cannot hit reliably; TestHLSWorkerFreesItsSlotBeforeExit
// pins that order.
func TestHLSFarSeekServesAtOneSlot(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) { c.LiveSlots = 1 })
	if got := restartFarAhead(t, env); got != 2 {
		t.Fatalf("%d workers ran, want 2: the far fetch has to restart the first", got)
	}
}

// TestHLSSegmentWritesNothingToAGoneClient: a segment request whose client has
// gone while it waited on the worker gets nothing written, not a 500 envelope
// sent to a closed connection.
func TestHLSSegmentWritesNothingToAGoneClient(t *testing.T) {
	env := newTestEnv(t, nil)
	segs := hlsSegmentURIs(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus", "segDur": "1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/hls/"+segs[2], nil)
	w := httptest.NewRecorder()
	env.srv.ServeHTTP(w, req)
	if w.Body.Len() != 0 {
		t.Errorf("a request whose client is gone got %d: %s", w.Code, w.Body)
	}
	waitForIdle(t, env)
}

// longRampSegments writes a 12 s ramp and returns its 1 s opus variant's
// segment URIs.
func longRampSegments(t *testing.T, env *testEnv) []string {
	t.Helper()
	writeLongRamp(t, env, "long.wav", 12)
	return hlsSegmentURIs(t, env, map[string]string{"src": "lib/long.wav", "format": "opus", "segDur": "1"})
}

func fetchSegment(t *testing.T, env *testEnv, uri string) {
	t.Helper()
	resp := keyless(t, env, "/hls/"+uri)
	if body := readBody(t, resp); resp.StatusCode != 200 {
		t.Fatalf("segment = %d: %s", resp.StatusCode, body)
	}
}

// TestHLSIdleWorkerCanceled: a variant worker nobody has fetched from for the
// idle grace is canceled: its live slot comes back, and it counts and logs as
// abandoned rather than failed. The gate holds the worker before segment 1, so
// only the idle timeout can end it.
func TestHLSIdleWorkerCanceled(t *testing.T) {
	env, rec := recordingEnv(t, func(c *server.Config) {
		c.LiveSlots = 1
		c.HLSIdleGrace = 50 * time.Millisecond
	})
	holdWorkersBefore(env, 1)
	fetchSegment(t, env, longRampSegments(t, env)[0])
	waitForIdle(t, env)

	if n := env.srv.Metrics().AbandonedHLS.Load(); n != 1 {
		t.Errorf("abandoned hls sessions = %d, want 1", n)
	}
	if release, ok := env.srv.HoldLiveSlot(); ok {
		release()
	} else {
		t.Error("the canceled worker kept its live slot")
	}
	if !rec.Has(slog.LevelInfo, "hls worker abandoned") {
		t.Error("no Info record says the worker was abandoned")
	}
	if w := rec.Warnings(); len(w) != 0 {
		t.Errorf("an idle worker's cancel logged %v", w)
	}
}

// TestHLSStalledIdleWorkerGivesItsSlotBack: an idle worker stalled where its
// cancel cannot reach (the gate ignores its context) gives its live slot back
// at the idle cancel, and is counted once it unwinds.
func TestHLSStalledIdleWorkerGivesItsSlotBack(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) {
		c.LiveSlots = 1
		c.HLSIdleGrace = 50 * time.Millisecond
	})
	stalled := make(chan struct{})
	open := sync.OnceFunc(func() { close(stalled) })
	t.Cleanup(open)
	env.srv.GateHLSPublish(func(_ context.Context, n int64) {
		if n == 1 {
			<-stalled
		}
	})
	fetchSegment(t, env, longRampSegments(t, env)[0])
	waitFor(t, "the slot to come back", func() bool {
		release, ok := env.srv.HoldLiveSlot()
		if ok {
			release()
		}
		return ok
	})
	if n := env.srv.Metrics().SessionsActive.Load(); n != 1 {
		t.Fatalf("%d sessions active, want the stalled worker still running", n)
	}
	open()
	waitForIdle(t, env)
	if n := env.srv.Metrics().AbandonedHLS.Load(); n != 1 {
		t.Errorf("abandoned hls sessions = %d, want 1", n)
	}
}

// TestHLSRestartKeepsTheSlotForTheSeek: a seek restart waits for the worker it
// cancels to leave, and that worker keeps its slot until it does, so the seek
// that is waiting gets it rather than whoever asks first. The worker is held
// where its cancel cannot reach.
func TestHLSRestartKeepsTheSlotForTheSeek(t *testing.T) {
	env := newTestEnv(t, func(c *server.Config) { c.LiveSlots = 1 })
	stalled := make(chan struct{})
	open := sync.OnceFunc(func() { close(stalled) })
	t.Cleanup(open)
	reached, canceled := make(chan struct{}), make(chan struct{})
	env.srv.GateHLSPublish(func(ctx context.Context, n int64) {
		if n == 1 {
			close(reached)
			<-ctx.Done()
			close(canceled)
			<-stalled
		}
	})
	segs := longRampSegments(t, env)
	fetchSegment(t, env, segs[0])
	within(t, reached, "the first worker to reach segment 1")
	seek := make(chan int, 1)
	go func() {
		resp, err := http.Get(env.ts.URL + "/hls/" + segs[10])
		if err != nil {
			seek <- 0
			return
		}
		resp.Body.Close()
		seek <- resp.StatusCode
	}()
	within(t, canceled, "the seek to cancel the first worker")
	if release, ok := env.srv.HoldLiveSlot(); ok {
		release()
		t.Fatal("the canceled worker gave its slot away before it left")
	}
	open()
	select {
	case status := <-seek:
		if status != 200 {
			t.Fatalf("the seek = %d, want 200", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the seek never answered")
	}
	waitForIdle(t, env)
}

// within waits for ch to close, failing after 10 s.
func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestHLSHitsKeepTheWorkerAlive: a fetch served from the cache counts as a
// listener, so a worker whose every fetch is a hit runs on past the grace, and
// stops a grace after the fetches end.
func TestHLSHitsKeepTheWorkerAlive(t *testing.T) {
	const grace = time.Second
	env := newTestEnv(t, func(c *server.Config) { c.HLSIdleGrace = grace })
	holdWorkersBefore(env, 1)
	segs := longRampSegments(t, env)
	fetchSegment(t, env, segs[0]) // the miss that starts the worker
	for start := time.Now(); time.Since(start) < 2*grace; time.Sleep(grace / 20) {
		fetchSegment(t, env, segs[0])
	}
	met := env.srv.Metrics()
	if n, active := met.AbandonedHLS.Load(), met.SessionsActive.Load(); n != 0 || active != 1 {
		t.Fatalf("with every fetch a hit the worker stopped (abandoned %d, active %d)", n, active)
	}
	waitForIdle(t, env)
	if n := met.AbandonedHLS.Load(); n != 1 {
		t.Errorf("abandoned hls sessions = %d after the fetches ended, want 1", n)
	}
}

// TestHLSWorkerEndingAsItIdlesIsNotAbandoned: a worker whose idle timeout
// fires as it publishes its stream's last segment has finished, not been
// abandoned, and is neither counted nor logged as abandoned. Its only fetch
// leaves while the gate holds that publish, so the timeout is what opens it.
func TestHLSWorkerEndingAsItIdlesIsNotAbandoned(t *testing.T) {
	env, rec := recordingEnv(t, func(c *server.Config) { c.HLSIdleGrace = 50 * time.Millisecond })
	segs := hlsSegmentURIs(t, env, map[string]string{"src": "lib/ramp.wav", "format": "opus", "segDur": "1"})
	last := int64(len(segs) - 1)
	reached := make(chan struct{})
	env.srv.GateHLSPublish(func(ctx context.Context, n int64) {
		if n == last {
			close(reached)
			<-ctx.Done()
		}
	})
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	served := make(chan struct{})
	go func() {
		defer close(served)
		env.srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/hls/"+segs[last], nil))
	}()
	select {
	case <-reached:
	case <-served:
		t.Fatal("the segment request ended before its worker reached the last segment")
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never reached the last segment")
	}
	leave()
	<-served
	waitForIdle(t, env)

	if n := env.srv.Metrics().AbandonedHLS.Load(); n != 0 {
		t.Errorf("abandoned hls sessions = %d, want 0: the worker finished its stream", n)
	}
	if rec.Has(slog.LevelInfo, "hls worker abandoned") || !rec.Has(slog.LevelDebug, "hls worker finished") {
		t.Errorf("a worker that finished its stream logged %v", rec.Records())
	}
}
