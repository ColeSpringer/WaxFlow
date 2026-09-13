package oracletest

// MP3 inside a WAV or an AIFF-C is a track two readers have to agree about,
// and until this stream WaxFlow refused both by name while waxlabel 1.8.0
// described them: a library scanned with one tool listed tracks the other
// would not open. Both now report MP3 for the same bytes, for the WAV format
// tag and for the AIFF-C compression type alike.
//
// The AIFF-C is built here rather than committed because nothing writes one.
// ffmpeg's aiff muxer refuses MP3 outright and its demuxer refuses the
// result, so waxlabel is the only second reader this file has, which is
// exactly why the cell is worth having.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

func TestWaxlabelAgreesMP3InWAVAndAIFC(t *testing.T) {
	wav, err := os.ReadFile(filepath.Join("..", "container", "riff", "testdata", "mp3.wav"))
	if err != nil {
		t.Fatal(err)
	}
	frames := testutil.WAVDataChunk(t, wav)
	for _, tc := range []struct {
		name    string
		raw     []byte
		hint    string
		profile string
	}{
		{"mp3.wav", wav, "wav", ""},
		// waxlabel reports an AIFF-C compression type as the bare fourcc and
		// keeps the spelling as the profile, the way it does for a .mov.
		{"mp3.aifc", testutil.AIFCFrames(".mp3", 22050, 1, 0, 41, frames), "aiff", ".mp3"},
		// QuickTime's escape hatch, which waxlabel names through the same
		// WAVE format tag table the WAV above is read with, so both spellings
		// canonicalize to one codec with no profile to keep.
		{"mp3-ms.aifc", testutil.AIFCFrames("ms\x00\x55", 22050, 1, 0, 41, frames), "aiff", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := waxflow.New().Probe(container.BytesSource(tc.raw), tc.hint, nil)
			if err != nil {
				t.Fatalf("waxflow.Probe: %v", err)
			}
			if got := info.Default().Codec; got != codec.MP3 {
				t.Errorf("waxflow codec = %q, want %q", got, codec.MP3)
			}
			track := waxlabelTrack(t, tc.raw)
			if track.Codec != "MP3" || track.CodecProfile != tc.profile {
				t.Errorf("waxlabel Codec = %q profile %q, want MP3 profile %q",
					track.Codec, track.CodecProfile, tc.profile)
			}
			// The parameters the two read from different places: waxlabel
			// takes the header chunk, WaxFlow builds the decoder's output
			// format from the first frame. They must describe one stream,
			// which for these files is also the check that the header and
			// the frames agree.
			if track.SampleRate != info.Default().Fmt.Rate || track.Channels != info.Default().Fmt.Channels {
				t.Errorf("waxlabel says %d Hz %dch, waxflow says %d Hz %dch",
					track.SampleRate, track.Channels, info.Default().Fmt.Rate, info.Default().Fmt.Channels)
			}
		})
	}
}
