package mp4

import (
	"math"
	"testing"
)

// TestHeaderDurationWidth is the guard on a bound that is not hypothetical
// at audio rates.
//
// A movie, track or media header states its duration in media ticks, and an
// audio track's tick is a sample, so a version-0 header runs out at 2^32
// samples: 27 hours at 44.1 kHz, under 25 at 48. The muxer this covers
// exists for audiobooks, and a 40-hour read (the plan's own example) is
// 6.35 billion samples. Written as version 0 that wraps, and the file then
// lies about its own length to every player, silently.
//
// Version 0 must stay the answer for everything short enough, or every
// ordinary file's bytes move.
func TestHeaderDurationWidth(t *testing.T) {
	for _, tc := range []struct {
		name string
		dur  int64
		want byte
	}{
		{"zero", 0, 0},
		{"an hour at 44.1k", 44100 * 3600, 0},
		{"the last 32-bit tick", math.MaxUint32, 0},
		{"one past it", math.MaxUint32 + 1, 1},
		{"a 40-hour audiobook at 44.1k", 40 * 3600 * 44100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := durVersion(tc.dur); got != tc.want {
				t.Fatalf("durVersion(%d) = %d, want %d", tc.dur, got, tc.want)
			}
			// The field must be as wide as the version claims, and must
			// carry the value rather than a wrapped one.
			got := durField(tc.want, tc.dur)
			wantLen := 4
			if tc.want == 1 {
				wantLen = 8
			}
			if len(got) != wantLen {
				t.Fatalf("durField at version %d is %d bytes, want %d", tc.want, len(got), wantLen)
			}
			var back int64
			if tc.want == 1 {
				back = int64(be64(got))
			} else {
				back = int64(be32(got))
			}
			if back != tc.dur {
				t.Fatalf("durField(%d, %d) reads back as %d", tc.want, tc.dur, back)
			}
			if n := len(zeroTimes(tc.want)); n != wantLen*2 {
				t.Fatalf("zeroTimes at version %d is %d bytes, want %d", tc.want, n, wantLen*2)
			}
		})
	}
}

// TestHeaderDurationWrapsAtVersionZero states the bug this guards against,
// rather than leaving a reader to take the comment's word for it: the
// obvious 32-bit write turns a 40-hour audiobook into a 12-hour one.
func TestHeaderDurationWrapsAtVersionZero(t *testing.T) {
	// A variable, not a constant: uint32(constant) is a compile error, which
	// is Go declining to let this bug be written down. The muxer's value is
	// a runtime int64, where the same conversion is silent.
	fortyHours := int64(40 * 3600 * 44100)
	wrapped := int64(uint32(fortyHours))
	if wrapped == fortyHours {
		t.Fatal("the premise moved: a 40-hour duration now fits 32 bits")
	}
	if hours := float64(wrapped) / 44100 / 3600; hours > 39 {
		t.Fatalf("the wrap yields %.1f hours; this test is not showing the bug", hours)
	}
	// And the muxer does not do that: it widens instead.
	if durVersion(fortyHours) != 1 {
		t.Fatal("a 40-hour duration is written as version 0 and wraps")
	}
}

// TestLongHeaderRoundTrips is the write-and-read half: a duration past 32
// bits must survive the muxer's own reader, which is what makes the version
// switch a fix rather than a different way to be wrong.
//
// It builds the boxes directly rather than muxing a real file, because the
// case under test is 6.35 billion samples and there is no fixture for that.
// The boxes are the whole of what changes.
func TestLongHeaderRoundTrips(t *testing.T) {
	const rate = 44100
	longDur := int64(40 * 3600 * rate) // a 40-hour audiobook

	t.Run("mdhd", func(t *testing.T) {
		mdia := progMdiaBox(rate, []byte("xxxx"), []uint32{1024}, []uint32{16}, 4096, longDur)
		mdhd := findBoxForTest(t, mdia[8:], "mdhd")
		ts, dur := mdhdTime(mdhd)
		if ts != rate {
			t.Errorf("timescale = %d, want %d", ts, rate)
		}
		if dur != longDur {
			t.Fatalf("duration reads back as %d, want %d (%.1f hours became %.1f)",
				dur, longDur, float64(longDur)/rate/3600, float64(dur)/rate/3600)
		}
	})

	// The short case must still be version 0, or every existing file's bytes
	// move for a bound they never reach.
	t.Run("a short movie stays version 0", func(t *testing.T) {
		mdia := progMdiaBox(rate, []byte("xxxx"), []uint32{1024}, []uint32{16}, 4096, rate*3600)
		mdhd := findBoxForTest(t, mdia[8:], "mdhd")
		if v, _, _, ok := fullBox(mdhd); !ok || v != 0 {
			t.Fatalf("an hour-long track writes mdhd version %d, want 0", v)
		}
		ts, dur := mdhdTime(mdhd)
		if ts != rate || dur != rate*3600 {
			t.Fatalf("timescale %d duration %d", ts, dur)
		}
	})

	t.Run("mvhd and tkhd", func(t *testing.T) {
		moov := progMoovBox(rate, []byte("xxxx"), nil, nil,
			[]uint32{1024}, []uint32{16}, 4096, longDur, longDur, nil)
		body := moov[8:]

		mvhd := findBoxForTest(t, body, "mvhd")
		if v, _, _, ok := fullBox(mvhd); !ok || v != 1 {
			t.Fatalf("mvhd version = %d, want 1 for a duration past 32 bits", v)
		}
		if ts := mvhdTimescale(mvhd); ts != rate {
			t.Errorf("mvhd timescale reads back as %d, want %d: the version-1 field offsets are wrong", ts, rate)
		}

		// tkhd's track_ID sits after the widened time pair, so reading it
		// back proves the version-1 layout as a whole, not just a duration.
		trak := findBoxForTest(t, body, "trak")
		tkhd := findBoxForTest(t, trak, "tkhd")
		if v, _, _, ok := fullBox(tkhd); !ok || v != 1 {
			t.Fatalf("tkhd version = %d, want 1", v)
		}
		if id := tkhdTrackID(tkhd); id != trackID {
			t.Fatalf("tkhd track_ID reads back as %d, want %d: the version-1 layout is wrong", id, trackID)
		}
	})
}
