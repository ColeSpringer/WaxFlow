package waxflow_test

// HE-AAC v2 decode and remux behavior, pinned against the committed
// fdk-encoded fixture (codec/aac/testdata/fdk_he_v2.m4a: explicit AOT-29
// signaling, 24000 mono core, 48000 stereo output, delay 7106, 144000
// samples). The differential compares our SBR+PS decode against ffmpeg's
// native one within the AAC RMS gate; the stereo sanity check pins that
// the parametric widening actually ran (dual mono would be the silent
// failure shape).

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

const (
	heV2FixtureSamples = 144000
	heV2FixtureDelay   = 7106
)

func heV2Fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "codec", "aac", "testdata", "fdk_he_v2.m4a"))
	if err != nil {
		t.Fatalf("reading the committed HE-AAC v2 fixture: %v", err)
	}
	return raw
}

// TestHEAACv2Identity pins what opening the fixture reports: codec he-aac,
// stereo at the 48000 extension rate, exact gapless trims, no warning.
func TestHEAACv2Identity(t *testing.T) {
	raw := heV2Fixture(t)
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if tr.Codec != codec.HEAAC {
		t.Errorf("codec = %q, want %q", tr.Codec, codec.HEAAC)
	}
	if tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
		t.Errorf("format = %d Hz %d ch, want stereo at the 48000 extension rate", tr.Fmt.Rate, tr.Fmt.Channels)
	}
	if tr.Delay != heV2FixtureDelay || tr.Samples != heV2FixtureSamples {
		t.Errorf("gapless = delay %d samples %d, want %d/%d", tr.Delay, tr.Samples, heV2FixtureDelay, heV2FixtureSamples)
	}
	if wr, ok := demux.(container.Warner); ok {
		for _, w := range wr.Warnings() {
			t.Errorf("unexpected warning on a v2 file: %q", w.Msg)
		}
	}
}

// stereoSpread reports the L/R difference energy relative to the total, a
// cheap witness that the parametric widening ran: dual mono spreads 0.
func stereoSpread(buf *audio.Buffer) float64 {
	l, r := buf.ChanF(0)[:buf.N], buf.ChanF(1)[:buf.N]
	var diff, tot float64
	for i := range l {
		d := float64(l[i] - r[i])
		s := float64(l[i] + r[i])
		diff += d * d
		tot += s*s + d*d
	}
	if tot == 0 {
		return 0
	}
	return diff / tot
}

// TestHEAACv2DecodeDifferential compares our SBR+PS decode of the fixture
// against ffmpeg's native HE-AAC v2 decode within the AAC RMS gate.
func TestHEAACv2DecodeDifferential(t *testing.T) {
	raw := heV2Fixture(t)
	got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer audio.Put(got)
	if got.N != heV2FixtureSamples {
		t.Errorf("decoded %d samples, want the gapless %d", got.N, heV2FixtureSamples)
	}
	if spread := stereoSpread(got); spread < 1e-4 {
		t.Errorf("stereo spread %g: the output is (near) dual mono, PS did not run", spread)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "he2.m4a")
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	rms, off := alignedRMS(testutil.InterleaveF(got), ref, 2)
	t.Logf("he-aac v2 differential: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("RMS %g exceeds gate %g (offset %d)", rms, aacRMSGate, off)
	}
}

// TestHEAACv2SeekDifferential seeks into the middle and compares the
// decoded tail against the same region of ffmpeg's full decode. The
// landing must be sample-exact and the tail stereo, but the RMS gate is
// deliberately looser than the cold-decode one: the SBR noise floors and
// added sinusoids advance their phase tables continuously from the
// stream head, in this decoder and every other, so a mid-stream join
// reproduces them at a different phase forever; no preroll can rebuild
// that state. The parametric layer itself (PS header, IID/ICC chains,
// envelopes) does resynchronize inside the 12-AU preroll, which is what
// the 0.02 gate still verifies structurally: a mono collapse measures
// about 0.04 on this fixture and a muted band far more, while the
// phase-only residual sits near 0.009.
func TestHEAACv2SeekDifferential(t *testing.T) {
	raw := heV2Fixture(t)
	med, err := waxflow.New().OpenStream(container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	const target = 70000
	landed, err := med.SeekSample(target)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if landed != target {
		t.Fatalf("seek landed at %d, want exactly %d (sample-exact contract)", landed, target)
	}
	f := med.Info().Default().Fmt
	tail := audio.Get(f, heV2FixtureSamples-target+4096)
	defer audio.Put(tail)
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err != nil {
			break
		}
		audio.CopyFrames(tail, tail.N, tmp, 0, tmp.N)
		tail.N += tmp.N
	}
	if tail.N != heV2FixtureSamples-target {
		t.Fatalf("tail decoded %d samples, want %d", tail.N, heV2FixtureSamples-target)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "he2.m4a")
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	if len(ref) < 2*heV2FixtureSamples {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want %d", len(ref), 2*heV2FixtureSamples)
	}
	rms, off := alignedRMS(testutil.InterleaveF(tail), ref[2*target:], 2)
	t.Logf("he-aac v2 seek differential: rms=%g offset=%d", rms, off)
	if off != 0 {
		t.Errorf("seek tail aligned at offset %d, want 0 (the seek must be sample-exact)", off)
	}
	const seekGate = 0.02 // see the doc comment: phase-only residual allowed
	if rms > seekGate {
		t.Errorf("seek-tail RMS %g exceeds gate %g", rms, seekGate)
	}
	if spread := stereoSpread(tail); spread < 1e-4 {
		t.Errorf("stereo spread %g after seek: PS did not resynchronize", spread)
	}
}

// TestHEAACv2FragmentedSeek pins the fragmented-MP4 seek path: it must
// pre-roll the same distance the progressive one does (the moof walk used
// to land at-or-before the target with zero preroll, starting the PS
// layer cold at every fragment boundary). The fixture is the committed
// v2 file remuxed into CMAF by our own segmenter-side muxer, and the tail
// is compared against ffmpeg's decode of that same fragmented file under
// the v2 seek gate.
func TestHEAACv2FragmentedSeek(t *testing.T) {
	raw := heV2Fixture(t)
	frag := &memWS{}
	if _, err := waxflow.New().Remux(context.Background(), container.BytesSource(raw), "m4a", frag,
		waxflow.TranscodeOptions{Format: "aac", Container: "fragmented"}); err != nil {
		t.Fatalf("fragmenting the fixture: %v", err)
	}

	med, err := waxflow.New().OpenStream(container.BytesSource(frag.Buf), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	const target = 70000
	landed, err := med.SeekSample(target)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if landed != target {
		t.Fatalf("seek landed at %d, want exactly %d", landed, target)
	}
	f := med.Info().Default().Fmt
	tail := audio.Get(f, heV2FixtureSamples-target+4096)
	defer audio.Put(tail)
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		if err := med.ReadChunk(tmp); err != nil {
			break
		}
		audio.CopyFrames(tail, tail.N, tmp, 0, tmp.N)
		tail.N += tmp.N
	}
	if tail.N != heV2FixtureSamples-target {
		t.Fatalf("tail decoded %d samples, want %d", tail.N, heV2FixtureSamples-target)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "he2frag.m4a")
	if err := os.WriteFile(path, frag.Buf, 0o666); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	if len(ref) < 2*heV2FixtureSamples {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want %d", len(ref), 2*heV2FixtureSamples)
	}
	rms, off := alignedRMS(testutil.InterleaveF(tail), ref[2*target:], 2)
	t.Logf("he-aac v2 fragmented seek: rms=%g offset=%d", rms, off)
	if off != 0 {
		t.Errorf("seek tail aligned at offset %d, want 0", off)
	}
	const seekGate = 0.02 // the progressive seek test's gate and rationale
	if rms > seekGate {
		t.Errorf("seek-tail RMS %g exceeds gate %g", rms, seekGate)
	}
	if spread := stereoSpread(tail); spread < 1e-4 {
		t.Errorf("stereo spread %g after seek: PS did not resynchronize", spread)
	}
}

// TestHEAACv2RemuxKeepsIdentity pins that the family redirect covers v2:
// format=aac on an AOT-29 source copies byte-identical access units under
// the he-aac identity through mka and back, keeping the stereo format and
// the gapless trims.
func TestHEAACv2RemuxKeepsIdentity(t *testing.T) {
	src := heV2Fixture(t)
	want := payloads(t, src, "m4a")
	if len(want) == 0 {
		t.Fatal("the fixture demuxed to no packets")
	}
	e := waxflow.New()

	var mka bytes.Buffer
	if _, err := e.Remux(context.Background(), container.BytesSource(src), "m4a", &mka,
		waxflow.TranscodeOptions{Format: "aac", Container: "mka"}); err != nil {
		t.Fatalf("format=aac remux of a v2 source must copy, got: %v", err)
	}
	_, info, err := format.OpenDemuxer(container.BytesSource(mka.Bytes()), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if tr.Codec != codec.HEAAC || tr.Fmt.Channels != 2 || tr.Fmt.Rate != 48000 {
		t.Errorf("mka track = %q %d ch %d Hz, want he-aac stereo 48000", tr.Codec, tr.Fmt.Channels, tr.Fmt.Rate)
	}

	m4a := &memWS{}
	if _, err := e.Remux(context.Background(), container.BytesSource(mka.Bytes()), "mka", m4a,
		waxflow.TranscodeOptions{Format: "aac", Container: "progressive"}); err != nil {
		t.Fatalf("mka -> m4a: %v", err)
	}
	got := payloads(t, m4a.Buf, "m4a")
	if len(got) != len(want) {
		t.Fatalf("round trip moved %d packets, source had %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d changed in the round trip", i)
		}
	}
	_, info2, err := format.OpenDemuxer(container.BytesSource(m4a.Buf), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr = info2.Default()
	if tr.Codec != codec.HEAAC || tr.Delay != heV2FixtureDelay || tr.Samples != heV2FixtureSamples {
		t.Errorf("round trip: codec %q delay %d samples %d, want he-aac/%d/%d",
			tr.Codec, tr.Delay, tr.Samples, heV2FixtureDelay, heV2FixtureSamples)
	}
}

// TestHEAACv2SegmentedRemuxCodecs pins that a segmented remux of a v2
// source advertises mp4a.40.29: the row's static mp4a.40.5 is v1's
// string, and remuxed v2 media must not ride under a v1 label the way
// HE content once rode under mp4a.40.2.
func TestHEAACv2SegmentedRemuxCodecs(t *testing.T) {
	raw := heV2Fixture(t)
	_, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := waxflow.New().PlanRemuxSegments(info.Default(), waxflow.TranscodeOptions{Format: "aac"}, 4, 2048)
	if err != nil {
		t.Fatalf("PlanRemuxSegments: %v", err)
	}
	if sp == nil {
		t.Fatal("segmented remux of a v2 source declined; the family redirect must keep it copying")
	}
	if sp.Codecs != "mp4a.40.29" {
		t.Errorf("playlist CODECS = %q, want mp4a.40.29", sp.Codecs)
	}
}

// TestHEAACv2FreshFDKDifferential guards against overfitting to the one
// committed fixture, when this ffmpeg build carries libfdk_aac.
func TestHEAACv2FreshFDKDifferential(t *testing.T) {
	if !testutil.HaveFDK(t) {
		t.Skip("ffmpeg has no libfdk_aac encoder; the committed-fixture differential covers this build")
	}
	dir := t.TempDir()
	const rate = 44100
	n := 3 * rate
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, n)
		for i := range chans[c] {
			ti := float64(i) / rate
			pan := 0.5 + 0.4*math.Sin(2*math.Pi*0.9*ti)
			v := 0.3*math.Sin(2*math.Pi*523.25*ti) +
				0.2*math.Sin(2*math.Pi*3135.96*ti+float64(c)*2)
			w := pan
			if c == 1 {
				w = 1 - pan
			}
			chans[c][i] = float32(v * w)
		}
	}
	wav := filepath.Join(dir, "src.wav")
	testutil.WriteFloatWAV(t, wav, rate, chans)
	enc := testutil.FFmpegFDKEncodeFile(t, dir, wav, 32, "aac_he_v2", "m4a")
	raw, err := os.ReadFile(enc)
	if err != nil {
		t.Fatal(err)
	}

	_, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := info.Default(); tr.Codec != codec.HEAAC || tr.Fmt.Channels != 2 {
		t.Fatalf("fresh fdk v2 encode probes as %q %d ch, want he-aac stereo (signalling form changed?)",
			tr.Codec, tr.Fmt.Channels)
	}
	got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(got)
	ref := testutil.FFmpegDecodeF32(t, enc)
	rms, off := alignedRMS(testutil.InterleaveF(got), ref, 2)
	t.Logf("fresh fdk v2 differential: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("RMS %g exceeds gate %g (offset %d)", rms, aacRMSGate, off)
	}
}
