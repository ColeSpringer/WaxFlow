package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/internal/meta"
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

// TestTranscodeDSP drives the DSP chain end to end: 96 kHz / 24-bit in,
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

// probedOutput is a container/codec probe of a written file.
type probedOutput struct{ container, codec string }

func probeOutput(t *testing.T, path string) probedOutput {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := container.FileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(src, "", nil)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return probedOutput{container: info.Container, codec: string(info.Default().Codec)}
}

// TestTranscodeAACInMatroskaLoudness guards the isMP4 classification: AAC in a
// Matroska container with loudness analysis previously misread the output as
// MP4 and tried to patch MP4 ReplayGain atoms in a Matroska file, which failed
// and deleted the output. It must now succeed and leave a real .mka.
func TestTranscodeAACInMatroskaLoudness(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	out := filepath.Join(dir, "out.mka")
	writeWAV(t, in, 48000)

	code, _, errOut := run(t, "transcode", "--format", "aac", "--container", "mka",
		"--loudness", "analyze", in, out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output missing (wrongly patched as MP4 and deleted?): %v", err)
	}
	if got := probeOutput(t, out); got.container != "mka" || got.codec != "aac-lc" {
		t.Errorf("output = %+v, want mka/aac-lc", got)
	}
}

// TestTranscodeExplicitFormatInfersContainer guards container inference: an
// explicit --format must not defeat the extension's container, so
// `--format opus out.webm` writes Opus-in-WebM (a Matroska file), not an Ogg
// stream misnamed .webm.
func TestTranscodeExplicitFormatInfersContainer(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 4800)
	out := filepath.Join(dir, "out.webm")

	code, _, errOut := run(t, "transcode", "--format", "opus", in, out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	// The mka demuxer names both Matroska and WebM "mka"; an Ogg-Opus stream
	// would probe as "ogg" instead.
	if got := probeOutput(t, out); got.container != "mka" || got.codec != "opus" {
		t.Errorf("output = %+v, want mka (Matroska/WebM)/opus", got)
	}
}

// TestTranscodeOggFLACLoudnessEmbedsRG guards the Ogg loudness path: the Ogg
// muxer embeds tags at Begin and the post-pass is skipped for Ogg, so the
// measured ReplayGain must be predicted and embedded up front rather than
// computed and dropped. The RG tag rides in the VORBIS_COMMENT as plain text.
func TestTranscodeOggFLACLoudnessEmbedsRG(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	out := filepath.Join(dir, "out.oga")
	writeWAV(t, in, 48000)

	code, _, errOut := run(t, "transcode", "--format", "flac", "--container", "ogg",
		"--loudness", "analyze", in, out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("REPLAYGAIN_TRACK_GAIN")) {
		t.Error("Ogg-FLAC + --loudness analyze output is missing its ReplayGain tag")
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

	// The output must decode bit-exactly back to the source ramp. Scoped so
	// the file handle is released before the --force leg below: a Windows
	// os.Open takes no FILE_SHARE_DELETE, so a handle still open here blocks
	// the forced replace of outPath and fails a leg that is about the CLI.
	func() {
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
	}()

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

// TestTranscodeFLAC drives the flac output through the real command:
// extension inference, the level flag's 0 spelling, level validation,
// and a bit-exact ramp round trip.
func TestTranscodeFLAC(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 4800)

	outPath := filepath.Join(dir, "out.flac")
	code, out, errOut := run(t, "transcode", in, outPath, "--flac-level", "0")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "4800 samples") {
		t.Errorf("output = %q", out)
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
	med, err := waxflow.New().OpenStream(src, "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	info := med.Info()
	if info.Container != "flac" || info.Default().Samples != 4800 {
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

	code, _, errOut = run(t, "transcode", in, filepath.Join(dir, "bad.flac"), "--flac-level", "9")
	if code != 2 {
		t.Errorf("level 9 exit = %d, want 2 (invalid), stderr: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.flac")); err == nil {
		t.Error("failed transcode left an output file behind")
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
	code, _, _ = run(t, "transcode", "--format", "nosuchformat", in, filepath.Join(dir, "out.bin"))
	if code != 5 {
		t.Errorf("unregistered format exit = %d, want 5 (unsupported)", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.bin")); err == nil {
		t.Error("failed transcode must not leave an output file")
	}
	// Opus is registered: a transcode to .opus succeeds and writes an Ogg file.
	code, _, _ = run(t, "transcode", in, filepath.Join(dir, "out.opus"))
	if code != 0 {
		t.Errorf("opus transcode exit = %d, want 0", code)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "out.opus")); err != nil || len(b) < 4 || string(b[:4]) != "OggS" {
		t.Errorf("opus output missing or not Ogg (err=%v)", err)
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

// TestTranscodeCreatesOutputDirectory pins C7: transcode creates its
// output's parent, as split does.
func TestTranscodeCreatesOutputDirectory(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 4096)

	out := filepath.Join(dir, "a", "b", "out.flac")
	if code, _, errOut := run(t, "transcode", in, out); code != 0 {
		t.Fatalf("exit = %d into a missing directory, want 0; stderr: %s", code, errOut)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output not written: %v", err)
	}
	// --force stages a .tmp sibling, so it takes the same path.
	forced := filepath.Join(dir, "c", "out.flac")
	if code, _, errOut := run(t, "transcode", "--force", in, forced); code != 0 {
		t.Fatalf("--force exit = %d, want 0; stderr: %s", code, errOut)
	}

	// A refused transcode leaves nothing behind, the directory included.
	for _, args := range [][]string{
		{"transcode", "--loudness", "bogus", in, filepath.Join(dir, "d", "out.flac")},
		{"transcode", "--loudness", "analyze", "--gain", "3", in, filepath.Join(dir, "e", "out.flac")},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v exit = %d, want 2 (invalid)", args, code)
		}
	}
	for _, sub := range []string{"d", "e"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err == nil {
			t.Errorf("a refused transcode left the directory %s behind", sub)
		}
	}
}

// TestMetadataReadRoutesThroughTheLogger pins U1: the metadata read goes
// through the logger --log-level configures, and warnings outrank source
// lint.
func TestMetadataReadRoutesThroughTheLogger(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 4096)

	// An encoder stamp: about whatever produced the file, not this transfer.
	stamped := filepath.Join(dir, "stamped.flac")
	if code, _, errOut := run(t, "transcode", in, stamped); code != 0 {
		t.Fatalf("building the stamped fixture: exit %d, stderr: %s", code, errOut)
	}
	err := label.New().Apply(t.Context(), stamped,
		&meta.Info{Tags: map[string][]string{"ENCODER": {"Lavf58.29.100"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _, errOut := run(t, "transcode", stamped, filepath.Join(dir, "quiet.flac"))
	if strings.Contains(errOut, "msg=metadata") {
		t.Errorf("source lint surfaced at the default level:\n%s", errOut)
	}
	_, _, errOut = run(t, "transcode", "--log-level", "debug", stamped, filepath.Join(dir, "loud.flac"))
	if !strings.Contains(errOut, "note=") {
		t.Errorf("--log-level debug produced no note:\n%s", errOut)
	}

	// A source the mapper cannot read at all is a real warning, so it shows by
	// default. Ogg FLAC is the case: the tag library's Ogg parser takes
	// \x01vorbis and OpusHead only, and container/ogg implements no Tagger, so
	// nothing softens it. cli/transcode.go records the same gap from the write
	// side.
	unread := filepath.Join(dir, "unread.oga")
	if code, _, errOut := run(t, "transcode", "--format", "flac", "--container", "ogg",
		in, unread); code != 0 {
		t.Fatalf("building the Ogg-FLAC fixture: exit %d, stderr: %s", code, errOut)
	}
	_, _, errOut = run(t, "transcode", unread, filepath.Join(dir, "fromunread.flac"))
	if !strings.Contains(errOut, "warning=") {
		t.Errorf("an unreadable source warned nothing at the default level:\n%s", errOut)
	}
	// --log-level error silences it; --no-tags was previously the only lever.
	_, _, errOut = run(t, "transcode", "--log-level", "error", unread, filepath.Join(dir, "hushed.flac"))
	if strings.Contains(errOut, "msg=metadata") {
		t.Errorf("--log-level error did not silence the metadata warning:\n%s", errOut)
	}
}

// TestTranscodeOpusComplexityZero pins the --opus-complexity flag's 0
// spelling: it must select the encoder's lowest setting, not silently fall
// back to the default. Complexity 0 disables the analysis stages, so its
// output bytes must differ from the default's on the same input.
func TestTranscodeOpusComplexityZero(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 48000)

	lowest := filepath.Join(dir, "c0.opus")
	if code, _, errOut := run(t, "transcode", in, lowest, "--opus-complexity", "0"); code != 0 {
		t.Fatalf("complexity 0 exit = %d, stderr: %s", code, errOut)
	}
	def := filepath.Join(dir, "c5.opus")
	if code, _, errOut := run(t, "transcode", in, def); code != 0 {
		t.Fatalf("default exit = %d, stderr: %s", code, errOut)
	}
	a, err := os.ReadFile(lowest)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("--opus-complexity 0 produced the default's bytes: the lowest setting is unreachable")
	}
}

// TestTranscodeHEAAC covers the he-aac CLI surface: a bare .aac output
// implies ADTS like it does for aac (it once wrote a progressive MP4 into
// the .aac file), the default bitrate is the encoder's 64 kb/s rather
// than an unconditional flag default (whose 128 made the documented
// default unreachable and encoded outside both gate-judged points), and
// the mp4-family ReplayGain post-pass reaches he-aac MP4s (the format
// check once named only aac and alac, so he-aac files got no RG at all).
func TestTranscodeHEAAC(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 48000)

	adtsOut := filepath.Join(dir, "out.aac")
	code, _, errOut := run(t, "transcode", "--format", "he-aac", in, adtsOut)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if got := probeOutput(t, adtsOut); got.container != "adts" || got.codec != "he-aac" {
		t.Errorf(".aac output = %+v, want adts/he-aac", got)
	}

	defOut := filepath.Join(dir, "def.m4a")
	if code, _, errOut := run(t, "transcode", "--format", "he-aac", in, defOut); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	hiOut := filepath.Join(dir, "hi.m4a")
	if code, _, errOut := run(t, "transcode", "--format", "he-aac", "--aac-bitrate", "128", in, hiOut); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	defSize, hiSize := fileBytes(t, defOut), fileBytes(t, hiOut)
	if hiSize < defSize*3/2 {
		t.Errorf("128k output (%d bytes) not clearly larger than the default (%d bytes): the 64k default is not reaching the encoder", hiSize, defSize)
	}

	rgOut := filepath.Join(dir, "rg.m4a")
	if code, _, errOut := run(t, "transcode", "--format", "he-aac", "--loudness", "analyze", in, rgOut); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	data, err := os.ReadFile(rgOut)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("REPLAYGAIN_TRACK_GAIN")) {
		t.Error("he-aac MP4 + --loudness analyze output is missing its ReplayGain tag")
	}
}

// TestTranscodeHEAACv2 covers --he-v2: the flag reaches the engine (the
// output stores an AOT-29 config and rides the 32 kb/s default, clearly
// smaller than v1's 64), and a mono chain under the flag is refused by
// name rather than silently downgraded.
func TestTranscodeHEAACv2(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	writeWAV(t, in, 48000)

	v2Out := filepath.Join(dir, "v2.m4a")
	if code, _, errOut := run(t, "transcode", "--format", "he-aac", "--he-v2", in, v2Out); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	f, err := os.Open(v2Out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := container.FileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	info, err := waxflow.New().Probe(src, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := aac.ParseASC(info.Default().CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PS || info.Default().Fmt.Channels != 2 {
		t.Errorf("--he-v2 output = PS %v %v, want an explicit AOT-29 stereo pair", cfg.PS, info.Default().Fmt)
	}

	v1Out := filepath.Join(dir, "v1.m4a")
	if code, _, errOut := run(t, "transcode", "--format", "he-aac", in, v1Out); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if v1, v2 := fileBytes(t, v1Out), fileBytes(t, v2Out); v1 < v2*3/2 {
		t.Errorf("v1 default (%d bytes) not clearly larger than v2 (%d): the 32k v2 default is not reaching the encoder", v1, v2)
	}

	if code, _, errOut := run(t, "transcode", "--format", "he-aac", "--he-v2", "--channels", "1", in,
		filepath.Join(dir, "mono.m4a")); code == 0 {
		t.Error("--he-v2 with a mono chain succeeded, want a named refusal")
	} else if !strings.Contains(errOut, "stereo") {
		t.Errorf("the mono refusal does not name the constraint: %s", errOut)
	}
}

func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
