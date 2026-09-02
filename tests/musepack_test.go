package waxflow_test

// Musepack end to end: probe, decode, transcode and seek .mpc streams through
// the public API, which is what registering the driver and decoder rows buys.
//
// The codec's own differentials live in codec/musepack and the framing tests
// in container/mpc; this is the integration layer. What it adds is the wiring
// nothing else covers: that the sniff table resolves both magics without a
// filename, that a track built by container/mpc drives codec/musepack without
// either side knowing about the other, that the gapless trims reach
// format.Media, that a seek through the engine is sample-exact for both stream
// versions with noise substitution in play, and that the refusals arrive by
// name.

import (
	"bytes"
	"context"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// mpcCases are the committed fixtures the engine tests run over, with the
// shape each one is known to have.
var mpcCases = []struct {
	path    []string
	version int
	rate    int
	samples int64
	delay   int64
}{
	{[]string{"testdata", "sine-s16.mpc"}, 8, 44100, 22050, 481},
	{[]string{"testdata", "noise-s16.mpc"}, 7, 44100, 22050, 481},
	{[]string{"container", "mpc", "testdata", "gapless-sv7.mpc"}, 7, 44100, 23740, 481},
	{[]string{"container", "mpc", "testdata", "seek.mpc"}, 8, 44100, 30000, 481},
	{[]string{"codec", "musepack", "testdata", "sv8-cut.mpc"}, 8, 44100, 7000, 481 + 392},
}

func mpcFixture(t testing.TB, path ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(repoPath(path...))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestMusepackProbeAndDecode: the sniff table resolves both magics with no
// filename to work from, the track carries the format's gapless trims, and
// the decode delivers exactly the trimmed length.
func TestMusepackProbeAndDecode(t *testing.T) {
	for _, tc := range mpcCases {
		t.Run(tc.path[len(tc.path)-1], func(t *testing.T) {
			raw := mpcFixture(t, tc.path...)
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if info.Container != "musepack" {
				t.Errorf("container %q, want musepack", info.Container)
			}
			if len(info.Warnings) != 0 {
				t.Errorf("warnings on a clean fixture: %v", info.Warnings)
			}
			tr := info.Default()
			if tr.Codec != "musepack" {
				t.Errorf("codec %q, want musepack", tr.Codec)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != 2 || tr.Fmt.Type != audio.Float || tr.Fmt.BitDepth != 32 {
				t.Errorf("format %v, want %d Hz stereo float32", tr.Fmt, tc.rate)
			}
			if tr.Samples != tc.samples || tr.Delay != tc.delay || tr.Padding != 0 || !tr.SamplesExact {
				t.Errorf("Samples %d Delay %d Padding %d exact %v, want %d/%d/0/true", tr.Samples, tr.Delay, tr.Padding, tr.SamplesExact, tc.samples, tc.delay)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			if int64(got.N) != tc.samples {
				t.Errorf("decoded %d frames, want %d", got.N, tc.samples)
			}
			var peak float32
			for ch := range got.Fmt.Channels {
				for _, v := range got.ChanF(ch) {
					peak = max(peak, v, -v)
				}
			}
			if peak < 0.1 {
				t.Errorf("the decode peaks at %v", peak)
			}
		})
	}
}

// TestMusepackTranscodes runs the whole pipeline: a .mpc source out to formats
// this tree writes, lossless and lossy.
func TestMusepackTranscodes(t *testing.T) {
	for _, name := range []string{"sine-s16.mpc", "noise-s16.mpc"} {
		raw := mpcFixture(t, "testdata", name)
		for _, format := range []string{"wav", "flac", "opus"} {
			t.Run(name+"/"+format, func(t *testing.T) {
				out := &memWS{}
				res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "", out,
					waxflow.TranscodeOptions{Format: format})
				if err != nil {
					t.Fatalf("transcode to %s: %v", format, err)
				}
				// Opus runs at 48 kHz, so the source's 22050 samples at 44.1 kHz
				// come out as 24000.
				want := int64(22050)
				if format == "opus" {
					want = 24000
				}
				if res.Samples != want {
					t.Errorf("%d samples written, want %d", res.Samples, want)
				}
				if len(out.Buf) == 0 {
					t.Fatal("no output")
				}
			})
		}
	}
}

// TestMusepackSeekSampleExact is the seek matrix through the engine: both
// stream versions, with and without noise substitution, against our own
// linear decode (the reference's own seeks are approximate, so it cannot be
// the oracle here).
func TestMusepackSeekSampleExact(t *testing.T) {
	for _, path := range [][]string{
		{"container", "mpc", "testdata", "seek.mpc"},
		{"container", "mpc", "testdata", "seek-pns.mpc"},
		{"container", "mpc", "testdata", "seek-sv7.mpc"},
		{"container", "mpc", "testdata", "gapless-sv7.mpc"},
		{"codec", "musepack", "testdata", "sv8-cut.mpc"},
		{"codec", "musepack", "testdata", "sv7-pns.mpc"},
	} {
		t.Run(path[len(path)-1], func(t *testing.T) {
			src := container.BytesSource(mpcFixture(t, path...))
			ref, err := decodeAllDynamic(t, src, "")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(ref)
			med, err := waxflow.New().OpenStream(src, "")
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			seekMatchesReference(t, med, ref, 40, 5)
		})
	}
}

// TestMusepackFATEPair runs the two real-world files end to end: both decode
// to the length the reference reports for them.
func TestMusepackFATEPair(t *testing.T) {
	for _, name := range []string{"inside-mp7.mpc", "inside-mp8.mpc"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(testutil.VectorPath(t, "musepack/"+name))
			if err != nil {
				t.Fatal(err)
			}
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if tr := info.Default(); tr.Samples != 524277 || tr.Fmt.Rate != 44100 || tr.Fmt.Channels != 2 {
				t.Errorf("track %+v, want 524277 samples at 44100 Hz stereo", tr)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(raw), "")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(got)
			if got.N != 524277 {
				t.Errorf("decoded %d frames, want 524277", got.N)
			}
		})
	}
}

// TestMusepackTagsReachTheProbe: the APEv2 tag mppenc wrote surfaces through
// the engine's Info, under the canonical spellings.
func TestMusepackTagsReachTheProbe(t *testing.T) {
	raw := mpcFixture(t, "container", "mpc", "testdata", "tagged.mpc")
	info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"ARTIST": "Wax Test", "ALBUM": "Fixtures", "TITLE": "Tagged", "TRACKNUMBER": "3"} {
		if got := info.Tags[key]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %q", key, got, want)
		}
	}
}

// TestMusepackChaptersReachTheProbe: the chapter run the reference chapter
// editor wrote into chapters.mpc surfaces through the engine's Info as
// chapters with their titles and start times, the untitled one included.
func TestMusepackChaptersReachTheProbe(t *testing.T) {
	raw := mpcFixture(t, "container", "mpc", "testdata", "chapters.mpc")
	info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	want := []struct {
		title string
		start float64 // seconds
	}{{"Intro", 0}, {"Middle", 8000.0 / 44100}, {"Coda", 16000.0 / 44100}, {"", 19000.0 / 44100}}
	if len(info.Chapters) != len(want) {
		t.Fatalf("chapters %+v, want %d", info.Chapters, len(want))
	}
	for i, w := range want {
		if ch := info.Chapters[i]; ch.Title != w.title || math.Abs(ch.Start.Seconds()-w.start) > 1e-6 {
			t.Errorf("chapter %d = %+v, want %q at %.4f s", i, ch, w.title, w.start)
		}
	}
}

// TestMusepackRefusalsNameTheVersion: the stream versions before SV7 share
// the magic and are refused by name, with the code a 415 needs.
func TestMusepackRefusalsNameTheVersion(t *testing.T) {
	for _, v := range []byte{4, 5, 6} {
		raw := append([]byte{'M', 'P', '+', v}, make([]byte, 64)...)
		_, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
		if err == nil {
			t.Fatalf("SV%d accepted", v)
		}
		if !strings.Contains(err.Error(), "SV"+string(rune('0'+v))) {
			t.Errorf("error %q does not name SV%d", err, v)
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
			t.Errorf("code %q, want %q", code, waxerr.CodeUnsupportedFormat)
		}
	}
}

// TestMusepackExtensionHints: every extension the driver claims routes to it,
// and the media type is the format's. The hints are tested on bytes no magic
// claims, since a real file is resolved by its magic before any hint is read.
func TestMusepackExtensionHints(t *testing.T) {
	raw := mpcFixture(t, "testdata", "sine-s16.mpc")
	for _, hint := range []string{"", "mpc", ".mpc", "mp+", "mpp"} {
		info, err := waxflow.New().Probe(container.BytesSource(raw), hint, nil)
		if err != nil {
			t.Fatalf("probe with hint %q: %v", hint, err)
		}
		if info.Container != "musepack" {
			t.Errorf("hint %q: container %q", hint, info.Container)
		}
	}
	junk := bytes.Repeat([]byte{'z'}, 4096)
	for _, hint := range []string{"mpc", ".mpc", "mp+", "mpp"} {
		_, err := waxflow.New().Probe(container.BytesSource(junk), hint, nil)
		if err == nil {
			t.Fatalf("probe accepted junk as %q", hint)
		}
		if !strings.HasPrefix(err.Error(), "musepack: ") {
			t.Errorf("hint %q routed to %q, not to the musepack driver", hint, err)
		}
	}
	if got := format.MediaTypeFor("musepack"); got != "audio/x-musepack" {
		t.Errorf("media type %q, want audio/x-musepack", got)
	}
}
