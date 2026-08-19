package waxflow_test

// WavPack encode, through the engine. Two gates, and they check different
// things: the round trip proves our own pair agrees with itself, and ffmpeg's
// decode of the same file proves the pair agrees with the format. A codec can
// pass the first and fail the second forever, which is why both are here.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// wavpackLevels is every level the option exposes, by the name a reader of a
// failure message needs.
var wavpackLevels = []struct {
	name  string
	level int
}{
	{"default", waxflow.WavPackLevelDefault},
	{"fast", waxflow.WavPackLevelFast},
	{"normal", waxflow.WavPackLevelNormal},
	{"high", waxflow.WavPackLevelHigh},
	{"very-high", waxflow.WavPackLevelVeryHigh},
}

// encodeWavPack transcodes a WAV to WavPack and returns the .wv bytes.
func encodeWavPack(t *testing.T, e *waxflow.Engine, wav []byte, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	opts.Format = "wavpack"
	out := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", out, opts)
	if err != nil {
		t.Fatalf("transcode to wavpack: %v", err)
	}
	if res.Container != "wavpack" || res.Format.Type != audio.Int {
		t.Fatalf("result = %+v", res)
	}
	return out.b
}

// TestWavPackEncodeRoundTrip is the lossless gate: what comes back out of our
// own decoder is what went in, at every depth, level, rate, and channel count
// the row accepts.
func TestWavPackEncodeRoundTrip(t *testing.T) {
	e := waxflow.New()
	const frames = 20011 // not a block multiple: the last block is short
	for _, bits := range []int{8, 16, 24, 32} {
		enc := pcm.SignedInt
		if bits == 8 {
			enc = pcm.UnsignedInt // WAV spells 8-bit unsigned
		}
		cfg := pcm.Config{Encoding: enc, Bits: bits}
		for _, channels := range []int{1, 2} {
			for _, lv := range wavpackLevels {
				t.Run(strings.Join([]string{lv.name, cfg.PCMFormat(48000, channels, audio.DefaultLayout(channels)).String()}, "/"), func(t *testing.T) {
					t.Parallel()
					e := waxflow.New()
					wav, src := makeWAV(t, cfg, channels, frames, uint64(bits*10+channels))
					defer audio.Put(src)
					raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{WavPackLevel: lv.level})
					got := readAll(t, e, raw, frames)
					defer audio.Put(got)
					equalPCM(t, src, got)
				})
			}
		}
	}
	_ = e
}

// TestWavPackEncodeDifferential is the external gate: ffmpeg's decoder, which
// shares no code with ours, must get the same samples out of our file. It is
// what turns "our decoder undoes our encoder" into "this is WavPack".
func TestWavPackEncodeDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	dir := t.TempDir()
	const frames = 30000
	cases := []struct {
		name     string
		bits     int
		channels int
		rate     int
		level    int
		seed     uint64
	}{
		{"s16-stereo-normal", 16, 2, 48000, waxflow.WavPackLevelNormal, 1},
		{"s16-mono-fast", 16, 1, 48000, waxflow.WavPackLevelFast, 2},
		{"s24-stereo-high", 24, 2, 48000, waxflow.WavPackLevelHigh, 3},
		{"s32-stereo-very-high", 32, 2, 48000, waxflow.WavPackLevelVeryHigh, 4},
		{"u8-stereo-normal", 8, 2, 48000, waxflow.WavPackLevelNormal, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := pcm.SignedInt
			if tc.bits == 8 {
				enc = pcm.UnsignedInt
			}
			wav, src := makeWAV(t, pcm.Config{Encoding: enc, Bits: tc.bits}, tc.channels, frames, tc.seed)
			defer audio.Put(src)
			raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{WavPackLevel: tc.level})

			path := filepath.Join(dir, tc.name+".wv")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			ref := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(src), ref); idx != -1 {
				t.Fatalf("ffmpeg's decode of our WavPack differs at interleaved sample %d "+
					"(wrote %d samples, ffmpeg read %d)", idx, src.N*tc.channels, len(ref))
			}
			// The file must also describe itself correctly, not merely decode:
			// a reader that never opens a packet still has to get the shape
			// right.
			probe := testutil.FFprobeFile(t, path)
			if probe.CodecName != "wavpack" || probe.SampleRate != tc.rate || probe.Channels != tc.channels {
				t.Errorf("ffprobe reports %+v, want wavpack %dHz %dch", probe, tc.rate, tc.channels)
			}
		})
	}
}

// TestWavPackEncodeCompresses pins that the levels do something. A lossless
// encoder wired backward still round-trips, since the decoder undoes whatever
// the encoder did, so size is the only check that the cascade is predicting
// rather than merely being reversed. It runs on tonal material because white
// noise is incompressible by construction and would pass a broken encoder.
//
// The ordering the levels promise is a wider search, not a smaller file to the
// byte: the search is greedy per block over an entropy state the blocks share,
// so a deeper level can land a hair above a shallower one. The tolerance is
// what that costs, measured; the real claim is the last line.
func TestWavPackEncodeCompresses(t *testing.T) {
	e := waxflow.New()
	const frames = 48000
	wav, src := makeWAVOf(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, partials)
	defer audio.Put(src)
	raw := frames * 2 * 2
	sizes := map[string]int{}
	prev, prevName := 0, ""
	for _, lv := range wavpackLevels[1:] { // skip "default", which is normal
		n := len(encodeWavPack(t, e, wav, waxflow.TranscodeOptions{WavPackLevel: lv.level}))
		sizes[lv.name] = n
		if n > raw/2 {
			t.Errorf("level %s: %d bytes for %d raw of partials, which should halve", lv.name, n, raw)
		}
		if prev != 0 && n > prev+prev/100 {
			t.Errorf("level %s grew to %d bytes from %s's %d, past the 1%% the greedy search costs",
				lv.name, n, prevName, prev)
		}
		prev, prevName = n, lv.name
	}
	if sizes["very-high"] >= sizes["fast"] {
		t.Errorf("the deepest search (%d bytes) did not beat the shallowest (%d)",
			sizes["very-high"], sizes["fast"])
	}
}

// partials fills a buffer with a few partials under a slow envelope: predictable
// enough to compress, and not so predictable that one decorrelation pass
// captures all of it.
func partials(b *audio.Buffer) {
	full := float64(int32(1)<<(b.Fmt.BitDepth-1) - 1)
	for c := range b.Fmt.Channels {
		s := b.ChanI(c)
		for i := range s {
			t := float64(i) / float64(b.Fmt.Rate)
			v := 0.5*math.Sin(2*math.Pi*(220+float64(c))*t) +
				0.25*math.Sin(2*math.Pi*443*t) + 0.1*math.Sin(2*math.Pi*1319*t)
			s[i] = int32(v * full * (0.6 + 0.4*math.Sin(2*math.Pi*1.7*t)))
		}
	}
}

// TestWavPackPlanMatchesRun pins the plan against the run it plans: the level
// is in the cache key (ADR-0004), so two levels must key differently and each
// must describe the file it actually produces.
func TestWavPackPlanMatchesRun(t *testing.T) {
	e := waxflow.New()
	const frames = 12000
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 21)
	defer audio.Put(src)
	info, err := e.Probe(container.BytesSource(wav), "wav", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()

	seen := map[string]string{}
	for _, lv := range wavpackLevels {
		opts := waxflow.TranscodeOptions{Format: "wavpack", WavPackLevel: lv.level}
		plan, err := e.PlanTranscode(track, opts)
		if err != nil {
			t.Fatalf("plan %s: %v", lv.name, err)
		}
		if plan.Container != "wavpack" || plan.MediaType != "audio/x-wavpack" || !plan.Live {
			t.Errorf("plan %s = %+v", lv.name, plan)
		}
		if plan.Samples != frames || plan.Format.BitDepth != 16 {
			t.Errorf("plan %s projects %d samples at %v", lv.name, plan.Samples, plan.Format)
		}
		// Lossless output is size-unknown, like FLAC: nothing may claim a rate.
		if plan.BitRate != 0 || plan.BytesPerFrame != 0 {
			t.Errorf("plan %s claims bitrate %d / %d bytes per frame for VBR lossless",
				lv.name, plan.BitRate, plan.BytesPerFrame)
		}
		key := strings.Join(plan.Versions, ",")
		if other, ok := seen[key]; ok && other != lv.name {
			// default and normal are the same level and must share a key;
			// anything else sharing one would serve stale cached bytes.
			if !(other == "default" && lv.name == "normal") {
				t.Errorf("levels %s and %s share cache key %q", other, lv.name, key)
			}
		}
		seen[key] = lv.name

		res, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", &memWS{}, opts)
		if err != nil {
			t.Fatalf("run %s: %v", lv.name, err)
		}
		if res.Samples != plan.Samples || res.Format != plan.Format || res.Container != plan.Container {
			t.Errorf("run %s = %+v, plan said %+v", lv.name, res, plan)
		}
	}
	if len(seen) != 4 {
		t.Errorf("%d distinct cache keys across five level spellings, want 4", len(seen))
	}
}

// TestWavPackEncodeRefuses pins the named refusals: an unknown level, a source
// wider than the format holds, and a container override the row has no second
// wrapper for.
func TestWavPackEncodeRefuses(t *testing.T) {
	e := waxflow.New()
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 4000, 31)
	defer audio.Put(src)
	wide, wsrc := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 6, 4000, 32)
	defer audio.Put(wsrc)

	cases := map[string]struct {
		wav  []byte
		opts waxflow.TranscodeOptions
		want string
	}{
		"a level below the scale": {wav, waxflow.TranscodeOptions{WavPackLevel: -1}, "WavPack level"},
		"a level above it":        {wav, waxflow.TranscodeOptions{WavPackLevel: 5}, "WavPack level"},
		// Lossless output never folds channels away: alac's rule, not the
		// lossy rows'. A 5.1 source is refused by name instead.
		"a source wider than stereo": {wide, waxflow.TranscodeOptions{}, "only mono and stereo"},
		"a container override":       {wav, waxflow.TranscodeOptions{Container: "mka"}, "container"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.Format = "wavpack"
			_, err := e.Transcode(context.Background(), container.BytesSource(tc.wav), "wav", &memWS{}, opts)
			if err == nil {
				t.Fatal("the transcode succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the problem (%q)", err, tc.want)
			}
		})
	}
}

// TestWavPackEncodeTags pins the metadata seam: the muxer's APEv2 trailer is
// what the demuxer reads back, so tags survive a transcode without a mapping
// post-pass.
func TestWavPackEncodeTags(t *testing.T) {
	e := waxflow.New()
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 8000, 41)
	defer audio.Put(src)
	tags := []container.Tag{{Key: "TITLE", Value: "Stage Five"}, {Key: "ALBUM", Value: "WaxFlow"}}
	raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{Tags: tags})

	info, err := e.Probe(container.BytesSource(raw), "wv", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range tags {
		if v := info.Tags[want.Key]; len(v) != 1 || v[0] != want.Value {
			t.Errorf("tag %s read back as %v, want %q", want.Key, v, want.Value)
		}
	}
	// The audio is unaffected by the trailer.
	got := readAll(t, e, raw, 8000)
	defer audio.Put(got)
	equalPCM(t, src, got)
}

// TestWavPackEncodeDepthPolicy pins the row's adjust hook: a float source
// quantizes to 24 bits, and an integer source at a width WavPack has no
// container for widens to the next one losslessly.
func TestWavPackEncodeDepthPolicy(t *testing.T) {
	e := waxflow.New()
	for _, tc := range []struct {
		name string
		cfg  pcm.Config
		want int
	}{
		{"float source", pcm.Config{Encoding: pcm.Float, Bits: 32}, 24},
		{"8-bit stays 8", pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, 8},
		{"16-bit stays 16", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 16},
		{"24-bit stays 24", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 24},
		{"32-bit stays 32", pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wav, src := makeWAV(t, tc.cfg, 2, 5000, 51)
			defer audio.Put(src)
			raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{})
			info, err := e.Probe(container.BytesSource(raw), "wv", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Default().Fmt.BitDepth; got != tc.want {
				t.Errorf("wrote %d-bit WavPack, want %d", got, tc.want)
			}
		})
	}
}

// TestWavPackRemuxMovesBlocks pins the rung the muxer opened: a .wv source
// asked for as WavPack has nothing to re-encode, so the blocks move through
// untouched and only the length field the muxer owns is rewritten. Without a
// muxer this request had no middle rung and fell through to a full
// re-encode, which for a lossless codec is correct output at several times
// the cost.
func TestWavPackRemuxMovesBlocks(t *testing.T) {
	e := waxflow.New()
	const frames = 20011
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 61)
	defer audio.Put(src)
	raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{})

	info, err := e.Probe(container.BytesSource(raw), "wv", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanRemux(info.Default(), waxflow.TranscodeOptions{Format: "wavpack"})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("WavPack to WavPack declined the remux rung")
	}
	var out memWS
	res, err := e.Remux(context.Background(), container.BytesSource(raw), "wv", &out,
		waxflow.TranscodeOptions{Format: "wavpack"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != frames {
		t.Errorf("remux reported %d samples, want %d", res.Samples, frames)
	}
	// The blocks are the source's, byte for byte: the whole file is, since a
	// .wv file is nothing but its blocks and the length was already right.
	if !bytes.Equal(out.b, raw) {
		t.Errorf("remuxed file is %d bytes, source was %d; a WavPack-to-WavPack "+
			"remux rewrites nothing but the length", len(out.b), len(raw))
	}
	got := readAll(t, e, out.b, frames)
	defer audio.Put(got)
	equalPCM(t, src, got)
}

// TestWavPackEncodeOddRate pins the one metadata sub-block a rate outside the
// header's sixteen-entry table needs. Nothing about the audio changes, so its
// absence would not fail a round trip through our own decoder (which would
// read the same wrong rate back): the check has to be a reader that was told
// the rate independently, and the cost of getting it wrong is every player
// pitching the file.
//
// The comparison is our decode against ffmpeg's of the same file rather than
// against the source, since a rate change resamples and the source samples are
// no longer the answer.
func TestWavPackEncodeOddRate(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const rate = 37000
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, 30000, 71)
	defer audio.Put(src)
	raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{Rate: rate})

	path := filepath.Join(t.TempDir(), "odd.wv")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if probe := testutil.FFprobeFile(t, path); probe.SampleRate != rate {
		t.Fatalf("ffprobe reads %d Hz, want %d: the sample-rate sub-block is missing or wrong",
			probe.SampleRate, rate)
	}
	ours := readAll(t, e, raw, 30000)
	defer audio.Put(ours)
	if ours.Fmt.Rate != rate {
		t.Fatalf("our own read-back reports %d Hz, want %d", ours.Fmt.Rate, rate)
	}
	ref := testutil.FFmpegDecodeS32(t, path)
	if idx := testutil.DiffI32(testutil.Interleave(ours), ref); idx != -1 {
		t.Fatalf("ffmpeg's decode differs at interleaved sample %d (ours %d samples, ffmpeg %d)",
			idx, ours.N*ours.Fmt.Channels, len(ref))
	}
}

// TestWavPackEncodeReferenceAccepts is the gate that matters most, and the one
// that found the bug the others could not. `wvunpack -v` is libwavpack's own
// verification of a stream, and its decode is the reference reading every
// header field rather than the subset a decoder needs: a 32-bit stream whose
// magnitude field claimed the nominal depth round-tripped through our decoder
// and ffmpeg's perfectly and was refused outright by the reference, because
// only it reads that field.
//
// 32-bit is first in the table for that reason. It is the depth where the
// header has something to get wrong.
func TestWavPackEncodeReferenceAccepts(t *testing.T) {
	testutil.WvUnpackTool(t) // skips unless the reference tools are installed
	e := waxflow.New()
	dir := t.TempDir()
	const frames = 30011
	for _, bits := range []int{32, 8, 16, 24} {
		enc := pcm.SignedInt
		if bits == 8 {
			enc = pcm.UnsignedInt
		}
		for _, channels := range []int{2, 1} {
			for _, lv := range wavpackLevels[1:] {
				name := fmt.Sprintf("%dbit/%dch/%s", bits, channels, lv.name)
				t.Run(name, func(t *testing.T) {
					wav, src := makeWAV(t, pcm.Config{Encoding: enc, Bits: bits}, channels, frames,
						uint64(bits*100+channels*10+lv.level))
					defer audio.Put(src)
					raw := encodeWavPack(t, e, wav, waxflow.TranscodeOptions{WavPackLevel: lv.level})
					path := filepath.Join(dir, strings.ReplaceAll(name, "/", "-")+".wv")
					if err := os.WriteFile(path, raw, 0o644); err != nil {
						t.Fatal(err)
					}
					testutil.WvUnpackVerify(t, path)
					got := testutil.WvUnpackDecodeFile(t, path)
					want := testutil.WAVData(t, wav)
					if !bytes.Equal(got, want) {
						t.Fatalf("the reference decoder got %d bytes back, %d went in", len(got), len(want))
					}
				})
			}
		}
	}
}
