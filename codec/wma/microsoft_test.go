//go:build !wmatablesgen

package wma_test

// The second-encoder differential, and the one that makes this decoder's gate
// mean "we decode WMA" rather than "we agree with FFmpeg".
//
// FFmpeg is a narrow oracle for this format, measured rather than suspected:
// its encoder emits a FLAT exponent curve, never noise-fills a band, always
// codes both channels, never sets a flags2 bit past the first, and refuses bit
// rates below 24 kbit/s. Windows' own encoder, reached through Media
// Foundation, does the opposite on nearly every count, so between them the two
// cover the format. Everything below was unreachable before this file existed:
//
//	bit reservoir              every cell
//	variable block lengths     every cell, two to five block sizes each
//	tabulated v2 band rows     six to eight blocks per cell at 22.05 kHz and up
//	non-flat exponent curves   every VLC exponent decode
//	LSP-coded exponents        the 8 and 16 kHz cells (flags2 bit 0 clear)
//	noise-filled bands         283 of 604 high bands at 16 kHz
//	exponent reuse             two to fifty-six blocks per cell
//	one-sided mid/side         most blocks of every stereo cell
//	blocks with nothing coded  two or three per cell
//
// Windows only. The cells are generated, never committed: the source audio is
// synthesized here, so nothing about them is anyone else's to license.

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/internal/testutil"
)

// msCell is one configuration for Windows' encoder. The bit rate is a request:
// the encoder picks its own nearby, and at 8 and 16 kHz ignores it entirely,
// which is how those cells end up under FFmpeg's 24 kbit/s floor and in the
// LSP-exponent regime.
type msCell struct {
	rate     int
	channels int
	bitRate  int
	// wants is what this cell exists to reach, asserted so a Windows update
	// that changed the encoder's choices could not quietly empty the file.
	wantFlags2 uint16
	what       string
	// nearMono asks for the almost-but-not-quite-identical pair that provokes
	// one-sided mid/side, the way corpusCells does. Every other stereo cell
	// gets the decorrelated pair. Neither may be `-ac 2` on a mono source:
	// that duplicates the channel, and a side channel that is identically zero
	// makes every mid/side claim in this file vacuous while leaving it green.
	nearMono bool
}

var msCells = []msCell{
	{8000, 1, 16000, 0x0026, "LSP-coded exponents, shortest frame", false},
	{16000, 1, 20000, 0x002e, "LSP-coded exponents, noise-filled bands", false},
	{22050, 1, 32000, 0x0017, "noise fill with variable block lengths", false},
	{22050, 2, 32000, 0x0017, "stereo, one-sided mid/side", true},
	{32000, 2, 48000, 0x001f, "the deepest block-size ladder", false},
	{44100, 1, 32000, 0x000f, "mono, tabulated band rows", false},
	{44100, 2, 64000, 0x000f, "ordinary stereo", false},
	{48000, 2, 96000, 0x000f, "highest rate", false},
}

func (c msCell) name() string {
	return fmt.Sprintf("ms-%d-%dch-%dk", c.rate, c.channels, c.bitRate/1000)
}

// msDeficits carries the cells this decoder does NOT match, with the bound it
// actually meets and why. Both causes are measured, not suspected, and both
// sit on the ORACLE's side of the comparison:
//
//   - The LSP cells miss the gate by two to three times because the LSP
//     path's per-bin float rounding order differs between implementations.
//     Measured on noise-off LSP streams through the WMA-in-WAV wrap
//     (TestLSPDifferential): ffmpeg's curve agrees with the exact
//     (p+q)^(-1/4) to ~6e-7 relative with NO structure in p+q, so there is no
//     coarser reference table to match, and the whole-stream relative
//     deviation there (~9e-7) is the same scale as these cells' misses.
//     -112 dBFS; per-ulp disagreement, not a defect on either side.
//
//   - ms-22050-1ch: one block reuses an exponent curve decoded at 128
//     samples in a 256-sample block, and ffmpeg reads the resampled curve
//     ONE SOURCE BIN BEHIND inside the noise regions (the per-bin fill and
//     the band powers behind the gains), while this decoder indexes the
//     reused curve the same way everywhere, as the notes describe. Pinned by
//     spectral recovery on the corpus cell (the shifted step reproduces both
//     the 0.95494 band gain and the 224/225 bin values exactly) and
//     reproduced on a deterministic stream in TestNoiseReuseCharacterization,
//     which detects ffmpeg changing its mind. Twelve noise-off reuse probes
//     matched exactly at every block-size ratio, so the divergence is
//     confined to the noise path. Windows' own decoder could not arbitrate:
//     its noise system differs too much to resolve a 2 percent question.
//
// The bounds are ceilings AND floors: a cell that beats its entry by more than
// 4x fails, so a fix cannot land without deleting the entry it fixed.
var msDeficits = map[string]struct {
	rms, max float64
	why      string
}{
	"ms-8000-1ch-16k":  {5e-7, 3e-6, "LSP-path float rounding order; the reference's curve itself matches exact pow (measured)"},
	"ms-16000-1ch-20k": {6e-7, 5e-6, "LSP-path float rounding order; the reference's curve itself matches exact pow (measured)"},
	"ms-22050-1ch-32k": {5e-4, 2e-2, "ffmpeg reads a reused exponent curve one source bin behind in noise bands; see TestNoiseReuseCharacterization"},
}

var msCorpus = sync.OnceValues(func() (map[string]string, error) {
	dir, err := os.MkdirTemp("", "waxflow-wma-ms")
	if err != nil {
		return nil, err
	}
	msCorpusDir = dir
	out := make(map[string]string, len(msCells))
	for _, c := range msCells {
		wav := filepath.Join(dir, c.name()+".wav")
		args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source(c.rate)}
		switch {
		case c.nearMono:
			args = append(args, "-filter_complex", nearMonoFilter)
		case c.channels == 2:
			args = append(args, "-filter_complex", stereoFilter)
		}
		args = append(args, "-ac", strconv.Itoa(c.channels), "-c:a", "pcm_s16le", wav)
		if b, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg %v: %v; %s", args, err, b)
		}
		out[c.name()] = filepath.Join(dir, c.name()+".wma")
	}
	return out, nil
})

// TestMicrosoftStereoCellsAreStereo is TestCorpusStereoCellsAreStereo for the
// second encoder's cells, which had the trap it exists to catch: `-ac 2` on a
// mono source, so max|L-R| was exactly 0 on all four stereo cells and the one
// labelled "one-sided mid/side" reached that case only degenerately.
func TestMicrosoftStereoCellsAreStereo(t *testing.T) {
	if !testutil.HaveWMFEnc(t) {
		t.Skip("Windows' Media Foundation WMA encoder is unavailable")
	}
	ffmpegPath = testutil.FFmpeg(t)
	paths, err := msCorpus()
	if err != nil {
		t.Fatalf("building the corpus: %v", err)
	}
	for _, c := range msCells {
		if c.channels != 2 {
			continue
		}
		t.Run(c.name(), func(t *testing.T) {
			wav := paths[c.name()]
			wav = wav[:len(wav)-4] + ".wav"
			pcm := testutil.FFmpegDecodeF32NoSIMD(t, wav)
			var worst float64
			for i := 0; i+1 < len(pcm); i += 2 {
				worst = max(worst, math.Abs(float64(pcm[i])-float64(pcm[i+1])))
			}
			// The near-mono pair is deliberately close, but it must not be
			// the same signal twice.
			floor := 0.1
			if c.nearMono {
				floor = 1e-4
			}
			if worst < floor {
				t.Fatalf("max|L-R| = %g: this is not a stereo fixture", worst)
			}
		})
	}
}

// TestMicrosoftEncoderDifferential decodes what Windows' own encoder writes and
// scores it against ffmpeg's decode of the same bytes. Both halves matter: the
// encoder is not ffmpeg's, so the bitstream reaches paths ffmpeg cannot write,
// and the decoder is ffmpeg's, so the comparison is still against an
// independent implementation.
func TestMicrosoftEncoderDifferential(t *testing.T) {
	if !testutil.HaveWMFEnc(t) {
		t.Skip("Windows' Media Foundation WMA encoder is unavailable; " +
			"see the WMA row in docs/quality-gates.md for what this covers that ffmpeg cannot")
	}
	ffmpegPath = testutil.FFmpeg(t)
	paths, err := msCorpus()
	if err != nil {
		t.Fatalf("building the corpus: %v", err)
	}
	for _, c := range msCells {
		t.Run(c.name(), func(t *testing.T) {
			path := paths[c.name()]
			wav := path[:len(path)-4] + ".wav"
			testutil.WMFEncode(t, wav, path, c.rate, c.channels, c.bitRate)

			track, pkts := demux(t, path)
			cfg, err := wma.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			if cfg.Flags2 != c.wantFlags2 {
				t.Errorf("flags2 = %#04x, want %#04x (%s): the encoder's choices moved and this cell may no longer reach what it is for",
					cfg.Flags2, c.wantFlags2, c.what)
			}
			got := decodeAll(t, track, pkts)
			want := testutil.FFmpegDecodeF32NoSIMD(t, path)
			lead := cfg.FrameLen() * cfg.Channels
			if len(got) < lead {
				t.Fatalf("decoded %d samples, fewer than the %d-sample head lead", len(got), lead)
			}
			got = got[lead:]
			n := min(len(got), len(want))
			if n == 0 {
				t.Fatal("nothing to compare")
			}
			d := testutil.CompareF32(got[:n], want[:n])

			rms, max_, why := gateRMS, gateMax, ""
			if df, ok := msDeficits[c.name()]; ok {
				rms, max_, why = df.rms, df.max, df.why
			}
			if d.RMS > rms || d.MaxAbs > max_ {
				t.Errorf("differential %v, want rms <= %g max <= %g%s", d, rms, max_, note(why))
			}
			// Staleness teeth: an allowed cell that has come good must lose
			// its entry, or the next reader inherits a deficit that is no
			// longer real.
			if why != "" && d.RMS < rms/4 && d.MaxAbs < max_/4 {
				t.Errorf("cell now measures %v, four times inside its recorded deficit (%s); delete the msDeficits entry",
					d, why)
			}
		})
	}
}

func note(why string) string {
	if why == "" {
		return ""
	}
	return " (" + why + ")"
}
