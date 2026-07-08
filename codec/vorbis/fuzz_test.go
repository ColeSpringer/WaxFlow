package vorbis

import "testing"

// FuzzParseConfig drives the Xiph-lacing split and the full header parse
// (codebooks, floors, residues, mappings, modes) on arbitrary bytes: no
// panic, no out-of-bounds, no unbounded allocation. The setup header is the
// most attacker-controlled surface in Vorbis, so it gets the fuzz budget; the
// decode path is covered by the ffmpeg differential tests.
func FuzzParseConfig(f *testing.F) {
	f.Add([]byte{2, 1, 1})
	f.Add([]byte{0})
	f.Add(append([]byte{2, 7, 7}, append([]byte("\x01vorbis"), make([]byte, 64)...)...))
	f.Fuzz(func(t *testing.T, blob []byte) {
		_, _ = ParseConfig(blob)
	})
}

// FuzzHeaders fuzzes the three-header parse directly, mutating each header
// independently around a valid identification header shape.
func FuzzHeaders(f *testing.F) {
	id := append([]byte("\x01vorbis"), make([]byte, 23)...)
	f.Add(id, []byte("\x03vorbis"), []byte("\x05vorbis"))
	f.Fuzz(func(t *testing.T, id, comment, setup []byte) {
		_, _ = ParseHeaders(id, comment, setup)
	})
}
