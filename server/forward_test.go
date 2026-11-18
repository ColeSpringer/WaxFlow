package server

import (
	"context"
	"errors"
	"testing"

	"github.com/colespringer/waxflow/format"
)

var errWalk = errors.New("walked")

// capableMedia answers every capability a wrapper forwards, each with an
// answer no Media without it gives.
type capableMedia struct{ format.Media }

func (capableMedia) Walk(context.Context) error                          { return errWalk }
func (capableMedia) Walked() bool                                        { return false }
func (capableMedia) MeasureSamples(context.Context) (int64, bool, error) { return 7, true, nil }
func (capableMedia) MixedWidth() bool                                    { return true }
func (capableMedia) Headroom() int64                                     { return 5 }

// TestClosingMediaForwardsEveryCapability: every capability of the Media a wrapper holds
// answers through it.
func TestClosingMediaForwardsEveryCapability(t *testing.T) {
	ctx := context.Background()
	inner := format.Forwarder{Media: capableMedia{}}
	for _, med := range []format.Media{closingMedia{Forwarder: inner}} {
		n, ok, err := format.MeasureMedia(ctx, med)
		if format.WalkMedia(ctx, med) != errWalk || format.MediaWalked(med) || n != 7 || !ok || err != nil ||
			!format.MediaMixedWidth(med) || format.MediaHeadroom(med) != 5 {
			t.Errorf("%T drops a capability of the Media it wraps", med)
		}
	}
}
