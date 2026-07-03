package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/colespringer/waxflow/internal/sign"
	"github.com/colespringer/waxflow/waxerr"
)

// presentedKey extracts the API key from X-API-Key or a Bearer token.
func presentedKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return tok
	}
	return ""
}

// keyAuthed reports whether r carries a valid API key. With no keys
// configured the daemon is in the (fail-closed-gated) keyless posture and
// every request passes.
func (s *Server) keyAuthed(r *http.Request) bool {
	if len(s.keyHashes) == 0 {
		return true
	}
	k := presentedKey(r)
	if k == "" {
		return false
	}
	h := sha256.Sum256([]byte(k))
	ok := false
	for _, want := range s.keyHashes {
		// SHA-256 both sides plus constant-time compare: length and
		// content of the configured keys never shape the timing.
		if subtle.ConstantTimeCompare(h[:], want[:]) == 1 {
			ok = true
		}
	}
	return ok
}

// requireKey guards control endpoints.
func (s *Server) requireKey(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.keyAuthed(r) {
			s.writeEnvelope(w, http.StatusUnauthorized, waxerr.CodeUnauthorized, "missing or invalid API key")
			return
		}
		h(w, r)
	}
}

// playbackAuth authorizes playback endpoints: a valid API key or a valid
// signed query (ADR-0003). sigAuthed reports which one succeeded, because
// signed requests additionally pin source identity.
//
// A valid API key wins outright: a key holder is already fully trusted,
// so stale or tampered signature parameters riding along (a proxy
// re-fetching an expired URL with its own key, say) must not fail the
// request. Only keyless requests are judged by their signature. Note
// this covers access only: source-identity pinning (the id parameter)
// guards which bytes are served and applies to every auth mode.
//
// q is the caller's parsed query (parsed once per request).
func (s *Server) playbackAuth(r *http.Request, q url.Values) (sigAuthed bool, err error) {
	if s.keyAuthed(r) {
		return false, nil
	}
	if q.Has(sign.ParamSig) || q.Has(sign.ParamExp) || q.Has(sign.ParamKID) {
		if s.signer == nil {
			return false, waxerr.New(waxerr.CodeSignatureInvalid, "signed URLs are not enabled on this daemon")
		}
		if err := s.signer.Verify(r.Method, r.URL.Path, q); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, waxerr.New(waxerr.CodeUnauthorized, "playback needs an API key or a signed URL")
}

// metricsAuthed authorizes GET /metrics: any API key, or the dedicated
// metricsKey.
func (s *Server) metricsAuthed(r *http.Request) bool {
	if s.keyAuthed(r) {
		return true
	}
	if !s.hasMetricsKey {
		return false
	}
	k := presentedKey(r)
	if k == "" {
		return false
	}
	h := sha256.Sum256([]byte(k))
	return subtle.ConstantTimeCompare(h[:], s.metricsKeyHash[:]) == 1
}
