package mpegframes

import (
	"context"
	"reflect"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestANilReaderIndexesNothing: without a frame run, Indexed keeps no index,
// declines a snapshot, has nothing to walk, and leaves the track as it was.
func TestANilReaderIndexesNothing(t *testing.T) {
	track := container.Track{Samples: 7, SamplesExact: true}
	want := track
	for name, ix := range map[string]Indexed{"zero": {}, "nil reader": NewIndexed(nil, &track)} {
		if ix.KeepsIndex() || ix.IndexSnapshot() != nil || ix.RestoreIndex([]byte(idxMagic+"\x01\x01\x00")) || !ix.Walked() {
			t.Errorf("%s: KeepsIndex %v, IndexSnapshot %v, RestoreIndex accepted or Walked %v", name, ix.KeepsIndex(), ix.IndexSnapshot(), ix.Walked())
		}
		if err := ix.Walk(context.Background()); err != nil {
			t.Errorf("%s: Walk = %v", name, err)
		}
	}
	if !reflect.DeepEqual(track, want) {
		t.Errorf("the walk moved the track to %+v", track)
	}
}

// TestIndexedWalkSettlesTheTrack: a walk counts the run and settles the
// owner's track at it, and the index it built restores into another open.
func TestIndexedWalkSettlesTheTrack(t *testing.T) {
	const n = IdxMinFrames + 50
	data := frames(n)
	w, _, _ := begin(t, data, &walkOpts{})
	track := container.Track{Samples: -1}
	ix := NewIndexed(w.Reader(), &track)
	if !ix.KeepsIndex() || ix.Walked() {
		t.Fatalf("before the walk: KeepsIndex %v, Walked %v", ix.KeepsIndex(), ix.Walked())
	}
	if err := ix.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ix.Walked() || !track.SamplesExact || track.Samples != n*frameSPF {
		t.Errorf("after the walk: Walked %v, track %d samples, exact %v; want %d exact", ix.Walked(), track.Samples, track.SamplesExact, n*frameSPF)
	}
	w2, _, _ := begin(t, data, &walkOpts{})
	if !NewIndexed(w2.Reader(), &container.Track{Samples: -1}).RestoreIndex(ix.IndexSnapshot()) {
		t.Error("the snapshot was declined by an identical source")
	}
}
