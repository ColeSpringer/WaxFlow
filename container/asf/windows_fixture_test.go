package asf_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/internal/testutil"
)

// windowsFixture is the one WMA v1/v2 file here that Windows wrote. ffmpeg
// writes no bit reservoir, so every other v1/v2 fixture is one frame per media
// object; this one carries the reservoir in every packet, and its stamps sit
// two frames under where a resumed decode lands. testdata/MANIFEST.md has
// the recipe.
const windowsFixture = "ms-v2-44100-2ch-128k.wma"

// TestRegenerateWindowsFixture rewrites the fixture, on request only: the
// encoder writes a fresh File ID GUID every run, so a regeneration is a new
// file even where the audio is not.
func TestRegenerateWindowsFixture(t *testing.T) {
	if os.Getenv("WAXFLOW_REGEN_ASF_FIXTURE") != "1" {
		t.Skip("set WAXFLOW_REGEN_ASF_FIXTURE=1 to rewrite testdata/" + windowsFixture)
	}
	if !testutil.HaveWMFEnc(t) {
		t.Fatal("Windows' Media Foundation WMA encoder is unavailable")
	}
	const rate, channels = 44100, 2
	wav := filepath.Join(t.TempDir(), "source.wav")
	testutil.WriteWAV(t, wav, audio.Format{Rate: rate, Channels: channels, Type: audio.Int, BitDepth: 16},
		chirpNoise(rate, channels, 6*rate))
	testutil.WMFEncode(t, wav, filepath.Join("testdata", windowsFixture), rate, channels, 128000)
}

// chirpNoise is the WMA corpus source (docs/notes/wma-oracle-corpus.md section
// 2) as 16-bit samples: a chirp from 200 Hz rising 2400 Hz a second at 0.28,
// plus seeded uniform noise at 0.22. The right channel of a stereo pair is the
// left delayed a sample and scaled by 0.85, so the side channel is not zero.
func chirpNoise(rate, channels, frames int) []int32 {
	seed := uint64(0x5eed)
	left := make([]float64, frames)
	for i := range left {
		s := float64(i) / float64(rate)
		seed = seed*6364136223846793005 + 1442695040888963407
		noise := float64(seed>>11)/(1<<53)*2 - 1
		left[i] = 0.28*math.Sin(2*math.Pi*(200+1200*s)*s) + 0.22*noise
	}
	out := make([]int32, 0, frames*channels)
	for i, v := range left {
		out = append(out, int32(math.Round(v*32767)))
		if channels == 2 {
			var r float64
			if i > 0 {
				r = 0.85 * left[i-1]
			}
			out = append(out, int32(math.Round(r*32767)))
		}
	}
	return out
}
