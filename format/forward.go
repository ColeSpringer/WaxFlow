package format

import "context"

// Forwarder is what a wrapper of a Media embeds in place of the Media, to
// pass on the optional capabilities a wrapper keeps: Walker, Measurer,
// MixedWidth and Headroomer. Embedding Media alone promotes none of them,
// so a wrapper that forwarded each by hand dropped any it missed. Where the
// inner Media lacks one, the answer is the one that Media gives unwrapped:
// nothing to walk, no measure, one width, no headroom.
//
// LengthConfirmer and Composite are not forwarded: implementing either is
// itself the answer, so neither has one that says "not supported".
type Forwarder struct {
	Media
}

func (f Forwarder) Walk(ctx context.Context) error { return WalkMedia(ctx, f.Media) }

func (f Forwarder) Walked() bool { return MediaWalked(f.Media) }

func (f Forwarder) MeasureSamples(ctx context.Context) (int64, bool, error) {
	return MeasureMedia(ctx, f.Media)
}

func (f Forwarder) MixedWidth() bool { return MediaMixedWidth(f.Media) }

func (f Forwarder) Headroom() int64 { return MediaHeadroom(f.Media) }
