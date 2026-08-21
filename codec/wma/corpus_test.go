//go:build !wmatablesgen

package wma_test

// The WMA oracle corpus, generated rather than committed. The cell table is
// transcribed from docs/notes/wma-oracle-corpus.md section 3, which is its
// specification: every derived column there was computed in the analysis pass,
// so the tests below check the decoder against numbers that were fixed before
// it existed rather than against what it happens to compute.
//
// What the corpus can and cannot reach is the load-bearing part.
// `flags2` is 1 in every file either ffmpeg encoder writes, so these cells
// exercise VLC-coded exponents, one block size per frame, and no bit
// reservoir. LSP-coded exponents, the reservoir, variable block lengths and
// the three tabulated v2 exponent-band tables are unreachable from any
// ffmpeg-produced file; so are four arms of the high-frequency ladder, which
// sit below the encoder's absolute 24 kbit/s floor. Those paths are covered by
// the structural tests in this package and by TestRealWorldFixture when a
// non-ffmpeg file is available, and by nothing else.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/internal/testutil"
)

// cell is one corpus configuration and everything the notes derive from it.
// The derived columns are asserted, not recomputed: they are the analysis
// pass's answers, and a decoder that disagrees with them is wrong even if it
// is self-consistent.
type cell struct {
	v2         bool
	rate       int
	channels   int
	kbps       int
	frameLen   int
	blockAlign int
	noise      bool
	highFreq   float64
	coefPair   int
	what       string
	// nearMono makes the two channels almost the same signal instead of the
	// decorrelated pair every other stereo cell uses. It is the only way to
	// reach the one-sided mid/side cases: with a side channel worth coding the
	// encoder codes both channels in every block, and the near-mono material
	// is exactly where a decoder that clears an uncoded channel silences one
	// side audibly. It is exempt from the max|L-R| check for the same reason.
	nearMono bool
}

func (c cell) name() string {
	v := 1
	if c.v2 {
		v = 2
	}
	suffix := ""
	if c.nearMono {
		suffix = "-nearmono"
	}
	return fmt.Sprintf("v%d-%d-%dch-%dk%s", v, c.rate, c.channels, c.kbps, suffix)
}

var corpusCells = []cell{
	{false, 8000, 1, 24, 512, 192, false, 1.00, 2, "shortest frame, v1", false},
	{false, 16000, 2, 24, 512, 96, true, 0.50, 2, "short frame, stereo, v1", false},
	{false, 22050, 1, 32, 1024, 185, false, 1.00, 2, "mid frame, v1", false},
	{false, 32000, 1, 32, 1024, 128, true, 0.75, 1, "v1 default rate arm, high bps", false},
	{false, 32000, 2, 32, 1024, 128, true, 0.50, 1, "v1 at 32 kHz frames 1024 where v2 frames 2048", false},
	{false, 44100, 1, 32, 2048, 185, false, 1.00, 1, "coef pair 1", false},
	{false, 44100, 2, 64, 2048, 371, false, 1.00, 2, "ordinary v1 stereo", false},
	{false, 48000, 2, 24, 2048, 128, true, 0.50, 0, "coef pair 0, v1 default rate arm", false},
	{false, 48000, 2, 64, 2048, 341, true, 0.60, 1, "v1 default rate arm, mid bps", false},
	{true, 8000, 1, 24, 512, 192, false, 1.00, 2, "shortest frame, v2", false},
	{true, 11025, 1, 24, 512, 139, true, 0.70, 2, "the 11025 arm", false},
	{true, 16000, 1, 24, 512, 96, true, 0.50, 2, "the 16000 arm", false},
	{true, 22050, 2, 24, 1024, 139, true, 0.70, 2, "mid frame, stereo, noise on", false},
	{true, 32000, 2, 32, 2048, 256, true, 0.70, 1, "v2 at 32 kHz normalises to the 22050 arm", false},
	{true, 44100, 1, 32, 2048, 185, false, 1.00, 1, "coef pair 1, noise off", false},
	{true, 44100, 2, 24, 2048, 139, true, 0.40, 0, "coef pair 0, noise on", false},
	{true, 44100, 2, 128, 2048, 743, false, 1.00, 2, "ordinary v2 stereo", false},
	{true, 48000, 2, 128, 2048, 682, false, 1.00, 2, "highest rate", false},
	// The mid/side one-sided cells, and the only two rows here that are NOT in
	// the notes' table. Everything above codes both channels in every block,
	// so the two asymmetric cases -- only the mid coded, and only the side
	// coded -- appear in none of them; these were added while writing the
	// decoder, once that gap was measured.
	//
	// Their derived columns therefore do not carry the pre-registration the
	// eighteen above do: those were computed in the analysis pass and are in
	// git before any decoder existed (commit "Analyse the WMA bitstream"), so
	// a decoder that disagrees with them is wrong even if it is
	// self-consistent. These two are only self-consistent, which is why they
	// are named here rather than left to look like the others.
	{true, 44100, 2, 64, 2048, 371, false, 1.00, 2, "near-mono stereo, v2", true},
	{false, 44100, 2, 64, 2048, 371, false, 1.00, 2, "near-mono stereo, v1", true},
}

// corpusPreRegistered is how many leading cells come from
// docs/notes/wma-oracle-corpus.md section 3, which is the table this one is
// transcribed from.
const corpusPreRegistered = 18

// corpusSeconds is how long each cell runs. Two seconds is several hundred
// frames at every frame length, which is enough for the exponent curve and the
// noise index to be well past their opening transient.
const corpusSeconds = 2.0

// source is the signal every cell encodes: a chirp for broadband coverage
// (without it the high bands are empty and noise coding is never exercised)
// plus deterministic noise for a tonal component to make the exponent curve
// peaky.
func source(rate int) string {
	return fmt.Sprintf("aevalsrc='0.28*sin(2*PI*(200+1200*t)*t)+0.22*(2*random(1)-1)':s=%d:d=%g",
		rate, corpusSeconds)
}

// stereoFilter decorrelates one broadband source against itself. `-ac 2` on a
// mono source duplicates the channel, which makes the side channel identically
// zero and every mid/side claim vacuous; aevalsrc does not rescue it either,
// since random(1) and random(2) index evaluator state rather than seeds and
// return the same sequence. TestCorpusStereoCellsAreStereo checks the result
// per cell rather than trusting this graph.
const stereoFilter = "asplit=2[l][r];[r]adelay=1S:all=1,volume=0.85[rd];[l][rd]amerge=inputs=2"

// nearMonoFilter makes a stereo pair whose side channel is tiny but not zero,
// which is what provokes the encoder into coding one channel of a mid/side
// block and leaving the other out.
const nearMonoFilter = "asplit=2[l][r];[r]volume=0.995[rd];[l][rd]amerge=inputs=2"

// corpusDir and msCorpusDir are the generated corpora's temp directories,
// removed by TestMain. The package-level sync.OnceValues shape rules out
// t.TempDir -- the corpus outlives any one test -- so the cleanup is owned by
// the run instead. Without it every `go test` left a few megabytes behind for
// good, and this package is the only codec here that does not use t.TempDir.
var corpusDir, msCorpusDir string

// TestMain removes the generated corpora after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	for _, d := range []string{corpusDir, msCorpusDir} {
		if d != "" {
			os.RemoveAll(d)
		}
	}
	os.Exit(code)
}

var corpus = sync.OnceValues(func() (map[string]string, error) {
	dir, err := os.MkdirTemp("", "waxflow-wma")
	if err != nil {
		return nil, err
	}
	corpusDir = dir
	out := make(map[string]string, len(corpusCells))
	for _, c := range corpusCells {
		path := filepath.Join(dir, c.name()+".wma")
		codec := "wmav2"
		if !c.v2 {
			codec = "wmav1"
		}
		args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source(c.rate)}
		switch {
		case c.nearMono:
			args = append(args, "-filter_complex", nearMonoFilter)
		case c.channels == 2:
			args = append(args, "-filter_complex", stereoFilter)
		}
		args = append(args, "-c:a", codec, "-b:a", strconv.Itoa(c.kbps)+"k", path)
		if b, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg %v: %v; %s", args, err, b)
		}
		out[c.name()] = path
	}
	return out, nil
})

var ffmpegPath string

// corpusFile builds the corpus once for the package and returns one cell's
// path. Twenty cells read by several tests each would otherwise be a hundred
// ffmpeg spawns for eighteen distinct results.
func corpusFile(t testing.TB, c cell) string {
	t.Helper()
	ffmpegPath = testutil.FFmpeg(t)
	files, err := corpus()
	if err != nil {
		t.Fatalf("building the corpus: %v", err)
	}
	return files[c.name()]
}

// TestCorpusMatchesItsSpecification guards the split between the cells whose
// derived columns were fixed before this decoder existed and the ones added
// afterwards. A new cell appended without a note lands past the boundary and
// fails here, which is the only thing keeping "asserted, not recomputed" true
// as the table grows.
func TestCorpusMatchesItsSpecification(t *testing.T) {
	if len(corpusCells) < corpusPreRegistered {
		t.Fatalf("%d cells, fewer than the %d in the notes", len(corpusCells), corpusPreRegistered)
	}
	for i, c := range corpusCells[:corpusPreRegistered] {
		if c.nearMono {
			t.Errorf("cell %d (%s) is near-mono; the notes' table has no such row", i, c.name())
		}
	}
	for i, c := range corpusCells[corpusPreRegistered:] {
		if !c.nearMono {
			t.Errorf("cell %d (%s) sits past the notes' table without being one of the two named additions",
				corpusPreRegistered+i, c.name())
		}
	}
}
