package waxflow

import (
	"context"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// measureCeiling is a seek target past the end of any stream, where a landing
// clamps to the true end. It sits well short of the int64 maximum so a demuxer
// converting it to a timestamp, or adding a seek margin, cannot overflow.
const measureCeiling = int64(1) << 61

// MeasureLength returns med's default track with its length measured, exact
// and not advisory, with any trims a walk settled. The answer does not
// depend on where med is, and a media at its start (as OpenStream leaves it)
// stays there. Errors keep their codes. The cheapest route that works: a
// length the open settled costs no read; a deferred walk (MP3, Matroska,
// fragmented MP4, WMA v1/v2 frame counts) or a demuxer that measures what its
// open would not promise (WavPack; FLAC in Ogg reads its packets) reads
// headers; exact positions are seeked past the end; rounded ones whose codec
// counts no frames (WMA Pro, Voice and Lossless in ASF) are decoded from the
// start and counted, since a seek there lands on a stated time. The walk and
// the count stop with CodeCanceled when ctx ends, the walk pausing where the
// next measure of the same media resumes it; a seek past the end runs whole.
func MeasureLength(ctx context.Context, med format.Media) (container.Track, error) {
	if med == nil {
		return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: MeasureLength of a nil Media")
	}
	route, err := measureRoute(ctx, med)
	if err != nil {
		return container.Track{}, err
	}
	t := med.Info().Default()
	if route == routeSettled {
		return t, nil
	}
	var n int64
	if route == routeSeek {
		// A demuxer that measures without decoding answers first: its positions
		// are its blocks' own, which a seek would take on trust, and the
		// seek's run-up decodes what a damaged tail may not let it.
		m, ok, err := format.MeasureMedia(ctx, med)
		if err != nil {
			return container.Track{}, err
		}
		if ok {
			t.Samples = m
			t.SamplesExact, t.SamplesAdvisory, t.SamplesCounted = true, false, false
			return t, nil
		}
	}
	if route == routeDecode {
		if _, err = med.SeekSample(ctx, 0); err == nil {
			n, err = countDecoded(ctx, med)
		}
	} else {
		n, err = med.SeekSample(ctx, measureCeiling)
	}
	if err == nil {
		// Both routes left med at its end.
		_, err = med.SeekSample(ctx, 0)
	}
	if err != nil {
		return container.Track{}, err
	}
	t.Samples = n
	t.SamplesExact, t.SamplesAdvisory, t.SamplesCounted = true, false, route == routeDecode
	return t, nil
}

// lengthRoute is what MeasureLength has left to do once the open and any
// deferred walk have had their say.
type lengthRoute int

const (
	routeSettled lengthRoute = iota // the open or the walk left an exact length
	routeSeek                       // positions are exact: seek past the end
	routeDecode                     // positions are stated times: decode and count
)

// measureRoute walks med unless its open already settled the length, and
// names the route MeasureLength takes from there.
func measureRoute(ctx context.Context, med format.Media) (lengthRoute, error) {
	if t := med.Info().Default(); t.SamplesExact && t.Samples >= 0 {
		return routeSettled, nil
	}
	if err := format.WalkMedia(ctx, med); err != nil {
		return 0, err
	}
	switch t := med.Info().Default(); {
	case t.SamplesExact && t.Samples >= 0:
		return routeSettled, nil
	case t.SamplesAdvisory:
		return routeDecode, nil
	}
	return routeSeek, nil
}

// countDecoded reads med to the end and counts what it delivered: always
// right, and always the most expensive, so it is the last route.
func countDecoded(ctx context.Context, med format.Media) (int64, error) {
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, waxerr.Wrap(waxerr.CodeCanceled, "waxflow: measure canceled", err)
		}
		err := med.ReadChunk(buf)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		n += int64(buf.N)
	}
}

// SliceMeasured is Slice bounded by a measured length rather than med's
// declared one: measured is the track MeasureLength returned, or one with a
// negative Samples to leave the declaration alone. A container that rounds
// its total can declare less audio than it holds, and a bare Slice refuses a
// window ending in that gap, so measure once and slice each window, ToEnd
// included, against that total. ctx bounds the seek to the window's start,
// which the window's first read makes after this returns, so ctx must stay
// live until then: the seek can be a walk (container.Seeker) that a read
// carries no context to stop. A caller that seeks the window itself seeks
// once. It takes ownership of med and closes it on error. It refuses a span
// as MeasuredMedia does; with a negative Samples a span is sliced as it
// stands.
func SliceMeasured(ctx context.Context, med format.Media, measured container.Track, from, to int64) (format.Media, error) {
	if med == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: SliceMeasured of a nil Media")
	}
	src, err := MeasuredMedia(med, measured)
	if err != nil {
		med.Close()
		return nil, err
	}
	sl, err := newSlice(ctx, src, from, to)
	if err != nil {
		src.Close()
		return nil, err
	}
	return sl, nil
}
