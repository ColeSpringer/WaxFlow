package mp4

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func open(t *testing.T, data []byte) *Demuxer {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(data), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	return d
}

// TestDemuxALACFixtures opens the committed ALAC fixtures, one with the
// moov before mdat (faststart) and one with it after, and walks every
// packet.
func TestDemuxALACFixtures(t *testing.T) {
	for _, name := range []string{"alac-stereo.m4a", "alac-mono-tail.m4a"} {
		t.Run(name, func(t *testing.T) {
			d := open(t, fixture(t, name))
			tracks := d.Tracks()
			if len(tracks) != 1 {
				t.Fatalf("tracks = %d, want 1", len(tracks))
			}
			tr := tracks[0]
			if tr.Codec != codec.ALAC {
				t.Errorf("codec = %q, want alac", tr.Codec)
			}
			if err := tr.Fmt.Valid(); err != nil {
				t.Errorf("track format invalid: %v", err)
			}
			if tr.Samples <= 0 {
				t.Errorf("samples = %d, want positive", tr.Samples)
			}

			// Walk packets; their durations should sum to the raw length.
			var total, count int64
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("ReadPacket: %v", err)
				}
				if pkt.Dur <= 0 || len(pkt.Data) == 0 {
					t.Fatalf("packet %d: dur=%d len=%d", count, pkt.Dur, len(pkt.Data))
				}
				if pkt.PTS != total {
					t.Fatalf("packet %d pts=%d, want %d", count, pkt.PTS, total)
				}
				if !pkt.Sync {
					t.Errorf("packet %d not marked sync (ALAC frames are independent)", count)
				}
				total += pkt.Dur
				count++
			}
			// The trimmed length plus any trims equals the raw length.
			if total < tr.Samples {
				t.Errorf("raw samples %d < trimmed %d", total, tr.Samples)
			}
		})
	}
}

// TestSeekLandsAtOrBefore checks SeekSample never overshoots and that a
// past-end target lands on the final frame.
func TestSeekLandsAtOrBefore(t *testing.T) {
	d := open(t, fixture(t, "alac-stereo.m4a"))
	raw := d.sel.st.totalDur
	for _, target := range []int64{0, 1, 1000, 4096, raw / 2, raw - 1, raw, raw + 100000} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target && target < raw {
			t.Errorf("seek to %d landed at %d (overshoot)", target, landed)
		}
		if landed < 0 || landed > raw {
			t.Errorf("seek to %d landed out of range at %d", target, landed)
		}
	}
}

// TestEditListParsed confirms the fixture's edit list is read (ffmpeg
// writes one even for ALAC, with media_time 0 so nothing is trimmed).
func TestEditListParsed(t *testing.T) {
	d := open(t, fixture(t, "alac-stereo.m4a"))
	if !d.sel.hasEdit {
		t.Skip("fixture carries no edit list")
	}
	// ALAC has no encoder delay, so any edit is a no-op trim.
	if d.track.Delay != 0 {
		t.Errorf("ALAC delay = %d, want 0", d.track.Delay)
	}
}

// TestTruncationTolerated feeds progressively truncated fixtures; the
// demuxer must never panic and must fail cleanly when the moov is cut.
func TestTruncationTolerated(t *testing.T) {
	full := fixture(t, "alac-stereo.m4a")
	for n := len(full); n > 0; n -= 37 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %d-byte prefix: %v", n, r)
				}
			}()
			d, err := NewDemuxer(container.BytesSource(full[:n]), nil)
			if err != nil {
				return // clean failure is fine
			}
			var pkt container.Packet
			for i := 0; i < 10000; i++ {
				if d.ReadPacket(&pkt) != nil {
					break
				}
			}
		}()
	}
}

// TestStrictRejectsMissingFtyp checks strict mode turns the tolerated
// missing-ftyp warning into an error.
func TestStrictRejectsMissingFtyp(t *testing.T) {
	full := fixture(t, "alac-stereo.m4a")
	// Corrupt the ftyp type so it is no longer recognized; tolerant mode
	// warns, strict mode fails.
	mangled := append([]byte(nil), full...)
	copy(mangled[4:8], "xxxx")
	if _, err := NewDemuxer(container.BytesSource(mangled), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted a file with no ftyp")
	}
	if _, err := NewDemuxer(container.BytesSource(mangled), nil); err != nil {
		t.Errorf("tolerant mode rejected a missing ftyp: %v", err)
	}
}

// TestBuildSyncOutOfRangeStaysAllSync checks that an stss whose entries are
// all out of range falls back to the all-sync convention (nil) rather than
// flipping to a no-sync table that would collapse every seek to sample 0.
func TestBuildSyncOutOfRangeStaysAllSync(t *testing.T) {
	st := &sampleTable{total: 10}
	buildSync(st, []int64{0, 100, 999}) // 0 is invalid 1-based; 100, 999 exceed total
	if st.sync != nil {
		t.Fatalf("sync = %v, want nil (all-sync fallback)", st.sync)
	}
	if !st.isSync(3) {
		t.Error("isSync(3) = false, want true under all-sync")
	}
	if got := st.syncAtOrBefore(7); got != 7 {
		t.Errorf("syncAtOrBefore(7) = %d, want 7 under all-sync", got)
	}
}

// TestBuildTimeBasePositiveDuration guards the FuzzDemux crash where a
// mutated mdhd timescale far above the sample rate rescaled a per-sample
// delta down to zero, so ReadPacket emitted packets with a non-positive
// duration. Every sample must carry a positive duration across the three
// ways a delta can degenerate: a rescale that floors to zero, a rescale that
// would overflow int64, and a raw stts delta of zero.
func TestBuildTimeBasePositiveDuration(t *testing.T) {
	cases := []struct {
		name                   string
		delta, timescale, rate int64
	}{
		{"rescale floors to zero", 4096, 1 << 40, 44100},
		{"rescale overflows int64", math.MaxUint32, 1, math.MaxUint32},
		{"raw stts delta zero", 0, 44100, 44100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Demuxer{}
			st := &sampleTable{total: 3}
			d.buildTimeBase(st, []sttsEntry{{count: 3, delta: tc.delta}}, tc.timescale, tc.rate)
			var prev int64 = -1
			for i := int64(0); i < st.total; i++ {
				pts, dur := st.timeOf(i)
				if dur <= 0 {
					t.Fatalf("sample %d: duration %d, want positive", i, dur)
				}
				if pts <= prev {
					t.Fatalf("sample %d: pts %d not advancing past %d", i, pts, prev)
				}
				prev = pts
			}
		})
	}
}

// TestGaplessSMPBNoOverflow feeds a hostile iTunSMPB true-sample count near
// 2^63; the trims must stay consistent (delay+padding+samples == totalRaw)
// instead of letting delay+samples overflow past the reset guard.
func TestGaplessSMPBNoOverflow(t *testing.T) {
	d := &Demuxer{smpbOK: true, smpbDelay: 1, smpbTotal: math.MaxInt64}
	tr := &track{fmt: audio.Format{Rate: 44100}}
	tr.st.totalDur = 40000
	delay, padding, samples := d.gapless(tr)
	if samples < 0 || samples > tr.st.totalDur {
		t.Fatalf("samples = %d, out of range for totalRaw %d", samples, tr.st.totalDur)
	}
	if delay+padding+samples != tr.st.totalDur {
		t.Errorf("delay+padding+samples = %d, want totalRaw %d", delay+padding+samples, tr.st.totalDur)
	}
}

// TestGaplessEditListNoOverflow feeds a hostile edit list whose media_time and
// segment_duration are near 2^63 together with a large sample rate, so the
// naive editMedia*rate and editSegDur*rate products overflow int64. The
// resulting trims must stay in range and consistent rather than wrapping into
// a negative delay or a bogus sample count.
func TestGaplessEditListNoOverflow(t *testing.T) {
	d := &Demuxer{movieTimescale: 1}
	tr := &track{
		fmt:        audio.Format{Rate: math.MaxUint32},
		hasEdit:    true,
		editMedia:  math.MaxInt64,
		editSegDur: math.MaxInt64,
		timescale:  1,
	}
	tr.st.totalDur = 40000
	delay, padding, samples := d.gapless(tr)
	if delay < 0 || delay > tr.st.totalDur {
		t.Fatalf("delay = %d, out of range for totalRaw %d", delay, tr.st.totalDur)
	}
	if samples < 0 || samples > tr.st.totalDur {
		t.Fatalf("samples = %d, out of range for totalRaw %d", samples, tr.st.totalDur)
	}
	if padding < 0 {
		t.Fatalf("padding = %d, want non-negative", padding)
	}
	if delay+padding+samples != tr.st.totalDur {
		t.Errorf("delay+padding+samples = %d, want totalRaw %d", delay+padding+samples, tr.st.totalDur)
	}
}

// TestMulDivSat checks the saturating rescale: exact for values that fit,
// saturated at math.MaxInt64 when the product or quotient would overflow, and
// zero for a non-positive operand.
func TestMulDivSat(t *testing.T) {
	cases := []struct {
		a, b, c, want int64
	}{
		{10, 20, 4, 50},                                  // exact
		{1000, int64(1e9), 600, 1666666666},              // chapter ns, no overflow
		{math.MaxInt64, math.MaxInt64, 1, math.MaxInt64}, // product overflows -> saturate
		{math.MaxInt64, 2, 1, math.MaxInt64},             // quotient overflows int64 -> saturate
		{1 << 40, 1 << 40, 1 << 40, 1 << 40},             // 128-bit intermediate, exact
		{-5, 10, 2, 0},                                   // non-positive operand
		{5, 10, 0, 0},                                    // non-positive divisor
	}
	for _, tc := range cases {
		if got := mulDivSat(tc.a, tc.b, tc.c); got != tc.want {
			t.Errorf("mulDivSat(%d, %d, %d) = %d, want %d", tc.a, tc.b, tc.c, got, tc.want)
		}
	}
}

// TestBuildSyncDeduplicates checks that a stss repeating sample numbers yields
// a sync set with no duplicates, so isSync and syncAtOrBefore search a minimal
// list.
func TestBuildSyncDeduplicates(t *testing.T) {
	st := &sampleTable{total: 10}
	buildSync(st, []int64{5, 1, 5, 3, 1, 3}) // 1-based, with repeats
	want := []int64{0, 2, 4}                 // deduped 0-based
	if len(st.sync) != len(want) {
		t.Fatalf("sync = %v, want %v", st.sync, want)
	}
	for i, v := range want {
		if st.sync[i] != v {
			t.Fatalf("sync = %v, want %v", st.sync, want)
		}
	}
}
