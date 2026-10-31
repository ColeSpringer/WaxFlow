package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
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

// TestStreamFinishesAfterItsClientLeaves: a live encode runs on after its only
// client leaves and caches what it wrote (read-behind), so the next request is
// a cache hit. The gate holds the pipeline until the handler has seen its
// client go.
func TestStreamFinishesAfterItsClientLeaves(t *testing.T) {
	env, rec := recordingEnv(t, nil)
	open := gatePipelines(t, env)
	const stream = "/stream?src=lib/ramp.wav&format=mp3"

	resp := env.get(t, stream, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for !rec.Has(slog.LevelDebug, "client left mid-stream") {
		if time.Now().After(deadline) {
			t.Fatal("the handler never saw its client leave")
		}
		time.Sleep(5 * time.Millisecond)
	}
	open()
	waitForIdle(t, env)

	resp = env.get(t, stream, nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Accept-Ranges") != "bytes" || resp.ContentLength != int64(len(body)) {
		t.Fatalf("after its client left the stream = %d, Accept-Ranges %q, Content-Length %d of %d bytes: "+
			"the encode did not finish into the cache", resp.StatusCode, resp.Header.Get("Accept-Ranges"),
			resp.ContentLength, len(body))
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

// restartFarAhead fetches segment 0 of a 12 s ramp and then segment 10, far
// outside the first worker's lookahead. The first worker waits before segment
// 1 until it is canceled, so it is still running, and in place, when the far
// fetch arrives; it returns the number of workers that ran.
func restartFarAhead(t *testing.T, env *testEnv) uint64 {
	t.Helper()
	env.srv.GateHLSPublish(func(ctx context.Context, n int64) {
		if n == 1 {
			<-ctx.Done()
		}
	})
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
