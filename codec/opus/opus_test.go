package opus

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestParseOpusHead round-trips a synthesized OpusHead.
func TestParseOpusHead(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xBB, 0, 0, 0, 0, 0)
	// version 1, 2ch, pre-skip 0x0138=312, rate 48000, gain 0, family 0.
	cfg, err := ParseOpusHead(head)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels != 2 || cfg.PreSkip != 312 || cfg.Family != 0 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if f := cfg.Format(); f.Rate != 48000 || f.Channels != 2 {
		t.Fatalf("format = %v", f)
	}
}

// TestFramingVsFFprobe checks the TOC parse and frame splitting against
// ffprobe's independent per-packet durations: the sum of a packet's frame
// sizes must equal ffprobe's reported packet duration.
func TestFramingVsFFprobe(t *testing.T) {
	ff := testutil.FFmpeg(t)
	fp := testutil.FFprobe(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.opus")
	if out, err := exec.Command(ff, "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1.0",
		"-ac", "2", "-c:a", "libopus", "-b:a", "96k", path).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}

	// ffprobe per-packet durations (in 48 kHz samples; opus stream timebase is
	// 1/48000).
	raw, err := exec.Command(fp, "-v", "error", "-select_streams", "a:0",
		"-show_packets", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var doc struct {
		Packets []struct {
			Duration json.Number `json:"duration"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	// Our demuxer's packets, split via splitPacket.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ogg.NewDemuxer(container.BytesSource(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	i := 0
	for {
		err := d.ReadPacket(&pkt)
		if err != nil {
			break
		}
		frames, ferr := splitPacket(pkt.Data)
		if ferr != nil {
			t.Fatalf("packet %d split: %v", i, ferr)
		}
		var total int
		for _, f := range frames {
			total += f.cfg.frameSize
		}
		// The final packet's ffprobe duration reflects the granule end-trim
		// (a container concern), so it is exempt: framing reports the full
		// decoded frame size.
		if i < len(doc.Packets)-1 {
			want, _ := strconv.Atoi(doc.Packets[i].Duration.String())
			if want != 0 && total != want {
				t.Errorf("packet %d: our duration %d, ffprobe %d", i, total, want)
			}
		}
		i++
	}
	if i == 0 {
		t.Fatal("no packets decoded")
	}
	t.Logf("validated %d Opus packets against ffprobe durations", i)
}
