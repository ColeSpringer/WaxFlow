package waxflow

import (
	"testing"

	"github.com/colespringer/waxflow/container"
)

// walkedSource is a lengthWalker whose walk has already run, as a seek that
// counted to the end or a sidecar restored complete leaves one.
type walkedSource struct{ walks int }

func (w *walkedSource) Walk() error  { w.walks++; return nil }
func (w *walkedSource) Walked() bool { return true }

// TestAWalkedSourceStillConfirms: a walk that finished some other way has not
// put its measurement on the track yet; Walk is what does, at no cost then.
// So a stated count on a walker is confirmed by its Walk whatever Walked
// says, and a run never projects the unchecked count beside a finished walk.
func TestAWalkedSourceStillConfirms(t *testing.T) {
	src := &walkedSource{}
	confirm := confirmableLength(container.Track{Samples: 1000}, src)
	if confirm == nil {
		t.Fatal("a stated count on a walked source asked for no confirming")
	}
	if err := confirm(); err != nil || src.walks != 1 {
		t.Errorf("the confirming ran %d walks (%v), want the source's own Walk", src.walks, err)
	}
	if confirmableLength(container.Track{Samples: 1000, SamplesExact: true}, src) != nil {
		t.Error("an exact count asked for confirming")
	}
}
