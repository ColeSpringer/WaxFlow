package oracletest

// Where both readers speak for a Matroska file, they agree on it: the format's
// default channel count and rate, an MP3 track's codec and geometry, and the
// length of a file cut short or holding junk between Segment children. A cut
// file is the one place they part, by design: waxlabel reports the length
// the Info Duration declares and warns that the audio is truncated, and the
// native demuxer delivers the whole blocks that are there.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

var (
	mkaClusterID = []byte{0x1F, 0x43, 0xB6, 0x75}
	mkaCuesID    = []byte{0x1C, 0x53, 0xBB, 0x6B}
)

// pcmMKA is ten seconds of 44.1 kHz stereo 16-bit PCM in Matroska, three
// clusters of at most 4 s.
func pcmMKA(t *testing.T, seekable bool) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	wav, _ := synthWAV(t, f, 10*44100)
	return transcodeMKA(t, wav, "wav", seekable)
}

// voidAudioChild overwrites the first element of the given one-byte ID and
// one-byte size ahead of the first cluster, an Audio child, with a Void of
// its length: how a muxer that leaves it out reads.
func voidAudioChild(t *testing.T, raw []byte, id byte, size int) []byte {
	t.Helper()
	end := bytes.Index(raw, mkaClusterID)
	i := bytes.Index(raw[:end], []byte{id, 0x80 | byte(size)})
	if i < 0 {
		t.Fatalf("no element %#x of %d bytes ahead of the first cluster", id, size)
	}
	out := append([]byte(nil), raw...)
	copy(out[i:], append([]byte{0xEC, 0x80 | byte(size)}, make([]byte, size)...))
	return out
}

// nativeRun opens raw tolerantly and decodes it to the end, returning the
// track and the frames delivered.
func nativeRun(t *testing.T, raw []byte) (container.Track, int64, []string) {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatalf("format.Open: %v", err)
	}
	defer med.Close()
	tr := med.Info().Default()
	buf := audio.Get(tr.Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		n += int64(buf.N)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
	}
	return tr, n, med.Info().Warnings
}

// waxlabelMKA parses raw with waxlabel, returning its one track and whether
// it raised the warning code.
func waxlabelMKA(t *testing.T, raw []byte, code waxlabel.WarningCode) (waxlabel.AudioTrack, bool) {
	t.Helper()
	doc, err := waxlabel.Parse(t.Context(), container.BytesSource(raw))
	if err != nil {
		t.Fatalf("waxlabel.Parse: %v", err)
	}
	tracks := doc.Properties().Tracks
	if len(tracks) != 1 {
		t.Fatalf("waxlabel found %d tracks, want 1", len(tracks))
	}
	warned := false
	for _, w := range doc.Warnings() {
		warned = warned || w.Code == code
	}
	return tracks[0], warned
}

// TestMKADefaultGeometryAgrees: a PCM track that leaves Channels or
// SamplingFrequency out has the format's defaults, one channel and 8000 Hz,
// in both readers.
func TestMKADefaultGeometryAgrees(t *testing.T) {
	raw := pcmMKA(t, true)
	for _, tc := range []struct {
		name           string
		raw            []byte
		rate, channels int
	}{
		{"no Channels", voidAudioChild(t, raw, 0x9F, 1), 44100, 1},
		{"no SamplingFrequency", voidAudioChild(t, raw, 0xB5, 8), 8000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native, _, _ := nativeRun(t, tc.raw)
			wl, _ := waxlabelMKA(t, tc.raw, 0)
			if native.Fmt.Rate != tc.rate || native.Fmt.Channels != tc.channels {
				t.Errorf("native reads %d Hz, %d channels; want %d, %d", native.Fmt.Rate, native.Fmt.Channels, tc.rate, tc.channels)
			}
			if wl.SampleRate != native.Fmt.Rate || wl.Channels != native.Fmt.Channels {
				t.Errorf("waxlabel reads %d Hz, %d channels; native %d, %d", wl.SampleRate, wl.Channels, native.Fmt.Rate, native.Fmt.Channels)
			}
		})
	}
}

// TestMKADamagedLengthAgrees: a file with junk between clusters, or cut where
// its Cues begin, still holds every block, so the native read delivers the
// length waxlabel reports, to the millisecond. A file cut inside a cluster
// delivers the whole blocks before the cut, short of what waxlabel reports,
// and waxlabel says the audio is truncated.
func TestMKADamagedLengthAgrees(t *testing.T) {
	sized := pcmMKA(t, true)
	streamed := pcmMKA(t, false)
	third := 0
	for range 3 {
		i := bytes.Index(streamed[third+1:], mkaClusterID)
		if i < 0 {
			t.Fatal("the streamed fixture has fewer than three clusters")
		}
		third += i + 1
	}
	junked := append(append(append([]byte(nil), streamed[:third]...), make([]byte, 100)...), streamed[third:]...)
	cues := bytes.LastIndex(sized, mkaCuesID)

	for _, tc := range []struct {
		name  string
		raw   []byte
		whole bool
		code  waxlabel.WarningCode
	}{
		{"junk between clusters", junked, true, waxlabel.WarnTrailingBytes},
		{"cut where the Cues begin", sized[:cues], true, waxlabel.WarnTruncatedAudio},
		{"cut inside a cluster", sized[:len(sized)/2], false, waxlabel.WarnTruncatedAudio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native, n, warnings := nativeRun(t, tc.raw)
			if len(warnings) == 0 {
				t.Error("the native read warned nothing")
			}
			wl, warned := waxlabelMKA(t, tc.raw, tc.code)
			if !warned {
				t.Errorf("waxlabel did not warn %v", tc.code)
			}
			ms := time.Duration(n) * time.Second / time.Duration(native.Fmt.Rate)
			diff := (wl.Duration - ms).Abs()
			switch {
			case tc.whole && diff > time.Millisecond:
				t.Errorf("native delivered %d samples (%v), waxlabel reports %v", n, ms, wl.Duration)
			case !tc.whole && wl.Duration-ms < 100*time.Millisecond:
				t.Errorf("native delivered %d samples (%v) of a file cut in half, waxlabel reports %v", n, ms, wl.Duration)
			}
		})
	}
}

// TestMKAMP3Agrees: both readers name an A_MPEG/L3 track MP3 at the rate and
// channel count its frames state.
func TestMKAMP3Agrees(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "container", "mka", "testdata", "mp3.mka"))
	if err != nil {
		t.Fatal(err)
	}
	native, n, _ := nativeRun(t, raw)
	wl, _ := waxlabelMKA(t, raw, 0)
	if native.Codec != codec.MP3 || n == 0 {
		t.Fatalf("native reads codec %q and delivers %d samples", native.Codec, n)
	}
	if wl.Codec != "MP3" || wl.SampleRate != native.Fmt.Rate || wl.Channels != native.Fmt.Channels {
		t.Errorf("waxlabel reads %s at %d Hz, %d channels; native mp3 at %d Hz, %d channels",
			wl.Codec, wl.SampleRate, wl.Channels, native.Fmt.Rate, native.Fmt.Channels)
	}
	if ms := time.Duration(n) * time.Second / time.Duration(native.Fmt.Rate); ms != time.Second {
		t.Errorf("native delivered %v, want the second the fixture lasts", ms)
	}
}
