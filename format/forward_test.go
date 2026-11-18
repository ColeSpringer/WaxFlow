package format_test

import (
	"context"
	"errors"
	"testing"

	"github.com/colespringer/waxflow/format"
)

var errWalk = errors.New("walked")

// capableMedia answers every capability a Forwarder forwards, each with an
// answer no Media without it gives.
type capableMedia struct{ format.Media }

func (capableMedia) Walk(context.Context) error                          { return errWalk }
func (capableMedia) Walked() bool                                        { return false }
func (capableMedia) MeasureSamples(context.Context) (int64, bool, error) { return 7, true, nil }
func (capableMedia) MixedWidth() bool                                    { return true }
func (capableMedia) Headroom() int64                                     { return 5 }

// bareMedia answers none of them.
type bareMedia struct{ format.Media }

// newWrapper is a wrapper written outside format, as every one is.
type newWrapper struct {
	format.Forwarder
	name string
}

func wrap(med format.Media) newWrapper { return newWrapper{Forwarder: format.Forwarder{Media: med}} }

// TestForwarderKeepsEveryCapability: a new wrapper on the forwarder passes
// every capability of what it wraps through, with no method of its own.
func TestForwarderKeepsEveryCapability(t *testing.T) {
	ctx := context.Background()
	w := wrap(capableMedia{})
	if err := format.WalkMedia(ctx, w); err != errWalk {
		t.Errorf("Walk through the wrapper = %v, want the inner walk's", err)
	}
	if format.MediaWalked(w) {
		t.Error("Walked through the wrapper is true, want the inner false")
	}
	if n, ok, err := format.MeasureMedia(ctx, w); n != 7 || !ok || err != nil {
		t.Errorf("a measure through the wrapper = %d, %v, %v; want 7", n, ok, err)
	}
	if !format.MediaMixedWidth(w) {
		t.Error("MixedWidth through the wrapper is false, want the inner true")
	}
	if got := format.MediaHeadroom(w); got != 5 {
		t.Errorf("Headroom through the wrapper = %d, want 5", got)
	}
}

// TestForwarderAnswersAsTheMediaItWraps: for each capability, a wrapped
// Media without it answers as that Media does unwrapped.
func TestForwarderAnswersAsTheMediaItWraps(t *testing.T) {
	ctx := context.Background()
	bare := bareMedia{}
	w := wrap(bare)
	t.Run("Walker", func(t *testing.T) {
		if a, b := format.WalkMedia(ctx, w), format.WalkMedia(ctx, bare); a != b {
			t.Errorf("Walk = %v wrapped, %v bare", a, b)
		}
		if a, b := format.MediaWalked(w), format.MediaWalked(bare); a != b {
			t.Errorf("Walked = %v wrapped, %v bare", a, b)
		}
	})
	t.Run("Measurer", func(t *testing.T) {
		n1, ok1, err1 := format.MeasureMedia(ctx, w)
		n2, ok2, err2 := format.MeasureMedia(ctx, bare)
		if n1 != n2 || ok1 != ok2 || err1 != err2 {
			t.Errorf("a measure = %d, %v, %v wrapped; %d, %v, %v bare", n1, ok1, err1, n2, ok2, err2)
		}
	})
	t.Run("MixedWidth", func(t *testing.T) {
		if a, b := format.MediaMixedWidth(w), format.MediaMixedWidth(bare); a != b {
			t.Errorf("MixedWidth = %v wrapped, %v bare", a, b)
		}
	})
	t.Run("Headroomer", func(t *testing.T) {
		if a, b := format.MediaHeadroom(w), format.MediaHeadroom(bare); a != b {
			t.Errorf("Headroom = %d wrapped, %d bare", a, b)
		}
	})
}
