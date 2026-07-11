package server

import (
	"encoding/json"
	"net/http"

	"github.com/colespringer/waxflow/waxerr"
)

// ErrorBody is the JSON error envelope shared server-to-client across the
// Wax family: {"error","code","schemaVersion"} with kebab-case codes.
type ErrorBody struct {
	Error         string      `json:"error"`
	Code          waxerr.Code `json:"code"`
	SchemaVersion int         `json:"schemaVersion"`
}

// statusFor maps envelope codes onto HTTP status. The code is the
// contract; the status is the closest HTTP fit.
func statusFor(code waxerr.Code) int {
	switch code {
	case waxerr.CodeInvalidRequest:
		return http.StatusBadRequest
	case waxerr.CodeUnauthorized:
		return http.StatusUnauthorized
	case waxerr.CodeSignatureInvalid, waxerr.CodeSignatureExpired:
		return http.StatusForbidden
	case waxerr.CodeNotFound:
		return http.StatusNotFound
	case waxerr.CodeSourceChanged:
		return http.StatusGone
	case waxerr.CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case waxerr.CodeUnsupportedFormat:
		return http.StatusUnsupportedMediaType
	case waxerr.CodeUnsupportedSource:
		return http.StatusNotImplemented
	case waxerr.CodeOverloaded, waxerr.CodeCatalogUnavailable:
		return http.StatusServiceUnavailable
	default:
		// source-unreadable, output-unwritable, canceled, internal
		return http.StatusInternalServerError
	}
}

// writeError classifies err and writes the envelope. Errors on already
// hijacked or written streams are the caller's problem; this is for
// pre-body failures.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	code := waxerr.CodeOf(err)
	if code == waxerr.CodeOverloaded || code == waxerr.CodeCatalogUnavailable {
		// Over admission limits or a briefly unreachable catalog, tell
		// clients when to come back.
		w.Header().Set("Retry-After", "2")
	}
	s.writeEnvelope(w, statusFor(code), code, err.Error())
}

// writeEnvelope writes an envelope with an explicit status (the 416 range
// refusal reuses invalid-request at a range-specific status).
func (s *Server) writeEnvelope(w http.ResponseWriter, status int, code waxerr.Code, msg string) {
	s.writeJSON(w, status, ErrorBody{Error: msg, Code: code, SchemaVersion: 1})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encode errors mean the client went away; nothing useful to do.
	_ = json.NewEncoder(w).Encode(v)
}
