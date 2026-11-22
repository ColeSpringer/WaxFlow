package waxflow

import (
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

// TestBitrateReachesEveryLossyRow: the one Bitrate reaches each bit-rate
// encoder's plan, and its zero value keeps that encoder's default.
func TestBitrateReachesEveryLossyRow(t *testing.T) {
	track := openWAV(t, New(), sineWAV(t)).Info().Default()
	for _, c := range []struct {
		name     string
		opts     TranscodeOptions
		def, set int
	}{
		{"mp3", TranscodeOptions{Format: "mp3"}, 128000, 192000},
		{"opus", TranscodeOptions{Format: "opus"}, 96000, 64000},
		{"aac", TranscodeOptions{Format: "aac"}, 128000, 96000},
		{"he-aac", TranscodeOptions{Format: "he-aac"}, 64000, 48000},
		{"he-aac v2", TranscodeOptions{Format: "he-aac", HEAACv2: true}, 32000, 24000},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, rate := range []int{0, c.set} {
				opts := c.opts
				opts.Bitrate = rate
				want := rate
				if rate == 0 {
					want = c.def
				}
				plan, err := New().PlanTranscode(track, opts)
				if err != nil {
					t.Fatalf("Bitrate %d: %v", rate, err)
				}
				if plan.BitRate != want {
					t.Errorf("Bitrate %d plans %d b/s, want %d", rate, plan.BitRate, want)
				}
			}
		})
	}
}

// TestNegativeBitrateIsRefusedOnEveryRow: a negative Bitrate is an invalid
// request on every output row, lossless ones included, before any encoder
// is built.
func TestNegativeBitrateIsRefusedOnEveryRow(t *testing.T) {
	track := openWAV(t, New(), sineWAV(t)).Info().Default()
	for _, row := range outputs {
		t.Run(row.name, func(t *testing.T) {
			opts := TranscodeOptions{Format: row.name, Bitrate: -128000}
			if err := opts.Check(); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Errorf("Check: %v, want an invalid request", err)
			}
			if _, err := New().PlanTranscode(track, opts); waxerr.CodeOf(err) != waxerr.CodeInvalidRequest {
				t.Errorf("PlanTranscode: %v, want an invalid request", err)
			}
		})
	}
}

// TestBitrateAboveMaxBitrateIsRefused: MaxBitrate is the largest Bitrate Check
// takes, so a boundary's kbit/s bound and the engine's agree.
func TestBitrateAboveMaxBitrateIsRefused(t *testing.T) {
	if err := (TranscodeOptions{Format: "opus", Bitrate: MaxBitrate}).Check(); err != nil {
		t.Errorf("Check at MaxBitrate: %v", err)
	}
	if math.MaxInt == MaxBitrate {
		t.Skip("no int is above MaxBitrate on a 32-bit build")
	}
	over := MaxBitrate
	over++
	err := TranscodeOptions{Format: "opus", Bitrate: over}.Check()
	if waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !strings.Contains(err.Error(), "Bitrate is 2147483648") {
		t.Errorf("Check above MaxBitrate: %v, want an invalid request naming the Bitrate", err)
	}
}

// TestCheckRefusesAKnobOutOfRange: Check holds each knob the format takes to
// its range, so a boundary refuses a bad level before a source opens. The
// sentinels and the top of each range pass.
func TestCheckRefusesAKnobOutOfRange(t *testing.T) {
	for _, c := range []struct {
		opts TranscodeOptions
		want string
	}{
		{TranscodeOptions{Format: "flac", FLACLevel: 9}, "waxflow: FLAC level 9 outside -1..8"},
		{TranscodeOptions{Format: "flac", FLACLevel: -2}, "waxflow: FLAC level -2 outside -1..8"},
		{TranscodeOptions{Format: "wavpack", WavPackLevel: 9}, "waxflow: WavPack level 9 outside 1..4"},
		{TranscodeOptions{Format: "ape", APELevel: 4000}, "waxflow: APE level 4000 decodes but is not written"},
		{TranscodeOptions{Format: "opus", OpusComplexity: 11}, "waxflow: Opus complexity 11 outside -1..10"},
		{TranscodeOptions{Format: "opus", OpusSignal: "foo"}, `opus signal hint "foo" is not auto, voice, or music`},
		{TranscodeOptions{Format: "vorbis", VorbisQuality: 50}, "waxflow: Vorbis quality 50 outside -1..10"},
		{TranscodeOptions{Format: "vorbis", VorbisQuality: -1.5}, "waxflow: Vorbis quality -1.5 outside -1..10"},
		{TranscodeOptions{Format: "vorbis", VorbisQuality: -1}, ""},
		{TranscodeOptions{Format: "vorbis", VorbisQuality: 10}, ""},
		{TranscodeOptions{Format: "flac", FLACLevel: FLACLevelFastest}, ""},
		{TranscodeOptions{Format: "flac", FLACLevel: 8}, ""},
		{TranscodeOptions{Format: "opus", OpusComplexity: OpusComplexityLowest, OpusSignal: "voice"}, ""},
		{TranscodeOptions{Format: "opus", OpusComplexity: 10}, ""},
	} {
		err := c.opts.Check()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%+v: %v, want it accepted", c.opts, err)
		case c.want != "" && (waxerr.CodeOf(err) != waxerr.CodeInvalidRequest || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: %v, want an invalid request %q", c.opts, err, c.want)
		}
	}
}
