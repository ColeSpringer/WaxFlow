package silence

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/internal/testutil"
)

// TestFFmpegDifferential runs the same synthesized signals through this
// detector and through ffmpeg's silencedetect, and compares the maps.
//
// Silence detection is not standardized: it is whatever silencedetect does,
// which is why this differential asserts the span count exactly rather than
// approximately. The boundaries carry a 1 ms tolerance for one reason only:
// the threshold is converted to a linear bound in float32 here and in
// double there, so a sample sitting within an ulp of the bound near a zero
// crossing can fall on either side of it. The fixtures cut hard between a
// -6 dBFS tone and true zero, never fading, so nothing else is in play.
func TestFFmpegDifferential(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)

	cases := []struct {
		name        string
		rate        int
		thresholdDB float64
		minDur      time.Duration
		regions     []region
		channels    int
	}{
		{
			name: "tone and silence mono 48k", rate: 48000, thresholdDB: -50,
			minDur: 500 * time.Millisecond, channels: 1,
			regions: []region{{false, 48000}, {true, 48000}, {false, 48000}, {true, 96000}, {false, 24000}},
		},
		{
			name: "leading and trailing silence mono 48k", rate: 48000, thresholdDB: -50,
			minDur: 500 * time.Millisecond, channels: 1,
			regions: []region{{true, 48000}, {false, 48000}, {true, 48000}},
		},
		{
			name: "stereo 48k lower threshold", rate: 48000, thresholdDB: -60,
			minDur: 300 * time.Millisecond, channels: 2,
			regions: []region{{false, 24000}, {true, 36000}, {false, 48000}, {true, 60000}, {false, 24000}},
		},
		{
			name: "mono 44.1k higher threshold", rate: 44100, thresholdDB: -40,
			minDur: time.Second, channels: 1,
			regions: []region{{false, 44100}, {true, 66150}, {false, 44100}, {true, 44100}, {false, 22050}},
		},
		{
			name: "gaps under the minimum are dropped by both", rate: 48000, thresholdDB: -50,
			minDur: 500 * time.Millisecond, channels: 1,
			// 200 ms and 100 ms gaps fall short; the 800 ms one does not.
			regions: []region{{false, 24000}, {true, 9600}, {false, 24000}, {true, 4800},
				{false, 24000}, {true, 38400}, {false, 24000}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mono := buildAt(c.rate, c.regions)
			chans := make([][]float32, c.channels)
			for i := range chans {
				chans[i] = mono
			}

			d := detectAt(t, c.rate, chans, c.thresholdDB, c.minDur)
			got := d.Spans()

			wav := filepath.Join(t.TempDir(), "in.wav")
			testutil.WriteFloatWAV(t, wav, c.rate, chans)
			want := ffmpegSilence(t, ffmpeg, wav, c.rate, c.thresholdDB, c.minDur)

			t.Logf("waxflow %v", got)
			t.Logf("ffmpeg  %v", want)
			if len(got) != len(want) {
				t.Fatalf("got %d spans, ffmpeg found %d\n  waxflow: %v\n  ffmpeg:  %v",
					len(got), len(want), got, want)
			}
			tol := int64(c.rate / 1000) // 1 ms
			for i := range got {
				if abs64(got[i].From-want[i].From) > tol {
					t.Errorf("span %d start: got %d, ffmpeg %d (delta %d frames, tolerance %d)",
						i, got[i].From, want[i].From, got[i].From-want[i].From, tol)
				}
				if abs64(got[i].To-want[i].To) > tol {
					t.Errorf("span %d end: got %d, ffmpeg %d (delta %d frames, tolerance %d)",
						i, got[i].To, want[i].To, got[i].To-want[i].To, tol)
				}
			}
		})
	}
}

// detectAt runs a whole signal through a detector at an explicit rate.
func detectAt(t *testing.T, rate int, chans [][]float32, thresholdDB float64, minDur time.Duration) *Detector {
	t.Helper()
	d, err := New(rate, len(chans), thresholdDB, minDur)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Process(chans); err != nil {
		t.Fatalf("Process: %v", err)
	}
	d.Flush()
	return d
}

// ffmpegSilence runs silencedetect over a file and parses the span map off
// stderr, converting its seconds back to frames. silence_start names the
// first silent frame and silence_end the first frame past the silence, so
// the pair maps onto Span's half-open convention directly.
func ffmpegSilence(t *testing.T, ffmpeg, path string, rate int, thresholdDB float64, minDur time.Duration) []Span {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-nostdin", "-nostats", "-hide_banner", "-i", path,
		"-af", fmt.Sprintf("silencedetect=n=%gdB:d=%g", thresholdDB, minDur.Seconds()),
		"-f", "null", "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg silencedetect: %v\n%s", err, out.String())
	}

	var spans []Span
	open := false
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := silenceValue(line, "silence_start:"); ok {
			if open {
				t.Fatalf("ffmpeg reported two starts with no end between them:\n%s", out.String())
			}
			spans = append(spans, Span{From: framesOf(v, rate), To: -1})
			open = true
		}
		if v, ok := silenceValue(line, "silence_end:"); ok {
			if !open {
				t.Fatalf("ffmpeg reported an end with no start:\n%s", out.String())
			}
			spans[len(spans)-1].To = framesOf(v, rate)
			open = false
		}
	}
	if open {
		t.Fatalf("ffmpeg left a span open:\n%s", out.String())
	}
	return spans
}

// silenceValue parses the number following a silencedetect key on a log
// line ("[silencedetect @ 0x..] silence_end: 2.005333 | silence_duration: ..").
func silenceValue(line, key string) (float64, bool) {
	i := strings.Index(line, key)
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(line[i+len(key):])
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// framesOf converts silencedetect's seconds back onto the sample timeline.
func framesOf(seconds float64, rate int) int64 {
	return int64(seconds*float64(rate) + 0.5)
}
