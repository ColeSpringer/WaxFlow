package waxflow_test

// Monkey's Audio decode differentials. APE is lossless, so a decode is either
// bit-for-bit right or wrong, and there are two ways to say so: against the
// samples that went into the reference encoder, and against ffmpeg's reading
// of the same file. Both run here, because they fail differently. Matching the
// source proves the decode; matching ffmpeg proves the file is what we think
// it is, and would catch a fixture generated from the wrong bytes.
//
// There is no APE conformance corpus and no second encoder: ffmpeg decodes the
// format but does not write it. So the reference `mac` tool generates every
// cell, and the committed fixtures below are what a machine without it still
// checks.

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// apeLevels is every compression level the format has. Each one chooses a
// different filter cascade, which is the whole difference between them, so a
// decoder that covers four of the five covers four of the five bitstreams.
var apeLevels = []int{1000, 2000, 3000, 4000, 5000}

// apeFormat is the int format for a depth and channel count.
func apeFormat(rate, channels, depth int) audio.Format {
	return audio.Format{
		Rate:     rate,
		Channels: channels,
		Layout:   audio.DefaultLayout(channels),
		Type:     audio.Int,
		BitDepth: depth,
	}
}

// apeSignal builds one of the named shapes, interleaved at the format's depth.
// "dup" is two identical channels, which the encoder codes as pseudo-stereo,
// and "silence" is what it codes as a silent frame: both are special frames
// that skip the entropy coder entirely, and neither happens by accident.
func apeSignal(t *testing.T, name string, f audio.Format, frames int) []int32 {
	t.Helper()
	switch name {
	case "sine":
		return released(testutil.Sine(f, frames, 440, 0.8))
	case "noise":
		return released(testutil.Noise(f, frames, 7))
	case "ramp":
		return released(testutil.Ramp(f, frames))
	case "silence":
		return make([]int32, frames*f.Channels)
	case "dup":
		b := testutil.Sine(f, frames, 300, 0.5)
		defer audio.Put(b)
		out := make([]int32, frames*f.Channels)
		src := b.ChanI(0)
		for i := range frames {
			for c := range f.Channels {
				out[i*f.Channels+c] = src[i]
			}
		}
		return out
	}
	t.Fatalf("unknown signal %q", name)
	return nil
}

// interleaveNative flattens a planar buffer without testutil.Interleave's
// shift to 32 bits: these samples are compared against a decode at the
// stream's own depth, not against ffmpeg's left-justified output. The buffer
// stays the caller's.
func interleaveNative(b *audio.Buffer) []int32 {
	ch := b.Fmt.Channels
	out := make([]int32, b.N*ch)
	for c := range ch {
		for i, v := range b.ChanI(c) {
			out[i*ch+c] = v
		}
	}
	return out
}

// apeEncode writes samps as a WAV and hands it to the reference encoder.
func apeEncode(t *testing.T, dir, name string, f audio.Format, samps []int32, level int) string {
	t.Helper()
	wav := filepath.Join(dir, name+".wav")
	testutil.WriteWAV(t, wav, f, samps)
	return testutil.APEEncodeFile(t, wav, name+".ape", level)
}

// apeCheck decodes path through the full stack and compares it to want, then
// asks ffmpeg for a second reading of the same file.
func apeCheck(t *testing.T, path string, f audio.Format, want []int32) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Strict mode as well as tolerant: a file the reference wrote must give
	// the demuxer nothing to warn about, which is what keeps the tail checks
	// from being noise on healthy files.
	if _, err := waxflow.New().Probe(container.BytesSource(raw), "ape", &waxflow.ProbeOptions{Strict: true}); err != nil {
		t.Fatalf("strict probe of a reference-encoded file: %v", err)
	}
	got, err := decodeAllDynamic(t, container.BytesSource(raw), "ape")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer audio.Put(got)
	if got.Fmt != f {
		t.Fatalf("format = %v, want %v", got.Fmt, f)
	}
	if idx := testutil.DiffI32(interleaveNative(got), want); idx != -1 {
		t.Fatalf("decode differs from the source at interleaved sample %d (decoded %d, source %d)",
			idx, got.N*got.Fmt.Channels, len(want))
	}
	if !testutil.HaveFFmpeg(t) {
		return
	}
	ref := testutil.FFmpegDecodeS32(t, path)
	if idx := testutil.DiffI32(testutil.Interleave(got), ref); idx != -1 {
		t.Fatalf("decode differs from ffmpeg at interleaved sample %d (decoded %d, ref %d)",
			idx, got.N*got.Fmt.Channels, len(ref))
	}
}

// released is interleaveNative for a buffer the caller is done with.
func released(b *audio.Buffer) []int32 {
	defer audio.Put(b)
	return interleaveNative(b)
}

// TestAPEDepthChannelLevel is the shape matrix: every depth the decoder
// covers, both channel counts, every compression level. Noise is the signal
// because it is the one that keeps the entropy coder's escape paths and the
// filters' full weight range in play.
func TestAPEDepthChannelLevel(t *testing.T) {
	testutil.APETool(t)
	dir := t.TempDir()
	const frames = 20000
	for _, depth := range []int{8, 16, 24} {
		for _, ch := range []int{1, 2} {
			f := apeFormat(44100, ch, depth)
			want := apeSignal(t, "noise", f, frames)
			for _, level := range apeLevels {
				cell := cellName("noise", depth, ch, level)
				t.Run(cell, func(t *testing.T) {
					apeCheck(t, apeEncode(t, dir, cell, f, want, level), f, want)
				})
			}
		}
	}
}

// cellName names one matrix cell, and so the fixture file it writes.
func cellName(signal string, depth, ch, level int) string {
	return fmt.Sprintf("%s-%dbit-%dch-c%d", signal, depth, ch, level)
}

// TestAPESignalShapes covers the shapes the entropy coder and the frame header
// treat specially: a tone, a ramp that walks the whole sample range, digital
// silence (a silent frame, which codes no values at all), and two identical
// channels (a pseudo-stereo frame, which codes one).
func TestAPESignalShapes(t *testing.T) {
	testutil.APETool(t)
	dir := t.TempDir()
	const frames = 20000
	f := apeFormat(44100, 2, 16)
	for _, signal := range []string{"sine", "ramp", "silence", "dup"} {
		want := apeSignal(t, signal, f, frames)
		for _, level := range apeLevels {
			cell := cellName(signal, 16, 2, level)
			t.Run(cell, func(t *testing.T) {
				apeCheck(t, apeEncode(t, dir, cell, f, want, level), f, want)
			})
		}
	}
}

// TestAPEMultiFrame covers what a single-frame fixture cannot: the frame
// boundary. Every frame re-primes the range coder and flushes the predictors,
// so a stream of them is the only thing that proves the reset is complete, and
// a final frame shorter than the rest is the only thing that proves the block
// count comes from the header rather than from the frame length.
func TestAPEMultiFrame(t *testing.T) {
	testutil.APETool(t)
	dir := t.TempDir()
	// Three frames at the default frame length, the last one short.
	const frames = 2*73728 + 4321
	f := apeFormat(37000, 2, 16)
	want := apeSignal(t, "noise", f, frames)
	path := apeEncode(t, dir, "multiframe", f, want, 2000)
	testutil.APEVerifyFile(t, path)
	apeCheck(t, path, f, want)
}

// TestAPEProbe checks that probe agrees with ffprobe on the stream's shape,
// and that the source resolves to the ape driver by magic alone.
func TestAPEProbe(t *testing.T) {
	testutil.APETool(t)
	dir := t.TempDir()
	f := apeFormat(44100, 2, 16)
	want := apeSignal(t, "sine", f, 20000)
	path := apeEncode(t, dir, "probe", f, want, 2000)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// No extension hint: the driver has to be found by magic.
	info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Container != "ape" {
		t.Errorf("container = %q, want ape", info.Container)
	}
	tr := info.Default()
	if tr.Codec != "ape" {
		t.Errorf("codec = %q, want ape", tr.Codec)
	}
	if tr.Fmt != f {
		t.Errorf("format = %v, want %v", tr.Fmt, f)
	}
	if tr.Samples != int64(len(want)/f.Channels) {
		t.Errorf("samples = %d, want %d", tr.Samples, len(want)/f.Channels)
	}
	if !testutil.HaveFFmpeg(t) {
		return
	}
	ff := testutil.FFprobeFile(t, path)
	if ff.SampleRate != f.Rate || ff.Channels != f.Channels {
		t.Errorf("ffprobe: %d Hz %dch, want %d Hz %dch", ff.SampleRate, ff.Channels, f.Rate, f.Channels)
	}
}

// TestAPEFixturesBitExact is the tool-free gate: the committed .ape fixtures
// decode through the whole stack to exactly the samples the reference encoder
// was handed. It needs no encoder, no ffmpeg, and no network, so it is the
// assertion that still holds on a bare machine.
func TestAPEFixturesBitExact(t *testing.T) {
	f := apeFormat(44100, 2, 16)
	for _, tc := range []struct {
		name string
		want func() []int32
	}{
		{"sine-s16.ape", func() []int32 { return released(testutil.Sine(f, 22050, 440, 0.8)) }},
		{"noise-s16.ape", func() []int32 { return released(testutil.Noise(f, 22050, 7)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "testdata", tc.name))
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			if got.Fmt != f {
				t.Fatalf("format = %v, want %v", got.Fmt, f)
			}
			want := tc.want()
			if idx := testutil.DiffI32(interleaveNative(got), want); idx != -1 {
				t.Fatalf("decode differs from the source at interleaved sample %d (decoded %d, source %d)",
					idx, got.N*got.Fmt.Channels, len(want))
			}
		})
	}
}

// TestAPETranscodeRoundsTrip runs a .ape through the engine to WAV, which is
// the path a caller actually takes: probe, decode, resample nothing, write.
// It is here rather than in the codec package because it is the wiring that is
// under test, not the decode.
func TestAPETranscodeRoundsTrip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "sine-s16.ape"))
	if err != nil {
		t.Fatal(err)
	}
	var out testutil.MemWriteSeeker
	res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "", &out,
		waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if res.Samples != 22050 {
		t.Errorf("wrote %d samples, want 22050", res.Samples)
	}
	if res.ClippedSamples != 0 {
		t.Errorf("a lossless int source clipped %d samples", res.ClippedSamples)
	}
	f := apeFormat(44100, 2, 16)
	want := released(testutil.Sine(f, 22050, 440, 0.8))
	data := testutil.WAVData(t, out.Buf)
	if len(data) != len(want)*2 {
		t.Fatalf("WAV data is %d bytes, want %d", len(data), len(want)*2)
	}
	for i, v := range want {
		if got := int32(int16(binary.LittleEndian.Uint16(data[i*2:]))); got != v {
			t.Fatalf("sample %d = %d, want %d", i, got, v)
		}
	}
}

// TestAPEMidFrameSeek is the seek behavior APE's frame granularity actually
// depends on. The demuxer can only land on a frame start, so every mid-frame
// target is answered by format.Media decoding the rest of the frame and
// throwing it away; the fixture is a ramp, so every sample says where it is
// and a pre-roll that stops one sample early is visible.
func TestAPEMidFrameSeek(t *testing.T) {
	path := filepath.Join("..", "container", "apen", "testdata", "seek.ape")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	med, err := waxflow.New().OpenStream(container.BytesSource(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	track := med.Info().Default()
	buf := audio.Get(track.Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	// Inside the first frame, inside the second, either side of a boundary,
	// and inside the short final one.
	for _, target := range []int64{0, 1, 12345, 73727, 73728, 73729, 100000, 147455, 147456, 150000, track.Samples - 1} {
		landed, err := med.SeekSample(target)
		if err != nil {
			t.Fatalf("seek %d: %v", target, err)
		}
		if landed != target {
			t.Fatalf("seek %d landed at %d; the pre-roll is meant to make it exact", target, landed)
		}
		if err := med.ReadChunk(buf); err != nil {
			t.Fatalf("read after seek %d: %v", target, err)
		}
		for i := range min(buf.N, 4) {
			want := testutil.RampAtI(track.Fmt, 0, target+int64(i))
			if got := buf.ChanI(0)[i]; got != want {
				t.Fatalf("seek %d: sample %d = %d, want %d", target, target+int64(i), got, want)
			}
		}
	}
}
