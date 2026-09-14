package riff

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

// TestOpenReadsTheHeader pins what opening a WAV costs, and that the cost does
// not depend on the payload. A PCM file is its chunk headers: the RIFF header,
// the fmt chunk, the header of the LIST chunk ffmpeg writes and the data
// chunk header, 52 bytes in 5 reads on the fixture. An MP3 payload adds the head of the frame run (the first header, one frame,
// the header behind it) and nothing else: the index is lazy, and before the
// walker's reads were exact this open cost a whole 128 KiB window for it. The
// synthetic payloads are longer than two windows because every committed
// fixture is smaller than one, where a round-up cannot show.
func TestOpenReadsTheHeader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sine-s16.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if reads, bytes := openCost(t, raw); reads != 5 || bytes != 52 {
		t.Errorf("opening the PCM fixture read %d bytes in %d reads, want its 52 bytes of chunk headers in 5", bytes, reads)
	}

	var costs [2][2]int64
	for i, n := range []int{2000, 4000} {
		payload := testutil.SyntheticMP3Frames(n)
		raw := wavHeader(tagMP3, 2, 44100, 1152, 0, mp3Extra(0), payload, -1)
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
