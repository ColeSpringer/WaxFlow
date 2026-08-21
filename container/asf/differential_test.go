package asf_test

// Differential tests for the ASF demuxer against ffmpeg-generated fixtures and
// ffprobe (testutil skips these when ffmpeg is absent; the CI differential job
// sets WAXFLOW_REQUIRE_FFMPEG=1).
//
// The oracle here is ffprobe's packet list rather than a decode, because this
// build has no WMA decoder yet. That is not a weaker check than it sounds: a
// demuxer's whole output is the packet run, and `ffprobe -show_packets`
// reports every packet's presentation time, duration, and size without
// decoding a sample. A reassembly that dropped a fragment, mistook a
// sub-payload boundary, or lost a packet to padding shows up there
// immediately.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// wmaSpec describes one fixture the differential generates with ffmpeg.
type wmaSpec struct {
	name       string
	acodec     string
	rate       int
	channels   int
	bitrate    string
	seconds    float64
	packetSize int // ffmpeg's ASF packet size; 0 leaves its 3200-byte default
}

var wmaSpecs = []wmaSpec{
	{"v2-44-stereo.wma", "wmav2", 44100, 2, "128k", 2.0, 0},
	{"v2-48-stereo.wma", "wmav2", 48000, 2, "192k", 2.0, 0},
	{"v2-32-stereo.wma", "wmav2", 32000, 2, "96k", 1.5, 0},
	{"v2-22-mono.wma", "wmav2", 22050, 1, "64k", 1.5, 0},
	{"v2-16-mono.wma", "wmav2", 16000, 1, "48k", 1.0, 0},
	{"v2-8-mono.wma", "wmav2", 8000, 1, "32k", 1.0, 0},
	{"v1-44-stereo.wma", "wmav1", 44100, 2, "128k", 2.0, 0},
	{"v1-22-mono.wma", "wmav1", 22050, 1, "64k", 1.5, 0},
	// Small packets against a high bit rate, so every media object spans
	// several packets and the fragment reassembly carries the whole file.
	{"v2-fragmented.wma", "wmav2", 44100, 2, "192k", 2.0, 512},
	{"v2-fragmented-mono.wma", "wmav2", 32000, 1, "128k", 1.5, 400},
}

// corpus builds the fixtures once for the whole package. Three tests read the
// same ten files, and encoding each one three times is thirty ffmpeg spawns
// for ten distinct results.
var corpus = sync.OnceValue(func() map[string]string {
	dir, err := os.MkdirTemp("", "waxflow-asf")
	if err != nil {
		panic(err)
	}
	built = dir
	out := make(map[string]string, len(wmaSpecs))
	for _, s := range wmaSpecs {
		path := filepath.Join(dir, s.name)
		extra := []string{"-b:a", s.bitrate}
		if s.packetSize != 0 {
			extra = append(extra, "-packet_size", strconv.Itoa(s.packetSize))
		}
		args := []string{"-v", "error", "-y", "-f", "lavfi",
			"-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%g", s.rate, s.seconds),
			"-ac", strconv.Itoa(s.channels), "-c:a", s.acodec}
		args = append(args, extra...)
		args = append(args, path)
		if b, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
			panic(fmt.Sprintf("ffmpeg %v: %v; %s", args, err, b))
		}
		out[s.name] = path
	}
	return out
})

var (
	ffmpegPath string
	built      string
)

// TestMain resolves ffmpeg once and cleans the corpus up after the run.
func TestMain(m *testing.M) {
	ffmpegPath, _ = exec.LookPath("ffmpeg")
	code := m.Run()
	if built != "" {
		os.RemoveAll(built)
	}
	os.Exit(code)
}

// genWMA returns the built fixture for a spec, applying the oracle's
// skip-or-require policy before touching the corpus.
func genWMA(t *testing.T, s wmaSpec) string {
	t.Helper()
	testutil.FFmpeg(t)
	return corpus()[s.name]
}

// TestDemuxMatchesFFprobePackets is the differential: our media objects must
// be ffmpeg's packets, one for one, in time, length, and byte count.
func TestDemuxMatchesFFprobePackets(t *testing.T) {
	for _, s := range wmaSpecs {
		t.Run(s.name, func(t *testing.T) {
			path := genWMA(t, s)
			want := testutil.FFprobePackets(t, path)
			if len(want) == 0 {
				t.Fatal("ffprobe reported no packets")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			rate := d.Tracks()[0].Fmt.Rate
			if rate != s.rate {
				t.Fatalf("rate = %d, want %d", rate, s.rate)
			}
			var pkt container.Packet
			for i := 0; ; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					if i != len(want) {
						t.Fatalf("%d packets, ffprobe reports %d", i, len(want))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if i >= len(want) {
					t.Fatalf("packet %d past ffprobe's %d", i, len(want))
				}
				if len(pkt.Data) != want[i].Size {
					t.Fatalf("packet %d is %d bytes, ffprobe reports %d", i, len(pkt.Data), want[i].Size)
				}
				// ffprobe reports ASF times in the container's own
				// millisecond time base; ours are the sample positions those
				// milliseconds name, so the comparison runs in milliseconds
				// and tolerates the half-sample the conversion rounds by.
				if got := msOf(pkt.PTS, rate); got != want[i].PTS {
					t.Fatalf("packet %d is at %d ms, ffprobe reports %d ms", i, got, want[i].PTS)
				}
			}
		})
	}
}

// msOf converts a sample position back to the millisecond the container
// stated, rounding to nearest to undo msToSamples.
func msOf(samples int64, rate int) int64 {
	return (samples*2000 + int64(rate)) / (2 * int64(rate))
}

// TestTrackLengthMatchesFFprobe pins the declared length against the container
// duration ffprobe reads out of the same File Properties Object: play duration
// less pre-roll, which is the arithmetic Track.Samples exists to carry.
func TestTrackLengthMatchesFFprobe(t *testing.T) {
	for _, s := range wmaSpecs {
		t.Run(s.name, func(t *testing.T) {
			path := genWMA(t, s)
			want := testutil.FFprobeFormatDuration(t, path)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			tr := d.Tracks()[0]
			got := float64(tr.Samples) / float64(tr.Fmt.Rate)
			// One millisecond is the container's own resolution.
			if diff := got - want; diff > 0.001 || diff < -0.001 {
				t.Errorf("duration = %.6f s (%d samples at %d Hz), ffprobe reports %.6f s",
					got, tr.Samples, tr.Fmt.Rate, want)
			}
			info := testutil.FFprobeFile(t, path)
			if info.SampleRate != tr.Fmt.Rate || info.Channels != tr.Fmt.Channels {
				t.Errorf("stream = %d Hz %d ch, ffprobe reports %d Hz %d ch",
					tr.Fmt.Rate, tr.Fmt.Channels, info.SampleRate, info.Channels)
			}
			if want := fmt.Sprintf("wma%s", s.acodec[len(s.acodec)-2:]); info.CodecName != want {
				t.Errorf("ffprobe reports codec %q, fixture asked for %q", info.CodecName, want)
			}
		})
	}
}

// TestSeekAgainstFFprobePackets checks the landing against the packet list:
// every seek must land on a packet ffmpeg also reports, at or before the
// target, with at least one whole object of run-up.
func TestSeekAgainstFFprobePackets(t *testing.T) {
	for _, s := range wmaSpecs {
		t.Run(s.name, func(t *testing.T) {
			path := genWMA(t, s)
			want := testutil.FFprobePackets(t, path)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			rate := d.Tracks()[0].Fmt.Rate
			at := map[int64]bool{}
			for _, p := range want {
				at[p.PTS] = true
			}
			for i, p := range want {
				// Target the middle of packet i, which is the hardest place
				// to land at-or-before.
				target := msToSamples(p.PTS, rate) + int64(p.Dur)*int64(rate)/2000
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek to packet %d: %v", i, err)
				}
				if landed > target {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
				ms := msOf(landed, rate)
				if !at[ms] {
					t.Fatalf("seek to packet %d landed at %d ms, which is no packet ffprobe reports", i, ms)
				}
				if i > 0 && ms >= p.PTS {
					t.Fatalf("seek to packet %d landed on packet %d ms with no run-up", i, ms)
				}
			}
		})
	}
}

func msToSamples(ms int64, rate int) int64 {
	return (ms*int64(rate) + 500) / 1000
}
