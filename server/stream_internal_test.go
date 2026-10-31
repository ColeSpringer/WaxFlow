package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/resample"
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
		cancel()
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
	defer cancel()
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
