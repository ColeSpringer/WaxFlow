package flacn_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/format"
)

// TestAMeasureIsWhatAReadDelivers: where the open does not call the length
// exact, the measure is a seek past the end, and it comes to what a read
// from the first frame counts: frames numbered off their gap are read
// there, and frames a hole leads to are placed where they are numbered. A
// walk that begins among those frames, or behind the hole, starts again
// further back.
func TestAMeasureIsWhatAReadDelivers(t *testing.T) {
	l := layOut(t, noiseStream(t, 60000))
	n := len(l.frames)
	tail := junk(4 << 10)
	for _, tc := range []struct {
		name  string
		parts [][]part
		lead  int // the lists a seek past the end walks from behind
		want  int64
	}{
		{"two frames numbered 200 on that fill their gap",
			[][]part{l.fs(0, 7), one(l.as(7, 207, 7), l.as(8, 208, 8)), l.fs(9, n), one(tail)}, 1, 60000},
		{"two frames numbered 200 on behind junk and a frame no read comes to",
			[][]part{l.fs(0, 5), one(junk(200 << 10)), one(l.gone(5), l.as(6, 206, 206), l.as(7, 207, 207), l.gone(8), l.gone(9), junk(120<<10))}, 2, 208 * 4096},
	} {
		raw, reads := l.stream(tc.parts...)
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tr := d.Tracks()[0]; tr.SamplesExact {
			t.Fatalf("%s: the open calls %d samples exact: this pins nothing", tc.name, tr.Samples)
		}
		lead, _ := l.stream(tc.parts[:tc.lead]...)
		if off, ok := d.BisectForTest(1 << 40); !ok || off < int64(len(lead)) {
			t.Fatalf("%s: a seek past the end walks from %d (%v), want %d or later: this pins nothing", tc.name, off, ok, len(lead))
		}
		// A read counts the same.
		if c := holdsCount(t, d, reads); c != tc.want {
			t.Errorf("%s: a read counted %d samples, want %d", tc.name, c, tc.want)
		}
		// MeasureLength comes to it, and a decode delivers that many.
		med, err := format.Open(container.BytesSource(raw), "flac", nil)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := waxflow.MeasureLength(context.Background(), med)
		if err != nil || tr.Samples != tc.want || !tr.SamplesExact {
			t.Errorf("%s: MeasureLength says %d, exact %v (%v), want %d exact", tc.name, tr.Samples, tr.SamplesExact, err, tc.want)
		}
		if c := decodeRest(t, med); c != tc.want {
			t.Errorf("%s: a decode delivered %d samples, want the measured %d", tc.name, c, tc.want)
		}
		med.Close()
	}
}

// holdsCount reads d to its end, holds the packets against reads, and returns
// what they count: their samples and the holes in front of them.
func holdsCount(t *testing.T, d *flacn.Demuxer, reads []part) int64 {
	t.Helper()
	var n int64
	for _, f := range holdsReads(t, "from the open", d, reads, -1) {
		n += f.dur + f.lost
	}
	return n
}
