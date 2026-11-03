package waxflow_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestSeekStopsAWalkWhenCanceled: on every sweep row the measure walks for, a
// seek that drives that walk ends with its context, and a seek with a live
// context then lands where a fresh media lands.
func TestSeekStopsAWalkWhenCanceled(t *testing.T) {
	for _, c := range measureSweep {
		if c.route != "walk" {
			continue
		}
		t.Run(c.what, func(t *testing.T) {
			raw, err := os.ReadFile(repoPath(c.path...))
			if err != nil {
				t.Fatal(err)
			}
			hint := filepath.Ext(c.path[len(c.path)-1])[1:]
			fresh, err := format.Open(container.BytesSource(raw), hint, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			tr := fresh.Info().Default()
			target := tr.Samples / 2
			if tr.Samples <= 0 {
				target = 10 * int64(tr.Fmt.Rate)
			}
			want, err := fresh.SeekSample(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			med, err := format.Open(container.BytesSource(raw), hint, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			if _, err := med.SeekSample(testutil.CancelAfterChecks(t, 2), target); waxerr.CodeOf(err) != waxerr.CodeCanceled {
				t.Fatalf("a seek whose context ended returned %v, want canceled", err)
			}
			if got, err := med.SeekSample(t.Context(), target); err != nil || got != want {
				t.Errorf("the seek after the cancel landed at %d (%v), a fresh media at %d", got, err, want)
			}
		})
	}
}

// TestSliceMeasuredPositionsUnderItsContext: the seek a measured window runs
// on its first read is bounded by the context the window was built with; the
// first chunk is still the window's own start, not a splice.
func TestSliceMeasuredPositionsUnderItsContext(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "container", "mka", "testdata", "seed-pcm.mka"))
	if err != nil {
		t.Fatal(err)
	}
	unmeasured := container.Track{Samples: -1}
	ended, err := waxflow.SliceMeasured(testutil.CancelAfterChecks(t, 2), openBytes(t, raw, "mka"), unmeasured, 1000, waxflow.ToEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer ended.Close()
	buf := audio.Get(ended.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	if err := ended.ReadChunk(buf); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("the first read of a window built under an ended context returned %v, want canceled", err)
	}
	sl, err := waxflow.SliceMeasured(t.Context(), openBytes(t, raw, "mka"), unmeasured, 1000, waxflow.ToEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	if err := sl.ReadChunk(buf); err != nil {
		t.Fatal(err)
	}
	if buf.Pos != 0 || buf.Discont {
		t.Errorf("the window's first chunk is at %d (discont %v), want 0 and continuous", buf.Pos, buf.Discont)
	}
}

// seekCounter counts the seeks run through a Media.
type seekCounter struct {
	format.Media
	seeks int
}

func (m *seekCounter) SeekSample(ctx context.Context, target int64) (int64, error) {
	m.seeks++
	return m.Media.SeekSample(ctx, target)
}

// TestSliceMeasuredSeeksOnce: a measured window positions itself under the
// context it was built with, once, on its first use. A caller that seeks the
// window itself pays one seek, not a build-time one and its own.
func TestSliceMeasuredSeeksOnce(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	unmeasured := container.Track{Samples: -1}
	counting := &seekCounter{Media: openBytes(t, raw, "mp3")}
	sl, err := waxflow.SliceMeasured(t.Context(), counting, unmeasured, 1000, waxflow.ToEnd)
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	if counting.seeks != 0 {
		t.Errorf("building the window ran %d seeks, want none until it is used", counting.seeks)
	}
	if _, err := sl.SeekSample(t.Context(), 500); err != nil {
		t.Fatal(err)
	}
	if counting.seeks != 1 {
		t.Errorf("a seek on the window ran %d seeks on its source, want one", counting.seeks)
	}
}

// TestSliceRefusesANilMediaWithNoMedia: a refused Slice returns no Media at
// all, so a caller that closes what it was given on success does not close a
// nil span.
func TestSliceRefusesANilMediaWithNoMedia(t *testing.T) {
	sl, err := waxflow.Slice(nil, 0, waxflow.ToEnd)
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
		t.Fatalf("Slice(nil) = %v, want invalid-request", err)
	}
	if sl != nil {
		t.Errorf("Slice(nil) returned a Media (%T) beside its error", sl)
	}
}

// TestPacketGridStopsWithItsContext: the packet walk a grid takes ends with
// its context, and a live one then measures the grid.
func TestPacketGridStopsWithItsContext(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	e := waxflow.New()
	if _, err := e.PacketGrid(testutil.CancelAfterChecks(t, 2), container.BytesSource(raw), "mp3"); waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("PacketGrid under an ended context = %v, want canceled", err)
	}
	if grid, err := e.PacketGrid(t.Context(), container.BytesSource(raw), "mp3"); err != nil || grid != 1152 {
		t.Errorf("PacketGrid = %d, %v; want the MP3 frame of 1152", grid, err)
	}
}
