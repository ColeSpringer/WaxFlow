package waxflow_test

// ADTS implicit HE-AAC signalling: the committed fdk fixtures
// (codec/aac/testdata/fdk_he_v1.aac: 22050 core, 44100 stereo;
// fdk_he_v2.aac: 24000 mono core, 48000 stereo via PS) carry no ASC at
// all, so the demuxer's first-frame probe is what upgrades them. The
// differentials compare our decode against ffmpeg's; the crafted
// mid-stream cases pin that the format chosen at open is immutable.

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

func adtsFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "codec", "aac", "testdata", name))
	if err != nil {
		t.Fatalf("reading the committed ADTS fixture: %v", err)
	}
	return raw
}

// TestHEAACADTSIdentity pins what the probe resolves each fixture to:
// the HE identity at the doubled rate, stereo for both (v1 codes a
// stereo core, v2 widens a mono one), with no warnings.
func TestHEAACADTSIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate int
	}{
		{"fdk_he_v1.aac", 44100},
		{"fdk_he_v2.aac", 48000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := adtsFixture(t, tc.name)
			demux, info, err := format.OpenDemuxer(container.BytesSource(raw), "aac", nil)
			if err != nil {
				t.Fatal(err)
			}
			tr := info.Default()
			if tr.Codec != codec.HEAAC {
				t.Errorf("codec = %q, want %q", tr.Codec, codec.HEAAC)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != 2 {
				t.Errorf("format = %d Hz %d ch, want stereo at %d", tr.Fmt.Rate, tr.Fmt.Channels, tc.rate)
			}
			if wr, ok := demux.(container.Warner); ok {
				for _, w := range wr.Warnings() {
					t.Errorf("unexpected warning: %q", w.Msg)
				}
			}
		})
	}
}

// TestHEAACADTSDecodeDifferential compares our decode of the implicit
// streams against ffmpeg's. ADTS carries no gapless trims, so both sides
// decode the full stream including priming and must align at offset 0.
func TestHEAACADTSDecodeDifferential(t *testing.T) {
	for _, name := range []string{"fdk_he_v1.aac", "fdk_he_v2.aac"} {
		t.Run(name, func(t *testing.T) {
			raw := adtsFixture(t, name)
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "aac")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, raw, 0o666); err != nil {
				t.Fatal(err)
			}
			ref := testutil.FFmpegDecodeF32(t, path)
			rms, off := alignedRMS(testutil.InterleaveF(got), ref, 2)
			t.Logf("%s differential: rms=%g offset=%d", name, rms, off)
			if off != 0 {
				t.Errorf("aligned at offset %d, want 0 (the PTS math must agree)", off)
			}
			if rms > aacRMSGate {
				t.Errorf("RMS %g exceeds gate %g", rms, aacRMSGate)
			}
		})
	}
}

// TestHEAACADTSSeekDifferential seeks into the middle of each stream and
// compares the tail against the same region of ffmpeg's full decode. The
// v1 leg holds the cold-decode gate; the v2 leg allows the free-running
// noise and sinusoid phase residual, exactly as the m4a seek test does
// (see TestHEAACv2SeekDifferential).
func TestHEAACADTSSeekDifferential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target int64
		gate   float64
	}{
		{"fdk_he_v1.aac", 60000, aacRMSGate},
		{"fdk_he_v2.aac", 70000, 0.02},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := adtsFixture(t, tc.name)
			med, err := waxflow.New().OpenStream(container.BytesSource(raw), "aac")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			landed, err := med.SeekSample(tc.target)
			if err != nil {
				t.Fatalf("seek: %v", err)
			}
			if landed != tc.target {
				t.Fatalf("seek landed at %d, want exactly %d", landed, tc.target)
			}
			f := med.Info().Default().Fmt
			tail := audio.Get(f, 8*48000)
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
			dir := t.TempDir()
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, raw, 0o666); err != nil {
				t.Fatal(err)
			}
			ref := testutil.FFmpegDecodeF32(t, path)
			// ADTS has no trims, so both sides decode the same AU count; the
			// tail must reach the stream end, or an early decode error would
			// pass the RMS gate over a fraction of the file.
			if want := int64(len(ref))/2 - tc.target; int64(tail.N) != want {
				t.Fatalf("seek tail decoded %d samples, want %d", tail.N, want)
			}
			rms, off := alignedRMS(testutil.InterleaveF(tail), ref[2*tc.target:], 2)
			t.Logf("%s seek differential: rms=%g offset=%d", tc.name, rms, off)
			if off != 0 {
				t.Errorf("seek tail aligned at offset %d, want 0 (PTS math vs ffmpeg)", off)
			}
			if rms > tc.gate {
				t.Errorf("seek-tail RMS %g exceeds gate %g", rms, tc.gate)
			}
		})
	}
}

// adtsFrameStarts walks an ADTS stream's frame boundaries by the declared
// frame lengths, for slicing test streams at frame offsets.
func adtsFrameStarts(raw []byte) []int {
	var starts []int
	off := 0
	for off+7 <= len(raw) && raw[off] == 0xFF && raw[off+1]&0xF0 == 0xF0 {
		starts = append(starts, off)
		fl := int(raw[off+3]&0x3)<<11 | int(raw[off+4])<<3 | int(raw[off+5]>>5)
		if fl < 7 {
			break
		}
		off += fl
	}
	return starts
}

// TestHEAACADTSMidStreamOpen pins that a v2 stream opened at any frame
// offset still resolves to stereo. fdk repeats the SBR header only every
// tenth frame, and the v1/v2 split is decidable only at a frame whose
// sbr_data parses, so a single-frame probe turned nine of ten start
// offsets (any cut, splice, or resync after damage) into a silent mono
// open; the probe now walks the head until it is conclusive.
func TestHEAACADTSMidStreamOpen(t *testing.T) {
	raw := adtsFixture(t, "fdk_he_v2.aac")
	starts := adtsFrameStarts(raw)
	if len(starts) < 16 {
		t.Fatalf("fixture walked to only %d frames", len(starts))
	}
	for drop := 1; drop <= 9; drop++ {
		_, info, err := format.OpenDemuxer(container.BytesSource(raw[starts[drop]:]), "aac", nil)
		if err != nil {
			t.Fatalf("drop %d: %v", drop, err)
		}
		tr := info.Default()
		if tr.Codec != codec.HEAAC || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
			t.Errorf("drop %d: opened as %q %d Hz %d ch, want he-aac stereo at 48000", drop, tr.Codec, tr.Fmt.Rate, tr.Fmt.Channels)
		}
	}
	// The v1 fixture must still resolve v1 (stereo core, no PS) at the
	// same offsets: the deeper probe must not overreach.
	rawV1 := adtsFixture(t, "fdk_he_v1.aac")
	startsV1 := adtsFrameStarts(rawV1)
	for drop := 1; drop <= 9; drop++ {
		_, info, err := format.OpenDemuxer(container.BytesSource(rawV1[startsV1[drop]:]), "aac", nil)
		if err != nil {
			t.Fatalf("v1 drop %d: %v", drop, err)
		}
		tr := info.Default()
		if tr.Codec != codec.HEAAC || tr.Fmt.Rate != 44100 || tr.Fmt.Channels != 2 {
			t.Errorf("v1 drop %d: opened as %q %d Hz %d ch, want he-aac stereo at 44100", drop, tr.Codec, tr.Fmt.Rate, tr.Fmt.Channels)
		}
	}
}

// TestHEAACADTSMidStreamShapeChanges pins that the format chosen at open
// is immutable. An HE stream that turns into plain LC mid-way keeps its
// 2048-sample AU shape (the high band conceals); an LC stream that grows
// SBR fills later keeps its LC shape (the fills stay skipped,
// spec-sanctioned legacy behavior).
func TestHEAACADTSMidStreamShapeChanges(t *testing.T) {
	he := adtsFixture(t, "fdk_he_v2.aac")

	// A same-shape LC tail: 24000 mono, encoded by our own aac row into
	// ADTS so the headers are kin with the fixture's (rate, channels,
	// profile all agree and resync accepts the splice).
	tone := make([]float32, 2*24000)
	for i := range tone {
		tone[i] = 0.3 * float32(math.Sin(2*math.Pi*440*float64(i)/24000))
	}
	wav := filepath.Join(t.TempDir(), "lc.wav")
	testutil.WriteFloatWAV(t, wav, 24000, [][]float32{tone})
	lcSrc, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	var lc bytes.Buffer
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(lcSrc), "wav", &lc,
		waxflow.TranscodeOptions{Format: "aac", Container: "adts"}); err != nil {
		t.Fatalf("encoding the LC tail: %v", err)
	}

	count := func(raw []byte, hint string) (int64, int, int) {
		demux, info, err := format.OpenDemuxer(container.BytesSource(raw), hint, nil)
		if err != nil {
			t.Fatal(err)
		}
		var pkt container.Packet
		frames := 0
		for demux.ReadPacket(&pkt) == nil {
			frames++
		}
		return int64(info.Default().Fmt.Rate), info.Default().Fmt.Channels, frames
	}

	heLC := append(append([]byte(nil), he...), lc.Bytes()...)
	rate, chans, frames := count(heLC, "aac")
	if rate != 48000 || chans != 2 {
		t.Errorf("HE-then-LC opens as %d Hz %d ch, want the HE shape 48000/2 fixed at open", rate, chans)
	}
	dec, err := decodeAllDynamic(t, container.BytesSource(heLC), "aac")
	if err != nil {
		t.Fatalf("HE-then-LC decode: %v", err)
	}
	defer audio.Put(dec)
	if dec.N != frames*2048 {
		t.Errorf("HE-then-LC decoded %d samples over %d frames, want %d (constant 2048/AU)", dec.N, frames, frames*2048)
	}

	lcHE := append(append([]byte(nil), lc.Bytes()...), he...)
	rate, chans, frames = count(lcHE, "aac")
	if rate != 24000 || chans != 1 {
		t.Errorf("LC-then-HE opens as %d Hz %d ch, want the LC shape 24000/1 fixed at open", rate, chans)
	}
	dec2, err := decodeAllDynamic(t, container.BytesSource(lcHE), "aac")
	if err != nil {
		t.Fatalf("LC-then-HE decode: %v", err)
	}
	defer audio.Put(dec2)
	if dec2.N != frames*1024 {
		t.Errorf("LC-then-HE decoded %d samples over %d frames, want %d (constant 1024/AU)", dec2.N, frames, frames*1024)
	}
}
