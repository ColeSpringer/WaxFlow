package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/hls"
	"github.com/colespringer/waxflow/source"
)

// ladderPlanner is a fake copy planner: it asks the grid asks times,
// declining on a grid of 0 and returning the grid's error as it is, as the
// engine's planners do, and then answers ok and err.
type ladderPlanner struct {
	asks int
	ok   bool
	err  error
}

func (p ladderPlanner) plan(grid func() (int, error)) (bool, error) {
	for range p.asks {
		g, err := grid()
		if err != nil {
			return false, err
		}
		if g <= 0 {
			return false, nil
		}
	}
	return p.ok, p.err
}

// TestWalkLadder: the walk picks the rung, asks the grid only through a
// planner, and names every cut decline's reason.
func TestWalkLadder(t *testing.T) {
	opus := container.Track{Codec: codec.Opus, Fmt: audio.Format{Rate: 48000, Channels: 2}}
	flac := container.Track{Codec: codec.FLAC, Fmt: audio.Format{Rate: 48000, Channels: 2}}
	gridErr := errors.New("the walk failed")
	planErr := errors.New("the planner refused")
	whole, part := span{}, span{from: 48000, to: 96000}
	const none = "" // no cut decline logged
	for _, tc := range []struct {
		name     string
		span     span
		capped   bool
		track    container.Track
		trackErr error
		grid     int
		gridErr  error
		remux    ladderPlanner
		cut      ladderPlanner
		want     rung
		wantErr  error
		ran      string // the planners that ran: "remux", "cut" or ""
		asked    bool
		reason   string
	}{
		{name: "a whole source the remux serves without the grid", span: whole, track: opus,
			remux: ladderPlanner{ok: true}, want: rungRemux, ran: "remux"},
		{name: "a whole source the remux serves on the grid", span: whole, track: opus, grid: 960,
			remux: ladderPlanner{asks: 1, ok: true}, want: rungRemux, ran: "remux", asked: true},
		{name: "a remux error before the grid declines", span: whole, track: opus,
			remux: ladderPlanner{err: planErr}, want: rungTranscode, ran: "remux"},
		{name: "a grid that fails a remux is the request's", span: whole, track: opus, gridErr: gridErr,
			remux: ladderPlanner{asks: 1, ok: true}, want: rungTranscode, wantErr: gridErr, ran: "remux", asked: true},
		{name: "a remux error after the grid is the request's", span: whole, track: opus, grid: 960,
			remux: ladderPlanner{asks: 1, err: planErr}, want: rungTranscode, wantErr: planErr, ran: "remux", asked: true},
		{name: "a remux on a grid of 0 declines", span: whole, track: opus,
			remux: ladderPlanner{asks: 1, ok: true}, want: rungTranscode, ran: "remux", asked: true},
		{name: "a capped whole source transcodes", span: whole, capped: true, track: opus, grid: 960,
			remux: ladderPlanner{asks: 1, ok: true}, want: rungTranscode},
		{name: "a capped span transcodes", span: part, capped: true, track: opus, grid: 960,
			cut: ladderPlanner{asks: 1, ok: true}, want: rungTranscode,
			reason: "a bit rate cap cannot be promised by a packet-move rung"},
		{name: "a span whose track cannot be measured", span: part, trackErr: planErr, grid: 960,
			cut: ladderPlanner{asks: 1, ok: true}, want: rungTranscode,
			reason: "the source track could not be measured"},
		{name: "a span of a codec off the allowlist", span: part, track: flac, grid: 960,
			cut: ladderPlanner{asks: 1, ok: true}, want: rungTranscode,
			reason: "the codec is not on the cut allowlist"},
		{name: "a span the cut declines before the grid", span: part, track: opus, grid: 960,
			cut: ladderPlanner{}, want: rungTranscode, ran: "cut",
			reason: "the track, span, format or options rule out a copy"},
		{name: "a span whose grid fails", span: part, track: opus, gridErr: gridErr,
			cut: ladderPlanner{asks: 1, ok: true}, want: rungTranscode, ran: "cut", asked: true,
			reason: "the packet grid could not be measured"},
		{name: "a span on a grid of 0", span: part, track: opus,
			cut: ladderPlanner{asks: 1, ok: true}, want: rungTranscode, ran: "cut", asked: true,
			reason: "the source's packet durations vary, so there is no grid to cut on"},
		{name: "a span the cut declines on the grid", span: part, track: opus, grid: 960,
			cut: ladderPlanner{asks: 1}, want: rungTranscode, ran: "cut", asked: true,
			reason: "no cut serves these options (gap, trims, or destination)"},
		{name: "a span the cut refuses", span: part, track: opus, grid: 960,
			cut: ladderPlanner{err: planErr}, want: rungTranscode, ran: "cut",
			reason: "the planner refused the span"},
		{name: "a span the cut serves", span: part, track: opus, grid: 960,
			cut: ladderPlanner{asks: 1, ok: true}, want: rungCut, ran: "cut", asked: true},
		{name: "a planner that asks twice measures once", span: part, track: opus, grid: 960,
			cut: ladderPlanner{asks: 2, ok: true}, want: rungCut, ran: "cut", asked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &LogRecorder{}
			s := &Server{log: slog.New(rec)}
			var ran []string
			asks := 0
			got, err := s.walkLadder(ladderQuery{
				src:      &source.File{Ref: "lib/x.opus"},
				delivery: "hls",
				span:     tc.span,
				capped:   tc.capped,
				track:    func() (container.Track, error) { return tc.track, tc.trackErr },
				grid: func() (int, error) {
					asks++
					return tc.grid, tc.gridErr
				},
			}, func(grid func() (int, error)) (bool, error) {
				ran = append(ran, "remux")
				return tc.remux.plan(grid)
			}, func(_ container.Track, grid func() (int, error)) (bool, error) {
				ran = append(ran, "cut")
				return tc.cut.plan(grid)
			})
			if got != tc.want || err != tc.wantErr {
				t.Errorf("walk = %v, %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
			if r := strings.Join(ran, ","); r != tc.ran {
				t.Errorf("planners run: %q, want %q", r, tc.ran)
			}
			wantAsks := 0
			if tc.asked {
				wantAsks = 1
			}
			if asks != wantAsks {
				t.Errorf("grid measured %d times, want %d", asks, wantAsks)
			}
			var reasons []string
			for _, r := range rec.Records() {
				if r.Msg != "cut declined" {
					continue
				}
				if r.Level != slog.LevelDebug || !strings.Contains(r.Attrs, "delivery=hls") || !strings.Contains(r.Attrs, "src=lib/x.opus") {
					t.Errorf("decline logged as %s", r)
				}
				reasons = append(reasons, r.Attrs)
			}
			switch {
			case tc.reason == none && len(reasons) != 0:
				t.Errorf("logged %q, want no cut decline", reasons)
			case tc.reason != none && (len(reasons) != 1 || !strings.Contains(reasons[0], "reason="+tc.reason)):
				t.Errorf("logged %q, want one decline for %q", reasons, tc.reason)
			}
		})
	}
}

// TestHLSRemuxReportsAFailedGridWalk: a remuxable HLS variant whose packet
// grid walk fails (here, on a context that has ended) fails the request
// rather than declining to the transcode, so the remux measures through the
// walk's grid.
func TestHLSRemuxReportsAFailedGridWalk(t *testing.T) {
	root := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "x.opus"), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	s := internalServerAt(t, root, nil)
	src, err := s.resolver.Resolve(context.Background(), "lib/x.opus")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	track, err := s.trackFor(context.Background(), src, false)
	if err != nil {
		t.Fatal(err)
	}
	desc := hls.Descriptor{Format: "opus", SegDur: 2}
	if vp, err := s.planHLSRung(context.Background(), desc, []container.Track{track}, nil, nil); err != nil || vp.rung() != rungTranscode {
		t.Fatalf("no source: rung %s, err %v; want the transcode", rungName(vp.rung()), err)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if vp, err := s.planHLSRung(gone, desc, []container.Track{track}, nil, src); err == nil {
		t.Errorf("a grid walk that failed declined to rung %s", rungName(vp.rung()))
	}
	if vp, err := s.planHLSRung(context.Background(), desc, []container.Track{track}, nil, src); err != nil || vp.rung() != rungRemux {
		t.Errorf("the control: rung %s, err %v; want the remux", rungName(vp.rung()), err)
	}
}
