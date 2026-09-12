package oracletest

// Uncompressed audio in an MP4 is a track two readers have to agree about,
// and the agreement is exact rather than approximate: there is no codec here
// to disagree over, only a width, a rate, a channel count and a byte order,
// all of them stated in boxes both readers parse.
//
// The gap this closes was ours. waxlabel 1.8.0 describes every one of these
// entries; WaxFlow refused them all, so a library scanned with one tool listed
// tracks the other would not open. The 96 kHz ipcm cell is the interesting
// one: its 16.16 rate field is zero and it carries no srat box, so both
// readers have to fall back to the media timescale, and a reader that took the
// field at face value would report 0 Hz.
//
// The fixtures are container/mp4's, whose provenance comment holds the command
// lines.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

func TestWaxlabelAgreesPCMInMP4(t *testing.T) {
	// CodecProfile carries the sample entry's own fourcc, which is the one
	// thing about these tracks that WaxFlow does not surface: it maps every
	// spelling onto one codec ID and puts the storage in the codec config.
	// Pinned anyway, so a fixture that stops being the entry it was generated
	// as fails here rather than silently testing the wrong path.
	for _, tc := range []struct {
		name    string
		codec   string
		profile string
	}{
		{"pcm-in24.mov", "PCM", "in24"},
		{"pcm-in24-chunks.mov", "PCM", "in24"},
		{"pcm-in24le.mov", "PCM", "in24"},
		{"pcm-sowt.mov", "PCM", "sowt"},
		{"pcm-twos.mov", "PCM", "twos"},
		{"pcm-in32.mov", "PCM", "in32"},
		{"pcm-raw.mov", "PCM", "raw "},
		{"pcm-fl32.mov", "IEEE float", "fl32"},
		{"pcm-fl64.mov", "IEEE float64", "fl64"},
		{"pcm-lpcm.mov", "PCM", "lpcm"},
		{"pcm-ipcm.mp4", "PCM", "ipcm"},
		{"pcm-ipcm-96k.mp4", "PCM", "ipcm"},
		{"pcm-fpcm.mp4", "IEEE float", "fpcm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "container", "mp4", "testdata", tc.name))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("waxflow.Probe: %v", err)
			}
			ours := info.Default()
			if ours.Codec != codec.PCM {
				t.Errorf("waxflow codec = %q, want %q", ours.Codec, codec.PCM)
			}
			track := waxlabelTrack(t, raw)
			if track.Codec != tc.codec || track.CodecProfile != tc.profile {
				t.Errorf("waxlabel Codec = %q profile %q, want %q profile %q",
					track.Codec, track.CodecProfile, tc.codec, tc.profile)
			}
			if track.SampleRate != ours.Fmt.Rate || track.Channels != ours.Fmt.Channels {
				t.Errorf("waxlabel says %d Hz %dch, waxflow says %d Hz %dch",
					track.SampleRate, track.Channels, ours.Fmt.Rate, ours.Fmt.Channels)
			}
			// The source's depth, which for a 64-bit float track is not the
			// pipeline's: audio.Format carries floats as float32, so the file's
			// own depth rides in SourceBitDepth and that is the number
			// waxlabel's read of the entry has to match.
			depth := ours.Fmt.BitDepth
			if ours.SourceBitDepth != 0 {
				depth = ours.SourceBitDepth
			}
			if track.BitsPerSample != depth {
				t.Errorf("waxlabel says %d bits, waxflow says %d", track.BitsPerSample, depth)
			}
			// Length: waxlabel reads the media header, WaxFlow counts the
			// samples the table describes. For uncompressed audio those are the
			// same number, so the two agree exactly rather than within a tick.
			ours2 := time.Duration(ours.Samples) * time.Second / time.Duration(ours.Fmt.Rate)
			if track.Duration != ours2 {
				t.Errorf("waxlabel says %v, waxflow says %v (%d samples at %d Hz)",
					track.Duration, ours2, ours.Samples, ours.Fmt.Rate)
			}
		})
	}
}
