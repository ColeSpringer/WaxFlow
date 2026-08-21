//go:build !wmatablesgen

package wma_test

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// demux reads one .wma into its track and its packets.
func demux(t testing.TB, path string) (container.Track, [][]byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("demux %s: %v", path, err)
	}
	var pkts [][]byte
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read packet: %v", err)
		}
		pkts = append(pkts, append([]byte(nil), pkt.Data...))
	}
	return d.Tracks()[0], pkts
}

// decodeAll decodes every packet and returns the interleaved samples.
func decodeAll(t *testing.T, track container.Track, pkts [][]byte) []float32 {
	t.Helper()
	cfg, err := wma.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	dec, err := wma.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	defer dec.Release()
	var got []float32
	emit := func(b *audio.Buffer) error {
		got = append(got, testutil.InterleaveF(b)...)
		return nil
	}
	for i, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

// TestConfigMatchesTheCorpus is the decode-free half of the gate: the frame
// length rule and the block-align identity, checked against the numbers the
// analysis pass derived, plus the noise and coefficient-book ladders. It needs
// ffmpeg only to make the files; nothing here decodes a sample.
func TestConfigMatchesTheCorpus(t *testing.T) {
	for _, c := range corpusCells {
		t.Run(c.name(), func(t *testing.T) {
			track, _ := demux(t, corpusFile(t, c))
			cfg, err := wma.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			if cfg.V2 != c.v2 || cfg.Rate != c.rate || cfg.Channels != c.channels {
				t.Fatalf("config = v2:%v %dHz %dch, want v2:%v %dHz %dch",
					cfg.V2, cfg.Rate, cfg.Channels, c.v2, c.rate, c.channels)
			}
			if got := cfg.FrameLen(); got != c.frameLen {
				t.Errorf("frame length %d, want %d (%s)", got, c.frameLen, c.what)
			}
			if cfg.BlockAlign != c.blockAlign {
				t.Errorf("block align %d, want %d", cfg.BlockAlign, c.blockAlign)
			}
			// The identity that pins the frame-length rule without decoding
			// anything, including the v1/v2 split at 32 kHz.
			want := cfg.BitRate * cfg.FrameLen() / (cfg.Rate * 8)
			if cfg.BlockAlign != want {
				t.Errorf("block align %d != floor(bitRate*frameLen/(rate*8)) = %d", cfg.BlockAlign, want)
			}
			mult, noise := wma.HighFreqMultForTest(cfg)
			if noise != c.noise {
				t.Errorf("noise coding %v, want %v", noise, c.noise)
			}
			if math.Abs(mult-c.highFreq) > 1e-9 {
				t.Errorf("high-frequency multiplier %v, want %v", mult, c.highFreq)
			}
			if got := wma.CoefBookPairForTest(cfg); got != c.coefPair {
				t.Errorf("coefficient book pair %d, want %d", got, c.coefPair)
			}
			// The demuxer builds the track format from WAVEFORMATEX and the
			// codec builds one from the same bytes; they have to be the same
			// format, including the channel LAYOUT. NewDecoder checks the
			// rate, the type and the channel count, so a layout mismatch would
			// pass here and surface a container away, as an encoder refusing
			// to assign channels it cannot name.
			if track.Fmt != cfg.Format() {
				t.Errorf("track format %v, codec format %v", track.Fmt, cfg.Format())
			}
		})
	}
}

// TestCorpusStereoCellsAreStereo checks the fixture generator rather than the
// decoder. `-ac 2` on a mono source produces bit-identical channels, which
// makes the side channel identically zero and every mid/side claim in the
// gate below vacuous. It has bitten this tree three times now, so the property
// is measured per cell rather than trusted.
func TestCorpusStereoCellsAreStereo(t *testing.T) {
	for _, c := range corpusCells {
		if c.channels != 2 || c.nearMono {
			continue
		}
		t.Run(c.name(), func(t *testing.T) {
			pcm := testutil.FFmpegDecodeF32NoSIMD(t, corpusFile(t, c))
			var worst float64
			for i := 0; i+1 < len(pcm); i += 2 {
				worst = max(worst, math.Abs(float64(pcm[i])-float64(pcm[i+1])))
			}
			if worst < 0.1 {
				t.Fatalf("max|L-R| = %g: this is not a stereo fixture", worst)
			}
		})
	}
}

// The differential's bounds, set from what the decoder measures rather than
// from a round number. The oracle has a noise floor of its own: ffmpeg's
// scalar and vectorised decodes of the same file differ by 2.5-2.9e-8 RMS and
// 1.5-2.4e-7 max full scale across this corpus, so nothing can gate below
// that. This decoder measures 3.7-4.9e-8 RMS and 1.8-3.0e-7 max against the
// scalar decode -- within 1.5x of the oracle disagreeing with itself, which is
// float32 rounding in two different transform factorizations and not a
// disagreement about the format. The bounds below are that worst case with
// about 2x of headroom, four to five decades under the 1e-3/1e-2 the plan
// opened with.
const (
	gateRMS = 1e-7
	gateMax = 1e-6
)

// TestDecodeMatchesFFmpeg is the differential. Fixture generator and oracle
// are the same program, so what it establishes is "we agree with ffmpeg", not
// "we decode WMA"; TestRealWorldFixture is what converts the one into the
// other, and the structural tests in this package are what hold without either.
//
// -cpuflags 0 pins ffmpeg's scalar path so the reference answer does not
// depend on the host CPU.
func TestDecodeMatchesFFmpeg(t *testing.T) {
	for _, c := range corpusCells {
		t.Run(c.name(), func(t *testing.T) {
			path := corpusFile(t, c)
			track, pkts := demux(t, path)
			got := decodeAll(t, track, pkts)
			want := testutil.FFmpegDecodeF32NoSIMD(t, path)

			// ffmpeg declares 2*frameLen of decoder delay and trims it; this
			// decoder trims the one frame the delay actually measures, so its
			// output leads ffmpeg's by exactly one frame.
			lead := c.frameLen * c.channels
			if len(got) < lead {
				t.Fatalf("decoded %d samples, fewer than the %d-sample head lead", len(got), lead)
			}
			got = got[lead:]
			n := min(len(got), len(want))
			if n == 0 {
				t.Fatal("nothing to compare")
			}
			// Lengths, before the samples: a decode that dropped or repeated a
			// frame would otherwise pass on the overlap the comparison takes.
			// The equality is structural, not lucky. Every packet here is one
			// frame, both decoders flush the accumulator's last half at end of
			// stream, and both trim from the head; ffmpeg just trims a frame
			// more than the delay measures.
			if len(got) != len(want) {
				t.Errorf("decoded %d samples, ffmpeg %d", len(got), len(want))
			}
			d := testutil.CompareF32(got[:n], want[:n])
			if d.RMS > gateRMS || d.MaxAbs > gateMax {
				t.Errorf("differential %v, want rms <= %g max <= %g", d, gateRMS, gateMax)
			}
		})
	}
}

// TestRealWorldFixture scores whatever .wma files a WAXFLOW_WMA_REALWORLD
// directory holds against ffmpeg's decode of the same bytes.
//
// The deficit it was written for is now mostly closed by
// microsoft_test.go, which drives Windows' own encoder rather than waiting for
// a file to turn up. What this still buys is files from encoders neither of
// those is -- a Windows Media Encoder 9 file, or anything off a shelf -- since
// two encoders agreeing is not the same as the format being covered. Point it
// at a directory and it says what each file reaches.
func TestRealWorldFixture(t *testing.T) {
	dir := os.Getenv("WAXFLOW_WMA_REALWORLD")
	if dir == "" {
		t.Skip("set WAXFLOW_WMA_REALWORLD to a directory of non-ffmpeg .wma files; " +
			"see the WMA row in docs/quality-gates.md for what this covers that the corpus cannot")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".wma") {
			continue
		}
		ran++
		t.Run(e.Name(), func(t *testing.T) {
			path := filepath.Join(dir, e.Name())
			track, pkts := demux(t, path)
			cfg, err := wma.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			// What the file reaches that the corpus cannot, so a run says
			// whether it bought anything.
			t.Logf("v2=%v %dHz %dch %d bit/s flags2=%#04x frameLen=%d",
				cfg.V2, cfg.Rate, cfg.Channels, cfg.BitRate, cfg.Flags2, cfg.FrameLen())
			got := decodeAll(t, track, pkts)
			want := testutil.FFmpegDecodeF32NoSIMD(t, path)
			lead := cfg.FrameLen() * cfg.Channels
			if len(got) < lead {
				t.Fatalf("decoded %d samples, fewer than the %d-sample head lead", len(got), lead)
			}
			got = got[lead:]
			n := min(len(got), len(want))
			if n == 0 {
				t.Fatal("nothing to compare")
			}
			if d := testutil.CompareF32(got[:n], want[:n]); d.RMS > gateRMS || d.MaxAbs > gateMax {
				t.Errorf("differential %v, want rms <= %g max <= %g", d, gateRMS, gateMax)
			}
		})
	}
	if ran == 0 {
		t.Fatalf("WAXFLOW_WMA_REALWORLD is %q but holds no .wma files", dir)
	}
}
