// Package waxerr defines WaxFlow's error taxonomy: machine-readable
// kebab-case codes shared across the HTTP boundary, sentinel errors for
// errors.Is classification, a boundary error type for errors.AsType
// extraction, and the documented CLI exit-code contract printed by
// `waxflow exit-codes`.
package waxerr

import (
	"context"
	"errors"
	"fmt"
)

// Code is a machine-readable, kebab-case error code. Codes are part of the
// public contract: they appear verbatim in the HTTP error envelope
// {"error","code","schemaVersion"} and map onto the CLI exit-code classes
// returned by ExitContract.
type Code string

const (
	CodeInvalidRequest     Code = "invalid-request"
	CodeUnauthorized       Code = "unauthorized"
	CodeSignatureInvalid   Code = "signature-invalid"
	CodeSignatureExpired   Code = "signature-expired"
	CodeSourceChanged      Code = "source-changed"
	CodeNotFound           Code = "not-found"
	CodeUnsupportedFormat  Code = "unsupported-format"
	CodeUnsupportedSource  Code = "unsupported-source"
	CodeMalformedInput     Code = "malformed-input"
	CodePayloadTooLarge    Code = "payload-too-large"
	CodeSourceUnreadable   Code = "source-unreadable"
	CodeOutputUnwritable   Code = "output-unwritable"
	CodeOverloaded         Code = "overloaded"
	CodeCanceled           Code = "canceled"
	CodeCatalogUnavailable Code = "catalog-unavailable"
	CodeInternal           Code = "internal"
)

// Codes returns every defined Code. The order is stable and matches the
// HTTP API documentation.
func Codes() []Code {
	return []Code{
		CodeInvalidRequest,
		CodeUnauthorized,
		CodeSignatureInvalid,
		CodeSignatureExpired,
		CodeSourceChanged,
		CodeNotFound,
		CodeUnsupportedFormat,
		CodeUnsupportedSource,
		CodeMalformedInput,
		CodePayloadTooLarge,
		CodeSourceUnreadable,
		CodeOutputUnwritable,
		CodeOverloaded,
		CodeCanceled,
		CodeCatalogUnavailable,
		CodeInternal,
	}
}

// Malformed and Unsupported build the two errors every demuxer and decoder in
// the tree has to tell apart, and they exist as a pair so that no package can
// quietly alias one to the other (several did, before the codes split).
//
// The rule, and it is the whole rule:
//
//   - Malformed is for bytes that deviate from their own format: truncated,
//     inconsistent, out of range, a field that contradicts another. The file
//     is broken. Strict mode escalates a tolerated oddity to this.
//   - Unsupported is for a well-formed stream this build does not cover: a
//     codec it has no decoder for, a channel configuration outside its scope,
//     a spec an encoder cannot produce. The file is fine; we are not.
//
// A caller can act on the second (convert it, ask for something else) and not
// on the first, which is why they carry different HTTP statuses and different
// exit classes.
//
// The prefix is a separate parameter rather than part of format, and that is
// load-bearing: go vet only recognizes a printf wrapper when the format string
// reaches the printf call unmodified, so folding the package name into it
// ("flac: "+format) turns off argument checking at every call site downstream.
// Split this way, both this function and the one-line package helpers that
// forward to it are checked. A package helper is still one line:
//
//	func malformed(format string, args ...any) error {
//		return waxerr.Malformed("flac: ", format, args...)
//	}
func Malformed(prefix, format string, args ...any) error {
	return New(CodeMalformedInput, prefix+fmt.Sprintf(format, args...))
}

// Unsupported names a well-formed stream outside this build's scope. See
// [Malformed] for the rule that divides the two, and for why the prefix is
// its own parameter.
func Unsupported(prefix, format string, args ...any) error {
	return New(CodeUnsupportedFormat, prefix+fmt.Sprintf(format, args...))
}

// Error is the boundary error: it carries a Code across package boundaries
// so HTTP handlers and the CLI can classify failures without string
// matching. Extract with errors.AsType[*waxerr.Error]; classify with
// errors.Is against the package sentinels.
type Error struct {
	Code Code
	Msg  string
	Err  error
}

// New returns an *Error with the given code and message.
func New(code Code, msg string) *Error {
	return &Error{Code: code, Msg: msg}
}

// Wrap returns an *Error with the given code and message wrapping err.
// The wrapped error remains visible to errors.Is/errors.AsType.
func Wrap(code Code, msg string, err error) *Error {
	return &Error{Code: code, Msg: msg, Err: err}
}

func (e *Error) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return e.Msg + ": " + e.Err.Error()
	case e.Msg != "":
		return e.Msg
	case e.Err != nil:
		return e.Err.Error()
	default:
		return string(e.Code)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is a *Error carrying the same Code, so
// errors.Is(err, waxerr.ErrNotFound) classifies by code without extraction.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Sentinels for errors.Is classification, one per Code. Boundary code should
// construct rich errors with New/Wrap; these exist for matching only.
var (
	ErrInvalidRequest     = New(CodeInvalidRequest, "")
	ErrUnauthorized       = New(CodeUnauthorized, "")
	ErrSignatureInvalid   = New(CodeSignatureInvalid, "")
	ErrSignatureExpired   = New(CodeSignatureExpired, "")
	ErrSourceChanged      = New(CodeSourceChanged, "")
	ErrNotFound           = New(CodeNotFound, "")
	ErrUnsupportedFormat  = New(CodeUnsupportedFormat, "")
	ErrUnsupportedSource  = New(CodeUnsupportedSource, "")
	ErrMalformedInput     = New(CodeMalformedInput, "")
	ErrPayloadTooLarge    = New(CodePayloadTooLarge, "")
	ErrOverloaded         = New(CodeOverloaded, "")
	ErrCanceled           = New(CodeCanceled, "")
	ErrCatalogUnavailable = New(CodeCatalogUnavailable, "")
	ErrSourceUnreadable   = New(CodeSourceUnreadable, "")
	ErrOutputUnwritable   = New(CodeOutputUnwritable, "")
	ErrInternal           = New(CodeInternal, "")
)

// CodeOf returns the Code carried by err: the Code of the outermost *Error
// in the chain, CodeCanceled for context cancellation, "" for nil, and
// CodeInternal for anything unclassified.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CodeCanceled
	}
	return CodeInternal
}

// ExitClass is one row of the documented CLI exit-code contract.
type ExitClass struct {
	Exit  int
	Name  string
	Codes []Code
}

// ExitContract returns the CLI exit-code contract, in exit-code order:
//
//	0 ok, 1 internal, 2 invalid, 3 not-found, 4 io, 5 unsupported,
//	6 canceled, 7 unauthorized, 8 overloaded, 9 malformed.
//
// This table is the single source of truth: Code.ExitCode derives from it
// and `waxflow exit-codes` prints it. Every defined Code appears in exactly
// one class (asserted by tests).
func ExitContract() []ExitClass {
	return []ExitClass{
		{Exit: 0, Name: "ok", Codes: nil},
		{Exit: 1, Name: "internal", Codes: []Code{CodeInternal}},
		{Exit: 2, Name: "invalid", Codes: []Code{CodeInvalidRequest, CodePayloadTooLarge}},
		{Exit: 3, Name: "not-found", Codes: []Code{CodeNotFound}},
		{Exit: 4, Name: "io", Codes: []Code{CodeSourceUnreadable, CodeOutputUnwritable, CodeSourceChanged, CodeCatalogUnavailable}},
		{Exit: 5, Name: "unsupported", Codes: []Code{CodeUnsupportedFormat, CodeUnsupportedSource}},
		{Exit: 6, Name: "canceled", Codes: []Code{CodeCanceled}},
		{Exit: 7, Name: "unauthorized", Codes: []Code{CodeUnauthorized, CodeSignatureInvalid, CodeSignatureExpired}},
		{Exit: 8, Name: "overloaded", Codes: []Code{CodeOverloaded}},
		{Exit: 9, Name: "malformed", Codes: []Code{CodeMalformedInput}},
	}
}

var exitByCode = func() map[Code]int {
	m := make(map[Code]int, len(Codes()))
	for _, class := range ExitContract() {
		for _, c := range class.Codes {
			m[c] = class.Exit
		}
	}
	return m
}()

// ExitCode returns the process exit code for c per ExitContract. The empty
// Code maps to 0; unrecognized codes map to 1 (internal).
func (c Code) ExitCode() int {
	if c == "" {
		return 0
	}
	if n, ok := exitByCode[c]; ok {
		return n
	}
	return 1
}

// ExitCode returns the process exit code for err: 0 for nil, otherwise the
// exit class of CodeOf(err).
func ExitCode(err error) int {
	return CodeOf(err).ExitCode()
}
