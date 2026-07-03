package testutil

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	gomp3 "github.com/hajimehoshi/go-mp3"
)

// GoMP3Decode decodes an MP3 stream with hajimehoshi/go-mp3, a pure-Go
// test-only oracle (see the testing policy: established pure-Go decoders
// let most differential tests run without ffmpeg installed; they never
// enter the runtime pipeline or the public tree). Output is interleaved
// stereo float32 (go-mp3 always emits two channels, duplicating mono)
// from its 16-bit PCM, untrimmed: the oracle applies no gapless trims
// and decodes metadata frames as audio, so callers compare against
// untagged fixtures.
func GoMP3Decode(t testing.TB, stream []byte) ([]float32, int) {
	t.Helper()
	dec, err := gomp3.NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("go-mp3: %v", err)
	}
	raw, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("go-mp3 decode: %v", err)
	}
	out := make([]float32, len(raw)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
	}
	return out, dec.SampleRate()
}
