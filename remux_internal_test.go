package waxflow

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/resample"
)

// TestRemuxTrailerDerivesPadding pins the correction TestRemuxGaplessRoundTrip
// caught, at the level where the reason is visible.
//
// The end padding is derived from the packet walk rather than copied off the
// track, because a container is free to express the end trim either way:
// mp4 states it outright in iTunSMPB, while Ogg-Opus encodes it in the final
// page's granule and so reports Padding 0 beside a Samples that is already
// short. Copying that zero is the obvious synthesis and it is wrong: Matroska
// needs the count explicitly, writes no DiscardPadding without one, and the
// encoder's tail padding leaks out as audible audio with no error anywhere.
//
// The e2e round trip proves the audio; this proves the arithmetic, which the
// round trip cannot show because both demuxers normalize the trim back into
// Samples before a test can see it.
func TestRemuxTrailerDerivesPadding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		track   container.Track
		decoded int64
		want    codec.Trailer
	}{
		{
			// The real Ogg-Opus shape: 51 packets of 960 is 48960 decoded, 312
			// of pre-skip, 48000 of audio, so 648 samples of padding the source
			// never stated.
			name:    "granule-encoded trim is recovered",
			track:   container.Track{Codec: codec.Opus, Samples: 48000, Delay: 312, Padding: 0},
			decoded: 48960,
			want:    codec.Trailer{Samples: 48000, Delay: 312, Padding: 648},
		},
		{
			// A container that states the padding agrees with the derivation by
			// construction, since decoded minus delay minus kept audio is the
			// padding by definition. The two conventions must not disagree.
			name:    "an explicit trim survives unchanged",
			track:   container.Track{Codec: codec.AACLC, Samples: 48000, Delay: 1024, Padding: 512},
			decoded: 49536,
			want:    codec.Trailer{Samples: 48000, Delay: 1024, Padding: 512},
		},
		{
			// No priming means no lookahead to flush, so there is no padding to
			// derive. Deriving here would turn a lying FLAC STREAMINFO (an
			// oddity format.Media tolerates) into a nonzero padding and a muxer
			// error at End, after a whole file had been written.
			name:    "an unprimed codec keeps its own zero",
			track:   container.Track{Codec: codec.FLAC, Samples: 47999, Delay: 0, Padding: 0},
			decoded: 48000,
			want:    codec.Trailer{Samples: 47999, Delay: 0, Padding: 0},
		},
		{
			// An unknown length inverts the arithmetic rather than defeating it:
			// the declared Padding stands (nothing checks it) and the *length*
			// is what the walk resolves. 48960 decoded minus 312 of pre-skip is
			// 48648, which is exactly what a transcode of the same source would
			// report: format.Media can trim the front it knows about and not a
			// back it does not, so its encoder sees the same 48648 samples.
			// Handing back -1 here would complete a cache entry with a length
			// the run had in fact measured.
			name:    "an unknown length is resolved from the walk",
			track:   container.Track{Codec: codec.Opus, Samples: -1, Delay: 312, Padding: 0},
			decoded: 48960,
			want:    codec.Trailer{Samples: 48648, Delay: 312, Padding: 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remuxTrailer(tc.track, tc.decoded); got != tc.want {
				t.Errorf("remuxTrailer = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGaplessSurvives pins the rule that keeps a track whose trims no muxer can
// write off this rung: a lossless codec has no encoder priming, so a container
// declaring one for it is claiming something the codec cannot mean, and the
// honest answer is to decode and trim for real.
func TestGaplessSurvives(t *testing.T) {
	for _, tc := range []struct {
		name  string
		track container.Track
		want  bool
	}{
		{"untrimmed flac", container.Track{Codec: codec.FLAC}, true},
		{"untrimmed alac", container.Track{Codec: codec.ALAC}, true},
		{"delayed flac", container.Track{Codec: codec.FLAC, Delay: 576}, false},
		{"padded flac", container.Track{Codec: codec.FLAC, Padding: 576}, false},
		{"delayed alac", container.Track{Codec: codec.ALAC, Delay: 576}, false},
		{"delayed opus", container.Track{Codec: codec.Opus, Delay: 312}, true},
		{"delayed aac", container.Track{Codec: codec.AACLC, Delay: 1024, Padding: 512}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gaplessSurvives(tc.track); got != tc.want {
				t.Errorf("gaplessSurvives(%v) = %v, want %v", tc.track.Codec, got, tc.want)
			}
		})
	}
}

// TestRemuxableIsDerived pins the derived rule's two halves: it must accept a
// bare container rewrite, and it must reject the seek, which is the one field
// the derivation cannot see (planOpts normalizes it out of the plan cache's key
// on purpose, since two seeks of one source share a plan).
// opusFmt is what an Opus track decodes to, which is the format remuxable
// resolves a request's parameters against.
var opusFmt = audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}

func TestRemuxableIsDerived(t *testing.T) {
	if !remuxable(TranscodeOptions{Format: "opus", Container: "mka"}, opusFmt) {
		t.Error("remuxable rejected a bare container rewrite; the rung would never engage")
	}
	if remuxable(TranscodeOptions{Format: "opus", FromSample: 480}, opusFmt) {
		t.Error("remuxable accepted a seek; a remux cannot cut mid-packet")
	}
	// Tags are not shaping and must not disqualify the rung: /stream attaches a
	// minimal tag set to essentially every request, so a rule that keyed on them
	// would leave rung 2 permanently unreachable in the daemon.
	if !remuxable(TranscodeOptions{Format: "opus", Tags: []container.Tag{{Key: "TITLE", Value: "x"}}}, opusFmt) {
		t.Error("remuxable rejected a tagged request; rung 2 would be unreachable from /stream")
	}
}

// TestCodecSurvivesRejectsPCM pins the one codec whose packets are not
// container-independent, and which therefore cannot ride this rung.
//
// The rung rests on container.Packet.Data being the codec-native access unit,
// so an Opus packet means the same bytes in any container that carries it. PCM
// breaks that: its packet is raw samples whose layout is the *container's*
// choice (RIFF little-endian, AIFF big-endian, Matroska signed 8-bit), and the
// difference lives in CodecConfig where a codec.ID comparison cannot see it.
// Without this, format=wav on an AIFF source planned as a remux and then died in
// the muxer with "riff: WAV is little-endian" — a request that worked before
// this rung existed.
//
// Declining costs nothing, which is why it is the right answer rather than a
// retreat: this rung exists to avoid generation loss, and PCM has none.
func TestCodecSurvivesRejectsPCM(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, out codec.ID
		want     bool
	}{
		{"opus into opus", codec.Opus, codec.Opus, true},
		{"flac into flac", codec.FLAC, codec.FLAC, true},
		{"flac into opus", codec.FLAC, codec.Opus, false},
		{"pcm into pcm", codec.PCM, codec.PCM, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codecSurvives(tc.src, tc.out); got != tc.want {
				t.Errorf("codecSurvives(%q, %q) = %v, want %v", tc.src, tc.out, got, tc.want)
			}
		})
	}
}

// TestRemuxableResolvesNoOpParameters pins rung 2 against rung 1's convention: a
// parameter naming what the source already is asks for no transform.
//
// directPlayable compares rate=, ch=, and bits= against the track rather than
// against zero, so format=flac&rate=44100 on a 44.1 kHz FLAC serves the original
// bytes. Reading them as transforms here would make the same request with
// container=mka decode and re-encode the file to produce samples it already had.
func TestRemuxableResolvesNoOpParameters(t *testing.T) {
	flacFmt := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	for _, tc := range []struct {
		name string
		opts TranscodeOptions
		want bool
	}{
		{"the source's own rate", TranscodeOptions{Format: "flac", Rate: 44100}, true},
		{"the source's own channels", TranscodeOptions{Format: "flac", Channels: 2}, true},
		{"the source's own depth", TranscodeOptions{Format: "flac", BitDepth: 16}, true},
		{"all three at once", TranscodeOptions{Format: "flac", Rate: 44100, Channels: 2, BitDepth: 16}, true},
		{"a rate that resamples", TranscodeOptions{Format: "flac", Rate: 48000}, false},
		{"a downmix", TranscodeOptions{Format: "flac", Channels: 1}, false},
		{"a depth that quantizes", TranscodeOptions{Format: "flac", BitDepth: 24}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remuxable(tc.opts, flacFmt); got != tc.want {
				t.Errorf("remuxable = %v, want %v", got, tc.want)
			}
		})
	}
	// A depth request against a float source does quantize, so it is left alone
	// rather than resolved away: the guard is src.Type == audio.Int, and this is
	// the case it exists for.
	if remuxable(TranscodeOptions{Format: "opus", BitDepth: 32}, opusFmt) {
		t.Error("remuxable resolved a depth request against a float source; that one quantizes")
	}
}

// TestRemuxableIgnoresKernelSelection pins the regression that made this rung
// dead code in the daemon for as long as it took a test to drive it.
//
// A resample profile and a dither shaping say how a transform is performed, not
// whether one is, and a request on this rung has neither node in its chain. The
// trap is that resample.ParseProfile("") resolves to hq, so the server stamps a
// non-empty ResampleProfile on every request it makes: a rule that treated the
// profile as shaping declined every real request, and nothing noticed, because
// rung 3 answers them correctly and merely slowly.
func TestRemuxableIgnoresKernelSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts TranscodeOptions
	}{
		{"the daemon's own resolved profile", TranscodeOptions{Format: "opus", ResampleProfile: resample.HQ}},
		{"an explicitly fast profile", TranscodeOptions{Format: "opus", ResampleProfile: resample.Fast}},
		{"a dither shaping with nothing to dither", TranscodeOptions{Format: "opus", Shaping: dither.Shaped}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !remuxable(tc.opts, opusFmt) {
				t.Error("remuxable declined a request whose chain has no such node; the rung would never engage")
			}
		})
	}
}
