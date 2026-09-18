package waxflow_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
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
	return ws.Buf
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

// TestMP3InWAVRemuxesToMP3 proves the rung was TAKEN for a wrapper this
// stream added, which is the assertion an optimization rung needs: the
// packets a WAV's data chunk holds are already the elementary stream's, so
// `format=mp3` must copy them rather than decode and re-encode, and the bytes
// are how that is visible.
//
// `format=wav` on the same source is the other half. An MP3 track does not
// survive into a PCM row, so the rung has to decline and the request falls to
// transcode; a rung that accepted it would write a WAV full of MP3 frames.
func TestMP3InWAVRemuxesToMP3(t *testing.T) {
	e := waxflow.New()
	raw := wavMP3Bytes(t)
	want := payloads(t, raw, "wav")
	if len(want) == 0 {
		t.Fatal("the WAV fixture demuxed to no packets")
	}
	in := probeTrack(t, raw, "wav")
	plan, err := e.PlanRemux(in, waxflow.TranscodeOptions{Format: "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("the remux rung declined an MP3-in-WAV source for format=mp3; the packets cross unchanged")
	}
	var out bytes.Buffer
	if _, err := e.Remux(context.Background(), container.BytesSource(raw), "wav", &out,
		waxflow.TranscodeOptions{Format: "mp3"}); err != nil {
		t.Fatal(err)
	}
	// The muxer writes its own metadata frame at the head, which the reader
	// consumes again, so the packet lists compare one to one.
	outBytes := out.Bytes()
	got := payloads(t, outBytes, "mp3")
	if len(got) != len(want) {
		t.Fatalf("remux moved %d packets, the source had %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d changed: %d bytes out, %d in", i, len(got[i]), len(want[i]))
		}
	}

	wavPlan, err := e.PlanRemux(in, waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatal(err)
	}
	if wavPlan != nil {
		t.Error("the remux rung accepted format=wav for an MP3 track; PCM output has to transcode")
	}

	// And what the length costs across the hop, on each kind of destination.
	// A WAV carrying MP3 frames states its length in a fact chunk, which counts
	// what those frames decode to and not where the audio inside them begins or
	// ends: an advisory total. This writer cannot seek, so whatever the
	// metadata frame declares at Begin is what stands, and an estimate stood
	// there as a fact. Declaring nothing is the honest form, and it is what the
	// muxer already writes for a source with no length at all.
	back := probeTrack(t, outBytes, "mp3")
	if back.Samples != -1 {
		t.Errorf("remuxed length on a pipe = %d, want none declared: the source's %d is an estimate "+
			"and this writer cannot go back and correct it", back.Samples, in.Samples)
	}

	// The same remux onto a writer the muxer can patch declares the count End
	// measured, so nothing is lost by not guessing at Begin.
	seek := &testutil.MemWriteSeeker{}
	if _, err := e.Remux(context.Background(), container.BytesSource(raw), "wav", seek,
		waxflow.TranscodeOptions{Format: "mp3"}); err != nil {
		t.Fatal(err)
	}
	// The LAME tag's head-trim field holds the ENCODER's share and every reader
	// adds Layer III's fixed 529-sample decoder latency back, so the smallest
	// trim it can express is 529: a zero is written as zero and read as 529,
	// and the kept length is short by the same amount. See mpa's tagTrims,
	// where the clamp and its reason live.
	patched := probeTrack(t, seek.Buf, "mp3")
	if patched.Delay != mpa.DecoderDelay {
		t.Errorf("patched head trim = %d, want %d: the LAME field cannot express a smaller one",
			patched.Delay, mpa.DecoderDelay)
	}
	if want := in.Samples - mpa.DecoderDelay; patched.Samples != want {
		t.Errorf("patched length = %d, want %d (the source's %d less the latency the tag restores)",
			patched.Samples, want, in.Samples)
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
// with "wav: WAV is little-endian".
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
	// The trailer synthesis and the muxer, and nothing else: no decoder and
	// no encoder ran, so no revision of either can reach these bytes.
	if len(plan.Versions) != 2 || plan.Versions[0] != waxflow.RemuxVersion ||
		plan.Versions[1] != mka.MuxerVersion {
		t.Errorf("remux Versions = %v, want exactly [%s %s]", plan.Versions,
			waxflow.RemuxVersion, mka.MuxerVersion)
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

// The mid-stream trim fixture: the block that states a DiscardPadding and how
// much it trims. Block 8 puts it well inside the first cluster, so a copy meets
// it early and a plan that declined it can be told from one that never got
// there. server.MidTrimWebM builds the same shape from seed-opus.webm, whose
// seven packets put the trim at block 2; see its comment for why the two are
// separate builders.
const (
	midTrimBlock = 8
	midTrimPad   = 480
)

// midTrimWebM builds an Opus-in-WebM whose block at midTrimBlock carries a
// DiscardPadding, out of our own encoder's packets through our own muxer. It is the shape
// mkvmerge leaves at an append seam, which no encoder in this tree produces.
//
// Every block holds one packet, because the muxer never laces, and that is
// what lets ffmpeg be an oracle for the file: ffmpeg hands a laced block's
// DiscardPadding to every lace and ignores one larger than the frame it rides
// on, so only an unlaced block whose trim fits means the same thing to both
// readers.
//
// declareLength writes the file through a seekable destination, so it carries
// an Info Duration and a Cues index; without it the track declares no length
// at all and the muxer writes no Duration element, which is the shape a plain
// /stream used to plan from unwalked.
func midTrimWebM(t *testing.T, declareLength bool) []byte {
	t.Helper()
	ogg := remuxFixture(t, waxflow.TranscodeOptions{Format: "opus"}, 48000)
	demux, info, err := format.OpenDemuxer(container.BytesSource(ogg), "opus", nil)
	if err != nil {
		t.Fatal(err)
	}
	src := info.Default()

	track := src
	track.ID, track.Default = 0, true
	track.Samples = src.Samples - midTrimPad
	// The Ogg source states its tail trim in the final page's granule, so the
	// packets run past its declared end; that difference is the end trim the
	// Matroska file has to state outright.
	endPad := srcRawSamples(t, ogg) - src.Delay - src.Samples
	if endPad <= 0 {
		t.Fatalf("the Opus fixture has no tail padding (%d); this cell needs one", endPad)
	}
	buf := &bytes.Buffer{}
	ws := &memWS{}
	var w io.Writer = buf
	if declareLength {
		w = ws
	} else {
		track.Samples = -1
	}
	m := mka.NewMuxer(w, &mka.MuxerOptions{WebM: true})
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket %d: %v", i, err)
		}
		out := container.Packet{Packet: pkt.Packet}
		if i == midTrimBlock {
			out.Padding = midTrimPad
		}
		if err := m.WritePacket(out); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(codec.Trailer{Samples: track.Samples, Delay: src.Delay, Padding: endPad}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if declareLength {
		return ws.Buf
	}
	return buf.Bytes()
}

// srcRawSamples is the raw decode duration a container's packets add up to.
func srcRawSamples(t *testing.T, raw []byte) int64 {
	t.Helper()
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := info.Default().ID
	var pkt container.Packet
	var total int64
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			return total
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.Track == id {
			total += pkt.Dur - min(pkt.Padding, pkt.Dur)
		}
	}
}

// walkedTrack opens raw, finishes its deferred walk, and returns the track the
// walk settled. It is what a measure leaves in the daemon's memo.
func walkedTrack(t *testing.T, raw []byte, hint string) container.Track {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	w, ok := med.(format.Walker)
	if !ok {
		t.Fatalf("a %s Media defers no walk", hint)
	}
	if err := w.Walk(); err != nil {
		t.Fatal(err)
	}
	return med.Info().Default()
}

// decodeWhole decodes a whole file to one interleaved float slice, with no
// expected frame count to check against: these cells are measuring the count.
func decodeWhole(t *testing.T, raw []byte, hint string) []float32 {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	var out []float32
	for {
		err := med.ReadChunk(tmp)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, testutil.InterleaveF(tmp)...)
	}
}

// TestRemuxForwardsAMidStreamTrim: Matroska is the one output container that
// states trims per packet, so a copy into it carries the source's inner trim
// through and the output plays exactly what the source played.
func TestRemuxForwardsAMidStreamTrim(t *testing.T) {
	src := midTrimWebM(t, true)
	want := decodeWhole(t, src, "webm")
	e := waxflow.New()
	for _, cont := range []string{"webm", "mka"} {
		t.Run(cont, func(t *testing.T) {
			demux, info, err := format.OpenDemuxer(container.BytesSource(src), "webm", nil)
			if err != nil {
				t.Fatal(err)
			}
			out := &memWS{}
			if _, err := e.RemuxDemuxer(context.Background(), demux, info.Default(), out,
				waxflow.TranscodeOptions{Format: "opus", Container: cont}); err != nil {
				t.Fatalf("RemuxDemuxer: %v", err)
			}
			// The trim survives as a trim, on the block that stated it.
			copied := walkedTrack(t, out.Buf, cont)
			if copied.MidPadding != midTrimPad {
				t.Errorf("the copy reports MidPadding %d, want the source's %d",
					copied.MidPadding, midTrimPad)
			}
			got := decodeWhole(t, out.Buf, cont)
			if len(got) != len(want) {
				t.Fatalf("the copy decodes to %d values, the source to %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("the copy differs from the source at value %d", i)
				}
			}
		})
	}
}

// TestRemuxMidStreamTrimMatchesFFmpeg is the same claim against the reference
// reader. The fixture's padded block is unlaced and its trim fits the frame,
// which is the one shape ffmpeg and this tree agree about (see midTrimWebM).
func TestRemuxMidStreamTrimMatchesFFmpeg(t *testing.T) {
	testutil.FFmpeg(t)
	src := midTrimWebM(t, true)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.webm")
	if err := os.WriteFile(srcPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	ours := decodeWhole(t, src, "webm")
	if n := len(testutil.FFmpegDecodeF32(t, srcPath)); n != len(ours) {
		t.Fatalf("ffmpeg decodes the source to %d values, we decode %d", n, len(ours))
	}

	e := waxflow.New()
	demux, info, err := format.OpenDemuxer(container.BytesSource(src), "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := &memWS{}
	if _, err := e.RemuxDemuxer(context.Background(), demux, info.Default(), out,
		waxflow.TranscodeOptions{Format: "opus", Container: "webm"}); err != nil {
		t.Fatalf("RemuxDemuxer: %v", err)
	}
	outPath := filepath.Join(dir, "out.webm")
	if err := os.WriteFile(outPath, out.Buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := len(testutil.FFmpegDecodeF32(t, outPath)); n != len(ours) {
		t.Errorf("ffmpeg decodes the copy to %d values, the source to %d", n, len(ours))
	}
}

// TestRemuxRefusesAMidStreamTrimItCannotCarry: an Ogg granule names one end
// trim, so a copy into it would play the inner trim's frames as audio. Nothing
// measured this source, so the plan could not know; the copy refuses on the
// packet after the padded one and the message names the way past it.
func TestRemuxRefusesAMidStreamTrimItCannotCarry(t *testing.T) {
	src := midTrimWebM(t, true)
	demux, info, err := format.OpenDemuxer(container.BytesSource(src), "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	if track.MidPadding != 0 {
		t.Fatalf("the source arrives measured (MidPadding %d); this cell needs the unwalked shape", track.MidPadding)
	}
	e := waxflow.New()
	var out bytes.Buffer
	_, err = e.RemuxDemuxer(context.Background(), demux, track, &out,
		waxflow.TranscodeOptions{Format: "opus"})
	if err == nil {
		t.Fatal("the copy into Ogg-Opus finished; the inner trim cannot be expressed there")
	}
	if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %v, want %v: %v", code, waxerr.CodeUnsupportedFormat, err)
	}
	for _, want := range []string{"trims 480 samples in the middle", "Matroska", "transcode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestPlanRemuxDeclinesAMeasuredMidStreamTrim: once a walk has found the inner
// trim, the plan declines rather than letting the copy start and fail, and the
// ladder falls through to the transcode rung. Matroska still plans, because it
// can carry the trim.
func TestPlanRemuxDeclinesAMeasuredMidStreamTrim(t *testing.T) {
	track := walkedTrack(t, midTrimWebM(t, true), "webm")
	if track.MidPadding != midTrimPad {
		t.Fatalf("the walk found MidPadding %d, want %d", track.MidPadding, midTrimPad)
	}
	e := waxflow.New()
	for _, tc := range []struct {
		cont string
		want bool
	}{
		{"", false},    // Ogg-Opus, the row's default: a granule names one end trim
		{"mka", true},  // states trims per packet
		{"webm", true}, // the same muxer
	} {
		name := tc.cont
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			plan, err := e.PlanRemux(track, waxflow.TranscodeOptions{Format: "opus", Container: tc.cont})
			if err != nil {
				t.Fatal(err)
			}
			if (plan != nil) != tc.want {
				t.Errorf("PlanRemux to %q returned %v, want planned=%v", tc.cont, plan != nil, tc.want)
			}
		})
	}
}

// TestMeasuredMatroskaTrimDeclinesTheCopy is the other half of what a measured
// Matroska track now carries: the final block's DiscardPadding reaches the
// plan, not just the length.
//
// It changes an answer, which is the point. With the trim invisible, the copy
// rung planned and then dropped it through remuxTrailer's unprimed rule (a
// lossless codec has no priming to flush, and every FLAC muxer refuses a
// nonzero one), so the trimmed frames played in the output. With it visible,
// gaplessSurvives declines the rung and the transcode serves, which honours
// the trim because it trims in PCM.
func TestMeasuredMatroskaTrimDeclinesTheCopy(t *testing.T) {
	const frames, endPad = 19200, 480
	wav, _ := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 11)
	e := waxflow.New()
	plain := &memWS{}
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", plain,
		waxflow.TranscodeOptions{Format: "flac", Container: "mka"}); err != nil {
		t.Fatal(err)
	}

	// The same packets, muxed again with an end trim on the final block. No
	// encoder here writes one for a lossless codec; a third-party muxer can.
	demux, info, err := format.OpenDemuxer(container.BytesSource(plain.Buf), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	track.ID, track.Default = 0, true
	track.Samples = frames - endPad
	out := &memWS{}
	m := mka.NewMuxer(out, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := m.WritePacket(container.Packet{Packet: pkt.Packet}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.End(codec.Trailer{Samples: track.Samples, Padding: endPad}); err != nil {
		t.Fatal(err)
	}

	measured := walkedTrack(t, out.Buf, "mka")
	if measured.Padding != endPad {
		t.Fatalf("the measure reports Padding %d, want the final block's %d", measured.Padding, endPad)
	}
	if measured.Samples != frames-endPad {
		t.Errorf("the measure reports %d samples, want %d", measured.Samples, frames-endPad)
	}
	for _, cont := range []string{"", "mka"} {
		plan, err := e.PlanRemux(measured, waxflow.TranscodeOptions{Format: "flac", Container: cont})
		if err != nil {
			t.Fatal(err)
		}
		if plan != nil {
			t.Errorf("PlanRemux to flac/%q planned a copy of a track carrying a trim no FLAC muxer writes", cont)
		}
	}

	// The rung below it honours the trim, which is what makes the decline the
	// right answer rather than merely a safe one.
	dec := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(out.Buf), "mka", dec,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != frames-endPad {
		t.Errorf("the transcode delivered %d samples, want the trimmed %d", res.Samples, frames-endPad)
	}
}
