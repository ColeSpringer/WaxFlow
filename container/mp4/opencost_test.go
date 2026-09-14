package mp4

import (
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestOpenReadsTheMoovAndAHeader pins what opening an MP3 movie costs: the
// boxes ahead of the payload, and the one frame header the entry is checked
// against. That header used to cost a whole 128 KiB window; the payloads
// here are longer than two windows so a round-up would show.
func TestOpenReadsTheMoovAndAHeader(t *testing.T) {
	var extras [2]int64
	for i, n := range []int{2000, 4000} {
		payload := testutil.SyntheticMP3Frames(n)
		raw := movie{entry: v1SoundEntry(".mp3", 2, 16, 44100, 0), unitBytes: testutil.SyntheticMP3FrameLen,
			frames: n, chunkFrames: 64, timescale: 44100, sttsDelta: 1152}.build()
		raw = append(raw[:len(raw)-len(payload)], payload...)
		head := int64(len(raw) - len(payload))
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		if _, err := NewDemuxer(src, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d frames: %d bytes in %d reads, %d ahead of the payload", n, src.Bytes, src.Reads, head)
		extras[i] = src.Bytes - head
		if extras[i] > 64 {
			t.Errorf("opening a %d-frame movie read %d bytes past the %d ahead of the payload, want one frame header", n, extras[i], head)
		}
	}
	if extras[0] != extras[1] {
		t.Errorf("the open read %d bytes of payload for 2000 frames and %d for 4000; it must not scale with the payload", extras[0], extras[1])
	}
}
