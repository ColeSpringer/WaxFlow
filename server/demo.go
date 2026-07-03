package server

import (
	_ "embed"
	"net/http"
)

// The committed browser test page (dev mode only): the dev loop's real
// asset, not something each contributor rebuilds. It mints signed URLs
// via POST /sign and drives an <audio> element; hls.js joins when HLS
// lands.
//
//go:embed demo.html
var demoHTML []byte

func (s *Server) handleDemo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(demoHTML)
}
