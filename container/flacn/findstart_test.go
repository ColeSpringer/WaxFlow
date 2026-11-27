package flacn_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container/flacn"
)

// TestFindStartConfirmsTheStream: the marker counts as a stream's start only
// with a STREAMINFO block that parses behind it and, behind that, what a
// stream goes on with: a block of one of the six other defined types, or a
// frame's sync code where STREAMINFO is the last block. Every committed
// stream is found behind stray bytes, whatever it puts second.
func TestFindStartConfirmsTheStream(t *testing.T) {
	junk := bytes.Repeat([]byte{0xAA}, 300)
	for _, dir := range []string{filepath.Join("..", "..", "testdata"), "testdata"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.flac"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if at, ok := flacn.FindStart(slices.Concat(junk, raw)); !ok || at != 300 {
				t.Errorf("%s: FindStart = %d, %v; want 300", file, at, ok)
			}
		}
	}

	head := fixture(t, "sine-s16.flac")[:42] // the marker and STREAMINFO
	behind := func(last bool, next ...byte) []byte {
		b := slices.Concat(junk, head, next, make([]byte, 16))
		if last {
			b[len(junk)+4] |= 0x80
		}
		return b
	}
	for typ := byte(1); typ <= 6; typ++ {
		if at, ok := flacn.FindStart(behind(false, typ)); !ok || at != 300 {
			t.Errorf("a block of type %d second: FindStart = %d, %v; want 300", typ, at, ok)
		}
	}
	for _, sync := range []byte{0xF8, 0xF9} {
		if at, ok := flacn.FindStart(behind(true, 0xFF, sync)); !ok || at != 300 {
			t.Errorf("a frame with sync FF%02X behind a last STREAMINFO: FindStart = %d, %v; want 300", sync, at, ok)
		}
	}
	for name, window := range map[string][]byte{
		"a second STREAMINFO":            behind(false, 0),
		"a reserved block type":          behind(false, 7),
		"the invalid block type":         behind(false, 127),
		"text":                           behind(false, ' '),
		"no frame behind the last block": behind(true, 0x4D, 0x50),
		"a block behind the last block":  behind(true, 4, 0),
		"nothing behind STREAMINFO":      slices.Concat(junk, head),
	} {
		if at, ok := flacn.FindStart(window); ok {
			t.Errorf("%s: FindStart = %d", name, at)
		}
	}
}
