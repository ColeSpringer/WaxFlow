package opus

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestDecodeVsFFmpeg is the full Opus decoder conformance oracle: encode a
// speech-like signal to Opus forcing each internal mode (SILK, hybrid, CELT via
// bandwidth/application), decode with our decoder and with ffmpeg, and require a
// high SNR. This exercises the SILK, hybrid, and CELT paths end to end.
func TestDecodeVsFFmpeg(t *testing.T) {
	voice := "aevalsrc=" +
		"0.35*sin(2*PI*(180+8*sin(2*PI*5*t))*t)+0.2*sin(2*PI*(360+16*sin(2*PI*5*t))*t)+" +
		"0.12*sin(2*PI*(540)*t)+0.06*sin(2*PI*(1500)*t)+0.03*random(0)" +
		":s=48000:d=1.2:c=mono"
	voiceStereo := "aevalsrc=" +
		"0.35*sin(2*PI*(180+8*sin(2*PI*5*t))*t)+0.15*sin(2*PI*1200*t)|" +
		"0.3*sin(2*PI*(220+8*sin(2*PI*4*t))*t)+0.12*sin(2*PI*900*t)" +
		":s=48000:d=1.0:c=stereo"

	cases := []struct {
		name     string
		src      string
		channels string
		app      string
		bitrate  string
		cutoff   string
		wantMode int
		minSNR   float64
	}{
		// SILK decode is a faithful port of the integer reference, so it matches
		// libopus essentially bit-for-bit (very high SNR). Hybrid and CELT are
		// float and match to a lower but ample margin.
		{"silk_nb_mono", voice, "1", "voip", "12k", "4000", modeSILK, 40},
		{"silk_mb_mono", voice, "1", "voip", "16k", "6000", modeSILK, 40},
		{"silk_wb_mono", voice, "1", "voip", "20k", "8000", modeSILK, 40},
		{"silk_wb_stereo", voiceStereo, "2", "voip", "24k", "8000", modeSILK, 40},
		{"hybrid_fb_mono", voice, "1", "voip", "40k", "20000", modeHybrid, 12},
		{"celt_fb_mono", voice, "1", "lowdelay", "64k", "20000", modeCELT, 18},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decodeCase(t, tc.src, tc.channels, tc.app, tc.bitrate, tc.cutoff, tc.wantMode, tc.minSNR)
		})
	}
}

func decodeCase(t *testing.T, src, channels, app, bitrate, cutoff string, wantMode int, minSNR float64) {
	ff := testutil.FFmpeg(t)
	dir := t.TempDir()
	opusPath := filepath.Join(dir, "s.opus")
	refPath := filepath.Join(dir, "ref.f32")

	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", src,
		"-c:a", "libopus", "-application", app, "-b:a", bitrate, "-vbr", "off"}
	if cutoff != "" {
		args = append(args, "-cutoff", cutoff)
	}
	args = append(args, opusPath)
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg encode: %v\n%s", err, out)
	}
	nch := 2
	if channels == "1" {
		nch = 1
	}
	// Decode with libopus (the fixed-point reference our port mirrors), not
	// ffmpeg's independent native decoder, so the SILK resampler and arithmetic
	// match bit-for-bit rather than only approximately.
	if out, err := exec.Command(ff, "-v", "error", "-y", "-c:a", "libopus", "-i", opusPath,
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

	data, err := os.ReadFile(opusPath)
	if err != nil {
		t.Fatal(err)
	}
	dmx, err := ogg.NewDemuxer(container.BytesSource(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracks := dmx.Tracks()
	cfg, err := ParseOpusHead(tracks[0].CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()

	got := make([][]float32, cfg.Channels)
	var pkt container.Packet
	sawMode := -1
	for {
		if err := dmx.ReadPacket(&pkt); err != nil {
			break
		}
		if frames, ferr := splitPacket(pkt.Data); ferr == nil && len(frames) > 0 {
			sawMode = frames[0].cfg.mode
		}
		err := dec.Decode(pkt.Data, func(b *audio.Buffer) error {
			for c := 0; c < cfg.Channels; c++ {
				got[c] = append(got[c], b.ChanF(c)...)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	if len(got[0]) == 0 {
		t.Fatal("no audio decoded")
	}
	if sawMode != wantMode {
		t.Fatalf("stream used mode %d, wanted %d (adjust encoder args)", sawMode, wantMode)
	}

	bestSNR, bestOff := math.Inf(-1), 0
	for off := cfg.PreSkip - 16; off <= cfg.PreSkip+16; off++ {
		if off < 0 {
			continue
		}
		if snr := interleavedSNR(got, ref, off, nch); snr > bestSNR {
			bestSNR, bestOff = snr, off
		}
	}
	t.Logf("%s: best SNR %.1f dB at offset %d (pre-skip %d)", t.Name(), bestSNR, bestOff, cfg.PreSkip)
	if bestSNR < minSNR {
		t.Errorf("decode SNR %.1f dB too low (want >=%.1f)", bestSNR, minSNR)
	}
}
