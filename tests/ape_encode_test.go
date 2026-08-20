package waxflow_test

// Monkey's Audio encode, through the engine. Four gates, and they fail
// differently. The round trip proves our own pair agrees with itself. ffmpeg's
// decode proves the pair agrees with the format. The reference tool's verify
// proves the file's own bookkeeping (its stored MD5 covers the frame data, the
// format header, and the whole seek table), which no decoder here reads. And
// the byte comparison against the reference encoder's frames proves the coding
// itself, not merely that something reversible happened.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// apeEncodeLevels is every level the option exposes, by the name a reader of a
// failure message needs.
var apeEncodeLevels = []struct {
	name  string
	level int
}{
	{"default", waxflow.APELevelDefault},
	{"fast", waxflow.APELevelFast},
	{"normal", waxflow.APELevelNormal},
	{"high", waxflow.APELevelHigh},
}

// encodeAPE transcodes a WAV to Monkey's Audio and returns the .ape bytes.
func encodeAPE(t *testing.T, e *waxflow.Engine, wav []byte, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	opts.Format = "ape"
	out := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", out, opts)
	if err != nil {
		t.Fatalf("transcode to ape: %v", err)
	}
	if res.Container != "ape" || res.Format.Type != audio.Int {
		t.Fatalf("result = %+v", res)
	}
	return out.Buf
}

// apeFile writes an encode to a temp file, which is what the external tools
// take.
func apeFile(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(dir, name+".ape")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// apeWireConfig is the WAV wire encoding for a depth: 8-bit rides unsigned,
// everything wider is signed little-endian.
func apeWireConfig(bits int) pcm.Config {
	if bits == 8 {
		return pcm.Config{Encoding: pcm.UnsignedInt, Bits: bits}
	}
	return pcm.Config{Encoding: pcm.SignedInt, Bits: bits}
}

// TestAPEEncodeRoundTrip is the lossless gate: what comes back out of our own
// decoder is what went in, at every depth, level, and channel count the row
// accepts. The frame count is deliberately not a frame multiple, so the last
// frame is short, which is the only frame whose block count the file header
// states separately.
func TestAPEEncodeRoundTrip(t *testing.T) {
	const frames = ape.BlocksPerFrame + 5011
	for _, bits := range []int{8, 16, 24} {
		for _, channels := range []int{1, 2} {
			for _, lv := range apeEncodeLevels {
				name := fmt.Sprintf("%s/%dbit/%dch", lv.name, bits, channels)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					e := waxflow.New()
					wav, src := makeWAV(t, apeWireConfig(bits), channels, frames, uint64(bits*10+channels))
					defer audio.Put(src)
					raw := encodeAPE(t, e, wav, waxflow.TranscodeOptions{APELevel: lv.level})
					got := readAll(t, e, raw, frames)
					defer audio.Put(got)
					equalPCM(t, src, got)
				})
			}
		}
	}
}

// TestAPEEncodeDifferential is the external gate: ffmpeg's decoder, which
// shares no code with ours, must get the same samples out of our file. It is
// what turns "our decoder undoes our encoder" into "this is Monkey's Audio".
func TestAPEEncodeDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	dir := t.TempDir()
	const frames = 30000
	cases := []struct {
		name     string
		bits     int
		channels int
		level    int
		seed     uint64
	}{
		{"s16-stereo-normal", 16, 2, waxflow.APELevelNormal, 1},
		{"s16-mono-fast", 16, 1, waxflow.APELevelFast, 2},
		{"s24-stereo-high", 24, 2, waxflow.APELevelHigh, 3},
		{"u8-stereo-normal", 8, 2, waxflow.APELevelNormal, 4},
		{"s24-mono-normal", 24, 1, waxflow.APELevelNormal, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wav, src := makeWAV(t, apeWireConfig(tc.bits), tc.channels, frames, tc.seed)
			defer audio.Put(src)
			path := apeFile(t, dir, tc.name, encodeAPE(t, e, wav, waxflow.TranscodeOptions{APELevel: tc.level}))

			ref := testutil.FFmpegDecodeS32(t, path)
			if idx := testutil.DiffI32(testutil.Interleave(src), ref); idx != -1 {
				t.Fatalf("ffmpeg's decode of our APE differs at interleaved sample %d "+
					"(wrote %d samples, ffmpeg read %d)", idx, src.N*tc.channels, len(ref))
			}
			// The file must also describe itself correctly, not merely decode:
			// a reader that never opens a frame still has to get the shape
			// right.
			probe := testutil.FFprobeFile(t, path)
			if probe.CodecName != "ape" || probe.SampleRate != 48000 || probe.Channels != tc.channels {
				t.Errorf("ffprobe reports %+v, want ape 48000Hz %dch", probe, tc.channels)
			}
		})
	}
}

// TestAPEEncodeReferenceAccepts is the gate no decoder can stand in for. The
// reference tool's quick verify recomputes the file's stored MD5, which covers
// the frame data, the format header, and the whole seek table -- fields our
// decoder and ffmpeg's both ignore, so a file that plays everywhere can still
// be wrong in ways only this notices. Its decode is checked too, against the
// samples that went in: a verify says the bytes are intact, not that they are
// the right bytes.
func TestAPEEncodeReferenceAccepts(t *testing.T) {
	testutil.APETool(t) // skips unless `make ape-tools` has run
	e := waxflow.New()
	dir := t.TempDir()
	const frames = 30011
	for _, bits := range []int{16, 8, 24} {
		for _, channels := range []int{2, 1} {
			for _, lv := range apeEncodeLevels[1:] {
				name := fmt.Sprintf("%dbit/%dch/%s", bits, channels, lv.name)
				t.Run(name, func(t *testing.T) {
					cfg := apeWireConfig(bits)
					wav, src := makeWAV(t, cfg, channels, frames, uint64(bits*100+channels*10+lv.level))
					defer audio.Put(src)
					raw := encodeAPE(t, e, wav, waxflow.TranscodeOptions{APELevel: lv.level})
					path := apeFile(t, dir, strings.ReplaceAll(name, "/", "-"), raw)

					testutil.APEVerifyFile(t, path)

					// The reference's own decode, compared against the WAV we
					// handed the engine. Both are canonical PCM WAVs at the
					// same width, so the payloads compare directly.
					back := testutil.APEDecodeFile(t, path)
					want, got := testutil.WAVData(t, wav), testutil.WAVData(t, back)
					if !bytes.Equal(want, got) {
						t.Fatalf("the reference's decode of our file is %d bytes against the source's %d, "+
							"and they differ", len(got), len(want))
					}
				})
			}
		}
	}
}

// TestAPEEncodeMatchesTheReferenceFrames is the strongest statement available
// about the port: given the same samples and the same level, our coded frames
// are the reference encoder's coded frames, byte for byte. Nothing about
// Monkey's Audio fixes an encoder's choices, so this is not required for
// correctness -- what it buys is that a drift in any of the pieces (the
// predictor's weight adaptation, the filter's step quantizer, the range
// coder's carry handling, the mid/side matrix) shows up as a diff at the first
// byte it touches rather than as a fraction of a percent of size nobody
// notices.
//
// The comparison is per frame and in the coder's own byte order rather than
// over whole files, because a file carries more than its frames: the reference
// stores the source WAV's header, states its own program version, and sizes
// its seek table from what it was told the length would be. None of that is
// the coding.
func TestAPEEncodeMatchesTheReferenceFrames(t *testing.T) {
	testutil.APETool(t)
	e := waxflow.New()
	dir := t.TempDir()
	const frames = 2*ape.BlocksPerFrame + 9000
	for _, bits := range []int{16, 8, 24} {
		for _, channels := range []int{2, 1} {
			for _, lv := range apeEncodeLevels[1:] {
				name := fmt.Sprintf("%dbit/%dch/%s", bits, channels, lv.name)
				t.Run(name, func(t *testing.T) {
					cfg := apeWireConfig(bits)
					wav, src := makeWAV(t, cfg, channels, frames, uint64(bits*7+channels))
					defer audio.Put(src)
					ours := apeCodedFrames(t, encodeAPE(t, e, wav, waxflow.TranscodeOptions{APELevel: lv.level}))

					srcWav := filepath.Join(dir, strings.ReplaceAll(name, "/", "-")+".wav")
					if err := os.WriteFile(srcWav, wav, 0o644); err != nil {
						t.Fatal(err)
					}
					refPath := testutil.APEEncodeFile(t, srcWav, strings.ReplaceAll(name, "/", "-")+".ape", lv.level)
					refRaw, err := os.ReadFile(refPath)
					if err != nil {
						t.Fatal(err)
					}
					want := apeCodedFrames(t, refRaw)

					if len(ours) != len(want) {
						t.Fatalf("wrote %d frames, the reference wrote %d", len(ours), len(want))
					}
					for i := range want {
						// Only the reference's non-final frames state their own length:
						// its last one runs to the end of the frame-data region, which
						// carries the trailing word every encoder writes there. So the
						// exact claim is made where the evidence is exact, and the last
						// frame is compared over what both files agree they hold.
						if i < len(want)-1 {
							if !bytes.Equal(ours[i], want[i]) {
								t.Fatalf("frame %d differs from the reference's at byte %d (ours %d bytes, theirs %d)",
									i, firstDiff(ours[i], want[i]), len(ours[i]), len(want[i]))
							}
							continue
						}
						n := min(len(ours[i]), len(want[i]))
						if d := firstDiff(ours[i][:n], want[i][:n]); d != n {
							t.Fatalf("the last frame differs from the reference's at byte %d of %d", d, n)
						}
					}
				})
			}
		}
	}
}

// apeCodedFrames returns each frame's coded bytes in the range coder's own
// order, trimmed to the frame's own length. The seek table gives the extent,
// which is the only place it is written down; the last frame runs to the end
// of the region and so carries the trailing word with it.
func apeCodedFrames(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	table := raw[h.SeekTableOffset:]
	end := int64(len(raw)) - h.TerminatingBytes
	out := make([][]byte, 0, h.TotalFrames)
	for i := range h.TotalFrames {
		start := int64(binary.LittleEndian.Uint32(table[i*4:]))
		stop := end
		if i+1 < h.TotalFrames {
			stop = int64(binary.LittleEndian.Uint32(table[(i+1)*4:]))
		}
		// Logical byte j of the frame data region sits at physical
		// j&^3 + 3 - j&3; the frame runs from its own logical offset to the
		// next one's.
		frame := make([]byte, 0, stop-start)
		for j := start - h.FrameDataOffset; j < stop-h.FrameDataOffset; j++ {
			p := h.FrameDataOffset + j&^3 + 3 - j&3
			if p >= int64(len(raw)) {
				break
			}
			frame = append(frame, raw[p])
		}
		out = append(out, frame)
	}
	return out
}

// firstDiff is the index of the first differing byte, or the shorter length.
func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestAPEEncodeCompresses pins that the levels do something. A lossless
// encoder wired backward still round-trips, since the decoder undoes whatever
// the encoder did, so size is the only check that the cascade is predicting
// rather than merely being reversed. It runs on tonal material because white
// noise is incompressible by construction and would pass a broken encoder.
//
// The claim is deliberately not "deeper is smaller". A level here is one fixed
// filter cascade, not a search over several, and on synthesized material the
// 64-tap chain loses to the 16-tap one by about half a percent at every length
// measured: a longer filter carries a longer transient and these signals give
// it nothing to earn that back with. The reference encoder does the same thing
// on the same input, which answers whether it is a defect. Matching the
// reference to the byte is the stronger statement anyway, and
// TestAPEEncodeMatchesTheReferenceFrames makes it.
func TestAPEEncodeCompresses(t *testing.T) {
	e := waxflow.New()
	const frames = 48000
	wav, src := makeWAVOf(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, partials)
	defer audio.Put(src)
	raw := frames * 2 * 2
	sizes := map[string]int{}
	for _, lv := range apeEncodeLevels[1:] {
		n := len(encodeAPE(t, e, wav, waxflow.TranscodeOptions{APELevel: lv.level}))
		sizes[lv.name] = n
		if n > raw/2 {
			t.Errorf("level %s: %d bytes for %d raw of partials, which should halve", lv.name, n, raw)
		}
	}
	// The one ordering that holds everywhere measured: the fast level runs no
	// filter at all, so a level that runs one has to beat it.
	if sizes["normal"] >= sizes["fast"] {
		t.Errorf("the 16-tap cascade (%d bytes) did not beat no cascade at all (%d)",
			sizes["normal"], sizes["fast"])
	}
	if sizes["high"] >= sizes["fast"] {
		t.Errorf("the 64-tap cascade (%d bytes) did not beat no cascade at all (%d)",
			sizes["high"], sizes["fast"])
	}
}

// TestAPEEncodeSpecialFramesFire is the "assert the optimization was taken"
// check. A frame whose channels are digitally silent, or identical, is coded
// as a header and nothing else: the entropy coder runs zero times. A stream
// that never took either path would still be correct, and would still
// round-trip, so nothing else here would notice it had stopped happening. The
// evidence is size, since a special frame is a couple of dozen bytes whatever
// its length.
func TestAPEEncodeSpecialFramesFire(t *testing.T) {
	e := waxflow.New()
	const frames = ape.BlocksPerFrame
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}

	// Explicitly zeroed: makeWAVOf fills a pooled buffer, so a no-op fill
	// would hand the encoder whatever the last caller left in it.
	silent, ssrc := makeWAVOf(t, cfg, 2, frames, func(b *audio.Buffer) {
		for c := range b.Fmt.Channels {
			clear(b.ChanI(c))
		}
	})
	defer audio.Put(ssrc)
	dual, dsrc := makeWAVOf(t, cfg, 2, frames, func(b *audio.Buffer) {
		full := float64(int32(1)<<15 - 1)
		for i := range b.N {
			v := int32(0.5 * full * math.Sin(2*math.Pi*440*float64(i)/float64(b.Fmt.Rate)))
			b.ChanI(0)[i], b.ChanI(1)[i] = v, v
		}
	})
	defer audio.Put(dsrc)
	stereo, tsrc := makeWAVOf(t, cfg, 2, frames, partials)
	defer audio.Put(tsrc)

	silentSize := len(encodeAPE(t, e, silent, waxflow.TranscodeOptions{}))
	dualSize := len(encodeAPE(t, e, dual, waxflow.TranscodeOptions{}))
	stereoSize := len(encodeAPE(t, e, stereo, waxflow.TranscodeOptions{}))

	// A silent frame codes no values at all, so the whole file is its own
	// header plus a seek table. Anything past a few hundred bytes means the
	// silence went through the entropy coder.
	if silentSize > 512 {
		t.Errorf("a whole frame of digital silence took %d bytes; the silent-frame code did not fire", silentSize)
	}
	// Two identical channels code one of them, so the file is about half what
	// the same material costs as real stereo. The mono side of a pseudo-stereo
	// frame is a tone, so the comparison is against tonal stereo.
	if dualSize*3 > stereoSize*2 {
		t.Errorf("duplicated channels took %d bytes against real stereo's %d; "+
			"the pseudo-stereo code did not fire", dualSize, stereoSize)
	}
	// A frame that codes no values still has to decode to what went in,
	// which is what makes the two above optimizations rather than damage.
	for _, pair := range []struct {
		name string
		wav  []byte
		src  *audio.Buffer
	}{{"silence", silent, ssrc}, {"duplicated", dual, dsrc}} {
		t.Run(pair.name, func(t *testing.T) {
			got := readAll(t, e, encodeAPE(t, e, pair.wav, waxflow.TranscodeOptions{}), frames)
			defer audio.Put(got)
			equalPCM(t, pair.src, got)
		})
	}
}

// TestAPEPlanMatchesRun pins the plan against the run it plans: the level is
// in the cache key (ADR-0004), so two levels must key differently and each
// must describe the file it actually produces. The row is not live, which the
// plan has to say too: a caller that streamed one would get a file whose seek
// table is still zeros.
func TestAPEPlanMatchesRun(t *testing.T) {
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
	for _, lv := range apeEncodeLevels {
		opts := waxflow.TranscodeOptions{Format: "ape", APELevel: lv.level}
		plan, err := e.PlanTranscode(track, opts)
		if err != nil {
			t.Fatalf("plan %s: %v", lv.name, err)
		}
		if plan.Container != "ape" || plan.MediaType != "audio/x-ape" || plan.Live {
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
	if len(seen) != 3 {
		t.Errorf("%d distinct cache keys across four level spellings, want 3", len(seen))
	}
}

// TestAPEEncodeRefuses pins the named refusals: levels outside the scale, the
// two levels the format has that this encoder deliberately does not write, a
// source wider than the format holds here, a container override the row has no
// second wrapper for, and a destination that cannot seek.
func TestAPEEncodeRefuses(t *testing.T) {
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
		"a level below the scale": {wav, waxflow.TranscodeOptions{APELevel: -1}, "APE level"},
		"a level off the scale":   {wav, waxflow.TranscodeOptions{APELevel: 1500}, "APE level"},
		// The likeliest wrong answer: the format documents five levels and
		// this writes three, so the refusal names which half that is.
		"extra high, which is not written": {wav, waxflow.TranscodeOptions{APELevel: 4000}, "decodes but is not written"},
		"insane, which is not written":     {wav, waxflow.TranscodeOptions{APELevel: 5000}, "decodes but is not written"},
		// Lossless output never folds channels away: alac's rule, not the
		// lossy rows'. A 5.1 source is refused by name instead.
		"a source wider than stereo": {wide, waxflow.TranscodeOptions{}, "only mono and stereo"},
		"a container override":       {wav, waxflow.TranscodeOptions{Container: "mka"}, "container"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.Format = "ape"
			_, err := e.Transcode(context.Background(), container.BytesSource(tc.wav), "wav", &memWS{}, opts)
			if err == nil {
				t.Fatal("the transcode succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the problem (%q)", err, tc.want)
			}
		})
	}

	t.Run("a destination that cannot seek", func(t *testing.T) {
		var sink onlyWriter
		_, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", sink,
			waxflow.TranscodeOptions{Format: "ape"})
		if err == nil {
			t.Fatal("ape to an unseekable writer succeeded")
		}
		if !strings.Contains(err.Error(), "seekable") {
			t.Errorf("error %q does not say the destination has to seek", err)
		}
	})
}

// TestAPEEncodeDeepLevelsSayWhy pins the wording of the one refusal a caller
// is most likely to hit by reading the format's documentation rather than
// ours: the two deepest levels exist, decode here, and are not written.
func TestAPEEncodeDeepLevelsSayWhy(t *testing.T) {
	_, err := ape.NewEncoder(apeFormat(44100, 2, 16), &ape.EncoderOptions{Level: ape.LevelInsane})
	if err == nil {
		t.Fatal("the insane level was accepted")
	}
	if !strings.Contains(err.Error(), "decodes but is not written") {
		t.Errorf("error %q does not say the level decodes", err)
	}
}

// TestAPEEncodeTags pins the metadata seam: the muxer's APEv2 trailer is what
// the demuxer reads back, so tags survive a transcode without a mapping
// post-pass. The trailer sits behind the audio, so it also has to leave the
// descriptor's frame-data byte count alone, which the strict probe checks.
func TestAPEEncodeTags(t *testing.T) {
	e := waxflow.New()
	const frames = 8000
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 41)
	defer audio.Put(src)
	tags := []container.Tag{{Key: "TITLE", Value: "Stage Seven"}, {Key: "ALBUM", Value: "WaxFlow"}}
	raw := encodeAPE(t, e, wav, waxflow.TranscodeOptions{Tags: tags})

	info, err := e.Probe(container.BytesSource(raw), "ape", &waxflow.ProbeOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range tags {
		if v := info.Tags[want.Key]; len(v) != 1 || v[0] != want.Value {
			t.Errorf("tag %s read back as %v, want %q", want.Key, v, want.Value)
		}
	}
	got := readAll(t, e, raw, frames)
	defer audio.Put(got)
	equalPCM(t, src, got)
}

// TestAPEEncodeDepthPolicy pins the row's adjust hook: a float source
// quantizes to 24 bits, and an integer source at a width this codec has no
// container for widens to the next one losslessly.
//
// Nothing narrows. A 32-bit integer source is refused by name rather than
// silently losing eight bits, which is the channel rule applied to depth:
// every other lossless output here carries 32-bit through, so this is the one
// row where a quiet narrow would be the odd behavior. An explicit bits=24
// still works, because then the caller asked.
func TestAPEEncodeDepthPolicy(t *testing.T) {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			wav, src := makeWAV(t, tc.cfg, 2, 5000, 51)
			defer audio.Put(src)
			raw := encodeAPE(t, e, wav, waxflow.TranscodeOptions{})
			info, err := e.Probe(container.BytesSource(raw), "ape", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Default().Fmt.BitDepth; got != tc.want {
				t.Errorf("wrote %d-bit APE, want %d", got, tc.want)
			}
		})
	}

	t.Run("32-bit is refused, not narrowed", func(t *testing.T) {
		wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, 2, 5000, 52)
		defer audio.Put(src)
		_, err := e.Transcode(context.Background(), container.BytesSource(wav), "wav", &memWS{},
			waxflow.TranscodeOptions{Format: "ape"})
		if err == nil {
			t.Fatal("a 32-bit source was accepted")
		}
		if !strings.Contains(err.Error(), "only 8, 16, and 24-bit") {
			t.Errorf("error %q does not name the widths this codec holds", err)
		}
		// The same source at an explicit 24 bits goes through: the narrow is
		// the caller's to ask for.
		raw := encodeAPE(t, e, wav, waxflow.TranscodeOptions{BitDepth: 24})
		info, err := e.Probe(container.BytesSource(raw), "ape", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Default().Fmt.BitDepth; got != 24 {
			t.Errorf("an explicit bits=24 wrote %d-bit APE", got)
		}
	})
}

// TestAPEEncodeSeeksExactly closes the loop the decode side opened: a file we
// wrote has to be seekable through our own reader, which means the seek table
// we filled in has to point at the frames we wrote. The fixture is a ramp, so
// every sample says where it is and a table off by one frame is loud.
func TestAPEEncodeSeeksExactly(t *testing.T) {
	e := waxflow.New()
	const frames = 2*ape.BlocksPerFrame + 5000
	f := apeFormat(48000, 1, 16)
	wav, src := makeWAVOf(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 1, frames, func(b *audio.Buffer) {
		for i := range b.ChanI(0) {
			b.ChanI(0)[i] = testutil.RampAtI(b.Fmt, 0, int64(i))
		}
	})
	defer audio.Put(src)
	raw := encodeAPE(t, e, wav, waxflow.TranscodeOptions{})

	med, err := e.OpenStream(container.BytesSource(raw), "ape")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	for _, target := range []int64{0, 1, ape.BlocksPerFrame - 1, ape.BlocksPerFrame,
		ape.BlocksPerFrame + 1, 2 * ape.BlocksPerFrame, frames - 1} {
		landed, err := med.SeekSample(target)
		if err != nil {
			t.Fatalf("seek %d: %v", target, err)
		}
		if landed != target {
			t.Fatalf("seek %d landed at %d", target, landed)
		}
		if err := med.ReadChunk(buf); err != nil {
			t.Fatalf("read after seek %d: %v", target, err)
		}
		for i := range min(buf.N, 4) {
			want := testutil.RampAtI(f, 0, target+int64(i))
			if got := buf.ChanI(0)[i]; got != want {
				t.Fatalf("seek %d: sample %d = %d, want %d", target, target+int64(i), got, want)
			}
		}
	}
}

// TestAPERemuxMovesFrames pins the rung the muxer opened, and pins that it is
// TAKEN rather than merely available: a .ape source asked for as APE has
// nothing to re-encode, so the frames move through and only the header, the
// seek table and the file MD5 are rebuilt. Without the check the request would
// fall through to a full re-encode, which for a lossless codec is correct
// output at many times the cost, and nothing else here would notice.
//
// The reference-encoded arm is the one that matters. Its frames share the word
// at each boundary (ours start on one), so a muxer that could only lay down
// what our own encoder produces fails on every real .ape in existence, which
// is exactly what it did before the packet learned to state its frame's byte
// length.
func TestAPERemuxMovesFrames(t *testing.T) {
	e := waxflow.New()
	const frames = 2*ape.BlocksPerFrame + 9000
	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 61)
	defer audio.Put(src)

	sources := map[string][]byte{"ours": encodeAPE(t, e, wav, waxflow.TranscodeOptions{})}
	if testutil.HaveAPETool(t) {
		dir := t.TempDir()
		wavPath := filepath.Join(dir, "in.wav")
		if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(testutil.APEEncodeFile(t, wavPath, "ref.ape", 2000))
		if err != nil {
			t.Fatal(err)
		}
		sources["the reference encoder"] = raw
	}

	for name, raw := range sources {
		t.Run(name, func(t *testing.T) {
			info, err := e.Probe(container.BytesSource(raw), "ape", nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := e.PlanRemux(info.Default(), waxflow.TranscodeOptions{Format: "ape"})
			if err != nil {
				t.Fatal(err)
			}
			if plan == nil {
				t.Fatal("APE to APE declined the remux rung")
			}
			var out memWS
			res, err := e.Remux(context.Background(), container.BytesSource(raw), "ape", &out,
				waxflow.TranscodeOptions{Format: "ape"})
			if err != nil {
				t.Fatal(err)
			}
			if res.Samples != frames {
				t.Errorf("remux reported %d samples, want %d", res.Samples, frames)
			}
			// The coded frames are the source's, byte for byte: only the
			// bookkeeping around them was rebuilt.
			if a, b := apeCodedFrames(t, out.Buf), apeCodedFrames(t, raw); len(a) != len(b) {
				t.Fatalf("remuxed %d frames from %d", len(a), len(b))
			} else {
				for i := range b[:len(b)-1] {
					if !bytes.Equal(a[i], b[i]) {
						t.Fatalf("frame %d changed at byte %d in a remux", i, firstDiff(a[i], b[i]))
					}
				}
			}
			got := readAll(t, e, out.Buf, frames)
			defer audio.Put(got)
			equalPCM(t, src, got)
			// The bookkeeping around the frames was rebuilt, so the file MD5
			// has to describe the file we wrote rather than the one we read.
			if testutil.HaveAPETool(t) {
				testutil.APEVerifyFile(t, apeFile(t, t.TempDir(), "remuxed", out.Buf))
			}
		})
	}
}

// TestAPEEncodeMatchesTheReferenceOnSpecialFrames is the byte-identity gate
// pointed at the frames that skip the entropy coder entirely. The matrix above
// feeds noise, which never triggers one, so the special codes were the one
// deliberate quirk in the port that the headline gate did not exercise.
//
// The single-silent-channel cells are the point. A stereo frame with exactly
// one silent channel sets one of the two silence bits, and which one is not
// obvious: the reference reads the channels in the order (R, L) and keys
// LEFT_SILENCE off the SECOND one it read, so a frame whose channel 0 is
// silent carries the RIGHT bit. That is decode-invisible -- every decoder acts
// only when both bits are set -- so nothing but a byte comparison against the
// reference can tell a faithful port from a transposed one.
func TestAPEEncodeMatchesTheReferenceOnSpecialFrames(t *testing.T) {
	testutil.APETool(t)
	e := waxflow.New()
	dir := t.TempDir()
	const frames = 30011
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}

	tone := func(b *audio.Buffer, c int, freq float64) {
		full := float64(int32(1)<<15 - 1)
		s := b.ChanI(c)
		for i := range s {
			s[i] = int32(0.6 * full * math.Sin(2*math.Pi*freq*float64(i)/float64(b.Fmt.Rate)))
		}
	}
	for _, tc := range []struct {
		name string
		fill func(*audio.Buffer)
	}{
		{"channel 0 silent", func(b *audio.Buffer) { clear(b.ChanI(0)); tone(b, 1, 440) }},
		{"channel 1 silent", func(b *audio.Buffer) { tone(b, 0, 440); clear(b.ChanI(1)) }},
		{"both silent", func(b *audio.Buffer) { clear(b.ChanI(0)); clear(b.ChanI(1)) }},
		{"identical channels", func(b *audio.Buffer) { tone(b, 0, 440); copy(b.ChanI(1), b.ChanI(0)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wav, src := makeWAVOf(t, cfg, 2, frames, tc.fill)
			defer audio.Put(src)
			ours := apeCodedFrames(t, encodeAPE(t, e, wav, waxflow.TranscodeOptions{}))

			base := strings.ReplaceAll(tc.name, " ", "-")
			srcWav := filepath.Join(dir, base+".wav")
			if err := os.WriteFile(srcWav, wav, 0o644); err != nil {
				t.Fatal(err)
			}
			refRaw, err := os.ReadFile(testutil.APEEncodeFile(t, srcWav, base+".ape", waxflow.APELevelNormal))
			if err != nil {
				t.Fatal(err)
			}
			want := apeCodedFrames(t, refRaw)
			if len(ours) != len(want) {
				t.Fatalf("wrote %d frames, the reference wrote %d", len(ours), len(want))
			}
			// One frame, and it runs to the end of the region on both sides,
			// so the comparison is over what they agree they hold.
			n := min(len(ours[0]), len(want[0]))
			if d := firstDiff(ours[0][:n], want[0][:n]); d != n {
				t.Fatalf("the frame differs from the reference's at byte %d of %d", d, n)
			}
			// And it really is a special frame: a whole frame of one of these
			// shapes codes at most one channel, so it cannot approach what two
			// channels of tone cost.
			if len(ours[0]) > frames {
				t.Errorf("the frame took %d bytes for %d blocks; no special code fired", len(ours[0]), frames)
			}
		})
	}
}
