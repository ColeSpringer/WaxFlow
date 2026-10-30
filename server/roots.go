package server

import "net/http"

// handleRootsReload serves POST /roots/reload: re-read the daemon's
// configuration and reconcile the live library roots to match, so a
// runtime-added root streams without a restart. Empty body; the reconcile
// scope is the fields source.Roots owns (the roots and sourceMaxBytes),
// nothing built once at startup.
//
// It mirrors handleCacheGC as an operator trigger. A reconcile that opened
// or closed mount handles logs at Info, where an operator debugging a root
// that started 404ing will see it; a no-op reconcile changed no handles, so
// it drops to Debug rather than claiming a change it did not make. A path
// that does not open is no failure: its root is reported in unmounted. A
// failed reload (a bad config edit) logs at Warn and surfaces synchronously
// as the waxerr the reload returns (a 400 via the envelope mapping), so the
// caller learns at once instead of after a 200 that changed nothing.
func (s *Server) handleRootsReload(w http.ResponseWriter, _ *http.Request) {
	res, err := s.cfg.ReloadRoots()
	if err != nil {
		// A failed reload goes to the daemon log too, not only the client:
		// an operator tailing logs to see why a root started 404ing wants the
		// reason a reload was rejected, the same "operator wants to know"
		// reason the success path logs.
		s.log.Warn("roots reload failed", "err", err)
		s.writeError(w, err)
		return
	}
	if len(res.Added) > 0 || len(res.Removed) > 0 || len(res.Changed) > 0 || len(res.Remounted) > 0 {
		s.log.Info("roots reloaded", "added", res.Added, "removed", res.Removed,
			"changed", res.Changed, "remounted", res.Remounted, "unmounted", res.Unmounted)
	} else {
		s.log.Debug("roots reloaded with no change", "roots", res.Roots, "unmounted", res.Unmounted)
	}
	s.writeJSON(w, http.StatusOK, RootsReloadResponse{
		SchemaVersion: 1,
		Added:         orEmpty(res.Added),
		Removed:       orEmpty(res.Removed),
		Changed:       orEmpty(res.Changed),
		Roots:         orEmpty(res.Roots),
		Unmounted:     orEmpty(res.Unmounted),
		Remounted:     orEmpty(res.Remounted),
	})
}

// handleRoots serves GET /roots: every configured library root, mounted or
// not, with the reason an unmounted one did not open. Paths are shown: the
// endpoint is key-gated, like the configuration they come from.
func (s *Server) handleRoots(w http.ResponseWriter, _ *http.Request) {
	all := s.cfg.RootsStatus()
	resp := RootsResponse{SchemaVersion: 1, Roots: make([]RootStatus, 0, len(all))}
	for _, r := range all {
		st := RootStatus{Name: r.Name, Path: r.Path, Mounted: r.Mounted, Since: r.Since}
		if r.Err != nil {
			st.Error = r.Err.Error()
		}
		resp.Roots = append(resp.Roots, st)
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// orEmpty renders a nil slice as [] on the wire rather than null, so a
// no-op reload's empty deltas stay arrays a client can iterate.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
