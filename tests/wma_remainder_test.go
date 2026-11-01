package waxflow_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/internal/testutil/wmacount"
)

// wmaCorpusCells is docs/notes/wma-oracle-corpus.md section 3: the eighteen
// ffmpeg cells, by version, rate, channels and bit rate.
var wmaCorpusCells = []struct {
	v2                   bool
	rate, channels, kbps int
}{
	{false, 8000, 1, 24}, {false, 16000, 2, 24}, {false, 22050, 1, 32}, {false, 32000, 1, 32},
	{false, 32000, 2, 32}, {false, 44100, 1, 32}, {false, 44100, 2, 64}, {false, 48000, 2, 24},
	{false, 48000, 2, 64}, {true, 8000, 1, 24}, {true, 11025, 1, 24}, {true, 16000, 1, 24},
	{true, 22050, 2, 24}, {true, 32000, 2, 32}, {true, 44100, 1, 32}, {true, 44100, 2, 24},
	{true, 44100, 2, 128}, {true, 48000, 2, 128},
}

// wmaWindowsCells are Media Foundation v2 encodes, long enough that a seek has
// somewhere to land past the first packets.
var wmaWindowsCells = []struct{ rate, channels, bitRate int }{
	{44100, 2, 128000}, {48000, 2, 128000}, {22050, 1, 32000}, {16000, 2, 24000},
}

// corpusSource is the corpus note's chirp plus seeded noise, decorrelated into
// real stereo for a two-channel cell.
func corpusSource(rate, channels int, seconds float64) []string {
	args := []string{"-f", "lavfi", "-i",
		fmt.Sprintf("aevalsrc='0.28*sin(2*PI*(200+1200*t)*t)+0.22*(2*random(1)-1)':s=%d:d=%g", rate, seconds)}
	if channels == 2 {
		args = append(args, "-filter_complex", "asplit=2[l][r];[r]adelay=1S:all=1,volume=0.85[rd];[l][rd]amerge=inputs=2")
	}
	return append(args, "-ac", strconv.Itoa(channels))
}

func ffmpegTo(t *testing.T, out string, args ...string) {
	t.Helper()
	argv := append(append([]string{"-v", "error", "-y"}, args...), out)
	if b, err := exec.Command(testutil.FFmpeg(t), argv...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", argv, err, b)
	}
}

// TestWMASeekDeliversTheRemainder is the symptom a late landing produced, and
// it needs no sample comparison, so it holds on noise-coded cells too: a seek
// to X reports X, its first chunk is at X, and a read from there to the end
// delivers exactly what the linear decode holds past X. A landing reported
// off a stamp came up short by the stamp's error at the end of the read: the
// open-ended last piece of a split, with a clean EOF.
func TestWMASeekDeliversTheRemainder(t *testing.T) {
	dir := t.TempDir()
	t.Run("ffmpeg", func(t *testing.T) {
		for _, tc := range wmaCases {
			t.Run(tc.name, func(t *testing.T) {
				path := genWMA(t, dir, tc.name, tc.source, tc.acodec, tc.bitrate, tc.channels)
				assertSeeksDeliverTheRemainder(t, path, false)
			})
		}
	})
	t.Run("corpus", func(t *testing.T) {
		for _, c := range wmaCorpusCells {
			v := map[bool]int{false: 1, true: 2}[c.v2]
			name := fmt.Sprintf("v%d-%d-%dch-%dk", v, c.rate, c.channels, c.kbps)
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(dir, name+".wma")
				ffmpegTo(t, path, append(corpusSource(c.rate, c.channels, 2),
					"-c:a", "wmav"+strconv.Itoa(v), "-b:a", strconv.Itoa(c.kbps)+"k")...)
				assertSeeksDeliverTheRemainder(t, path, false)
			})
		}
	})
	t.Run("windows", func(t *testing.T) {
		if !testutil.HaveWMFEnc(t) {
			t.Skip("Windows' Media Foundation WMA encoder is unavailable")
		}
		for _, c := range wmaWindowsCells {
			name := fmt.Sprintf("ms-%d-%dch-%dk", c.rate, c.channels, c.bitRate/1000)
			t.Run(name, func(t *testing.T) {
				wav := filepath.Join(dir, name+".wav")
				ffmpegTo(t, wav, append(corpusSource(c.rate, c.channels, 6), "-c:a", "pcm_s16le")...)
				path := filepath.Join(dir, name+".wma")
				testutil.WMFEncode(t, wav, path, c.rate, c.channels, c.bitRate)
				assertSeeksDeliverTheRemainder(t, path, true)
			})
		}
	})
}

// assertSeeksDeliverTheRemainder seeks to every object's start and to ten
// points inside objects. pastFirst additionally requires at least half the
// landings to be past the first object, so a short file whose every seek
// lands at the top cannot pass for the wrong reason.
func assertSeeksDeliverTheRemainder(t *testing.T, path string, pastFirst bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	objs, fl := wmacount.Objects(t, raw)
	starts := make([]int64, len(objs))
	for i, o := range objs {
		starts[i] = o.Before * fl
	}
	src := container.BytesSource(raw)
	ref, err := decodeAllDynamic(t, src, "wma")
	if err != nil {
		t.Fatal(err)
	}
	total := int64(ref.N)
	audio.Put(ref)
	targets := append([]int64(nil), starts...)
	for k := range 10 {
		targets = append(targets, starts[k*len(starts)/10]+fl/2)
	}
	med, err := waxflow.New().OpenStream(src, "wma")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	for _, x := range targets {
		x = min(x, total)
		landed, err := med.SeekSample(x)
		if err != nil {
			t.Fatalf("seek to %d: %v", x, err)
		}
		if landed != x {
			t.Fatalf("seek to %d landed at %d", x, landed)
		}
		var delivered int64
		for first := true; ; first = false {
			err := med.ReadChunk(buf)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("read after a seek to %d: %v", x, err)
			}
			if first && (buf.Pos != x || !buf.Discont) {
				t.Fatalf("seek to %d: first chunk at %d (discont %v)", x, buf.Pos, buf.Discont)
			}
			delivered += int64(buf.N)
		}
		if delivered != total-x {
			t.Fatalf("seek to %d delivered %d samples to the end, the linear decode holds %d past it",
				x, delivered, total-x)
		}
	}
	if !pastFirst {
		return
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	past := 0
	for _, x := range targets {
		if landed, err := d.SeekSample(0, x); err == nil && landed > 0 {
			past++
		}
	}
	if 2*past < len(targets) {
		t.Errorf("%d of %d landings were past the first object", past, len(targets))
	}
}
