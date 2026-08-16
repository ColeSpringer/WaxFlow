package mp4

import "testing"

// TestBuildTimeBaseHEAACTimescale pins the interaction between the ASC-
// authoritative rate and the media timescale for HE-AAC.
//
// Since the SBR decode landed, an HE-AAC track reports the extension rate
// (48000) and each AU decodes to 2048 output samples. HE-AAC M4As are
// commonly muxed with the media timescale at that output rate (mdhd 48000,
// stts deltas 2048), but some writers keep the core timescale (24000 with
// 1024-tick deltas) for the same stream. buildTimeBase converts stts runs
// from media ticks to output samples at the codec rate, so both conventions
// land on 2048 samples per AU; adopting the ticks directly would halve the
// core-timescale files' durations and land their seeks at half target.
func TestBuildTimeBaseHEAACTimescale(t *testing.T) {
	const frames = 100
	for _, tc := range []struct {
		name      string
		timescale int64 // mdhd
		delta     int64 // stts, in media ticks
		rate      int64 // what the ASC reports
		wantDelta int64 // output samples per frame
	}{
		// The common convention: timescale at the SBR output rate, no
		// rescale, 2048 output samples per AU.
		{"timescale at sbr output rate", 48000, 2048, 48000, 2048},
		// The other convention: timescale at the core rate; 1024 core ticks
		// rescale x2 to the output timeline.
		{"timescale at core rate", 24000, 1024, 48000, 2048},
		// A timescale above the codec rate exercises the down-rescale
		// direction (division), which no real HE-AAC convention hits but
		// mulDivSat must still get right.
		{"timescale above codec rate", 96000, 4096, 48000, 2048},
		// Plain AAC-LC at 44100, the common non-SBR case: no rescale.
		{"aac-lc no rescale", 44100, 1024, 44100, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Demuxer{}
			st := &sampleTable{total: frames}
			d.buildTimeBase(st, []sttsEntry{{count: frames, delta: tc.delta}}, tc.timescale, tc.rate)

			if len(st.runDelta) != 1 {
				t.Fatalf("runDelta has %d runs, want 1", len(st.runDelta))
			}
			if st.runDelta[0] != tc.wantDelta {
				t.Errorf("per-frame delta = %d output samples, want %d", st.runDelta[0], tc.wantDelta)
			}
			// The whole point: the track length in output samples must match
			// what the decoder actually emits (frames * samples per AU), or
			// the duration and every seek are off by the timescale ratio.
			if want := int64(frames) * tc.wantDelta; st.totalDur != want {
				t.Errorf("totalDur = %d, want %d samples at %d Hz", st.totalDur, want, tc.rate)
			}
			// And it must agree with wall-clock duration against the rate the
			// track reports, which is the property a mismatch breaks.
			gotSec := float64(st.totalDur) / float64(tc.rate)
			if want := 100 * 2048.0 / 48000.0; tc.rate == 48000 && absDiff(gotSec, want) > 1e-9 {
				t.Errorf("duration = %.9fs, want %.9fs", gotSec, want)
			}
		})
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
