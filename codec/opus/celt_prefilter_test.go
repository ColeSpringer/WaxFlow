package opus

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// parsePF reads the CELT-frame header bits of one packet (skipping the TOC)
// and returns the post-filter parameters: on (-1 for a silence frame),
// period, qg, tapset.
func parsePF(pkt []byte) (on, period, qg, tapset int) {
	dec := newRangeDecoder(pkt[1:])
	if dec.decodeBitLogp(15) != 0 {
		return -1, 0, 0, 0
	}
	totalBits := len(pkt[1:]) * 8
	if dec.tell()+16 > totalBits {
		return 0, 0, 0, 0
	}
	if dec.decodeBitLogp(1) == 0 {
		return 0, 0, 0, 0
	}
	octave := int(dec.decodeUint(6))
	period = (16 << uint(octave)) + int(dec.decodeRawBits(uint(4+octave))) - 1
	qg = int(dec.decodeRawBits(3))
	if dec.tell()+2 <= totalBits {
		tapset = dec.decodeICDF(celtTapsetICDF, 2)
	}
	return 1, period, qg, tapset
}

// TestPrefilterMatchesLibopus is the differential gate for the pitch
// pre-filter port: on a clearly pitched signal, the per-frame post-filter
// parameters our encoder codes (on, period, gain index, tapset) must agree
// with libopus's own choices at the same settings on nearly every frame. It
// needs the reference tools (`make opus-tools`) and self-skips without them.
func TestPrefilterMatchesLibopus(t *testing.T) {
	demo := filepath.Join(os.Getenv("WAXFLOW_OPUS_TOOLS"), "opus_demo")
	if os.Getenv("WAXFLOW_OPUS_TOOLS") == "" {
		demo = filepath.Join("..", "..", "testdata", "tools", "opus-1.6.1", "opus_demo")
	}
	if _, err := os.Stat(demo); err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_OPUS_TOOLS") == "1" {
			t.Fatalf("opus_demo required by WAXFLOW_REQUIRE_OPUS_TOOLS=1 but not found (run `make opus-tools`)")
		}
		t.Skipf("opus_demo not found (run `make opus-tools`); skipping")
	}
	const C, sr, secs, kbps = 2, SampleRate, 3, 96
	n := secs * sr
	f := audio.Format{Rate: sr, Channels: C, Layout: audio.DefaultLayout(C), Type: audio.Float, BitDepth: 32}

	// A clearly pitched signal: a 220 Hz harmonic stack with slow vibrato.
	src := make([]float32, C*n)
	for c := 0; c < C; c++ {
		for i := 0; i < n; i++ {
			ts := float64(i) / sr
			var v float64
			for k := 1; k <= 6; k++ {
				v += 0.08 / float64(k) * math.Sin(2*math.Pi*220*float64(k)*ts)
			}
			src[c*n+i] = float32(v)
		}
	}

	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: kbps * 1000, Complexity: 10})
	if err != nil {
		t.Fatal(err)
	}
	var ours [][]byte
	emit := func(p codec.Packet) error { ours = append(ours, append([]byte(nil), p.Data...)); return nil }
	buf := &audio.Buffer{Fmt: f, F: src, Stride: n, N: n}
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	in := filepath.Join(dir, "in.sw")
	bit := filepath.Join(dir, "out.bit")
	raw := make([]byte, 2*C*n)
	for i := 0; i < n; i++ {
		for c := 0; c < C; c++ {
			s := int16(max(-32768, min(32767, int(src[c*n+i]*32768))))
			binary.LittleEndian.PutUint16(raw[2*(i*C+c):], uint16(s))
		}
	}
	os.WriteFile(in, raw, 0o644)
	if out, err := exec.Command(demo, "-e", "audio", strconv.Itoa(sr), strconv.Itoa(C),
		strconv.Itoa(kbps*1000), "-cbr", "-complexity", "10", in, bit).CombinedOutput(); err != nil {
		t.Fatalf("opus_demo -e: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bit)
	if err != nil {
		t.Fatal(err)
	}
	var lib [][]byte
	for off := 0; off+8 <= len(data); {
		ln := int(binary.BigEndian.Uint32(data[off:]))
		off += 8
		lib = append(lib, data[off:off+ln])
		off += ln
	}

	m := min(len(ours), len(lib))
	if m < 100 {
		t.Fatalf("too few comparable frames: %d", m)
	}
	agree, pfFrames := 0, 0
	for i := 0; i < m; i++ {
		oOn, oP, oQ, oT := parsePF(ours[i])
		lOn, lP, lQ, lT := parsePF(lib[i])
		if lOn == 1 {
			pfFrames++
		}
		if oOn == lOn && oP == lP && oQ == lQ && oT == lT {
			agree++
		} else {
			t.Logf("frame %3d ours: on=%d T=%4d qg=%d tap=%d | lib: on=%d T=%4d qg=%d tap=%d",
				i, oOn, oP, oQ, oT, lOn, lP, lQ, lT)
		}
	}
	t.Logf("prefilter agreement: %d/%d frames (libopus enabled it on %d)", agree, m, pfFrames)
	if pfFrames < m/2 {
		t.Fatalf("libopus enabled the prefilter on only %d/%d frames; the fixture is not exercising it", pfFrames, m)
	}
	if agree < m*9/10 {
		t.Errorf("prefilter decisions agree on only %d/%d frames, want >= 90%%", agree, m)
	}
}
