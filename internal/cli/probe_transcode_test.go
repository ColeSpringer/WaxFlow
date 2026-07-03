package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/testutil"
)

// writeWAV writes a synthesized 16-bit stereo WAV fixture.
func writeWAV(t *testing.T, path string, frames int) {
	t.Helper()
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	buf := testutil.Ramp(f, frames)
	defer audio.Put(buf)

	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	m := riff.NewMuxer(out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
}

// writeSineWAV writes a WAV fixture with the given wire depth and a
// 997 Hz half-scale sine.
func writeSineWAV(t *testing.T, path string, rate, channels, bits, frames int) {
	t.Helper()
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: bits}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	buf := testutil.Sine(f, frames, 997, 0.5)
	defer audio.Put(buf)

	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	m := riff.NewMuxer(out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
}

// TestTranscodeDSP is the M3 CLI exit criterion: 96 kHz / 24-bit in,
// 44.1 kHz / 16-bit out, dithered, through the real command.
func TestTranscodeDSP(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in96k24.wav")
	outPath := filepath.Join(dir, "out.wav")
	const frames = 96000
	writeSineWAV(t, in, 96000, 2, 24, frames)

	code, cmdOut, errOut := run(t, "transcode", in, outPath, "--rate", "44100", "--bits", "16")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	// ceil(96000 * 147/320) output frames.
	wantN := int64((frames*147 + 319) / 320)
	if !strings.Contains(cmdOut, fmt.Sprintf("%d samples", wantN)) {
		t.Errorf("output %q missing %d samples", cmdOut, wantN)
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := container.FileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	med, err := waxflow.New().OpenStream(src, "wav")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	track := med.Info().Default()
	if track.Fmt.Rate != 44100 || track.Fmt.BitDepth != 16 || track.Fmt.Channels != 2 {
		t.Fatalf("output format %v, want 44100Hz 2ch int16", track.Fmt)
	}
	if track.Samples != wantN {
		t.Fatalf("output samples %d, want %d", track.Samples, wantN)
	}

	// The tone must come through at level: read channel 0, measure 997 Hz
	// by Hann-windowed correlation over the steady-state middle.
	dst := audio.Get(track.Fmt, audio.StandardChunk)
	defer audio.Put(dst)
	var samples []float64
	for {
		if err := med.ReadChunk(dst); err != nil {
			break
		}
		for _, v := range dst.ChanI(0) {
			samples = append(samples, float64(v)/32768)
		}
	}
	mid := samples[8000 : len(samples)-8000]
	var a, b, wsum float64
	n := float64(len(mid))
	for i, v := range mid {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/n)
		ph := 2 * math.Pi * 997 * float64(i) / 44100
		a += v * w * math.Cos(ph)
		b += v * w * math.Sin(ph)
		wsum += w
	}
	amp := 2 * math.Hypot(a, b) / wsum
	if lvl := 20 * math.Log10(amp/0.5); math.Abs(lvl) > 0.05 {
		t.Errorf("tone level error %+.4f dB, want within 0.05 (hq ripple gate)", lvl)
	}
}

func TestTranscodeFlagErrors(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 100)

	code, _, _ := run(t, "transcode", in, filepath.Join(dir, "a.wav"), "--resample-profile", "ultra")
	if code != 2 {
		t.Errorf("bad profile exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "b.wav"), "--dither", "extreme")
	if code != 2 {
		t.Errorf("bad dither exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "c.wav"), "--bits", "64")
	if code != 2 {
		t.Errorf("bad depth exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "d.wav"), "--channels", "5")
	if code != 5 {
		t.Errorf("unsupported channels exit = %d, want 5 (unsupported)", code)
	}
	// pflag parses NaN/Inf floats and absurd-but-valid ints; both must
	// surface as clean errors, never a panic or corrupt output.
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "e.wav"), "--gain", "NaN")
	if code != 2 {
		t.Errorf("NaN gain exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "f.wav"), "--gain", "+Inf")
	if code != 2 {
		t.Errorf("Inf gain exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "g.wav"), "--rate", "9223372036854775807")
	if code != 5 {
		t.Errorf("extreme rate exit = %d, want 5 (unsupported)", code)
	}
	for _, f := range []string{"e.wav", "f.wav", "g.wav"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("failed transcode left %s behind", f)
		}
	}
}

func TestProbeCommand(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "test.wav")
	writeWAV(t, in, 4800)

	code, out, errOut := run(t, "probe", in)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{"container: wav", "pcm", "48000Hz 2ch int16", "4800 (0.100s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	code, out, _ = run(t, "probe", "--json", in)
	if code != 0 {
		t.Fatalf("json exit = %d", code)
	}
	var doc struct {
		SchemaVersion int    `json:"schemaVersion"`
		Container     string `json:"container"`
		Tracks        []struct {
			Codec           string  `json:"codec"`
			Rate            int     `json:"rate"`
			Channels        int     `json:"channels"`
			Layout          string  `json:"layout"`
			BitDepth        int     `json:"bitDepth"`
			Samples         int64   `json:"samples"`
			DurationSeconds float64 `json:"durationSeconds"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if doc.SchemaVersion != 1 || doc.Container != "wav" || len(doc.Tracks) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	tr := doc.Tracks[0]
	if tr.Codec != "pcm" || tr.Rate != 48000 || tr.Channels != 2 || tr.Layout != "FL|FR" || tr.BitDepth != 16 || tr.Samples != 4800 {
		t.Errorf("track = %+v", tr)
	}
}

func TestProbeCommandErrors(t *testing.T) {
	dir := t.TempDir()
	code, _, _ := run(t, "probe", filepath.Join(dir, "nope.wav"))
	if code != 3 {
		t.Errorf("missing file exit = %d, want 3 (not-found)", code)
	}
	junk := filepath.Join(dir, "junk.bin")
	if err := os.WriteFile(junk, []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, _ = run(t, "probe", junk)
	if code != 5 {
		t.Errorf("junk file exit = %d, want 5 (unsupported)", code)
	}
}

func TestTranscodeCommand(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	outPath := filepath.Join(dir, "out.aiff")
	writeWAV(t, in, 4800)

	code, out, errOut := run(t, "transcode", in, outPath)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "4800 samples") {
		t.Errorf("output = %q", out)
	}

	// The output must decode bit-exactly back to the source ramp.
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := container.FileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	med, err := waxflow.New().OpenStream(src, "aiff")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	info := med.Info()
	if info.Container != "aiff" || info.Default().Samples != 4800 {
		t.Fatalf("output probe = %+v", info)
	}
	fm := info.Default().Fmt
	dst := audio.Get(fm, audio.StandardChunk)
	defer audio.Put(dst)
	pos := int64(0)
	for {
		err := med.ReadChunk(dst)
		if err != nil {
			break
		}
		for c := 0; c < fm.Channels; c++ {
			for i, v := range dst.ChanI(c) {
				if want := testutil.RampAtI(fm, c, pos+int64(i)); v != want {
					t.Fatalf("ch%d sample %d = %d, want %d", c, pos+int64(i), v, want)
				}
			}
		}
		pos += int64(dst.N)
	}
	if pos != 4800 {
		t.Fatalf("decoded %d frames, want 4800", pos)
	}

	// Existing output without --force refuses; with --force succeeds.
	code, _, _ = run(t, "transcode", in, outPath)
	if code != 2 {
		t.Errorf("overwrite exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", "--force", in, outPath)
	if code != 0 {
		t.Errorf("forced overwrite exit = %d, want 0", code)
	}
}

// TestTranscodeForcePreservesExisting pins the staged overwrite: a
// --force transcode that fails, at any stage, must leave the
// pre-existing output byte-identical and no temp file behind. In-place
// truncation would destroy it on a mere flag typo.
func TestTranscodeForcePreservesExisting(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	outPath := filepath.Join(dir, "out.wav")
	writeWAV(t, in, 4800)

	if code, _, errOut := run(t, "transcode", in, outPath); code != 0 {
		t.Fatalf("setup transcode exit = %d, stderr: %s", code, errOut)
	}
	before, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}

	// Chain validation failure (invalid gain) and source failure
	// (missing input) both fail after the old code had truncated.
	code, _, _ := run(t, "transcode", "--force", "--gain", "NaN", in, outPath)
	if code != 2 {
		t.Errorf("NaN gain exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", "--force", filepath.Join(dir, "missing.wav"), outPath)
	if code != 3 {
		t.Errorf("missing input exit = %d, want 3 (not-found)", code)
	}

	after, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("existing output destroyed: %v", err)
	}
	if string(after) != string(before) {
		t.Error("existing output bytes changed by failed --force transcodes")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}

	// A successful --force still replaces the file.
	if code, _, errOut := run(t, "transcode", "--force", "--bits", "24", in, outPath); code != 0 {
		t.Fatalf("forced overwrite exit = %d, stderr: %s", code, errOut)
	}
	replaced, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(replaced) == string(before) {
		t.Error("successful --force did not replace the output")
	}
}

// TestTranscodeRefusesInPlace pins the destructive-overwrite guard:
// forcing output onto the input path (by any spelling of the same file)
// must fail up front and leave the input untouched, because O_TRUNC would
// otherwise zero the source before it is ever read.
func TestTranscodeRefusesInPlace(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 200)
	before, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}

	code, _, errOut := run(t, "transcode", "--force", in, in)
	if code != 2 {
		t.Errorf("in-place exit = %d, want 2 (invalid); stderr: %s", code, errOut)
	}

	link := filepath.Join(dir, "hardlink.wav")
	if err := os.Link(in, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	code, _, _ = run(t, "transcode", "--force", in, link)
	if code != 2 {
		t.Errorf("hard-link in-place exit = %d, want 2 (invalid)", code)
	}

	after, err := os.ReadFile(in)
	if err != nil {
		t.Fatalf("input destroyed: %v", err)
	}
	if !strings.HasPrefix(string(after), "RIFF") || len(after) != len(before) {
		t.Error("input bytes changed by a refused in-place transcode")
	}
}

func TestTranscodeCommandErrors(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 100)

	code, _, _ := run(t, "transcode", in, filepath.Join(dir, "out.xyz"))
	if code != 2 {
		t.Errorf("unknown extension exit = %d, want 2 (invalid)", code)
	}
	code, _, _ = run(t, "transcode", "--format", "opus", in, filepath.Join(dir, "out.opus"))
	if code != 5 {
		t.Errorf("unregistered format exit = %d, want 5 (unsupported)", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.opus")); err == nil {
		t.Error("failed transcode must not leave an output file")
	}
	code, _, _ = run(t, "transcode", filepath.Join(dir, "missing.wav"), filepath.Join(dir, "out.wav"))
	if code != 3 {
		t.Errorf("missing input exit = %d, want 3 (not-found)", code)
	}

	// The .afc spelling of AIFF resolves through the engine's output
	// table, which the read-side registry also accepts.
	code, _, errOut := run(t, "transcode", in, filepath.Join(dir, "out.afc"))
	if code != 0 {
		t.Errorf(".afc transcode exit = %d, want 0; stderr: %s", code, errOut)
	}
}
