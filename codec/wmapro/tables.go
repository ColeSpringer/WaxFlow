// Package wmapro decodes Windows Media Audio 9 Professional (wFormatTag
// 0x0162) from the WAVEFORMATEX and packets container/asf delivers.
//
// Provenance: WMA Pro has no published bitstream specification, so unlike most
// codecs here the tables in this package are not a restatement of a normative
// document. They are the black-box parameter artifact ADR-0001 provides for,
// extracted mechanically from FFmpeg's data file in the dedicated analysis
// pass that also produced docs/notes/wma-pro-bitstream.md. The extraction is
// tablesgen_test.go, which pins the upstream file by SHA-256. No decoder logic
// was taken, and the session that writes the decoder consumes the notes and
// these tables only. Licensing is recorded in THIRD-PARTY-NOTICES.md.
package wmapro

//go:generate go test -tags wmaprotablesgen -run ^TestGenerateTables$ -count=1
