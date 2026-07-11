// Package oracletest holds the tests whose oracles are third-party Go
// modules (waxlabel as the metadata round-trip oracle, hajimehoshi's
// go-mp3 as an independent MP3 decoder). They live in this nested
// module, not the main one, so the main module's require block stays
// empty: importers of the codec packages inherit nothing, structurally
// (the resolver/ precedent, applied to test-only dependencies at the
// v1.0 extraction).
//
// `go test ./...` at the repository root does not descend here; `make
// test-oracle` (part of `make check`) and the CI oracle job run it.
package oracletest
