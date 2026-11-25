package waxflow

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// TestUnreadEndTrimAsksOnlyWhatMayHideOne: the copy looks for an end trim
// only on a track whose trims ride on its packets, unread, with no delay and
// a codec that has a trim to state.
func TestUnreadEndTrimAsksOnlyWhatMayHideOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		codec  codec.ID
		delay  int64
		unread bool
		want   bool
	}{
		{"Matroska AAC, no delay", codec.AACLC, 0, true, true},
		{"Matroska HE-AAC, no delay", codec.HEAAC, 0, true, true},
		{"Matroska AAC with a delay", codec.AACLC, 1024, true, false},
		{"Matroska AAC, walked", codec.AACLC, 0, false, false},
		{"ADTS or MP4 AAC, no delay", codec.AACLC, 0, false, false},
		{"Matroska ALAC", codec.ALAC, 0, true, false},
		{"Matroska FLAC", codec.FLAC, 0, true, false},
	} {
		tr := container.Track{Codec: tc.codec, Delay: tc.delay, TrimsUnread: tc.unread}
		if got := unreadEndTrim(tr); got != tc.want {
			t.Errorf("%s: unreadEndTrim = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAdoptMeasuredTakesOnlyAMeasurement: a track whose length is absent or
// an estimate, or whose trims nothing read, is no measurement, and the
// header's own track stands; a measurement brings its length and trims.
func TestAdoptMeasuredTakesOnlyAMeasurement(t *testing.T) {
	header := aacTrack(0, 51168)
	header.SamplesAdvisory, header.TrimsUnread = true, true
	measured := aacTrack(0, 51024)
	measured.SamplesExact, measured.Padding = true, 176
	for _, tc := range []struct {
		name string
		edit func(*container.Track)
	}{
		{"no length", func(m *container.Track) { m.Samples = -1 }},
		{"an estimate", func(m *container.Track) { m.SamplesExact, m.SamplesAdvisory = false, true }},
		{"trims unread", func(m *container.Track) { m.TrimsUnread = true }},
	} {
		m := measured
		tc.edit(&m)
		if got := adoptMeasured(header, m); got.Samples != header.Samples || !got.SamplesAdvisory || got.SamplesExact || got.Padding != 0 || !got.TrimsUnread {
			t.Errorf("%s: adopted Samples %d, exact %v, Padding %d, TrimsUnread %v; want the header's track", tc.name, got.Samples, got.SamplesExact, got.Padding, got.TrimsUnread)
		}
	}
	if got := adoptMeasured(header, measured); got.Samples != 51024 || !got.SamplesExact || got.SamplesAdvisory || got.Padding != 176 || got.TrimsUnread {
		t.Errorf("a measurement: adopted Samples %d, exact %v, advisory %v, Padding %d, TrimsUnread %v; want 51024, true, false, 176, false",
			got.Samples, got.SamplesExact, got.SamplesAdvisory, got.Padding, got.TrimsUnread)
	}
}

// TestDerivedTracksHaveNoUnreadTrims: a cut's trims are its own arithmetic
// and a span of a decoded view has none, whatever the source had read.
func TestDerivedTracksHaveNoUnreadTrims(t *testing.T) {
	src := aacTrack(0, 96000)
	src.TrimsUnread = true
	cut, _, err := CutTrack(src, TranscodeOptions{Format: "aac", Container: "mka"}, []Span{{From: 0, To: 50000}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if cut.TrimsUnread || cut.Padding != 176 {
		t.Errorf("the cut track: TrimsUnread %v, Padding %d; want false, 176", cut.TrimsUnread, cut.Padding)
	}
	span, err := SpanTrack(src, 1000, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if span.TrimsUnread {
		t.Error("the spanned track still says its trims are unread")
	}
}

// TestCutProbeCarriesTheCutsDelay: the destination is asked about the track
// as the cut will present it, so a primed source or a span past sample 0
// brings a delay, and the config's priming matches it.
func TestCutProbeCarriesTheCutsDelay(t *testing.T) {
	opus := container.Track{Codec: codec.Opus, CodecConfig: testOpusHead(312), Delay: 312, Samples: 480000}
	for _, tc := range []struct {
		name  string
		track container.Track
		from  int64
		want  int64
	}{
		{"unprimed AAC from 0", aacTrack(0, 96000), 0, 0},
		{"unprimed AAC past 0", aacTrack(0, 96000), 4096, 1024},
		{"unprimed AAC inside the pre-roll", aacTrack(0, 96000), 100, 100},
		{"primed AAC from 0", aacTrack(1024, 96000), 0, 1024},
		{"Opus from 0", opus, 0, 312},
		{"Opus past 0", opus, 96000, cutCodecs[codec.Opus].preroll},
	} {
		probe, ok := cutProbe(tc.track, []Span{{From: tc.from, To: ToEnd}})
		if !ok || probe.Delay != tc.want {
			t.Errorf("%s: probe delay %d (ok %v), want %d", tc.name, probe.Delay, ok, tc.want)
		}
		if tc.track.Codec == codec.Opus {
			if got := int64(binary.LittleEndian.Uint16(probe.CodecConfig[10:])); got != tc.want {
				t.Errorf("%s: the probe's OpusHead pre-skip is %d, want %d", tc.name, got, tc.want)
			}
		}
	}
	// A primed track that states a padding and no length, cut to its end as
	// fragmented MP4: the cut has a delay, so the muxer states the padding.
	primed := aacTrack(1024, -1)
	primed.Padding = 535
	e := New()
	for _, from := range []int64{0, 4096} {
		plan, err := e.PlanCut(primed, TranscodeOptions{Format: "aac"}, []Span{{From: from, To: ToEnd}}, knownGrid(1024))
		if err != nil || plan == nil {
			t.Errorf("from %d: PlanCut = %v, %v; want a cut", from, plan, err)
		}
	}
}
