// Package wma will decode Windows Media Audio v1 (wFormatTag 0x0160) and v2
// (0x0161). The analysis artifacts landed first: this package currently
// carries parameter tables and nothing that reads a bitstream.
//
// Provenance: WMA has no published bitstream specification, so unlike every
// other codec here the tables below are not a restatement of a normative
// document. They are the black-box parameter artifact ADR-0001 provides for,
// extracted mechanically from FFmpeg's data files in the dedicated analysis
// pass that also produced docs/notes/wma-bitstream.md. The extraction is
// tablesgen_test.go, which pins each upstream file by SHA-256. No decoder
// logic was taken, and the session that writes the decoder consumes the notes
// and these tables only. Licensing is recorded in THIRD-PARTY-NOTICES.md.
package wma

//go:generate go test -tags wmatablesgen -run ^TestGenerateTables$ -count=1

// The coefficient books come in pairs, flattened here so pair p occupies
// indices 2*p and 2*p+1. Within a pair the first book codes an ordinary
// channel and the second codes channel 1 of a mid/side block, where less
// energy is expected. Which pair a stream uses is a rule rather than data, so
// it lives in docs/notes/wma-bitstream.md section 7 and arrives with the
// decoder.

// nbLSPCoefs is the LSP exponent coder's order: ten coefficients, each an
// index into its own row of lspCodebook.
const nbLSPCoefs = 10
