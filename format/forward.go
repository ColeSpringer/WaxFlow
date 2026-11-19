package format

import (
	"context"
	"io"

	"github.com/colespringer/waxflow/waxerr"
)

// Forwarder is what a wrapper of a Media embeds in place of the Media, to
// pass on the optional capabilities a wrapper keeps: Walker, Measurer,
// MixedWidth and Headroomer. Embedding Media alone promotes none of them,
// so a wrapper that forwarded each by hand dropped any it missed. Where the
// inner Media lacks one, the answer is the one that Media gives unwrapped:
// nothing to walk, no measure, one width, no headroom.
//
// LengthConfirmer and Composite are not forwarded: implementing either is
// itself the answer, so neither has one that says "not supported". A wrapper
// that keeps the members adds Composite in a type of its own.
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

// WithCloser returns med with c closed after it; the first error wins. It
// refuses a Media that confirms its own length, which the wrapper would hide.
// That refusal and a nil med or c are CodeInvalidRequest, with nothing
// closed. Close closes both once, and a later Close returns nil. Where med
// is a Composite the result answers Members as med does.
func WithCloser(med Media, c io.Closer) (Media, error) {
	if med == nil || c == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "format: WithCloser of a nil Media or closer")
	}
	if _, ok := med.(LengthConfirmer); ok {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"format: WithCloser of a Media that confirms its own length")
	}
	w := &withCloser{Forwarder: Forwarder{Media: med}, c: c}
	if members, ok := med.(Composite); ok {
		return compositeCloser{withCloser: w, Composite: members}, nil
	}
	return w, nil
}

type withCloser struct {
	Forwarder
	c      io.Closer
	closed bool
}

func (m *withCloser) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	err := m.Media.Close()
	if cerr := m.c.Close(); err == nil {
		err = cerr
	}
	return err
}

// compositeCloser is a withCloser over a Composite: a closer changes no
// member, so the members are the wrapped Media's.
type compositeCloser struct {
	*withCloser
	Composite
}
