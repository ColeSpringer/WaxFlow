package asf_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/waxerr"
)

// foldAt is where ws lists foldedStream's fold, -1 where it does not.
func foldAt(ws []container.Warning) int64 {
	if i := slices.IndexFunc(ws, func(w container.Warning) bool { return w.Msg == "presentation times run backwards" }); i >= 0 {
		return ws[i].Offset
	}
	return -1
}

// walkedCount opens raw under opts and walks its frame count.
func walkedCount(t *testing.T, raw []byte, opts *container.DemuxerOptions) *asf.Demuxer {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestASFIndexBlobIsBaseIndependent: the findings a sidecar carries are
// stored at the source's offsets, so a count taken behind a leading tag is
// the blob a count taken without one is.
func TestASFIndexBlobIsBaseIndependent(t *testing.T) {
	raw := foldedStream(t)
	bare := walkedCount(t, raw, nil).IndexSnapshot()
	if bare == nil {
		t.Fatal("the count left no sidecar: this pins nothing")
	}
	if got := walkedCount(t, raw, &container.DemuxerOptions{Base: 4096}).IndexSnapshot(); !bytes.Equal(got, bare) {
		t.Error("the sidecar a count took behind a leading tag differs from the one taken without")
	}
}

// TestReplayedFindingIsTranslatedOnce: what the frame count holds for Walk to
// report, its own finding or a restored sidecar's, is reported behind a
// leading tag at its offset in the file, translated once.
func TestReplayedFindingIsTranslatedOnce(t *testing.T) {
	raw := foldedStream(t)
	const base = 4096
	bare := walkedCount(t, raw, nil)
	at := foldAt(bare.Warnings())
	if at < 0 {
		t.Fatal("the walk found no fold: this pins nothing")
	}
	blob := bare.IndexSnapshot()

	if got := foldAt(walkedCount(t, raw, &container.DemuxerOptions{Base: base}).Warnings()); got != at+base {
		t.Errorf("a walk behind %d bytes reported the fold at %d, want %d", base, got, at+base)
	}
	r, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if !r.RestoreIndex(blob) {
		t.Fatal("the sidecar was refused")
	}
	if err := r.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := foldAt(r.Warnings()); got != at+base {
		t.Errorf("a restore behind %d bytes reported the fold at %d, want %d", base, got, at+base)
	}
	s, err := asf.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if !s.RestoreIndex(blob) {
		t.Fatal("the sidecar was refused under Strict")
	}
	err = s.Walk(context.Background())
	if want := fmt.Sprintf("wma: presentation times run backwards (at offset %d)", at+base); waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != want {
		t.Errorf("a strict restore behind %d bytes walked to %v, want %q", base, err, want)
	}
}
