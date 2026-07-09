package opus

import (
	"encoding/binary"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestCELTDecodeVsFFmpeg is the CELT conformance oracle: encode a signal to
// Opus in CELT-only mode (ffmpeg -application lowdelay disables SILK), decode it
// with our CELT decoder and with ffmpeg, and require the two PCM streams to
// match to a high SNR. This exercises the whole CELT path end to end.
func TestCELTDecodeVsFFmpeg(t *testing.T) {
	// A spectrally rich, partly transient signal exercises band shapes,
	// stereo, and the transient/anti-collapse path across channel counts and
	// frame durations. lowdelay forces CELT for every frame.
	stereoSrc := "aevalsrc=" +
		"0.3*sin(2*PI*440*t)+0.15*sin(2*PI*2500*t)+0.08*sin(2*PI*9000*t)+0.1*sin(2*PI*100*t)*(1+sin(2*PI*8*t))|" +
		"0.25*sin(2*PI*660*t)+0.12*sin(2*PI*4000*t)+0.06*sin(2*PI*12000*t)" +
		":s=48000:d=0.8:c=stereo"
	monoSrc := "aevalsrc=" +
		"0.3*sin(2*PI*500*t)+0.2*sin(2*PI*3000*t)+0.1*sin(2*PI*7000*t)" +
		":s=48000:d=0.6:c=mono"
	cases := []struct {
		name     string
		src      string
		channels string
		frameDur string
		minSNR   float64
	}{
		{"stereo_20ms", stereoSrc, "2", "20", 30},
		{"stereo_10ms", stereoSrc, "2", "10", 30},
		{"mono_20ms", monoSrc, "1", "20", 30},
		{"mono_2.5ms", monoSrc, "1", "2.5", 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			celtDecodeCase(t, tc.src, tc.channels, tc.frameDur, tc.minSNR)
		})
	}
}

func celtDecodeCase(t *testing.T, src, channels, frameDur string, minSNR float64) {
	ff := testutil.FFmpeg(t)
	dir := t.TempDir()
	opusPath := filepath.Join(dir, "s.opus")
	refPath := filepath.Join(dir, "ref.f32")

	if out, err := exec.Command(ff, "-v", "error", "-y", "-f", "lavfi", "-i", src,
		"-c:a", "libopus", "-application", "lowdelay", "-b:a", "160k",
		"-frame_duration", frameDur, opusPath).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg encode: %v\n%s", err, out)
	}
	nch := 2
	if channels == "1" {
		nch = 1
	}
	// ffmpeg reference decode to interleaved float32.
	if out, err := exec.Command(ff, "-v", "error", "-y", "-i", opusPath,
		"-f", "f32le", "-ac", channels, "-ar", "48000", refPath).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg decode: %v\n%s", err, out)
	}
	refBytes, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatal(err)
	}
	ref := make([]float32, len(refBytes)/4)
	for i := range ref {
		ref[i] = math.Float32frombits(binary.LittleEndian.Uint32(refBytes[4*i:]))
	}

	// Our decode: demux Ogg packets and run each frame through celtDecode.
	data, err := os.ReadFile(opusPath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ogg.NewDemuxer(container.BytesSource(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracks := d.Tracks()
	cfg, err := ParseOpusHead(tracks[0].CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec := newCELTDecoder(cfg.Channels)
	var got [][]float32 // planar accumulators per channel
	got = make([][]float32, cfg.Channels)

	var pkt container.Packet
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			break
		}
		frames, ferr := splitPacket(pkt.Data)
		if ferr != nil {
			t.Fatalf("split: %v", ferr)
		}
		for _, fr := range frames {
			if fr.cfg.mode != modeCELT {
				t.Fatalf("expected CELT-only frames, got mode %d", fr.cfg.mode)
			}
			LM := bits.Len(uint(fr.cfg.frameSize/120)) - 1
			C := 1
			if fr.stereo {
				C = 2
			}
			end := celtEndBand(fr.cfg.bandwidth)
			N := fr.cfg.frameSize
			out := make([][]float32, cfg.Channels)
			for c := range out {
				out[c] = make([]float32, N)
			}
			if err := dec.celtDecode(fr.data, LM, C, 0, end, out); err != nil {
				t.Fatalf("celtDecode: %v", err)
			}
			for c := 0; c < cfg.Channels; c++ {
				got[c] = append(got[c], out[c]...)
			}
		}
	}
	if len(got[0]) == 0 {
		t.Fatal("no audio decoded")
	}

	// Align: our output leads ffmpeg's by the OpusHead pre-skip. Search a small
	// window for the offset that maximizes SNR, to absorb any ±1 delay.
	bestSNR, bestOff := math.Inf(-1), 0
	for off := cfg.PreSkip - 8; off <= cfg.PreSkip+8; off++ {
		if off < 0 {
			continue
		}
		snr := interleavedSNR(got, ref, off, nch)
		if snr > bestSNR {
			bestSNR, bestOff = snr, off
		}
	}
	t.Logf("CELT vs ffmpeg: best SNR %.1f dB at offset %d (pre-skip %d)", bestSNR, bestOff, cfg.PreSkip)
	if bestSNR < minSNR {
		t.Errorf("CELT decode SNR %.1f dB too low (want >=%.0f)", bestSNR, minSNR)
	}
}

// celtEndBand maps a CELT bandwidth to the coded band count (libopus
// opus_decoder.c). MB is not used in CELT-only mode.
func celtEndBand(bw int) int {
	switch bw {
	case bandNB:
		return 13
	case bandMB, bandWB:
		return 17
	case bandSWB:
		return 19
	default:
		return 21
	}
}

// interleavedSNR compares planar `got` (per channel) against interleaved `ref`,
// skipping the first `off` frames of got, and returns 10log10(signal/error) dB.
func interleavedSNR(got [][]float32, ref []float32, off, ch int) float64 {
	n := len(got[0]) - off
	if n > len(ref)/ch {
		n = len(ref) / ch
	}
	if n <= 0 {
		return math.Inf(-1)
	}
	var sig, errE float64
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			r := float64(ref[i*ch+c])
			g := float64(got[c][off+i])
			sig += r * r
			e := r - g
			errE += e * e
		}
	}
	if errE == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/errE)
}
