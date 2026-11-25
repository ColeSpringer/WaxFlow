package server

import (
	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/source"
)

// ladderQuery is what the walk reads of one single-source request.
type ladderQuery struct {
	src      *source.File                    // named in the decline log
	delivery string                          // "progressive" or "hls"
	span     span                            // a narrowed span takes the cut, else the remux
	capped   bool                            // a maxBitRate cap: only a transcode promises a rate
	track    func() (container.Track, error) // the full source track, measured
	grid     func() (int, error)             // the packet grid; the walk asks it at most once
}

// walkLadder returns the first rung below direct play that serves q: remux for
// a whole source, cut for a span, transcode otherwise, and always transcode
// under a cap. remux and cut are the delivery's own planners and report
// ok=false to decline. A remux error raised once the grid was asked is the
// request's; any other declines, so the transcode rung reports it in its own
// words. Every cut decline is logged at Debug with its reason.
func (s *Server) walkLadder(q ladderQuery,
	remux func(grid func() (int, error)) (ok bool, err error),
	cut func(track container.Track, grid func() (int, error)) (ok bool, err error)) (rung, error) {
	g := &onceGrid{measure: q.grid}
	// Only a whole source asks remux: a span narrows the Media and PlanRemux
	// reads only the options, so it would copy the whole file.
	if !q.span.narrowed() {
		if q.capped {
			return rungTranscode, nil
		}
		ok, err := remux(g.get)
		switch {
		case err != nil && g.asked:
			return rungTranscode, err
		case ok && err == nil:
			return rungRemux, nil
		}
		return rungTranscode, nil
	}
	decline := func(reason string, args ...any) (rung, error) {
		kv := append([]any{"delivery", q.delivery, "reason", reason, "src", source.Ref(q.src.Ref)}, args...)
		s.log.Debug("cut declined", kv...)
		return rungTranscode, nil
	}
	if q.capped {
		return decline("a bit rate cap cannot be promised by a packet-move rung")
	}
	track, err := q.track()
	if err != nil {
		return decline("the source track could not be measured", "err", err)
	}
	if !waxflow.Cuttable(track) {
		return decline("the codec is not on the cut allowlist", "codec", track.Codec)
	}
	ok, err := cut(track, g.get)
	// A cut error never fails the request: the cut validates a span more
	// strictly than the transcode, which serves an empty span the cut refuses.
	switch {
	case g.err != nil:
		return decline("the packet grid could not be measured", "err", g.err)
	case err != nil:
		return decline("the planner refused the span", "err", err)
	case ok:
		return rungCut, nil
	case !g.asked:
		return decline("the track, span, format or options rule out a copy")
	case g.grid <= 0:
		return decline("the source's packet durations vary, so there is no grid to cut on")
	}
	return decline("no cut serves these options (gap, trims, or destination)")
}

// onceGrid asks measure at most once and keeps what it answered.
type onceGrid struct {
	measure func() (int, error)
	asked   bool
	grid    int
	err     error
}

func (g *onceGrid) get() (int, error) {
	if !g.asked {
		g.asked = true
		g.grid, g.err = g.measure()
	}
	return g.grid, g.err
}
