package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/cache"
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
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		level slog.Level
		why   string
	}{
		{"abandoned", live, cache.ErrAbandoned, slog.LevelInfo, "abandoned"},
		{"abandoned through a muxer", live, waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp3: writing frame", cache.ErrAbandoned),
			slog.LevelInfo, "abandoned"},
		{"canceled", live, waxerr.Wrap(waxerr.CodeCanceled, "transcode canceled", context.Canceled), slog.LevelDebug, "canceled"},
		{"bare context", live, context.Canceled, slog.LevelDebug, "canceled"},
		{"canceled under another code", gone, waxerr.New(waxerr.CodeSourceUnreadable, "reading a member"),
			slog.LevelDebug, "canceled"},
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

// internalServer is a Server over an empty library with one live slot,
// logging to log (nil discards).
func internalServer(t *testing.T, log *slog.Logger) *Server {
	t.Helper()
	roots, err := source.OpenRoots([]source.Root{{Name: "lib", Path: t.TempDir()}}, 0)
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
	s.startPipeline(s.baseCtx, cache.NewMemEntry(0, cache.Meta{}), "lib/missing.wav", source.Identity{}, "wav",
		waxflow.TranscodeOptions{}, span{}, rungTranscode, 0, container.Track{},
		func() { activeAtRelease <- s.met.SessionsActive.Load(); free() }, false)
	if got := <-activeAtRelease; got != 1 {
		t.Errorf("the slot was released with %d sessions active, want 1: the pipeline went idle first", got)
	}
}

// spawnMissing starts a variant worker over a source that is not there, which
// fails before its first segment, and calls atExit from its exit callback.
func spawnMissing(t *testing.T, s *Server, atExit func()) {
	t.Helper()
	key := cache.NewKey("lib/missing.wav|0", "hls", nil)
	variant, err := s.store.HLS(key, cache.Meta{Ref: "lib/missing.wav", Ext: "mp4", ContentType: hlsMediaType})
	if err != nil {
		t.Fatal(err)
	}
	req := &hlsRequest{members: []hlsSource{{Ref: "lib/missing.wav"}}, key: key, plan: &waxflow.SegmentPlan{}}
	exited := make(chan struct{})
	cancel, err := s.hlsSpawner(req, variant)(0, func(int64) {}, func(error) {
		atExit()
		close(exited)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	<-exited
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
	spawnMissing(t, s, func() {})
	if !rec.Has(slog.LevelWarn, "hls worker failed") {
		t.Errorf("a worker whose source is gone logged %v, want a Warn", rec.Warnings())
	}
}
