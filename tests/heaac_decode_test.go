package waxflow_test

// HE-AAC v1 decode and remux behavior, pinned against the committed
// fdk-encoded fixture (codec/aac/testdata/fdk_he_v1.m4a: explicit
// hierarchical signaling, 22050 core, 44100 output, delay 5058, 132300
// samples). The differential compares our SBR decode against ffmpeg's
// native one within the AAC RMS gate; the remux tests pin the family
// redirect that keeps format=aac copying HE sources.

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

const heFixtureSamples = 132300

func heFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "codec", "aac", "testdata", "fdk_he_v1.m4a"))
	if err != nil {
		t.Fatalf("reading the committed HE-AAC fixture: %v", err)
	}
	return raw
}

// TestHEAACIdentity pins what opening the fixture reports: codec he-aac at
// the 44100 extension rate, exact gapless trims, and no band-limit warning.
func TestHEAACIdentity(t *testing.T) {
	raw := heFixture(t)
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if tr.Codec != codec.HEAAC {
		t.Errorf("codec = %q, want %q", tr.Codec, codec.HEAAC)
	}
	if tr.Fmt.Rate != 44100 {
		t.Errorf("rate = %d, want the 44100 extension rate", tr.Fmt.Rate)
	}
	if tr.Delay != 5058 || tr.Samples != heFixtureSamples {
		t.Errorf("gapless = delay %d samples %d, want 5058/%d", tr.Delay, tr.Samples, heFixtureSamples)
	}
	if wr, ok := demux.(container.Warner); ok {
		for _, w := range wr.Warnings() {
			t.Errorf("unexpected warning on a v1 file: %q", w.Msg)
		}
	}
}

// TestHEAACDecodeDifferential compares our SBR decode of the fixture
// against ffmpeg's native HE-AAC decode within the AAC RMS gate.
func TestHEAACDecodeDifferential(t *testing.T) {
	raw := heFixture(t)
	got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer audio.Put(got)
	if got.N != heFixtureSamples {
		t.Errorf("decoded %d samples, want the gapless %d", got.N, heFixtureSamples)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "he.m4a")
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	rms, off := alignedRMS(testutil.InterleaveF(got), ref, 2)
	t.Logf("he-aac differential: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("RMS %g exceeds gate %g (offset %d)", rms, aacRMSGate, off)
	}
}

// TestHEAACSeekDifferential seeks into the middle and compares the decoded
// tail against the same region of ffmpeg's full decode. The QMF and
// adjuster histories rebuild during the container's pre-roll, so the tail
// must land within the same gate as a cold decode.
func TestHEAACSeekDifferential(t *testing.T) {
	raw := heFixture(t)
	med, err := waxflow.New().OpenStream(container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	const target = 60000
	landed, err := med.SeekSample(target)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if landed != target {
		t.Fatalf("seek landed at %d, want exactly %d (sample-exact contract)", landed, target)
	}
	f := med.Info().Default().Fmt
	tail := audio.Get(f, heFixtureSamples-target+4096)
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
	if tail.N != heFixtureSamples-target {
		t.Fatalf("tail decoded %d samples, want %d", tail.N, heFixtureSamples-target)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "he.m4a")
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFmpegDecodeF32(t, path)
	if len(ref) < 2*heFixtureSamples {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want %d", len(ref), 2*heFixtureSamples)
	}
	rms, off := alignedRMS(testutil.InterleaveF(tail), ref[2*target:], 2)
	t.Logf("he-aac seek differential: rms=%g offset=%d", rms, off)
	if off != 0 {
		t.Errorf("seek tail aligned at offset %d, want 0 (the seek must be sample-exact)", off)
	}
	if rms > aacRMSGate {
		t.Errorf("seek-tail RMS %g exceeds gate %g", rms, aacRMSGate)
	}
}

// TestHEAACRemuxFamilyRedirect pins the compatibility keystone: format=aac
// on an HE-AAC source remuxes (copies) under the he-aac identity instead of
// silently falling through to a lossy LC re-encode, on both the progressive
// and the mka path, with byte-identical access units.
func TestHEAACRemuxFamilyRedirect(t *testing.T) {
	src := heFixture(t)
	want := payloads(t, src, "m4a")
	if len(want) == 0 {
		t.Fatal("the fixture demuxed to no packets")
	}

	e := waxflow.New()

	// m4a -> mka under format=aac (the redirect), then mka -> m4a under
	// format=aac again: both hops must copy.
	var mka bytes.Buffer
	res, err := e.Remux(context.Background(), container.BytesSource(src), "m4a", &mka,
		waxflow.TranscodeOptions{Format: "aac", Container: "mka"})
	if err != nil {
		t.Fatalf("format=aac remux of an HE source must copy, got: %v", err)
	}
	if res.Container != "mka" {
		t.Errorf("container = %q, want mka", res.Container)
	}
	gotMKA := payloads(t, mka.Bytes(), "mka")
	if len(gotMKA) != len(want) {
		t.Fatalf("mka remux moved %d packets, source had %d", len(gotMKA), len(want))
	}
	for i := range want {
		if !bytes.Equal(gotMKA[i], want[i]) {
			t.Fatalf("mka packet %d changed", i)
		}
	}
	// The mka track must keep the he-aac identity for the second hop.
	_, info, err := format.OpenDemuxer(container.BytesSource(mka.Bytes()), "mka", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Default().Codec; got != codec.HEAAC {
		t.Errorf("mka track codec = %q, want %q", got, codec.HEAAC)
	}

	m4a := &memWS{}
	if _, err := e.Remux(context.Background(), container.BytesSource(mka.Bytes()), "mka", m4a,
		waxflow.TranscodeOptions{Format: "aac", Container: "progressive"}); err != nil {
		t.Fatalf("mka -> m4a: %v", err)
	}
	gotM4A := payloads(t, m4a.b, "m4a")
	if len(gotM4A) != len(want) {
		t.Fatalf("m4a remux moved %d packets, source had %d", len(gotM4A), len(want))
	}
	for i := range want {
		if !bytes.Equal(gotM4A[i], want[i]) {
			t.Fatalf("m4a packet %d changed", i)
		}
	}
	// And the round trip keeps the gapless trims.
	_, info2, err := format.OpenDemuxer(container.BytesSource(m4a.b), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info2.Default()
	if tr.Codec != codec.HEAAC || tr.Delay != 5058 || tr.Samples != heFixtureSamples {
		t.Errorf("round trip: codec %q delay %d samples %d, want he-aac/5058/%d",
			tr.Codec, tr.Delay, tr.Samples, heFixtureSamples)
	}
}

// TestHEAACSegmentedRemuxCodecs pins the segmented path: a format=aac
// segmented remux of an HE source plans under the he-aac row and the
// playlist carries mp4a.40.5, not the LC string.
func TestHEAACSegmentedRemuxCodecs(t *testing.T) {
	raw := heFixture(t)
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = demux
	track := info.Default()

	e := waxflow.New()
	sp, err := e.PlanRemuxSegments(track, waxflow.TranscodeOptions{Format: "aac"}, 4, 2048)
	if err != nil {
		t.Fatalf("PlanRemuxSegments: %v", err)
	}
	if sp == nil {
		t.Fatal("segmented remux of an HE source declined; the family redirect must keep it copying")
	}
	if sp.Codecs != "mp4a.40.5" {
		t.Errorf("playlist CODECS = %q, want mp4a.40.5", sp.Codecs)
	}
}

// TestCutOfHEAACMatchesFullDecode drives the cut rung on an HE source: a
// head-anchored span copies the source's own packets, and decoding the cut
// yields bit-identical samples to the same region of the full decode (same
// packets, same deterministic decoder, both from AU zero). A mid-stream
// span must decline to the transcode rung instead: the fixture carries its
// SBR header only in the first fill, so a slice without it would play with
// a muted high band.
func TestCutOfHEAACMatchesFullDecode(t *testing.T) {
	src := heFixture(t)
	const cutTo = 40960 // 20 AUs on the 2048-sample grid

	out, plan := runCut(t, src, "m4a", waxflow.TranscodeOptions{Format: "aac"},
		[]waxflow.Span{{From: 0, To: cutTo}})
	if out == nil {
		t.Fatal("PlanCut declined a head-anchored HE-AAC cut, which it must serve")
	}
	if plan.Samples != cutTo {
		t.Errorf("plan.Samples = %d, want %d", plan.Samples, cutTo)
	}
	want := payloads(t, src, "m4a")
	got := payloads(t, out, "m4a")
	if len(got) == 0 || len(got) >= len(want) {
		t.Fatalf("cut kept %d of %d packets", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("cut packet %d is not the source packet's bytes", i)
		}
	}

	full, err := decodeAllDynamic(t, container.BytesSource(src), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(full)
	dec, err := decodeAllDynamic(t, container.BytesSource(out), "m4a")
	if err != nil {
		t.Fatalf("decoding the cut: %v", err)
	}
	defer audio.Put(dec)
	if dec.N != cutTo {
		t.Fatalf("cut decoded %d samples, want %d", dec.N, cutTo)
	}
	di, fi := testutil.InterleaveF(dec), testutil.InterleaveF(full)
	for i := range di {
		if di[i] != fi[i] {
			t.Fatalf("cut sample %d = %g, full decode has %g; the head-anchored cut must decode bit-identically", i, di[i], fi[i])
		}
	}

	if out, _ := runCut(t, src, "m4a", waxflow.TranscodeOptions{Format: "aac"},
		[]waxflow.Span{{From: 8192, To: cutTo}}); out != nil {
		t.Error("a mid-stream HE-AAC cut must decline (no SBR header past the head); rung 3 serves it")
	}
}

// TestHEAACADTSRemux pins format=aac&container=adts on an HE source: the
// remux writes the implicit ADTS form (core-rate LC header, the SBR
// extension riding in the payloads) with byte-identical access units.
// Before the ADTS muxer learned HE-AAC this planned fine and then refused
// in Begin, after the response status was committed.
func TestHEAACADTSRemux(t *testing.T) {
	src := heFixture(t)
	var out bytes.Buffer
	if _, err := waxflow.New().Remux(context.Background(), container.BytesSource(src), "m4a", &out,
		waxflow.TranscodeOptions{Format: "aac", Container: "adts"}); err != nil {
		t.Fatalf("HE-AAC to ADTS must remux as the implicit form: %v", err)
	}
	want := payloads(t, src, "m4a")
	got := payloads(t, out.Bytes(), "aac")
	if len(got) != len(want) {
		t.Fatalf("adts remux moved %d packets, source had %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("adts packet %d changed", i)
		}
	}
	_, info, err := format.OpenDemuxer(container.BytesSource(out.Bytes()), "aac", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := info.Default()
	if tr.Codec != codec.AACLC || tr.Fmt.Rate != 22050 {
		t.Errorf("adts reads back as %q at %d Hz, want the aac-lc core at 22050 until ADTS SBR detection lands",
			tr.Codec, tr.Fmt.Rate)
	}
}

// TestHEAACFreshFDKDifferential guards against overfitting to the one
// committed fixture: when ffmpeg carries libfdk_aac (non-free, so most
// builds skip here), it encodes a fresh HE-AAC stream and differentials
// our decode against ffmpeg's within the same gate. This is also the
// caller of record for testutil.FFmpegFDKEncodeFile, which regenerates
// the committed fixtures.
func TestHEAACFreshFDKDifferential(t *testing.T) {
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
			v := 0.3*math.Sin(2*math.Pi*523.25*ti+float64(c)) +
				0.2*math.Sin(2*math.Pi*3135.96*ti) +
				0.1*math.Sin(2*math.Pi*8372.02*ti*(1+0.1*math.Sin(2*math.Pi*0.5*ti)))
			chans[c][i] = float32(v)
		}
	}
	wav := filepath.Join(dir, "src.wav")
	testutil.WriteFloatWAV(t, wav, rate, chans)
	enc := testutil.FFmpegFDKEncodeFile(t, dir, wav, 56, "aac_he", "m4a")
	raw, err := os.ReadFile(enc)
	if err != nil {
		t.Fatal(err)
	}

	_, info, err := format.OpenDemuxer(container.BytesSource(raw), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Default().Codec; got != codec.HEAAC {
		t.Fatalf("fresh fdk encode probes as %q, want %q (signalling form changed?)", got, codec.HEAAC)
	}
	got, err := decodeAllDynamic(t, container.BytesSource(raw), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(got)
	ref := testutil.FFmpegDecodeF32(t, enc)
	rms, off := alignedRMS(testutil.InterleaveF(got), ref, 2)
	t.Logf("fresh fdk differential: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("RMS %g exceeds gate %g (offset %d)", rms, aacRMSGate, off)
	}
}
