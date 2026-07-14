package waxflow_test

// Progressive-MP4 differential: the flat moov/stbl form the aac/alac
// rows write through the "progressive" container override must be a real .m4a a
// third-party tool reads. Transcode through the engine, confirm ffmpeg parses
// it as a non-fragmented (no moof) AAC movie, and decode it back to the right
// length. Skips without ffmpeg unless WAXFLOW_REQUIRE_FFMPEG=1.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

func TestProgressiveMuxerDifferential(t *testing.T) {
	testutil.FFmpeg(t)
	e := waxflow.New()
	const frames = 9600

	wav, src := makeWAV(t, pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2, frames, 61)
	defer audio.Put(src)
	path := filepath.Join(t.TempDir(), "prog.m4a")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Transcode(context.Background(), container.BytesSource(wav), "", f,
		waxflow.TranscodeOptions{Format: "aac", Container: "progressive"}); err != nil {
		f.Close()
		t.Fatalf("transcode aac/progressive: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ref := testutil.FFprobeFile(t, path)
	if ref.CodecName != "aac" || ref.SampleRate != 48000 || ref.Channels != 2 {
		t.Errorf("ffprobe = %+v", ref)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if hasTopLevelBox(raw, "moof") {
		t.Error("progressive output contains a moof box (should be flat)")
	}
	if !hasTopLevelBox(raw, "moov") || !hasTopLevelBox(raw, "mdat") {
		t.Error("progressive output missing moov or mdat")
	}
	got := len(testutil.FFmpegDecodeF32(t, path)) / 2
	if diff := got - frames; diff < -2048 || diff > 2048 {
		t.Errorf("ffmpeg decoded %d frames from progressive output, want ~%d", got, frames)
	}
}

// hasTopLevelBox reports whether the file has a top-level box of the given type.
func hasTopLevelBox(data []byte, typ string) bool {
	off := 0
	for off+8 <= len(data) {
		size := int(data[off])<<24 | int(data[off+1])<<16 | int(data[off+2])<<8 | int(data[off+3])
		if string(data[off+4:off+8]) == typ {
			return true
		}
		if size == 1 { // 64-bit largesize
			if off+16 > len(data) {
				return false
			}
			var s int64
			for i := 0; i < 8; i++ {
				s = s<<8 | int64(data[off+8+i])
			}
			size = int(s)
		}
		if size < 8 {
			return false
		}
		off += size
	}
	return false
}
