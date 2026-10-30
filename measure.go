package waxflow

import (
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
// read, a deferred walk (MP3, Matroska, fragmented MP4) reads headers, a source
// with exact positions is seeked past its end, and one whose positions are
// rounded (ASF) is decoded from the start and counted, since a seek there lands
// short of what the decode delivers.
func MeasureLength(med format.Media) (container.Track, error) {
	if med == nil {
		return container.Track{}, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: MeasureLength of a nil Media")
	}
	if t := med.Info().Default(); t.SamplesExact && t.Samples >= 0 {
		return t, nil
	}
	if err := format.WalkMedia(med); err != nil {
		return container.Track{}, err
	}
	if t := med.Info().Default(); t.SamplesExact && t.Samples >= 0 {
		return t, nil
	}
	t := med.Info().Default()
	var n int64
	var err error
	if t.SamplesAdvisory {
		if _, err = med.SeekSample(0); err == nil {
			n, err = countDecoded(med)
		}
	} else {
		n, err = med.SeekSample(measureCeiling)
	}
	if err == nil {
		// Both routes left med at its end.
		_, err = med.SeekSample(0)
	}
	if err != nil {
		return container.Track{}, err
	}
	t.Samples = n
	t.SamplesExact, t.SamplesAdvisory = true, false
	return t, nil
}

// countDecoded reads med to the end and counts what it delivered: always
// right, and always the most expensive, so it is the last route.
func countDecoded(med format.Media) (int64, error) {
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
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
// declared one: samples is the source's total as MeasureLength found it, or
// negative to leave the declaration alone. A container that rounds its total
// can declare less audio than it holds, and a window ending in that gap is one
// the file can serve but a bare Slice refuses. Measure once and slice each
// window of a source against the same total. It takes ownership of med and
// closes it on error.
func SliceMeasured(med format.Media, samples, from, to int64) (format.Media, error) {
	if med == nil {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "waxflow: SliceMeasured of a nil Media")
	}
	med = MeasuredMedia(med, samples)
	sl, err := Slice(med, from, to)
	if err != nil {
		med.Close()
		return nil, err
	}
	return sl, nil
}
