package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/internal/cache"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// TestStopReason: a run nobody was left to read, and one that was canceled,
// are routine; everything else is a failure worth a Warn. A canceled run is
// routine whatever code its error surfaced under.
func TestStopReason(t *testing.T) {
	live := context.Background()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	idle, cancelIdle := context.WithCancelCause(context.Background())
	cancelIdle(hls.ErrIdle)
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		level slog.Level
		why   string
	}{
		{"canceled", live, waxerr.Wrap(waxerr.CodeCanceled, "transcode canceled", context.Canceled), slog.LevelDebug, "canceled"},
		{"bare context", live, context.Canceled, slog.LevelDebug, "canceled"},
		{"canceled under another code", gone, waxerr.New(waxerr.CodeSourceUnreadable, "reading a member"),
			slog.LevelDebug, "canceled"},
		{"idle", idle, waxerr.Wrap(waxerr.CodeCanceled, "transcode canceled", context.Canceled), slog.LevelInfo, "abandoned"},
		{"source", live, waxerr.New(waxerr.CodeSourceUnreadable, "reading"), slog.LevelWarn, "failed"},
		{"unclassified", live, errors.New("boom"), slog.LevelWarn, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if level, why := stopReason(tc.ctx, tc.err); level != tc.level || why != tc.why {
				t.Errorf("got %s %q, want %s %q", level, why, tc.level, tc.why)
			}
		})
	}
}

// internalServer is a Server with one live slot over a library holding one
// placeholder (an empty folder does not mount), logging to log (nil discards).
func internalServer(t *testing.T, log *slog.Logger) *Server {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "placeholder.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return internalServerAt(t, root, log)
}

// internalServerAt is internalServer over the library at root.
func internalServerAt(t *testing.T, root string, log *slog.Logger) *Server {
	t.Helper()
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Addr: "127.0.0.1:4418", CacheDir: t.TempDir(), Resolver: roots, LiveSlots: 1, Logger: log})
	if err != nil {
		roots.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		roots.Close()
	})
	return s
}

// missingPipeline is the run for a source that is not there, which fails
// before it writes a byte.
func missingPipeline(s *Server) pipeline {
	return s.sourcePipeline(&streamRequest{p: &streamParams{src: "lib/missing.wav"},
		src: &source.File{Ref: "lib/missing.wav", Ext: "wav"}})
}

// TestPipelineFreesItsSlotBeforeGoingIdle: a pipeline gives its live slot back
// before it stops counting as active, so a caller that sees no active session
// can take the slot. A missing source fails the pipeline at once; the order of
// its exit is the point.
func TestPipelineFreesItsSlotBeforeGoingIdle(t *testing.T) {
	s := internalServer(t, nil)
	free, ok := s.pools.AcquireLive()
	if !ok {
		t.Fatal("no live slot")
	}
	activeAtRelease := make(chan int64, 1)
	s.startPipeline(s.baseCtx, cache.NewMemEntry(0, cache.Meta{}), missingPipeline(s),
		func() { activeAtRelease <- s.met.SessionsActive.Load(); free() }, false)
	if got := <-activeAtRelease; got != 1 {
		t.Errorf("the slot was released with %d sessions active, want 1: the pipeline went idle first", got)
	}
}

// TestOnlyLivePipelinesCountAsAbandoned: a /stream encode stopped for want of
// readers counts as an abandoned live session; a sync one-shot, which dies with
// its request anyway, does not. Each entry's only reader has left, so its ring
// abandons once full.
func TestOnlyLivePipelinesCountAsAbandoned(t *testing.T) {
	s := internalServer(t, nil)
	unread := pipeline{ref: "lib/a.wav", run: func(_ context.Context, dst io.Writer) (*waxflow.TranscodeResult, error) {
		for {
			if _, err := dst.Write(make([]byte, 512)); err != nil {
				return nil, err
			}
		}
	}}
	for _, sync := range []bool{true, false} {
		entry := cache.NewMemEntry(4096, cache.Meta{})
		r, err := entry.NewReader()
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		s.startPipeline(s.baseCtx, entry, unread, func() {}, sync)
	}
	s.wg.Wait()
	if n := s.met.AbandonedLive.Load(); n != 1 {
		t.Errorf("abandoned live sessions = %d, want 1: the sync one-shot is not one", n)
	}
}

// TestAbandonCancelsAStalledRun: a run stalled waiting on its context, its
// reader gone, is canceled a grace later with ErrAbandoned as the cause, counts
// as abandoned, and its entry's temp file goes even though it never wrote
// again.
func TestAbandonCancelsAStalledRun(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "placeholder.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: root}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	s, err := New(Config{Addr: "127.0.0.1:4418", CacheDir: cacheDir, Resolver: roots, LiveSlots: 1,
		LiveIdleGrace: 20 * time.Millisecond})
	if err != nil {
		roots.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		roots.Close()
	})
	entry, err := s.store.Begin(cache.NewKey("lib/a.wav|0", "stalled", nil), cache.Meta{Ext: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	free, ok := s.pools.AcquireLive()
	if !ok {
		t.Fatal("no live slot")
	}
	cause := make(chan error, 1)
	s.startPipeline(s.baseCtx, entry, pipeline{ref: "lib/a.wav", run: func(ctx context.Context, _ io.Writer) (*waxflow.TranscodeResult, error) {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return nil, ctx.Err()
	}}, free, false)
	r, err := entry.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	select {
	case got := <-cause:
		if !errors.Is(got, cache.ErrAbandoned) {
			t.Errorf("the stalled run was canceled by %v, want ErrAbandoned", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandon never canceled the stalled run")
	}
	s.wg.Wait()
	if n := s.met.AbandonedLive.Load(); n != 1 {
		t.Errorf("abandoned live sessions = %d, want 1", n)
	}
	if tmps, _ := filepath.Glob(filepath.Join(cacheDir, "v1", "*", "*", "*.tmp-*")); len(tmps) != 0 {
		t.Errorf("the abandoned entry left %v", tmps)
	}
}

// TestPipelineLogsItsOwnFailure: a pipeline whose source is gone by the time
// it starts says so, as a variant worker does. The client it was started for
// may already have left, and nothing else reports it.
func TestPipelineLogsItsOwnFailure(t *testing.T) {
	rec := &LogRecorder{}
	s := internalServer(t, slog.New(rec))
	free, ok := s.pools.AcquireLive()
	if !ok {
		t.Fatal("no live slot")
	}
	exited := make(chan struct{})
	entry := cache.NewMemEntry(0, cache.Meta{})
	s.startPipeline(s.baseCtx, entry, missingPipeline(s), func() { free(); close(exited) }, false)
	<-exited
	if code := waxerr.CodeOf(entry.Err()); code != waxerr.CodeNotFound {
		t.Fatalf("the pipeline failed as %s (%v), want %s: the source has to be missing, not its root", code, entry.Err(), waxerr.CodeNotFound)
	}
	w := rec.Warnings()
	if len(w) != 1 || w[0].Msg != "pipeline failed" ||
		!strings.Contains(w[0].Attrs, "src=lib/missing.wav") || !strings.Contains(w[0].Attrs, "rung=transcode") {
		t.Errorf("a pipeline whose source is gone logged %v, want one Warn naming its source and rung", w)
	}
}

// TestSourcePipelineRefusesAChangedSource: a pipeline whose source changed
// after its request checked the identity fails as source-changed, before it
// reads the new bytes.
func TestSourcePipelineRefusesAChangedSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.wav"), []byte("not the bytes the request saw"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := internalServerAt(t, root, nil)
	p := s.sourcePipeline(&streamRequest{p: &streamParams{src: "lib/a.wav"},
		src: &source.File{Ref: "lib/a.wav", Ext: "wav", ID: source.Identity{Size: 1, MtimeNS: 1}}})
	if _, err := p.run(context.Background(), io.Discard); waxerr.CodeOf(err) != waxerr.CodeSourceChanged {
		t.Errorf("a pipeline over a changed source = %v, want %s", err, waxerr.CodeSourceChanged)
	}
}

// TestPrepareSourceRefusesATimeline: the single-source front half refuses a tl
// request by name, so a caller that forgets to route a timeline gets that
// refusal instead of resolving an empty source reference.
func TestPrepareSourceRefusesATimeline(t *testing.T) {
	s := internalServer(t, nil)
	q := url.Values{"tl": {strings.Repeat("a", 43)}, "format": {"flac"}, "gain": {"off"}}
	_, err := s.prepareSource(context.Background(), q, false)
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || err == nil || !strings.Contains(err.Error(), "tl names a timeline") {
		t.Fatalf("prepareSource of a tl request = %v, want the named refusal", err)
	}
}

// TestNoPipelineStartsOnceCloseBegins: a pipeline asked for once Close has
// begun is refused: a goroutine counted then races Close's Wait. Its entry
// fails, so a reader does not wait for bytes that never come, and its slot
// goes back.
func TestNoPipelineStartsOnceCloseBegins(t *testing.T) {
	s := internalServer(t, nil)
	s.Close()
	ran, released := false, false
	entry := cache.NewMemEntry(0, cache.Meta{})
	s.startPipeline(s.baseCtx, entry, pipeline{ref: "lib/a.wav",
		run: func(context.Context, io.Writer) (*waxflow.TranscodeResult, error) {
			ran = true
			return &waxflow.TranscodeResult{}, nil
		}}, func() { released = true }, false)
	s.wg.Wait()
	if ran || !released || waxerr.CodeOf(entry.Err()) != waxerr.CodeOverloaded {
		t.Errorf("after Close the pipeline ran %t, released its slot %t, failed %v; want refused as %s",
			ran, released, entry.Err(), waxerr.CodeOverloaded)
	}
}

// TestCloseRacingNewPipelines: pipelines asked for while Close runs start
// before its Wait or are refused, never between; -race checks the flag.
func TestCloseRacingNewPipelines(t *testing.T) {
	s := internalServer(t, nil)
	done := func(context.Context, io.Writer) (*waxflow.TranscodeResult, error) {
		return &waxflow.TranscodeResult{}, nil
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Go(func() {
			<-start
			s.startPipeline(s.baseCtx, cache.NewMemEntry(0, cache.Meta{}), pipeline{ref: "lib/a.wav", run: done},
				func() {}, false)
		})
	}
	close(start)
	s.Close()
	wg.Wait()
}

// TestNoVariantWorkerStartsOnceCloseBegins is the same rule for an HLS variant
// worker: once Close has begun, Spawn refuses and keeps no slot.
func TestNoVariantWorkerStartsOnceCloseBegins(t *testing.T) {
	s := internalServer(t, nil)
	key := cache.NewKey("lib/a.wav|0", "hls", nil)
	variant, err := s.store.HLS(key, cache.Meta{Ref: "lib/a.wav", Ext: "mp4", ContentType: hlsMediaType})
	if err != nil {
		t.Fatal(err)
	}
	req := &hlsRequest{members: []memberSource{{Ref: "lib/a.wav"}}, key: key,
		variantPlan: variantPlan{plan: &waxflow.SegmentPlan{}}}
	s.Close()
	cancel, err := s.hlsSpawner(req, variant)(0, func(int64) {}, func(error) {})
	if cancel != nil {
		cancel(nil)
	}
	s.wg.Wait()
	if waxerr.CodeOf(err) != waxerr.CodeOverloaded || s.pools.LiveInUse() != 0 {
		t.Errorf("after Close Spawn = %v holding %d slot(s); want refused as %s with none",
			err, s.pools.LiveInUse(), waxerr.CodeOverloaded)
	}
}

// spawnMissing starts a variant worker over a source that is not there, which
// fails before its first segment, calls atExit from its exit callback, and
// returns the worker's error.
func spawnMissing(t *testing.T, s *Server, atExit func()) error {
	t.Helper()
	key := cache.NewKey("lib/missing.wav|0", "hls", nil)
	variant, err := s.store.HLS(key, cache.Meta{Ref: "lib/missing.wav", Ext: "mp4", ContentType: hlsMediaType})
	if err != nil {
		t.Fatal(err)
	}
	req := &hlsRequest{members: []memberSource{{Ref: "lib/missing.wav"}}, key: key,
		variantPlan: variantPlan{plan: &waxflow.SegmentPlan{}}}
	exited := make(chan struct{})
	var exitErr error
	cancel, err := s.hlsSpawner(req, variant)(0, func(int64) {}, func(err error) {
		exitErr = err
		atExit()
		close(exited)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel(nil)
	<-exited
	return exitErr
}

// TestHLSWorkerFreesItsSlotBeforeExit: a variant worker gives back its live
// slot before exit wakes the manager's waiters, since a waiter may spawn the
// next worker into that slot at once. It still counts as a session there: idle
// has to mean the worker is retired, which exit is.
func TestHLSWorkerFreesItsSlotBeforeExit(t *testing.T) {
	s := internalServer(t, nil)
	var slots int
	var active int64
	spawnMissing(t, s, func() { slots, active = s.pools.LiveInUse(), s.met.SessionsActive.Load() })
	if slots != 0 || active != 1 {
		t.Errorf("at exit the worker held %d live slot(s), want 0, and counted %d session(s), want 1", slots, active)
	}
}

// TestHLSTranscodeWorkerLogsItsOwnFailure: a transcode-rung worker that fails
// before its first segment says so, as the copy rungs' workers do. Nothing
// else does once its waiting client has gone.
func TestHLSTranscodeWorkerLogsItsOwnFailure(t *testing.T) {
	rec := &LogRecorder{}
	s := internalServer(t, slog.New(rec))
	if err := spawnMissing(t, s, func() {}); waxerr.CodeOf(err) != waxerr.CodeNotFound {
		t.Fatalf("the worker failed with %v, want %s: the source has to be missing, not its root", err, waxerr.CodeNotFound)
	}
	if !rec.Has(slog.LevelWarn, "hls worker failed") {
		t.Errorf("a worker whose source is gone logged %v, want a Warn", rec.Warnings())
	}
}

// TestStreamOptionsUseTheDaemonsResampleProfile: a progressive transcode
// resamples with the profile the daemon was configured with, which its cache
// key names.
func TestStreamOptionsUseTheDaemonsResampleProfile(t *testing.T) {
	s := &Server{profile: resample.Fast}
	opts, err := s.streamOptions(&streamParams{format: "auto"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if opts.ResampleProfile != resample.Fast {
		t.Errorf("a /stream transcode resamples with profile %q, want the daemon's %q", opts.ResampleProfile, resample.Fast)
	}
}
