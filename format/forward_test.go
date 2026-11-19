package format_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
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

// claimingMedia confirms its own length and names its members, the two
// capabilities whose presence alone is the answer.
type claimingMedia struct{ format.Media }

func (claimingMedia) ConfirmLength(context.Context) error { return nil }
func (claimingMedia) Members() []container.Track          { return nil }

// compositeMedia is a capableMedia that names its members and confirms
// nothing.
type compositeMedia struct {
	capableMedia
	members []container.Track
}

func (m compositeMedia) Members() []container.Track { return m.members }

// TestForwarderNeverConfirmsLength: nothing built on a Forwarder confirms a
// length, whatever it wraps, and a Forwarder alone names no members.
func TestForwarderNeverConfirmsLength(t *testing.T) {
	closed, err := format.WithCloser(compositeMedia{}, io.NopCloser(nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, med := range map[string]format.Media{
		"a Forwarder":           format.Forwarder{Media: claimingMedia{}},
		"a wrapper on one":      wrap(claimingMedia{}),
		"a Media with a closer": closed,
	} {
		if _, ok := med.(format.LengthConfirmer); ok {
			t.Errorf("%s is a LengthConfirmer", name)
		}
	}
	for name, med := range map[string]format.Media{
		"a Forwarder":      format.Forwarder{Media: claimingMedia{}},
		"a wrapper on one": wrap(claimingMedia{}),
	} {
		if _, ok := med.(format.Composite); ok {
			t.Errorf("%s is a Composite", name)
		}
	}
}

// TestWithCloserKeepsTheMembers: a Media with a closer names the members the
// Media under it names, and none where that Media names none.
func TestWithCloserKeepsTheMembers(t *testing.T) {
	members := []container.Track{{Samples: 3}, {Samples: 5}}
	med, err := format.WithCloser(compositeMedia{members: members}, io.NopCloser(nil))
	if err != nil {
		t.Fatal(err)
	}
	c, ok := med.(format.Composite)
	if !ok {
		t.Fatal("a Composite with a closer is not a Composite")
	}
	if got := c.Members(); len(got) != 2 || got[0].Samples != 3 || got[1].Samples != 5 {
		t.Errorf("Members = %v, want the wrapped Media's", got)
	}
	if !format.MediaMixedWidth(med) || format.MediaHeadroom(med) != 5 {
		t.Error("a Composite with a closer drops a capability of the Media it wraps")
	}
	bare, err := format.WithCloser(capableMedia{}, io.NopCloser(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bare.(format.Composite); ok {
		t.Error("a Media that names no members is a Composite once it has a closer")
	}
}

// TestWithCloserRefusesNil: a nil Media or a nil closer is the caller's
// mistake, refused where it is made.
func TestWithCloserRefusesNil(t *testing.T) {
	for name, build := range map[string]func() (format.Media, error){
		"a nil Media":  func() (format.Media, error) { return format.WithCloser(nil, io.NopCloser(nil)) },
		"a nil closer": func() (format.Media, error) { return format.WithCloser(capableMedia{}, nil) },
	} {
		if med, err := build(); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || med != nil {
			t.Errorf("%s = a %T, %v; want no Media and %s", name, med, err, waxerr.CodeInvalidRequest)
		}
	}
}

// TestWithCloserForwardsEveryCapability: a Media with a closer answers every
// forwarded capability as the Media under it does.
func TestWithCloserForwardsEveryCapability(t *testing.T) {
	ctx := context.Background()
	med, err := format.WithCloser(capableMedia{}, io.NopCloser(nil))
	if err != nil {
		t.Fatal(err)
	}
	n, ok, err := format.MeasureMedia(ctx, med)
	if format.WalkMedia(ctx, med) != errWalk || format.MediaWalked(med) || n != 7 || !ok || err != nil ||
		!format.MediaMixedWidth(med) || format.MediaHeadroom(med) != 5 {
		t.Error("a Media with a closer drops a capability of the Media it wraps")
	}
}

// closeMedia records its Close and fails it with err.
type closeMedia struct {
	format.Media
	err   error
	order *[]string
}

func (m closeMedia) Close() error {
	*m.order = append(*m.order, "media")
	return m.err
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestWithCloserReportsTheFirstError: Close closes the Media and then the
// closer, whichever fails, and reports the first error of the two.
func TestWithCloserReportsTheFirstError(t *testing.T) {
	errMedia, errCloser := errors.New("media close"), errors.New("closer close")
	for _, tc := range []struct {
		name          string
		media, closer error
		want          error
	}{
		{"neither fails", nil, nil, nil},
		{"the closer fails", nil, errCloser, errCloser},
		{"the Media fails", errMedia, nil, errMedia},
		{"both fail", errMedia, errCloser, errMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			med, err := format.WithCloser(closeMedia{err: tc.media, order: &order}, closerFunc(func() error {
				order = append(order, "closer")
				return tc.closer
			}))
			if err != nil {
				t.Fatal(err)
			}
			if got := med.Close(); got != tc.want {
				t.Errorf("Close = %v, want %v", got, tc.want)
			}
			if len(order) != 2 || order[0] != "media" || order[1] != "closer" {
				t.Errorf("closed %v, want the media and then the closer", order)
			}
		})
	}
}

// TestWithCloserClosesOnce: the first Close closes both and reports their
// error; a later one closes nothing and returns nil.
func TestWithCloserClosesOnce(t *testing.T) {
	errCloser := errors.New("closer close")
	var order []string
	med, err := format.WithCloser(closeMedia{order: &order}, closerFunc(func() error {
		order = append(order, "closer")
		return errCloser
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := med.Close(); got != errCloser {
		t.Errorf("the first Close = %v, want %v", got, errCloser)
	}
	if got := med.Close(); got != nil {
		t.Errorf("the second Close = %v, want nil", got)
	}
	if len(order) != 2 {
		t.Errorf("closed %v, want the media and the closer once each", order)
	}
}
