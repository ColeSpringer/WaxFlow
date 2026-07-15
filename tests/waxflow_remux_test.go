package waxflow_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/format"
)

// remuxFixture transcodes a synthesized WAV to opts and returns the bytes, for
// use as a remux source. It is the only way to get an Opus-in-Ogg or an
// AAC-in-fMP4 here: the testdata corpus has no such fixture, and building one
// through our own encoder is what the rest of the suite does.
// The destination is seekable because some outputs require it (AIFF and
// progressive MP4 back-patch their headers), and the ones that do not are
// unaffected in the ways these tests read: a seekable destination only lets a
// muxer improve its own framing (exact sizes, a FLAC seek table), never the
// packets it carries.
func remuxFixture(t *testing.T, opts waxflow.TranscodeOptions, frames int) []byte {
	t.Helper()
	raw, _ := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 7)
	e := waxflow.New()
	ws := &memWS{}
	if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "wav", ws, opts); err != nil {
		t.Fatalf("building the %s fixture: %v", opts.Format, err)
	}
	return ws.b
}

// payloads walks a container and returns every packet's payload, copied.
// Copies are the point: the demuxer reuses pkt.Data across calls, so a test
// that kept the slices would compare the last packet against itself.
func payloads(t *testing.T, raw []byte, hint string) [][]byte {
	t.Helper()
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := info.Default().ID
	var out [][]byte
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err != nil {
			break
		}
		if pkt.Track != id {
			continue
		}
		out = append(out, bytes.Clone(pkt.Data))
	}
	return out
}

// TestRemuxPayloadsAreByteIdentical is the headline proof of the middle rung:
// remux an Opus-in-Ogg to Matroska, demux it back, and assert the packet
// payloads are byte-for-byte the input's. That is exactly the claim remux makes
// (no generation loss) and it is directly checkable, unlike a listening test.
//
// Payload equality is a stronger assertion than a decode differential would be,
// and cheaper: it fails on a single flipped bit in one access unit, where a
// PCM comparison would have to decide how close is close enough.
func TestRemuxPayloadsAreByteIdentical(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	want := payloads(t, src, "opus")
	if len(want) == 0 {
		t.Fatal("the Opus fixture demuxed to no packets")
	}

	e := waxflow.New()
	var out bytes.Buffer
	res, err := e.Remux(context.Background(), container.BytesSource(src), "opus", &out,
		waxflow.TranscodeOptions{Format: "opus", Container: "mka"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Container != "mka" {
		t.Errorf("remux reported container %q, want mka", res.Container)
	}
	got := payloads(t, out.Bytes(), "mka")
	if len(got) != len(want) {
		t.Fatalf("remux moved %d packets, source had %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d changed: %d bytes out, %d in", i, len(got[i]), len(want[i]))
		}
	}
}

// TestRemuxGaplessRoundTrip is the milestone's gate. Remux deliberately
// bypasses format.Media's trimming: the packets carry the full untrimmed audio
// and the trim is re-signalled through the synthesized codec.Trailer instead. So
// one file's trim is expressed in two places, and a muxer that ignored the
// trailer would drop gapless silently, with correct-looking audio that plays a
// few milliseconds long. This pins the trims across both hops.
func TestRemuxGaplessRoundTrip(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	in := probeTrack(t, src, "opus")
	if in.Delay <= 0 {
		t.Fatalf("the Opus fixture must carry a pre-skip to test gapless, got Delay=%d", in.Delay)
	}

	e := waxflow.New()
	var mka bytes.Buffer
	if _, err := e.Remux(context.Background(), container.BytesSource(src), "opus", &mka,
		waxflow.TranscodeOptions{Format: "opus", Container: "mka"}); err != nil {
		t.Fatal(err)
	}
	hop1 := probeTrack(t, mka.Bytes(), "mka")

	// And back, which is what makes this a round trip rather than one hop: the
	// second remux reads the gapless the first one wrote.
	var back bytes.Buffer
	if _, err := e.Remux(context.Background(), container.BytesSource(mka.Bytes()), "mka", &back,
		waxflow.TranscodeOptions{Format: "opus"}); err != nil {
		t.Fatal(err)
	}
	hop2 := probeTrack(t, back.Bytes(), "opus")

	for _, tc := range []struct {
		name string
		got  container.Track
	}{{"mka", hop1}, {"ogg", hop2}} {
		if tc.got.Delay != in.Delay {
			t.Errorf("%s hop: Delay %d, want %d", tc.name, tc.got.Delay, in.Delay)
		}
		if tc.got.Padding != in.Padding {
			t.Errorf("%s hop: Padding %d, want %d", tc.name, tc.got.Padding, in.Padding)
		}
		if tc.got.Samples != in.Samples {
			t.Errorf("%s hop: Samples %d, want %d", tc.name, tc.got.Samples, in.Samples)
		}
	}
}

// TestRemuxFallsBackWhenGaplessCannotSurvive guards the quiet failure: rung 2
// accepts, the muxer drops the delay, and the audio plays untrimmed with no
// error anywhere. A rung that fails loudly is fine; a rung that succeeds wrongly
// is the thing to prevent.
//
// A FLAC track declaring a delay is a container claiming something the codec
// cannot mean (lossless streams have no encoder priming), and every muxer that
// writes FLAC refuses one. So the plan must decline and let rung 3 decode and
// trim honestly, rather than write a wrong edit list or die at End with a file
// already on the wire.
func TestRemuxFallsBackWhenGaplessCannotSurvive(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "flac"}, 4800)
	track := probeTrack(t, src, "flac")
	e := waxflow.New()

	// The undelayed track is the control: without it, a decline below could be
	// the codec rule rather than the gapless one.
	plan, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: "flac", Container: "mka"})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanRemux declined an ordinary FLAC to mka; the delayed case below would pass vacuously")
	}

	for _, tc := range []struct {
		name  string
		mutid func(container.Track) container.Track
	}{
		{"delayed", func(x container.Track) container.Track { x.Delay = 576; return x }},
		{"padded", func(x container.Track) container.Track { x.Padding = 576; return x }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := e.PlanRemux(tc.mutid(track), waxflow.TranscodeOptions{Format: "flac", Container: "mka"})
			if err != nil {
				return
			}
			if plan != nil {
				t.Fatal("PlanRemux accepted a FLAC carrying gapless trims no FLAC muxer can write")
			}
		})
	}
}

// probeTrack returns raw's default track.
func probeTrack(t *testing.T, raw []byte, hint string) container.Track {
	t.Helper()
	e := waxflow.New()
	info, err := e.Probe(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	return info.Default()
}

// TestPlanRemuxDeclinesEveryShapingOption pins the derived rule: every option
// that transforms samples must push the request off this rung. The list is
// spelled out here on purpose even though remuxable derives its own, because a
// test that derived it the same way would agree with the code by construction
// and prove nothing.
//
// The bitrate family is the reason this is worth a table. On the HLS path
// bitrate is per-variant and rides in the cache key, so a remux that ignored a
// set bitrate would serve one set of source packets under two different bitrate
// labels: two entries claiming different things about identical bytes.
func TestPlanRemuxDeclinesEveryShapingOption(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 4800)
	track := probeTrack(t, src, "opus")
	e := waxflow.New()

	// The baseline must be accepted, or every case below passes vacuously.
	base := waxflow.TranscodeOptions{Format: "opus", Container: "mka"}
	plan, err := e.PlanRemux(track, base)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanRemux declined the zero-option case; every case below would pass vacuously")
	}

	for _, tc := range []struct {
		name string
		opts waxflow.TranscodeOptions
	}{
		{"Rate", waxflow.TranscodeOptions{Rate: 44100}},
		{"Channels", waxflow.TranscodeOptions{Channels: 1}},
		{"BitDepth", waxflow.TranscodeOptions{BitDepth: 16}},
		{"GainDB", waxflow.TranscodeOptions{GainDB: -6}},
		{"Dynamics", waxflow.TranscodeOptions{Dynamics: gain.PresetVoice}},
		{"FromSample", waxflow.TranscodeOptions{FromSample: 480}},
		{"OpusBitrate", waxflow.TranscodeOptions{OpusBitrate: 64000}},
		{"OpusVBR", waxflow.TranscodeOptions{OpusVBR: true}},
		{"OpusComplexity", waxflow.TranscodeOptions{OpusComplexity: 3}},
		{"OpusSignal", waxflow.TranscodeOptions{OpusSignal: "voice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Format, opts.Container = base.Format, base.Container
			plan, err := e.PlanRemux(track, opts)
			if err != nil {
				return // rejected outright is fine: it is not a silent remux
			}
			if plan != nil {
				t.Fatalf("PlanRemux accepted %s, which transforms samples: rung 2 would ignore it", tc.name)
			}
		})
	}
}

// TestRemuxDeclinesPCMAcrossContainers is the regression test for a request this
// rung broke: format=wav on an AIFF source. Both tracks are codec.PCM, so a
// codec.ID comparison alone plans it as a remux, and it then dies in the muxer
// with "riff: WAV is little-endian".
//
// PCM's packet is not a container-independent access unit the way a real codec's
// is: its wire layout (endianness, 8-bit signedness) is the container's choice
// and lives in CodecConfig. The rung declines it and loses nothing, because a
// PCM-to-PCM transcode is a bit-exact byte repack with no generation to lose.
func TestRemuxDeclinesPCMAcrossContainers(t *testing.T) {
	e := waxflow.New()
	for _, tc := range []struct{ name, from, hint, to string }{
		{"aiff to wav", "aiff", "aiff", "wav"},
		{"wav to aiff", "wav", "wav", "aiff"},
		{"wav to mka", "wav", "wav", "wav"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := remuxFixture(t, waxflow.TranscodeOptions{Format: tc.from}, 4800)
			track := probeTrack(t, src, tc.hint)
			if track.Codec != codec.PCM {
				t.Fatalf("fixture codec %q, want pcm: this pins the PCM rule", track.Codec)
			}
			plan, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: tc.to})
			if err != nil {
				t.Fatal(err)
			}
			if plan != nil {
				t.Fatal("PlanRemux accepted a PCM track; its wire layout is the container's, not the codec's")
			}
		})
	}

	// And the request end to end: it must still be served, by rung 3.
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "aiff"}, 4800)
	var out bytes.Buffer
	if _, err := e.Transcode(context.Background(), container.BytesSource(src), "aiff", &out,
		waxflow.TranscodeOptions{Format: "wav"}); err != nil {
		t.Fatalf("transcoding an AIFF source to WAV: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("empty WAV output")
	}
}

// TestPlanRemuxDeclinesCodecMismatch pins the other half of the rule: the codec
// must survive unchanged, so a FLAC source cannot ride the opus row.
func TestPlanRemuxDeclinesCodecMismatch(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "flac"}, 4800)
	track := probeTrack(t, src, "flac")
	e := waxflow.New()
	plan, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Fatal("PlanRemux accepted a FLAC source as opus output; the codec cannot survive that")
	}
}

// TestRemuxPlanNamesNoCodecVersion pins what the cache key says. A remux runs no
// decoder and no encoder, so naming their revisions would invalidate entries for
// fixes that cannot reach them; the trailer synthesis is the one thing here that
// can go wrong in a way that is wrong playback rather than merely older bytes.
func TestRemuxPlanNamesNoCodecVersion(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 4800)
	track := probeTrack(t, src, "opus")
	e := waxflow.New()
	plan, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: "opus", Container: "mka"})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("PlanRemux declined a plain container rewrite")
	}
	if len(plan.Versions) != 1 || plan.Versions[0] != waxflow.RemuxVersion {
		t.Errorf("remux Versions = %v, want exactly [%s]", plan.Versions, waxflow.RemuxVersion)
	}
	// The plan must promise the source's own format, not a chain's output.
	if plan.Format != track.Fmt {
		t.Errorf("remux Format = %v, want the source's %v", plan.Format, track.Fmt)
	}
	if plan.Samples != track.Samples {
		t.Errorf("remux Samples = %d, want the source's %d", plan.Samples, track.Samples)
	}
	if plan.BitRate != 0 || plan.EstimatedBytes != -1 {
		t.Errorf("remux must not project a bit rate it cannot know: BitRate=%d EstimatedBytes=%d",
			plan.BitRate, plan.EstimatedBytes)
	}
}

// TestRemuxTrackCarriesCodecConfig pins that the config crosses unchanged: the
// OpusHead the output declares must be the one the source declared, since the
// packets it describes are the same packets.
func TestRemuxTrackCarriesCodecConfig(t *testing.T) {
	src := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 4800)
	in := probeTrack(t, src, "opus")

	e := waxflow.New()
	var out bytes.Buffer
	if _, err := e.Remux(context.Background(), container.BytesSource(src), "opus", &out,
		waxflow.TranscodeOptions{Format: "opus", Container: "mka"}); err != nil {
		t.Fatal(err)
	}
	got := probeTrack(t, out.Bytes(), "mka")
	if got.Codec != codec.Opus {
		t.Fatalf("remuxed track codec = %q, want opus", got.Codec)
	}
	if !bytes.Equal(got.CodecConfig, in.CodecConfig) {
		t.Errorf("CodecConfig changed across the remux:\n got %x\nwant %x", got.CodecConfig, in.CodecConfig)
	}
}
