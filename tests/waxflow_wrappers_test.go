package waxflow_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// measuredAs is MeasuredMedia for a source it accepts.
func measuredAs(t *testing.T, med format.Media, measured container.Track) format.Media {
	t.Helper()
	m, err := waxflow.MeasuredMedia(med, measured)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// openSpan is a span to the end of a 10000-sample WAV from sample 1000, and
// the length it declares.
func openSpan(t *testing.T) (format.Media, int64) {
	t.Helper()
	raw, _ := ratedWAV(t, 48000, 1, 10000, 9)
	span := sliceOf(t, waxflow.New(), raw, 1000, waxflow.ToEnd)
	t.Cleanup(func() { span.Close() })
	if _, ok := span.(format.LengthConfirmer); !ok {
		t.Fatal("a span does not confirm its own length; these cells need one that does")
	}
	return span, span.Info().Default().Samples
}

// wantSpanRefused checks a wrapper's refusal of span: the request's fault,
// no Media, and the span left open with every sample still to read.
func wantSpanRefused(t *testing.T, got format.Media, err error, span format.Media, length int64) {
	t.Helper()
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || got != nil {
		t.Fatalf("over a span = a %T, %v; want no Media and %s", got, err, waxerr.CodeInvalidRequest)
	}
	if n := drain(t, span); n != length {
		t.Errorf("the refused span read %d samples, want its %d", n, length)
	}
}

// TestMeasuredMediaRefusesASpan: a span confirms its own length, which a
// Media restating that length would hide, so the restatement is refused.
func TestMeasuredMediaRefusesASpan(t *testing.T) {
	span, length := openSpan(t)
	got, err := waxflow.MeasuredMedia(span, measuredLen(length-1))
	wantSpanRefused(t, got, err, span, length)
}

// TestWithCloserRefusesASpan: the same refusal from a Media with a closer,
// and the closer is left alone.
func TestWithCloserRefusesASpan(t *testing.T) {
	span, length := openSpan(t)
	closed := false
	got, err := format.WithCloser(span, closerFunc(func() error {
		closed = true
		return nil
	}))
	wantSpanRefused(t, got, err, span, length)
	if closed {
		t.Error("the refusal closed the closer")
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestSliceMeasuredOverASpanWithNoMeasure: with no measure to restate, or one
// the span already declares, there is no wrapper to build, so a span of a
// span is still served.
func TestSliceMeasuredOverASpanWithNoMeasure(t *testing.T) {
	t.Run("no measure", func(t *testing.T) {
		span, length := openSpan(t)
		sl, err := waxflow.SliceMeasured(context.Background(), span, container.Track{Samples: -1}, 500, waxflow.ToEnd)
		if err != nil {
			t.Fatalf("SliceMeasured over a span with no measure = %v, want it served", err)
		}
		defer sl.Close()
		if n := drain(t, sl); n != length-500 {
			t.Errorf("the inner span read %d samples, want %d", n, length-500)
		}
	})
	t.Run("the measure a bounded span declares", func(t *testing.T) {
		const length = 8000
		raw, _ := ratedWAV(t, 48000, 1, 10000, 9)
		span := sliceOf(t, waxflow.New(), raw, 1000, 1000+length)
		if got, err := waxflow.MeasuredMedia(span, measuredLen(length)); err != nil || got != span {
			span.Close()
			t.Fatalf("MeasuredMedia at the span's own exact length = a %T, %v; want the span untouched", got, err)
		}
		sl, err := waxflow.SliceMeasured(context.Background(), span, measuredLen(length), 500, waxflow.ToEnd)
		if err != nil {
			t.Fatalf("SliceMeasured at the span's own exact length = %v, want it served", err)
		}
		defer sl.Close()
		if n := drain(t, sl); n != length-500 {
			t.Errorf("the inner span read %d samples, want %d", n, length-500)
		}
	})
}

// TestSliceMeasuredOverASpanClosesWhatItRefuses: SliceMeasured owns its
// source, so a span it cannot restate is closed with the refusal.
func TestSliceMeasuredOverASpanClosesWhatItRefuses(t *testing.T) {
	span, length := openSpan(t)
	sl, err := waxflow.SliceMeasured(context.Background(), span, measuredLen(length-1), 0, waxflow.ToEnd)
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || sl != nil {
		t.Fatalf("SliceMeasured over a span = a %T, %v; want no Media and %s", sl, err, waxerr.CodeInvalidRequest)
	}
	buf := audio.Get(span.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	if err := span.ReadChunk(buf); waxerr.CodeOf(err) != waxerr.CodeInternal {
		t.Errorf("a read of the refused span = %v, want a closed span's refusal", err)
	}
}

// mediaWrapper builds one wrapper over a Media.
type mediaWrapper struct {
	name string
	wrap func(t *testing.T, med format.Media) format.Media
}

// timelineWrappers are the two wrappers that keep a timeline's members.
var timelineWrappers = []mediaWrapper{
	{"with a closer", func(t *testing.T, med format.Media) format.Media {
		w, err := format.WithCloser(med, closerFunc(func() error { return nil }))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}},
	{"measured", func(t *testing.T, med format.Media) format.Media {
		w := measuredAs(t, med, countedLen(med.Info().Default().Samples))
		if w == med {
			t.Fatal("MeasuredMedia returned its source; this cell needs the wrapper")
		}
		return w
	}},
}

// TestAWrappedTimelineKeepsItsMembers: neither a closer nor a restated length
// changes a timeline's members, so the wrapped timeline names them as the
// bare one does, and a wrapped file still names none.
func TestAWrappedTimelineKeepsItsMembers(t *testing.T) {
	e := waxflow.New()
	a, _ := ratedWAV(t, 44100, 2, 4410, 61)
	b, _ := ratedWAV(t, 48000, 2, 4800, 62)
	for _, w := range timelineWrappers {
		t.Run(w.name, func(t *testing.T) {
			tl, err := waxflow.Concat(timelineMembers(t, e, a, b), waxflow.ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := tl.(format.Composite).Members()
			med := w.wrap(t, tl)
			defer med.Close()
			c, ok := med.(format.Composite)
			if !ok {
				t.Fatal("the wrapped timeline is not a format.Composite")
			}
			if got := c.Members(); !reflect.DeepEqual(got, want) || len(got) != 2 {
				t.Errorf("Members = %v through the wrapper, want the timeline's %v", got, want)
			}

			file, err := e.OpenStream(container.BytesSource(a), "wav")
			if err != nil {
				t.Fatal(err)
			}
			wrapped := w.wrap(t, file)
			defer wrapped.Close()
			if _, ok := wrapped.(format.Composite); ok {
				t.Error("a wrapped file is a format.Composite")
			}
		})
	}
}

// TestAWrappedTimelineStillAnnouncesItsFold: a 5.1 member folded before the
// seam is announced once per run, and the wrappers keep the member list the
// announcement reads.
func TestAWrappedTimelineStillAnnouncesItsFold(t *testing.T) {
	five, _ := ratedWAV(t, 48000, 6, 4096, 63)
	stereo, _ := ratedWAV(t, 48000, 2, 4096, 64)
	bare := mediaWrapper{"bare", func(_ *testing.T, med format.Media) format.Media { return med }}
	for _, w := range append([]mediaWrapper{bare}, timelineWrappers...) {
		t.Run(w.name, func(t *testing.T) {
			var logged bytes.Buffer
			e := waxflow.New(waxflow.WithLogger(slog.New(slog.NewTextHandler(&logged, nil))))
			opts := waxflow.TranscodeOptions{Format: "opus"}
			members := timelineMembers(t, e, five, stereo)
			ch, err := e.TimelineChannels([]container.Track{members[0].Track, members[1].Track}, opts)
			if err != nil {
				t.Fatal(err)
			}
			tl, err := waxflow.Concat(members, waxflow.ConcatOptions{Channels: ch})
			if err != nil {
				t.Fatal(err)
			}
			med := w.wrap(t, tl)
			defer med.Close()
			if _, err := e.TranscodeMedia(context.Background(), med, io.Discard, opts); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(logged.String(), "downmixed to fit the output format"); n != 1 {
				t.Errorf("the fold was announced %d times, want 1:\n%s", n, logged.String())
			}
		})
	}
}
