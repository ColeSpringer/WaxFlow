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
// and not advisory, with any trims a walk settled. The answer does not depend
// on where med is, and a media at its start (as OpenStream leaves it) is left
// there. Errors keep their codes.
//
// It takes the cheapest route that works: a length the open settled costs no
// read, a deferred walk (MP3, Matroska, fragmented MP4, the frame counts of a
// WMA v1/v2 track) reads headers, a source with exact positions is seeked past
// its end, and one whose positions are rounded and whose codec counts no
// frames (WMA Pro, Voice and Lossless in ASF) is decoded from the start and
// counted, since a seek there lands on a stated time, not on the decode. The
// walk and the count both stop when ctx ends, with CodeCanceled, the walk
// pausing where the next measure of the same media resumes it; a seek past
// the end is a few reads and runs whole.
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
// declared one: measured is the source's track as MeasureLength found it, or
// one with a negative Samples to leave the declaration alone. A container that rounds its total
// can declare less audio than it holds, and a window ending in that gap is one
// the file can serve but a bare Slice refuses. Measure once and slice each
// window of a source against the same total, which a ToEnd window is held to
// as well (see Slice). ctx bounds the seek the window's first read runs to
// its start, which can be a walk (container.Seeker) that a read carries no
// context to stop; a caller that seeks the window itself seeks once. It takes
// ownership of med and closes it on error.
func SliceMeasured(ctx context.Context, med format.Media, measured container.Track, from, to int64) (format.Media, error) {
	if med == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: SliceMeasured of a nil Media")
	}
	med = MeasuredMedia(med, measured)
	sl, err := newSlice(ctx, med, from, to)
	if err != nil {
		med.Close()
		return nil, err
	}
	return sl, nil
}
