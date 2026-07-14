package testutil

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// WriteFloatWAV writes planar channels as an interleaved IEEE float32 WAV.
//
// The float domain is the point: an ffmpeg differential that measured a
// quantized int16 file would be comparing its filter against samples the Go
// side never saw, so a rounding difference would read as a detection
// difference. Writing float32 makes ffmpeg measure bit-identical samples to
// what the analyzer saw, which is what lets these differentials assert tight
// bounds rather than fuzzy ones.
//
// chans must be non-empty and its channels equal length.
func WriteFloatWAV(t *testing.T, path string, rate int, chans [][]float32) {
	t.Helper()
	if len(chans) == 0 || len(chans[0]) == 0 {
		t.Fatalf("WriteFloatWAV: no samples")
	}
	n, ch := len(chans[0]), len(chans)
	for c, s := range chans {
		if len(s) != n {
			t.Fatalf("WriteFloatWAV: channel %d has %d samples, channel 0 has %d", c, len(s), n)
		}
	}
	data := 4 * n * ch
	buf := make([]byte, 44+data)
	le := binary.LittleEndian
	copy(buf[0:], "RIFF")
	le.PutUint32(buf[4:], uint32(36+data))
	copy(buf[8:], "WAVEfmt ")
	le.PutUint32(buf[16:], 16)
	le.PutUint16(buf[20:], 3) // WAVE_FORMAT_IEEE_FLOAT
	le.PutUint16(buf[22:], uint16(ch))
	le.PutUint32(buf[24:], uint32(rate))
	le.PutUint32(buf[28:], uint32(4*rate*ch))
	le.PutUint16(buf[32:], uint16(4*ch))
	le.PutUint16(buf[34:], 32)
	copy(buf[36:], "data")
	le.PutUint32(buf[40:], uint32(data))
	off := 44
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			le.PutUint32(buf[off:], math.Float32bits(chans[c][i]))
			off += 4
		}
	}
	if err := os.WriteFile(path, buf, 0o666); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
