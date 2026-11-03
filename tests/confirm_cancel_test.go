package waxflow_test

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestConfirmWalkStopsWithItsContext: the walk a run on a pipe makes to
// confirm a declared length before committing it runs on the run's context,
// through every entry: a transcode of an open span, a remux, and the span's
// own ConfirmLength. A context that ends stops it with canceled and nothing
// is written.
func TestConfirmWalkStopsWithItsContext(t *testing.T) {
	raw := truncatedMP3(t, 100)
	e := waxflow.New()

	t.Run("transcode", func(t *testing.T) {
		med := openBytes(t, raw, "mp3")
		sl, err := waxflow.Slice(med, 1000, waxflow.ToEnd)
		if err != nil {
			t.Fatal(err)
		}
		defer sl.Close()
		var out bytes.Buffer
		_, err = e.TranscodeMedia(testutil.CancelAfterChecks(t, 1), sl, &out, waxflow.TranscodeOptions{Format: "wav"})
		if waxerr.CodeOf(err) != waxerr.CodeCanceled {
			t.Fatalf("TranscodeMedia = %v, want canceled in the confirm walk", err)
		}
		if out.Len() != 0 {
			t.Errorf("%d bytes were written before the confirm walk stopped", out.Len())
		}
	})

	t.Run("remux", func(t *testing.T) {
		demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "mp3", nil)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		_, err = e.RemuxDemuxer(testutil.CancelAfterChecks(t, 1), demux, info.Default(), &out, waxflow.TranscodeOptions{Format: "mp3"})
		if waxerr.CodeOf(err) != waxerr.CodeCanceled {
			t.Fatalf("RemuxDemuxer = %v, want canceled in the confirm walk", err)
		}
		if out.Len() != 0 {
			t.Errorf("%d bytes were written before the confirm walk stopped", out.Len())
		}
	})

	t.Run("confirm length", func(t *testing.T) {
		med := openBytes(t, raw, "mp3")
		sl, err := waxflow.Slice(med, 1000, waxflow.ToEnd)
		if err != nil {
			t.Fatal(err)
		}
		defer sl.Close()
		c, ok := sl.(format.LengthConfirmer)
		if !ok {
			t.Fatal("an open span of a declared-length source must confirm its length")
		}
		if err := c.ConfirmLength(testutil.CancelAfterChecks(t, 1)); waxerr.CodeOf(err) != waxerr.CodeCanceled {
			t.Errorf("ConfirmLength = %v, want canceled", err)
		}
		if sl.Info().Default().SamplesExact {
			t.Error("a canceled confirm settled the span's length")
		}
	})
}
