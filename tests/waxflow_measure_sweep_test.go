package waxflow_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// measureSweep is one committed fixture per container and codec the tree
// decodes, with the route MeasureLength takes for it and how far a read of an
// un-walked open runs past the measure.
var measureSweep = []struct {
	what, route string
	over        int64
	path        []string
}{
	{"flac", "exact", 0, []string{"testdata", "sine-s16.flac"}},
	{"wav pcm", "exact", 0, []string{"testdata", "sine-s16.wav"}},
	{"wav a-law", "exact", 0, []string{"testdata", "sine-alaw.wav"}},
	{"wav mu-law", "exact", 0, []string{"testdata", "sine-ulaw.wav"}},
	{"wav ima adpcm", "exact", 0, []string{"testdata", "sine-ima.wav"}},
	{"wav ms adpcm", "exact", 0, []string{"testdata", "sine-msadpcm.wav"}},
	{"wav mp3", "walk", 0, []string{"container", "riff", "testdata", "mp3.wav"}},
	{"aiff pcm", "exact", 0, []string{"testdata", "sine-s16.aiff"}},
	{"aiff-c ima4", "exact", 0, []string{"testdata", "sine-ima4.aifc"}},
	{"ogg vorbis", "exact", 0, []string{"container", "ogg", "testdata", "golden-vorbis.ogg"}},
	{"ogg opus", "exact", 0, []string{"container", "ogg", "testdata", "golden-opus.opus"}},
	{"ogg flac", "exact", 0, []string{"testdata", "sine-s16.oga"}},
	{"mp4 alac", "seek", 0, []string{"container", "mp4", "testdata", "alac-stereo.m4a"}},
	{"mp4 he-aac", "seek", 0, []string{"codec", "aac", "testdata", "fdk_he_v1.m4a"}},
	{"mp4 pcm", "seek", 0, []string{"container", "mp4", "testdata", "pcm-sowt.mov"}},
	{"mp4 g.711", "seek", 0, []string{"container", "mp4", "testdata", "g711-alaw.mov"}},
	{"mp4 mp3", "seek", 0, []string{"container", "mp4", "testdata", "mp3.mp4"}},
	// Un-walked, a fragmented movie delivers its last packet whole, past the
	// last trun's duration: TestFragmentedLengthWalkedAndNot.
	{"mp4 fragmented ima4", "walk", 60, []string{"container", "mp4", "testdata", "ima4-frag.mov"}},
	{"matroska opus", "walk", 0, []string{"container", "mka", "testdata", "seed-opus.webm"}},
	{"matroska flac", "walk", 0, []string{"container", "mka", "testdata", "seed-flac.mka"}},
	{"matroska pcm", "walk", 0, []string{"container", "mka", "testdata", "seed-pcm.mka"}},
	{"adts aac-lc", "walk", 0, []string{"container", "adts", "testdata", "golden-lc-44k-stereo.aac"}},
	{"adts he-aac", "walk", 0, []string{"codec", "aac", "testdata", "fdk_he_v1.aac"}},
	{"monkey's audio", "exact", 0, []string{"testdata", "sine-s16.ape"}},
	{"wavpack", "exact", 0, []string{"testdata", "sine-s16.wv"}},
	{"wma v1", "walk", 0, []string{"container", "asf", "testdata", "sine-wmav1.wma"}},
	{"wma v2", "walk", 0, []string{"testdata", "sine-s16.wma"}},
	{"wma v2 windows", "walk", 0, []string{"container", "asf", "testdata", "ms-v2-44100-2ch-128k.wma"}},
	{"wma pro", "decode", 0, []string{"container", "asf", "testdata", "pro-s16.wma"}},
	{"wma lossless", "decode", 0, []string{"container", "asf", "testdata", "lossless-s16.wma"}},
	{"wma voice", "decode", 0, []string{"container", "asf", "testdata", "voice-mono.wma"}},
	{"musepack sv7", "exact", 0, []string{"container", "mpc", "testdata", "seek-sv7.mpc"}},
	{"musepack sv8", "exact", 0, []string{"container", "mpc", "testdata", "seek.mpc"}},
	{"mp3 cbr", "walk", 0, []string{"testdata", "sine-cbr128.mp3"}},
	{"mp3 vbr", "walk", 0, []string{"testdata", "sine-vbr.mp3"}},
}

// measureRouteOf names the route MeasureLength takes for a freshly opened
// media, the way it chooses: a settled length, then a walk, then a decode for
// a rounded container, then a seek past the end.
func measureRouteOf(t *testing.T, med format.Media) string {
	t.Helper()
	exact := func() bool { tr := med.Info().Default(); return tr.SamplesExact && tr.Samples >= 0 }
	if exact() {
		return "exact"
	}
	if err := format.WalkMedia(med); err != nil {
		t.Fatal(err)
	}
	switch {
	case exact():
		return "walk"
	case med.Info().Default().SamplesAdvisory:
		return "decode"
	default:
		return "seek"
	}
}

// TestMeasureLengthEqualsTheDecode is the premise an exact length rests on:
// whatever route MeasureLength takes, it comes back exact and equal to what a
// read of the same file delivers. A slice holds an open span to that length,
// so a route that over-measured would turn every open span of the format into
// a refusal, and one that under-measured would cut the tail off. The routes
// are recorded so a format that changes route is seen to.
func TestMeasureLengthEqualsTheDecode(t *testing.T) {
	for _, c := range measureSweep {
		t.Run(c.what, func(t *testing.T) {
			raw, err := os.ReadFile(repoPath(c.path...))
			if err != nil {
				t.Fatal(err)
			}
			hint := filepath.Ext(c.path[len(c.path)-1])[1:]
			open := func() format.Media {
				med, err := format.Open(container.BytesSource(raw), hint, nil)
				if err != nil {
					t.Fatal(err)
				}
				return med
			}
			med := open()
			route := measureRouteOf(t, med)
			med.Close()
			if route != c.route {
				t.Errorf("MeasureLength takes the %s route, the table says %s", route, c.route)
			}
			med = open()
			got, err := waxflow.MeasureLength(med)
			if err != nil {
				med.Close()
				t.Fatal(err)
			}
			if !got.SamplesExact || got.SamplesAdvisory {
				t.Errorf("measured %d with exact %v advisory %v", got.Samples, got.SamplesExact, got.SamplesAdvisory)
			}
			// The measure leaves med at its start, so this is the read the
			// measure promises; a fresh open's read is the one nothing walked.
			if read := readLength(t, med); got.Samples != read {
				t.Errorf("measured %d, a read of the measured media delivers %d", got.Samples, read)
			}
			if fresh := readLength(t, open()); fresh != got.Samples+c.over {
				t.Errorf("measured %d, a read of a fresh open delivers %d, want %d past the measure",
					got.Samples, fresh, c.over)
			}
		})
	}
}

// readLength reads med to the end, closes it, and returns what it delivered.
func readLength(t *testing.T, med format.Media) int64 {
	t.Helper()
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n += int64(buf.N)
	}
}
