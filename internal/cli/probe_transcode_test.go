package cli

import (
	"encoding/json"
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
