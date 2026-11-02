package waxflow_test

// Native FLAC whose first frame is not numbered 0, end to end: the renumbered
// twin of this tree's own encode, and the cut `ffmpeg -ss X -c copy` writes,
// each held to the stream it came from.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/internal/testutil"
)

// flacFirstFrame is the sample a native FLAC's first frame header names.
func flacFirstFrame(t *testing.T, raw []byte) int64 {
	t.Helper()
	off := 4
	for last := false; !last; {
		last = raw[off]&0x80 != 0
		off += 4 + (int(raw[off+1])<<16 | int(raw[off+2])<<8 | int(raw[off+3]))
	}
	fi, err := flac.ParseFrameHeader(raw[off:])
	if err != nil || fi.Variable {
		t.Fatalf("no fixed-block-size frame header after the metadata: %v", err)
	}
	return int64(fi.Coded) * int64(fi.BlockSize)
}

// requireFLACTwin holds raw, a FLAC stream (in the container hint names)
// whose audio is twin's from sample skip on, to twin, a native FLAC: its
// length, exact but for Matroska's, is what a decode delivers, the decode is
// twin's bit for bit, seeks land on twin's samples, an open slice ends
// cleanly, and a cut declines for the codec, before any length is weighed.
func requireFLACTwin(t *testing.T, raw []byte, hint string, twin []byte, skip int64) {
	t.Helper()
	ref, err := decodeAllDynamic(t, container.BytesSource(twin), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ref)

	med := openBytes(t, raw, hint)
	tr := med.Info().Default()
	med.Close()
	got, err := decodeAllDynamic(t, container.BytesSource(raw), hint)
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(got)
	// A Matroska open leaves its length unmeasured (container.Track.SamplesExact),
	// so only its count is held.
	if (!tr.SamplesExact && hint != "mka") || tr.Samples != int64(got.N) {
		t.Errorf("the track says %d samples (exact %v), a decode delivers %d", tr.Samples, tr.SamplesExact, got.N)
	}
	if want := int64(ref.N) - skip; int64(got.N) != want {
		t.Fatalf("decoded %d samples, want its twin's %d less %d", got.N, ref.N, skip)
	}
	if i := sameFrames(got, 0, ref, int(skip), got.N); i >= 0 {
		t.Errorf("the decode differs from its twin's at frame %d", i)
	}

	med, twinMed := openBytes(t, raw, hint), openBytes(t, twin, "flac")
	a, b := audio.Get(ref.Fmt, 512), audio.Get(ref.Fmt, 512)
	total := int64(got.N)
	state := int64(11)
	for i := range 40 {
		state = state*6364136223846793005 + 1442695040888963407
		target := (state >> 16) % total
		if target < 0 {
			target += total
		}
		if i == 0 {
			target = 0
		}
		if landed, err := med.SeekSample(target); err != nil || landed != target {
			t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
		}
		if landed, err := twinMed.SeekSample(target + skip); err != nil || landed != target+skip {
			t.Fatalf("twin seek to %d landed at %d (%v)", target+skip, landed, err)
		}
		if err := med.ReadChunk(a); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if err := twinMed.ReadChunk(b); err != nil {
			t.Fatal(err)
		}
		if a.Pos != target || a.N != b.N {
			t.Fatalf("seek to %d: chunk of %d at %d, twin's %d", target, a.N, a.Pos, b.N)
		}
		if j := sameFrames(a, 0, b, 0, a.N); j >= 0 {
			t.Fatalf("seek to %d: differs from its twin's at frame %d", target, j)
		}
	}
	if landed, err := med.SeekSample(total + 100); err != nil || landed != total {
		t.Fatalf("past-end seek landed at %d (%v), want %d", landed, err, total)
	}
	audio.Put(a)
	audio.Put(b)
	med.Close()
	twinMed.Close()

	sl, err := waxflow.Slice(openBytes(t, raw, hint), 0, waxflow.ToEnd)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := readUntilEnd(sl); err != nil || n != total {
		t.Fatalf("the open slice delivered %d of %d: %v", n, total, err)
	}

	// FLAC is not on the cut rung's allowlist, so the cut declines for the
	// codec and the short-delivery refusal is never reached.
	requireCutDeclines(t, raw, hint)
}

// TestFLACFirstFrameNotZero: this tree's encode with its frames renumbered
// from 1000 reads as the encode.
func TestFLACFirstFrameNotZero(t *testing.T) {
	twin := transcodeBytes(t, loopedWAV(t, 30), waxflow.TranscodeOptions{Format: "flac"})
	raw := testutil.RenumberFLACFrames(t, twin, 1000)
	info := openBytes(t, raw, "flac")
	ws := info.Info().Notes
	info.Close()
	if len(ws) != 1 || !strings.Contains(ws[0], "first frame") {
		t.Errorf("notes %v, want one about the first frame", ws)
	}
	requireFLACTwin(t, raw, "flac", twin, 0)
}

// TestFLACStreamCopyCut is the real case: ffmpeg's stream-copy cut keeps the
// frames' numbers and the source's STREAMINFO, reads as the source from the
// cut's first frame on, and opens strictly, the source's total being a note.
func TestFLACStreamCopyCut(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	dir := t.TempDir()
	twin := transcodeBytes(t, loopedWAV(t, 30), waxflow.TranscodeOptions{Format: "flac"})
	src, cut := filepath.Join(dir, "src.flac"), filepath.Join(dir, "cut.flac")
	if err := os.WriteFile(src, twin, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-ss", "10", "-i", src, "-c", "copy", cut).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(cut)
	if err != nil {
		t.Fatal(err)
	}
	skip := flacFirstFrame(t, raw)
	if skip == 0 || bytes.Equal(raw, twin) {
		t.Fatal("ffmpeg's cut starts at frame 0; there is nothing to test")
	}
	t.Logf("ffmpeg's cut starts at sample %d", skip)
	requireFLACTwin(t, raw, "flac", twin, skip)
	requireStrictCopyCut(t, raw, "flac")
}

// requireStrictCopyCut asserts ffmpeg's stream-copy cut opens strictly with
// notes only: one for the first frame's number and, where the cut keeps its
// source's STREAMINFO, one for that total, the source's length, which is where
// the frames end counted from sample 0.
func requireStrictCopyCut(t *testing.T, raw []byte, hint string) {
	t.Helper()
	var (
		tr container.Track
		ws []container.Warning
	)
	if hint == "flac" {
		d, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true})
		if err != nil {
			t.Fatalf("a strict open refused ffmpeg's cut: %v", err)
		}
		tr, ws = d.Tracks()[0], d.Warnings()
	} else {
		d, err := ogg.NewDemuxer(container.BytesSource(raw), &ogg.DemuxerOptions{Strict: true})
		if err != nil {
			t.Fatalf("a strict open refused ffmpeg's cut: %v", err)
		}
		tr, ws = d.Tracks()[0], d.Warnings()
	}
	// The file's own STREAMINFO: "fLaC" and its block header lead it in both
	// containers.
	at := bytes.Index(raw, []byte("fLaC"))
	if at < 0 || at+8+flac.StreamInfoLen > len(raw) {
		t.Fatal("no STREAMINFO in the cut")
	}
	si, err := flac.ParseStreamInfo(raw[at+8 : at+8+flac.StreamInfoLen])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first frame"}
	if si.Samples != tr.Samples {
		want = append(want, "source's length")
		// The track hands on STREAMINFO as the frames have it.
		got, err := flac.ParseStreamInfo(tr.CodecConfig)
		if err != nil {
			t.Fatal(err)
		}
		if got.Samples != tr.Samples || got.MD5 != ([16]byte{}) {
			t.Errorf("the track's STREAMINFO states %d samples and MD5 %x, want %d and none",
				got.Samples, got.MD5, tr.Samples)
		}
	}
	ok := len(ws) == len(want)
	for i := 0; ok && i < len(ws); i++ {
		ok = ws[i].Kind == container.Note && strings.Contains(ws[i].Msg, want[i])
	}
	if !ok {
		t.Errorf("STREAMINFO declares %d samples; warnings %v, want notes naming %q", si.Samples, ws, want)
	}
}

// TestOggFLACStreamCopyCut is the third-party shape: ffmpeg's stream-copy cut
// into Ogg keeps the frames' numbers and the source's STREAMINFO, times its
// pages from the cut point, reads as the source from the cut's first frame on,
// and opens strictly.
func TestOggFLACStreamCopyCut(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	dir := t.TempDir()
	twin := transcodeBytes(t, loopedWAV(t, 30), waxflow.TranscodeOptions{Format: "flac"})
	src, cut := filepath.Join(dir, "src.flac"), filepath.Join(dir, "cut.oga")
	if err := os.WriteFile(src, twin, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-ss", "10", "-i", src, "-c", "copy", cut).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(cut)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ogg.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	fi, err := flac.ParseFrameHeader(pkt.Data)
	if err != nil || fi.Variable || fi.Coded == 0 {
		t.Fatalf("the cut's first frame (%v) is not a fixed-block-size frame past 0", err)
	}
	skip := int64(fi.Coded) * int64(fi.BlockSize)
	t.Logf("ffmpeg's cut starts at sample %d", skip)
	requireFLACTwin(t, raw, "oga", twin, skip)
	requireStrictCopyCut(t, raw, "oga")
}

// TestOggFLACLoneEndPageCut: ffmpeg's stream-copy cut of a short tail into
// Ogg is one page, ending the stream, whose granule is the cut's length while
// its frames run from the frame before the cut point. A FLAC granule does not
// trim, so the stream is its frames, as ffmpeg reads it.
func TestOggFLACLoneEndPageCut(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	dir := t.TempDir()
	twin := transcodeBytes(t, loopedWAV(t, 5), waxflow.TranscodeOptions{Format: "flac"})
	src, cut := filepath.Join(dir, "src.flac"), filepath.Join(dir, "cut.oga")
	if err := os.WriteFile(src, twin, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-ss", "4.5", "-i", src, "-c", "copy", cut).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(cut)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ogg.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	fi, err := flac.ParseFrameHeader(pkt.Data)
	if err != nil || fi.Variable {
		t.Fatalf("the cut's first frame (%v) is not a fixed-block-size frame", err)
	}
	skip := int64(fi.Coded) * int64(fi.BlockSize)
	requireFLACTwin(t, raw, "oga", twin, skip)
	n, err := readUntilEnd(openBytes(t, raw, "oga"))
	if err != nil {
		t.Fatal(err)
	}
	if ref := int64(len(testutil.FFmpegDecodeF32(t, cut)) / 2); ref != n {
		t.Errorf("ffmpeg decodes %d samples, this tree %d", ref, n)
	}
}
