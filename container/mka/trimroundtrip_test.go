package mka

import "testing"

// TestWholePacketTrimRoundTrips holds the nanosecond conversions to naming a
// whole packet exactly.
//
// A cut with exact interior splices writes a DiscardPadding covering a
// packet's whole duration, so the pre-roll it decodes is heard by nobody.
// The trim travels as nanoseconds and 1024 samples at 44.1 kHz is not a
// whole nanosecond count; a conversion that lost a sample there would leave
// one sample of the removed span audible at every join, which is the kind of
// thing nothing else would catch.
func TestWholePacketTrimRoundTrips(t *testing.T) {
	rates := []int{8000, 11025, 16000, 22050, 24000, 32000, 44100, 48000, 88200, 96000}
	packets := []int64{120, 240, 480, 512, 576, 960, 1024, 1152, 2048, 2304, 4096}
	for _, rate := range rates {
		for _, n := range packets {
			ns := samplesToNs(n, rate)
			if got := nsToSamples(ns, rate); got != n {
				t.Errorf("%d samples at %d Hz is %d ns, which reads back as %d", n, rate, ns, got)
			}
		}
	}
}

// TestBlockDurationCoversItsDiscard: BlockDuration is whole milliseconds and
// DiscardPadding is exact nanoseconds, so the two are not the same grid. A
// block that discards its whole payload must not end up declaring less
// duration than it discards, which is what rounding to nearest does for a
// 1024-sample packet at 44.1 kHz (23 ms against 23.22).
func TestBlockDurationCoversItsDiscard(t *testing.T) {
	rates := []int{8000, 11025, 16000, 22050, 24000, 32000, 44100, 48000, 88200, 96000}
	packets := []int64{120, 240, 480, 512, 576, 960, 1024, 1152, 2048, 2304, 4096}
	for _, rate := range rates {
		for _, n := range packets {
			durNS := ceilMsAt(n, rate) * 1_000_000
			if discard := samplesToNs(n, rate); durNS < discard {
				t.Errorf("%d samples at %d Hz: BlockDuration %d ns is under the %d ns it discards",
					n, rate, durNS, discard)
			}
			if over := ceilMsAt(n, rate) - msAt(n, rate); over > 1 {
				t.Errorf("%d samples at %d Hz rounds up by %d ms", n, rate, over)
			}
		}
	}
}
