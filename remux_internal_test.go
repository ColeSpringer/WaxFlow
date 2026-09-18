package waxflow

import (
	"cmp"
	"io"
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
			// derive: a lying FLAC STREAMINFO (an oddity format.Media tolerates,
			// and reads straight past) must not become a nonzero padding and a
			// muxer error at End, after a whole file had been written. The
			// length still follows the packets, which is the number the remux
			// wrote and the number a read of the source delivers.
			name:    "an unprimed codec keeps its own zero",
			track:   container.Track{Codec: codec.FLAC, Samples: 47999, Delay: 0, Padding: 0},
			decoded: 48000,
			want:    codec.Trailer{Samples: 48000, Delay: 0, Padding: 0},
		},
		{
			// Ogg Vorbis: the playable length is the final page granule, with
			// no Delay to key the derivation off. Copying its Padding 0 across
			// is the same mistake the Opus row above exists to prevent, and
			// adopting the raw run instead would declare the encoder's tail as
			// audio.
			name:    "an exact length with no trims still caps the run",
			track:   container.Track{Codec: codec.Vorbis, Samples: 48000, SamplesExact: true},
			decoded: 48960,
			want:    codec.Trailer{Samples: 48000, Delay: 0, Padding: 960},
		},
		{
			// The shape this rung used to get wrong: a truncated LAME MP3 whose
			// Xing count promises more audio than its frames hold. Copying the
			// declared 48000 writes an edit list longer than the packets behind
			// it; the length shrinks to what they deliver, and the raw tail the
			// cap discards is the padding.
			name:    "a truncated source shrinks to its packets",
			track:   container.Track{Codec: codec.MP3, Samples: 48000, Delay: 1105, Padding: 1151},
			decoded: 40000,
			want:    codec.Trailer{Samples: 38895, Delay: 1105, Padding: 0},
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
			if got := remuxTrailer(tc.track, copiedRun{samples: tc.decoded}); got != tc.want {
				t.Errorf("remuxTrailer = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRemuxTrailerTakesThePacketsTrim covers the source that states its trims
// per packet and no total at all (Matroska's DiscardPadding). The header has
// nothing to settle against there, so the trailer is the packets': the last
// packet's trim, which is the only one an output container can carry.
//
// A trim before the last is not missing from it, it is already out of the run
// the copy counted (see container.Packet.Padding), so the arithmetic here
// never sees one. A destination that cannot carry one never gets that far
// either: the copy rungs decline such a source.
func TestRemuxTrailerTakesThePacketsTrim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		track container.Track
		run   copiedRun
		want  codec.Trailer
	}{
		{
			// The mid-stream fixture's shape: 500 blocks of 480 whose final
			// packet states 96 and whose block 100 states 48. The copy counted
			// 240000 less that 48, so the trailer takes the 96 off that and
			// the inner trim's frames are in neither number.
			name:  "a mid-stream trim is already out of the run",
			track: container.Track{Codec: codec.PCM, Samples: -1},
			run:   copiedRun{samples: 239952, lastPadding: 96, trimmed: true},
			want:  codec.Trailer{Samples: 239856, Delay: 0, Padding: 96},
		},
		{
			// The Opus-in-WebM shape, un-measured: the header's advisory total
			// is not consulted at all, since the packets are the source that
			// agrees with both conventions.
			name:  "an advisory total does not enter it",
			track: container.Track{Codec: codec.Opus, Samples: 6144, Delay: 312, SamplesAdvisory: true},
			run:   copiedRun{samples: 6720, lastPadding: 648, trimmed: true},
			want:  codec.Trailer{Samples: 5760, Delay: 312, Padding: 648},
		},
		{
			// A trim longer than the audio behind it clamps rather than going
			// negative; a muxer takes the length at its word.
			name:  "a trim past the run clamps",
			track: container.Track{Codec: codec.Opus, Samples: -1, Delay: 312},
			run:   copiedRun{samples: 480, lastPadding: 480, trimmed: true},
			want:  codec.Trailer{Samples: 0, Delay: 312, Padding: 480},
		},
		{
			// A lossless codec has no lookahead to flush, and every muxer that
			// writes one refuses a nonzero trim outright. A DiscardPadding on
			// such a track is nonsense a third-party Matroska muxer can still
			// write, and taking it would fail at End with the whole file
			// already on the wire: the same rule the header-side arm applies,
			// now that the header states no trim for anyone to check.
			name:  "an unprimed codec refuses the packets' trim too",
			track: container.Track{Codec: codec.FLAC, Samples: -1},
			run:   copiedRun{samples: 48000, lastPadding: 648, trimmed: true},
			want:  codec.Trailer{Samples: 48000, Delay: 0, Padding: 0},
		},
		{
			// A cut's track carries trims its own arithmetic computed, while
			// the packets it forwards are the source's: taking theirs would
			// throw the cut's away. The packets only ever fill a trim the
			// header left at zero.
			name:  "a track that states its own trim keeps it",
			track: container.Track{Codec: codec.Opus, Samples: 4000, Delay: 312, Padding: 88},
			run:   copiedRun{samples: 4400, lastPadding: 648, trimmed: true},
			want:  codec.Trailer{Samples: 4000, Delay: 312, Padding: 88},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remuxTrailer(tc.track, tc.run); got != tc.want {
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
// the muxer with "wav: WAV is little-endian" - a request that worked before
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

// TestPlanRemuxDeclinesInnerTrimsPerContainer is the destination half of the
// rule, one row per container each row can write. Only a container that states
// its trims per packet can carry a trim that is not at the end; a granule and
// an edit list each name one end trim, and ADTS names none at all, so a copy
// into any of them would play the trimmed frames back as audio.
func TestPlanRemuxDeclinesInnerTrimsPerContainer(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	e := New()
	for _, tc := range []struct {
		format, cont string
		want         bool
	}{
		{"opus", "", false},
		{"opus", "mka", true},
		{"opus", "webm", true},
		{"flac", "", false},
		{"flac", "ogg", false},
		{"flac", "mka", true},
		{"aac", "progressive", false},
		{"aac", "fragmented", false},
		{"aac", "adts", false},
		{"aac", "mka", true},
	} {
		t.Run(tc.format+"/"+cmp.Or(tc.cont, "default"), func(t *testing.T) {
			track := container.Track{
				Codec: codecOf(t, tc.format), Fmt: f, Samples: 48000, Default: true,
			}
			if plan, err := e.PlanRemux(track, TranscodeOptions{Format: tc.format, Container: tc.cont}); err != nil {
				t.Fatalf("the control (no inner trim) errored: %v", err)
			} else if plan == nil {
				t.Fatalf("the control (no inner trim) declined; this row proves nothing")
			}
			track.MidPadding = 480
			plan, err := e.PlanRemux(track, TranscodeOptions{Format: tc.format, Container: tc.cont})
			if err != nil {
				t.Fatal(err)
			}
			if (plan != nil) != tc.want {
				t.Errorf("PlanRemux planned=%v with an inner trim, want %v", plan != nil, tc.want)
			}
		})
	}
}

// codecOf is the codec an output row copies, for the table above.
func codecOf(t *testing.T, format string) codec.ID {
	t.Helper()
	row, err := outputRow(format)
	if err != nil {
		t.Fatal(err)
	}
	return row.codecID
}

// TestCopiedRunClampsAHostileTrim: RemuxDemuxer takes a caller's demuxer, so a
// packet may state a trim no demuxer in this tree would. Neither the timeline
// nor the trailer may come out of that longer than the run.
//
// A negative trim is the one that inverts: unclamped it *adds* to the run
// through the subtraction, and reaches the trailer as a padding SettleLength's
// uncapped arm then subtracts again, yielding a length past the samples the
// packets held, marked exact.
func TestCopiedRunClampsAHostileTrim(t *testing.T) {
	const dur = 1000
	for _, tc := range []struct {
		name           string
		pads           []int64
		wantRun, wantP int64
	}{
		{"no trims", []int64{0, 0, 0}, 3 * dur, 0},
		{"a negative final trim", []int64{0, 0, -500}, 3 * dur, 0},
		{"a negative trim in the middle", []int64{0, -500, 0}, 3 * dur, 0},
		// A final trim larger than its own frame is still the end trim: the
		// settled length removes the excess, so it must not be clamped away.
		{"an oversized final trim", []int64{0, 0, 1500}, 3 * dur, 1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &padDemuxer{dur: dur, pads: tc.pads}
			run, err := copyPackets(t.Context(), d, 0, true, func(container.Packet) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if run.samples != tc.wantRun {
				t.Errorf("run.samples = %d, want %d", run.samples, tc.wantRun)
			}
			if run.lastPadding != tc.wantP {
				t.Errorf("run.lastPadding = %d, want %d", run.lastPadding, tc.wantP)
			}
			tr := remuxTrailer(container.Track{Codec: codec.Opus, Samples: -1}, run)
			if tr.Samples < 0 || tr.Samples > run.samples {
				t.Errorf("trailer says %d samples against a run of %d", tr.Samples, run.samples)
			}
			if tr.Padding < 0 {
				t.Errorf("trailer padding = %d", tr.Padding)
			}
		})
	}
}

// padDemuxer yields fixed-duration packets with chosen trims, for the clamp
// table above. It is a caller's demuxer, not one of this tree's.
type padDemuxer struct {
	dur  int64
	pads []int64
	i    int
}

func (d *padDemuxer) Tracks() []container.Track {
	return []container.Track{{Codec: codec.Opus, Samples: -1, Default: true}}
}

func (d *padDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.i >= len(d.pads) {
		return io.EOF
	}
	*pkt = container.Packet{Padding: d.pads[d.i], Packet: codec.Packet{Dur: d.dur, Sync: true}}
	d.i++
	return nil
}

// TestRemuxTrailerDropsTrimsTheCodecCannotCarry: SettleLength's capped arm
// derives a padding whenever the run outlives the track's authoritative length,
// which an Ogg-FLAC whose final page granule under-reports its own frames does
// now that that length is verified. FLAC and ALAC muxers refuse a nonzero trim
// outright, so handing one on would fail at End with the file already written.
func TestRemuxTrailerDropsTrimsTheCodecCannotCarry(t *testing.T) {
	// 48000 samples of packets behind a verified 47000: the settle would derive
	// 1000 samples of padding.
	track := container.Track{Codec: codec.FLAC, Samples: 47000, SamplesExact: true}
	got := remuxTrailer(track, copiedRun{samples: 48000})
	if got.Padding != 0 || got.Delay != 0 {
		t.Errorf("trailer = %+v, want no trims: no FLAC muxer writes one", got)
	}
	if got.Samples != 48000 {
		t.Errorf("trailer says %d samples, want the %d the packets hold", got.Samples, 48000)
	}
	// The same shape on a codec that can carry a trim keeps it.
	opus := container.Track{Codec: codec.Opus, Samples: 47000, SamplesExact: true}
	if o := remuxTrailer(opus, copiedRun{samples: 48000}); o.Padding != 1000 {
		t.Errorf("opus trailer = %+v, want the derived 1000-sample trim", o)
	}
}

// TestPlanRemuxSegmentsDeclinesInnerTrims: a segmented run always writes fMP4,
// whatever Container the options name, and fMP4 states no trim per packet. The
// progressive gate reads the resolved container name, so without a decline of
// its own this rung accepted a Matroska container override and then met
// segmentWalk's mid-copy refusal with the playlist already out.
func TestPlanRemuxSegmentsDeclinesInnerTrims(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	e := New()
	for _, cont := range []string{"", "mka", "webm"} {
		name := cmp.Or(cont, "default")
		t.Run(name, func(t *testing.T) {
			track := container.Track{
				Codec: codec.Opus, CodecConfig: testOpusHead(312), Fmt: f,
				Samples: 48000, Delay: 312, Default: true,
			}
			opts := TranscodeOptions{Format: "opus", Container: cont}
			if plan, err := e.PlanRemuxSegments(track, opts, 4, 960); err != nil {
				t.Fatalf("the control (no inner trim) errored: %v", err)
			} else if plan == nil {
				t.Fatal("the control (no inner trim) declined; this row proves nothing")
			}
			track.MidPadding = 480
			plan, err := e.PlanRemuxSegments(track, opts, 4, 960)
			if err != nil {
				t.Fatal(err)
			}
			if plan != nil {
				t.Error("PlanRemuxSegments planned an fMP4 copy of a source with a trim inside the run")
			}
		})
	}
}

// testOpusHead is a minimal OpusHead for a plan that has to parse one.
func testOpusHead(preSkip int) []byte {
	h := make([]byte, 19)
	copy(h, "OpusHead")
	h[8], h[9] = 1, 2
	h[10], h[11] = byte(preSkip), byte(preSkip>>8)
	h[12], h[13], h[14], h[15] = 0x80, 0xBB, 0, 0
	return h
}
