package mp4

import "testing"

// TestBuildTimeBaseHEAACTimescale pins the interaction between the ASC-
// authoritative rate and the media timescale for HE-AAC.
//
// HE-AAC M4As are commonly muxed with the media timescale at the SBR *output*
// rate: mdhd timescale 48000 with stts deltas of 2048 per frame, describing a
// core that codes at 24000 with 1024-sample frames. Since aac-dec-2 reports
// the core rate (24000) rather than an octave below it, the concern is that a
// stts read in media-tick units against a 24000 rate would double the duration
// and land every seek at half its target.
//
// It does not, because buildTimeBase converts stts runs from media ticks to
// output samples at the codec rate rather than adopting the ticks directly.
// The halving deleted in aac-dec-2 was therefore not papering over a timescale
// mismatch: the rescale already handled it, and the halving simply made the
// rescale disagree with the decoder's frame length.
func TestBuildTimeBaseHEAACTimescale(t *testing.T) {
	const frames = 100
	for _, tc := range []struct {
		name      string
		timescale int64 // mdhd
		delta     int64 // stts, in media ticks
		rate      int64 // what the ASC reports
		wantDelta int64 // output samples per frame
	}{
		// The convention the plan flags: timescale at the SBR output rate.
		// 2048 ticks at 48000 is 1024 samples at 24000, the AAC frame length.
		{"timescale at sbr output rate", 48000, 2048, 24000, 1024},
		// The other convention: timescale at the core rate, no rescale.
		{"timescale at core rate", 24000, 1024, 24000, 1024},
		// Plain AAC-LC at 44100, the common non-SBR case: also no rescale.
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
			// what the decoder actually emits (frames * frameLength), or the
			// duration and every seek are off by the timescale ratio.
			if want := int64(frames) * tc.wantDelta; st.totalDur != want {
				t.Errorf("totalDur = %d, want %d samples at %d Hz", st.totalDur, want, tc.rate)
			}
			// And it must agree with wall-clock duration against the rate the
			// track reports, which is the property a doubled duration breaks.
			gotSec := float64(st.totalDur) / float64(tc.rate)
			if want := 100 * 1024.0 / 24000.0; tc.rate == 24000 && absDiff(gotSec, want) > 1e-9 {
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
