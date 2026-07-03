package server

import "net/http"

// applyCORS sets response CORS headers on playback endpoints when the
// Origin is allowlisted. Playback endpoints only, for WaxDeck-web and
// hls.js; the control API is same-origin tooling.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	w.Header().Add("Vary", "Origin")
	if !s.anyOrigin && !s.origins[origin] {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Expose-Headers",
		"Accept-Ranges, Content-Range, Content-Length, ETag, X-Content-Duration, X-Estimated-Content-Length")
}

// handlePreflight answers OPTIONS on playback endpoints. Preflights carry
// no credentials, so there is no auth here; a disallowed origin simply
// gets no allow headers.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD")
		w.Header().Set("Access-Control-Allow-Headers", "Range, X-API-Key, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
	w.WriteHeader(http.StatusNoContent)
}
