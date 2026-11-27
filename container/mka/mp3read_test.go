package mka_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The fixture, its recipe and its numbers are in mp3_test.go.
const (
	mp3Delay   = 1105
	mp3Samples = 44100
)

func openMP3(t *testing.T) (format.Media, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "mp3.mka"))
	if err != nil {
		t.Fatal(err)
	}
	med, err := format.Open(container.BytesSource(raw), "", &format.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	return med, raw
}

// TestMP3InMatroskaDecodes: the read is gapless, the CodecDelay's priming
// off the front and the DiscardPadding off the end, so the signal's 44100
// samples arrive. ffmpeg keeps the priming and drops the padding, so its
// sample i+1105 is ours i, held to the MP3 gate (RMS 1e-4, peak 1e-3).
func TestMP3InMatroskaDecodes(t *testing.T) {
	med, _ := openMP3(t)
	defer med.Close()
	_, got := readAll(t, med)
	if len(got) != mp3Samples {
		t.Fatalf("decoded %d samples, want %d", len(got), mp3Samples)
	}

	testutil.FFmpeg(t)
	want := testutil.FFmpegDecodeF32(t, filepath.Join("testdata", "mp3.mka"))
	if len(want) != mp3Samples+mp3Delay {
		t.Fatalf("ffmpeg decoded %d samples, want %d with the priming kept", len(want), mp3Samples+mp3Delay)
	}
	d := testutil.CompareF32(got, want[mp3Delay:])
	if d.RMS > 1e-4 || d.MaxAbs > 1e-3 {
		t.Errorf("decode differs from ffmpeg: %v", d)
	}
}

// TestMP3InMatroskaSeeks: a seek lands exactly and decodes what the linear
// read does from there, bit for bit. A landing has to back off far enough for
// the filterbank's history and for the bit reservoir the first frames after
// it reach into, which at 32 kbit/s three frames' worth of samples does not
// cover.
func TestMP3InMatroskaSeeks(t *testing.T) {
	med, _ := openMP3(t)
	_, linear := readAll(t, med)
	med.Close()
	for target := int64(1); target < mp3Samples; target += 997 {
		seeked, _ := openMP3(t)
		landed, err := seeked.SeekSample(context.Background(), target)
		if err != nil || landed != target {
			seeked.Close()
			t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
		}
		_, got := readAll(t, seeked)
		seeked.Close()
		want := linear[target:]
		if len(got) != len(want) {
			t.Fatalf("after a seek to %d: %d samples, want %d", target, len(got), len(want))
		}
		if d := testutil.CompareF32(got, want); d.MaxAbs != 0 {
			t.Errorf("after a seek to %d the decode differs from the linear one: %v", target, d)
		}
	}
}

// TestMP3SeeksPastAnotherTracksBlocks: in clusters shared with another track,
// only the MP3 track's own bytes refill the bit reservoir, so a seek still
// decodes what the linear read does, bit for bit.
func TestMP3SeeksPastAnotherTracksBlocks(t *testing.T) {
	raw := mka.TwoTrackMP3(t)
	open := func() format.Media {
		med, err := format.Open(container.BytesSource(raw), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return med
	}
	med := open()
	_, linear := readAll(t, med)
	med.Close()
	for target := int64(1); target < int64(len(linear)); target += 997 {
		seeked := open()
		landed, err := seeked.SeekSample(context.Background(), target)
		if err != nil || landed != target {
			seeked.Close()
			t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
		}
		_, got := readAll(t, seeked)
		seeked.Close()
		if d := testutil.CompareF32(got, linear[target:]); len(got) != len(linear)-int(target) || d.MaxAbs != 0 {
			t.Errorf("after a seek to %d the decode differs from the linear one: %v", target, d)
		}
	}
}
