package adts_test

import (
	"errors"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

// failAfter serves a stream up to limit and fails every read past it.
type failAfter struct {
	data  []byte
	limit int64
}

func (s failAfter) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > s.limit {
		return 0, errors.New("source is down")
	}
	return copy(p, s.data[off:]), nil
}

func (s failAfter) Size() int64 { return int64(len(s.data)) }

// TestWalkSurfacesAReadFailure pins that a read failing in the middle of the
// walk comes back as the error, from a stream long enough that the failure
// lands past the first window: the combined frame-and-header read used to
// slice a nil view there and panic.
func TestWalkSurfacesAReadFailure(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	raw, _ := encodeADTS(t, f, museSrc(44100*24, 2, 44100))
	if len(raw) < 2*srcwin.Chunk {
		t.Fatalf("the stream is %d bytes, want over two windows", len(raw))
	}
	src := failAfter{data: raw, limit: int64(len(raw)) - srcwin.Chunk/2}
	d, err := adts.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(); !errors.Is(err, waxerr.ErrSourceUnreadable) {
		t.Fatalf("Walk = %v, want the read failure as source-unreadable", err)
	}
	if d.Walked() {
		t.Error("a walk stopped by a read failure claims to be finished")
	}
}
