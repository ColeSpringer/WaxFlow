package musepack_test

import (
	"path/filepath"

	"github.com/colespringer/waxflow/internal/testutil"
)

// The committed .mpc fixtures and the signals they hold. Each one was produced
// by a reference encoder from exactly the samples its signal function
// rebuilds, so a decode can be checked against the source on a machine with no
// encoder, no ffmpeg, and no network. See fixturegen_test.go for how to
// regenerate them.
type mpcFixture struct {
	path     string
	sv7      bool
	args     []string
	rate     int
	channels int // the source's; SV7 streams are always two channels
	frames   int
	signal   string // sine, noise
	tags     []string
	id3v1    bool   // an ID3v1 tag appended after encoding
	repackOf string // the SV7 fixture this is the lossless mpc2sv8 repack of
	cutOf    string // the SV8 fixture this is an mpccut cut of
	cutFrom  int64
	// chaptersOf is the SV8 fixture this is a copy of, and chapters the
	// mpcchap .ini chapter file written into it (or into a fresh encode).
	chaptersOf string
	chapters   string
	// seed built the cell's source noise and is frozen: the committed stream
	// encodes exactly that noise. The values began as the length of the
	// fixture's absolute path, so they look arbitrary; deriving one from the
	// checkout again would rebuild a different signal on a different machine.
	seed uint64
	// minCorr is the tool-free gate for a noise cell: the correlation floor
	// its decode holds against the source, set from the measurement with
	// headroom; the value measured on 2026-09-01 stands beside each floor. A
	// perceptual codec keeps little of white noise at low bitrates and noise
	// substitution replaces bands with different noise, so the floor is per
	// cell; a decoder that broke would fall toward zero.
	minCorr float64
}

// samples rebuilds the fixture's source, interleaved 16-bit, below -3 dBFS so
// no encoder ever clipped it.
func (f mpcFixture) samples() []int32 {
	fm := intFormat(f.rate, f.channels)
	switch f.signal {
	case "noise":
		return scaled(interleave(testutil.Noise(fm, f.frames, f.seed)), 0.5)
	default:
		return interleave(testutil.Sine(fm, f.frames, 440, 0.7))
	}
}

func codecFixture(name string) string { return repoPath("codec", "musepack", "testdata", name) }
func containerFixture(name string) string {
	return repoPath("container", "mpc", "testdata", name)
}

// mpcFixtures is the committed matrix: both stream versions, every rate, mono
// and stereo, the profiles that reach each quantiser path, noise substitution,
// mid/side off, both block shapes, a stream with no seek table and one with no
// encoder info, source lengths in all three SV7 tail regimes (N mod 1152 at
// most 671, above 671, exactly 0), a cut with a nonzero beginning silence, the
// lossless repacks that pin the two readers against each other, and a chapter
// run the reference chapter editor wrote.
var mpcFixtures = []mpcFixture{
	{path: codecFixture("sv7-standard.mpc"), sv7: true, rate: 44100, channels: 2, frames: 12000},
	{path: codecFixture("sv7-thumb.mpc"), sv7: true, args: []string{"--thumb"}, rate: 44100, channels: 2, frames: 23740, signal: "noise", seed: 70, minCorr: 0.4},         // measured 0.47
	{path: codecFixture("sv7-braindead.mpc"), sv7: true, args: []string{"--braindead"}, rate: 44100, channels: 2, frames: 12520, signal: "noise", seed: 74, minCorr: 0.9}, // measured 0.98
	{path: codecFixture("sv7-32k.mpc"), sv7: true, rate: 32000, channels: 2, frames: 11520},
	{path: codecFixture("sv7-37800.mpc"), sv7: true, rate: 37800, channels: 2, frames: 20000, signal: "noise", seed: 70, minCorr: 0.85}, // measured 0.95
	{path: codecFixture("sv7-48k.mpc"), sv7: true, rate: 48000, channels: 2, frames: 30000},
	{path: codecFixture("sv7-mono.mpc"), sv7: true, rate: 44100, channels: 1, frames: 23740},
	{path: codecFixture("sv7-pns.mpc"), sv7: true, args: []string{"--pns", "0.25"}, rate: 44100, channels: 2, frames: 23740, signal: "noise", seed: 68, minCorr: 0.65}, // measured 0.77
	{path: codecFixture("sv7-ms-off.mpc"), sv7: true, args: []string{"--ms", "0"}, rate: 44100, channels: 2, frames: 12520, signal: "noise", seed: 71, minCorr: 0.8},   // measured 0.90
	{path: codecFixture("sv8-stereo.mpc"), rate: 44100, channels: 2, frames: 20000},
	{path: codecFixture("sv8-mono.mpc"), rate: 44100, channels: 1, frames: 20000, signal: "noise", seed: 69, minCorr: 0.8}, // measured 0.90
	{path: codecFixture("sv8-32k.mpc"), rate: 32000, channels: 2, frames: 12000, signal: "noise", seed: 68, minCorr: 0.85}, // measured 0.95
	{path: codecFixture("sv8-37800.mpc"), rate: 37800, channels: 2, frames: 12520},
	{path: codecFixture("sv8-48k.mpc"), rate: 48000, channels: 2, frames: 23740, signal: "noise", seed: 68, minCorr: 0.75},                                         // measured 0.87
	{path: codecFixture("sv8-frames0.mpc"), args: []string{"--num_frames", "0"}, rate: 44100, channels: 2, frames: 20000, signal: "noise", seed: 72, minCorr: 0.8}, // measured 0.90
	{path: codecFixture("sv8-no-st.mpc"), args: []string{"--no_st"}, rate: 44100, channels: 2, frames: 20000},
	{path: codecFixture("sv8-q0-pns.mpc"), args: []string{"--quality", "0"}, rate: 44100, channels: 2, frames: 30000, signal: "noise", seed: 71, minCorr: 0.15}, // measured 0.21
	{path: codecFixture("sv8-q10.mpc"), args: []string{"--quality", "10"}, rate: 44100, channels: 2, frames: 20000, signal: "noise", seed: 68, minCorr: 0.95},   // measured 0.99
	{path: codecFixture("sv8-no-ei.mpc"), args: []string{"--no_ei"}, rate: 44100, channels: 2, frames: 20000, signal: "noise", seed: 70, minCorr: 0.8},          // measured 0.89
	{path: codecFixture("sv8-thumb.mpc"), args: []string{"--thumb"}, rate: 44100, channels: 2, frames: 30000, signal: "noise", seed: 70, minCorr: 0.4},          // measured 0.49
	{path: codecFixture("sv8-blocks.mpc"), args: []string{"--num_frames", "1"}, rate: 44100, channels: 2, frames: 12000},
	{path: codecFixture("sv8-cut.mpc"), rate: 44100, channels: 2, frames: 12000, cutOf: codecFixture("sv8-blocks.mpc"), cutFrom: 5000},
	{path: codecFixture("sv8-repack.mpc"), rate: 44100, channels: 2, frames: 12000, repackOf: codecFixture("sv7-standard.mpc")},
	{path: codecFixture("sv8-repack-pns.mpc"), rate: 44100, channels: 2, frames: 23740, signal: "noise", repackOf: codecFixture("sv7-pns.mpc"), minCorr: 0.65}, // measured 0.77
	{path: containerFixture("tagged.mpc"), sv7: true, rate: 44100, channels: 2, frames: 8000,
		tags: []string{"Artist=Wax Test", "Album=Fixtures", "Title=Tagged", "Year=2026", "Track=3"}, id3v1: true},
	{path: containerFixture("seek.mpc"), args: []string{"--num_frames", "1"}, rate: 44100, channels: 2, frames: 30000, signal: "noise", seed: 64, minCorr: 0.8},                        // measured 0.89
	{path: containerFixture("seek-pns.mpc"), args: []string{"--quality", "0", "--num_frames", "1"}, rate: 44100, channels: 2, frames: 30000, signal: "noise", seed: 68, minCorr: 0.15}, // measured 0.21
	{path: containerFixture("seek-sv7.mpc"), sv7: true, args: []string{"--thumb", "--pns", "0.25"}, rate: 44100, channels: 2, frames: 92160, signal: "noise", seed: 68, minCorr: 0.45}, // measured 0.56
	{path: containerFixture("gapless-sv7.mpc"), sv7: true, rate: 44100, channels: 2, frames: 23740},
	{path: containerFixture("chapters.mpc"), rate: 44100, channels: 2, frames: 20000, chaptersOf: codecFixture("sv8-stereo.mpc"), chapters: chapterINI},
	{path: repoPath("testdata", "sine-s16.mpc"), rate: 44100, channels: 2, frames: 22050},
	{path: repoPath("testdata", "noise-s16.mpc"), sv7: true, rate: 44100, channels: 2, frames: 22050, signal: "noise", seed: 55, minCorr: 0.8}, // measured 0.90
}

// chapterINI is the mpcchap chapter file behind chapters.mpc: a title alone,
// three items (the editor writes them in ascending value length, so the title
// lands last), an upper-case key, and a chapter with no items, which the
// editor writes with no tag bytes at all. The sections are out of start
// order, and the editor writes the run in section order, so the committed
// run is unsorted and the reader's start order is pinned on a real file.
const chapterINI = "[0]\nTitle=Intro\n\n[16000]\nTITLE=Coda\n\n[8000]\nTitle=Middle\nArtist=A\nTrack=2/3\n\n[19000]\ngain=0\npeak=0\n"

// fixtureByPath looks a fixture up by its base name.
func fixtureByPath(path string) (mpcFixture, bool) {
	for _, f := range mpcFixtures {
		if f.path == path {
			return f, true
		}
	}
	return mpcFixture{}, false
}

// source is the signal a fixture's decode should track: its own, or the
// origin's for a repack, or the origin's tail for a cut.
func (f mpcFixture) source() []int32 {
	switch {
	case f.repackOf != "":
		o, _ := fixtureByPath(f.repackOf)
		return o.samples()
	case f.cutOf != "":
		o, _ := fixtureByPath(f.cutOf)
		return o.samples()[int(f.cutFrom)*o.channels:]
	case f.chaptersOf != "":
		o, _ := fixtureByPath(f.chaptersOf)
		return o.samples()
	}
	return f.samples()
}

// streamChannels is the channel count the stream decodes to: SV7 has no mono
// form, so mppenc writes a mono source as two identical channels.
func (f mpcFixture) streamChannels() int {
	if f.sv7 {
		return 2
	}
	return f.channels
}

// expected lays the source out in the stream's channel count as float32 full
// scale, a mono source duplicated across a stereo decode.
func (f mpcFixture) expected(src []int32) []float32 {
	sc, ch := f.channels, f.streamChannels()
	frames := len(src) / sc
	out := make([]float32, frames*ch)
	for i := range frames {
		for c := range ch {
			out[i*ch+c] = float32(src[i*sc+min(c, sc-1)]) / 32768
		}
	}
	return out
}

// name is the fixture's base name.
func (f mpcFixture) name() string { return filepath.Base(f.path) }
