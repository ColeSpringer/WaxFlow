package mka

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestTrimsAreUnreadUntilAWalk: a track opens with its trims marked unread,
// since they ride on its blocks, and the walk that reads them says so.
func TestTrimsAreUnreadUntilAWalk(t *testing.T) {
	p := BuildMidPadding(t)
	d, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; !tr.TrimsUnread || tr.Padding != 0 || tr.MidPadding != 0 {
		t.Fatalf("the track opens with TrimsUnread %v, Padding %d, MidPadding %d; want true, 0, 0", tr.TrimsUnread, tr.Padding, tr.MidPadding)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.TrimsUnread || tr.Padding != p.EndClamped() || tr.MidPadding != p.MidClamped() {
		t.Fatalf("the walk leaves TrimsUnread %v, Padding %d, MidPadding %d; want false, %d, %d",
			tr.TrimsUnread, tr.Padding, tr.MidPadding, p.EndClamped(), p.MidClamped())
	}
}
