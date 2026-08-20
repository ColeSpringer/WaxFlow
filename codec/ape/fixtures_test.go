package ape_test

import (
	"io"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The committed .ape fixtures and the signals they hold. Each one was produced
// by the reference encoder from exactly the samples its signal function
// rebuilds, so a decode can be checked against the source on a machine with no
// encoder, no ffmpeg, and no network. See fixturegen_test.go for how to
// regenerate them.
type apeFixture struct {
	path  string
	fmt   audio.Format
	level int
	tags  string
	// samples returns the interleaved source, at the format's own depth.
	samples func() []int32
}

func intFormat(rate, channels, depth int) audio.Format {
	return audio.Format{
		Rate:     rate,
		Channels: channels,
		Layout:   audio.DefaultLayout(channels),
		Type:     audio.Int,
		BitDepth: depth,
	}
}

// interleave flattens a planar buffer at its own depth (testutil.Interleave
// left-justifies to 32 bits for the ffmpeg comparison, which is not what a
// source-sample check wants) and returns the buffer to the pool.
func interleave(b *audio.Buffer) []int32 {
	defer audio.Put(b)
	ch := b.Fmt.Channels
	out := make([]int32, b.N*ch)
	for c := range ch {
		for i, v := range b.ChanI(c) {
			out[i*ch+c] = v
		}
	}
	return out
}

// repoPath resolves a path relative to the repository root.
func repoPath(rel ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, rel...)...)
}

var (
	fixtureStereo = intFormat(44100, 2, 16)
	fixtureMono   = intFormat(44100, 1, 16)
	fixtureDeep   = intFormat(48000, 2, 24)
	fixtureNarrow = intFormat(22050, 1, 8)
)

// cascadeFrames is short on purpose: the cascade fixtures exist to put every
// filter chain in the committed set, and the deepest one runs 1552 taps per
// sample.
const cascadeFrames = 6000

// cascadeFixture is one level's fixture, the same noise through a different
// filter chain each time.
func cascadeFixture(level int, f audio.Format, name string) apeFixture {
	return apeFixture{
		path:  repoPath("codec", "ape", "testdata", name),
		fmt:   f,
		level: level,
		samples: func() []int32 {
			return interleave(testutil.Noise(f, cascadeFrames, uint64(level)))
		},
	}
}

// seekFixtureFrames spans three frames at the default frame length, the last
// one short: the shape a seek test needs, since a seek lands on a frame
// boundary and a single-frame file has only the one.
const seekFixtureFrames = 2*73728 + 5000

// The cascade fixtures cover what the shared ones cannot: every compression
// level (which is to say every filter chain, from the fast level's none to the
// insane level's three), and the depths whose sample packing differs.
var apeFixtures = append([]apeFixture{
	cascadeFixture(1000, fixtureStereo, "noise-c1000-s16.ape"),
	cascadeFixture(2000, fixtureStereo, "noise-c2000-s16.ape"),
	cascadeFixture(3000, fixtureStereo, "noise-c3000-s16.ape"),
	cascadeFixture(4000, fixtureDeep, "noise-c4000-s24.ape"),
	cascadeFixture(5000, fixtureNarrow, "noise-c5000-s8.ape"),
}, sharedFixtures...)

// sharedFixtures live in the repository's own testdata because other packages
// read them: the engine-level tests, the seek matrix, and the benchmarks.
var sharedFixtures = []apeFixture{
	{
		path:  repoPath("testdata", "sine-s16.ape"),
		level: 2000,
		fmt:   fixtureStereo,
		samples: func() []int32 {
			return interleave(testutil.Sine(fixtureStereo, 22050, 440, 0.8))
		},
	},
	{
		path:  repoPath("testdata", "noise-s16.ape"),
		level: 2000,
		fmt:   fixtureStereo,
		samples: func() []int32 {
			return interleave(testutil.Noise(fixtureStereo, 22050, 7))
		},
	},
	{
		path:  repoPath("container", "apen", "testdata", "tagged.ape"),
		level: 2000,
		fmt:   fixtureStereo,
		tags:  "Artist=Wax Test|Album=Fixtures|Title=Tagged|Year=2026|Track=3",
		samples: func() []int32 {
			return interleave(testutil.Sine(fixtureStereo, 8000, 523, 0.6))
		},
	},
	{
		path:  repoPath("container", "apen", "testdata", "seek.ape"),
		level: 2000,
		fmt:   fixtureMono,
		samples: func() []int32 {
			return interleave(testutil.Ramp(fixtureMono, seekFixtureFrames))
		},
	},
}

// frames returns a fixture's decoder packets, from the demuxer that builds
// them in production. Restating the packetization here instead would be a
// second copy of a rule the tests exist to check, and it drifted once already:
// a hand-rolled version left off the read-ahead slack ReadPacket gives every
// frame, which is exactly what the range coder's end-of-data handling is tuned
// around. codec/wavpack's benchmarks reach for their container the same way.
func frames(t testing.TB, raw []byte) (ape.Config, [][]byte) {
	t.Helper()
	d, err := apen.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	track := d.Tracks()[0]
	cfg, err := ape.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return cfg, out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), pkt.Data...))
	}
}
