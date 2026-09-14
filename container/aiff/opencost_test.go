package aiff

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// openCost opens raw through a counting source and reports what the open
// read from it.
func openCost(t *testing.T, raw []byte) (reads int, bytes int64) {
	t.Helper()
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	if _, err := NewDemuxer(src, nil); err != nil {
		t.Fatal(err)
	}
	return src.Reads, src.Bytes
}

// TestOpenReadsTheHeader is the riff pin's twin: a PCM AIFF opens in its
// chunk headers, and an MP3 payload adds the head of the frame run and
// nothing that scales with the payload. The synthetic payloads are longer
// than two windows because every committed fixture is smaller than one.
func TestOpenReadsTheHeader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.aiff"))
	if err != nil {
		t.Fatal(err)
	}
	if reads, bytes := openCost(t, raw); bytes > 256 {
		t.Errorf("opening the PCM fixture read %d bytes in %d reads, want its chunk headers only", bytes, reads)
	} else {
		t.Logf("pcm fixture: %d bytes in %d reads", bytes, reads)
	}

	var costs [2][2]int64
	for i, n := range []int{2000, 4000} {
		payload := testutil.SyntheticMP3Frames(n)
		raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, payload, uint32(n))
		reads, bytes := openCost(t, raw)
		t.Logf("%d frames: %d bytes in %d reads", n, bytes, reads)
		if bytes > 2048 {
			t.Errorf("opening an MP3 payload of %d frames read %d bytes, want the head of the run, under 2 KiB", n, bytes)
		}
		costs[i] = [2]int64{int64(reads), bytes}
	}
	if costs[0] != costs[1] {
		t.Errorf("opening 2000 frames cost %v (reads, bytes) and 4000 cost %v; the open must not scale with the payload", costs[0], costs[1])
	}
}
