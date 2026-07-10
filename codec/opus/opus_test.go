package opus

import (
	"bytes"
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

// TestPacketSamples pins the prefix-only duration helper against known TOC
// framings (RFC 6716 Table 2), including the 120 ms (5760-sample) cap that
// bounds a legal packet.
func TestPacketSamples(t *testing.T) {
	cases := []struct {
		name string
		pkt  []byte
		want int // -1 means an error is expected
	}{
		{"silk-nb-10ms", []byte{0x00, 0}, 480},         // config 0, code 0
		{"silk-nb-20ms", []byte{0x08, 0}, 960},         // config 1, code 0
		{"silk-nb-60ms", []byte{0x18, 0}, 2880},        // config 3, code 0
		{"celt-nb-2.5ms", []byte{0x80, 0}, 120},        // config 16, code 0
		{"code1-two-frames", []byte{0x09, 0, 0}, 1920}, // config 1, code 1: 2 * 960
		{"code3-six-frames", []byte{0x0B, 0x06}, 5760}, // 6 * 960, at the cap
		{"code3-seven-over-cap", []byte{0x0B, 0x07}, -1},
		{"code3-truncated", []byte{0x0B}, -1},
		{"empty", []byte{}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PacketSamples(c.pkt)
			if c.want < 0 {
				if err == nil {
					t.Errorf("PacketSamples = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PacketSamples: %v", err)
			}
			if got != c.want {
				t.Errorf("PacketSamples = %d, want %d", got, c.want)
			}
		})
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

// TestOpusPacketPad pins the CBR padding encoding across every structural
// shape (RFC 6716 section 3.2.5): growth by exactly one byte must produce a
// plain code 3 packet with the padding bit clear (the padding form has two
// bytes of minimum overhead, so it cannot express one), and larger growth
// pads with correctly chained length bytes. Every padded packet must parse
// back through the decoder's packet splitter to the original frame at the
// exact target length.
func TestOpusPacketPad(t *testing.T) {
	toc := genTOC(modeSILK, 50, bandwidthWide, 1) // config 9, code 0
	frame := make([]byte, 40)
	for i := range frame {
		frame[i] = byte(i + 1)
	}
	pkt := append([]byte{toc}, frame...)

	for _, tc := range []struct {
		name    string
		target  int
		wantPad bool
	}{
		{"short-by-1", len(pkt) + 1, false},
		{"short-by-2", len(pkt) + 2, true},
		{"short-by-3", len(pkt) + 3, true},
		{"short-by-256", len(pkt) + 256, true},
		{"short-by-257", len(pkt) + 257, true},
		{"short-by-258", len(pkt) + 258, true},
		{"short-by-512", len(pkt) + 512, true},
		{"already-at-target", len(pkt), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := opusPacketPad(append([]byte(nil), pkt...), tc.target)
			if len(out) != max(tc.target, len(pkt)) {
				t.Fatalf("padded length %d, want %d", len(out), tc.target)
			}
			if tc.target > len(pkt) {
				if out[0] != toc|0x3 {
					t.Fatalf("TOC %#x, want code 3 %#x", out[0], toc|0x3)
				}
				if got := out[1]&0x40 != 0; got != tc.wantPad {
					t.Fatalf("padding bit %v, want %v (count byte %#x)", got, tc.wantPad, out[1])
				}
			}
			frames, err := splitPacket(out)
			if err != nil {
				t.Fatalf("padded packet does not parse: %v", err)
			}
			if len(frames) != 1 {
				t.Fatalf("parsed %d frames, want 1", len(frames))
			}
			if !bytes.Equal(frames[0].data, frame) {
				t.Fatalf("frame data corrupted by padding")
			}
		})
	}
}
