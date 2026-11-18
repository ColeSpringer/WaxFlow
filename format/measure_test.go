package format

import (
	"context"
	"errors"
	"testing"
)

// measuringDemux is a demuxer that measures its track as told.
type measuringDemux struct {
	historyDemux
	n   int64
	err error
}

func (d *measuringDemux) MeasureSamples(context.Context, int) (int64, error) { return d.n, d.err }

// TestMeasureMediaAsksTheDemuxer: a Media answers a measure from a demuxer
// that takes one, and has none where the demuxer has none or does not measure.
func TestMeasureMediaAsksTheDemuxer(t *testing.T) {
	ctx := context.Background()
	down := errors.New("down")
	for _, tc := range []struct {
		name string
		n    int64
		err  error
		ok   bool
	}{
		{"a length", 4096, nil, true},
		{"no answer", -1, nil, false},
		{"a failed walk", -1, down, false},
	} {
		med := &media{demux: &measuringDemux{n: tc.n, err: tc.err}}
		if n, ok, err := MeasureMedia(ctx, med); ok != tc.ok || err != tc.err || ok && n != tc.n {
			t.Errorf("%s: measured %d, %v, %v; want %d, %v, %v", tc.name, n, ok, err, tc.n, tc.ok, tc.err)
		}
	}
	if n, ok, err := MeasureMedia(ctx, &media{demux: &historyDemux{}}); ok || err != nil {
		t.Errorf("a demuxer that measures nothing answered %d, %v, %v", n, ok, err)
	}
}
