package opus

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// synthSpeech generates a speech-like test signal at 48 kHz: a decaying
// harmonic series over a 110 Hz pitch with a slow AM envelope and a little
// noise, which the SILK analysis should classify as voiced most of the time.
func synthSpeech(n int) []int16 {
	out := make([]int16, n)
	rng := uint32(12345)
	for i := 0; i < n; i++ {
		t := float64(i) / 48000.0
		f0 := 110.0 + 20.0*math.Sin(2*math.Pi*0.8*t)
		var v float64
		for h := 1; h <= 12; h++ {
			v += math.Sin(2*math.Pi*f0*float64(h)*t+float64(h)) / float64(h)
		}
		env := 0.5 + 0.45*math.Sin(2*math.Pi*1.3*t)
		rng = rng*1664525 + 1013904223
		noise := (float64(int32(rng))/float64(1<<31))*0.02 - 0.0
		v = (v*0.28 + noise) * env * 12000
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}

// silkTOC builds the TOC byte for a SILK-only 20 ms mono packet at the given
// internal bandwidth (RFC 6716 section 3.1).
func silkTOC(fsKHz int, stereo bool) byte {
	var config int
	switch fsKHz {
	case 8:
		config = 1 // NB 20 ms
	case 12:
		config = 5 // MB 20 ms
	default:
		config = 9 // WB 20 ms
	}
	toc := byte(config << 3)
	if stereo {
		toc |= 1 << 2
	}
	return toc
}

// encodeSILKFrames drives the SILK encoder over 20 ms frames and returns one
// Opus packet per frame.
func encodeSILKFrames(t *testing.T, pcm []int16, fsKHz int, bitrate int32, useCBR bool, complexity int) [][]byte {
	t.Helper()
	e := newSILKEncoder(1)
	ctrl := &silkEncControl{
		nChannelsAPI:              1,
		nChannelsInternal:         1,
		apiSampleRate:             48000,
		maxInternalSampleRate:     fsKHz * 1000,
		minInternalSampleRate:     fsKHz * 1000,
		desiredInternalSampleRate: fsKHz * 1000,
		payloadSizeMS:             20,
		bitRate:                   bitrate,
		complexity:                complexity,
		useCBR:                    useCBR,
	}
	const frame = 960
	var pkts [][]byte
	for off := 0; off+frame <= len(pcm); off += frame {
		buf := make([]byte, maxFrameBytes)
		enc := newRangeEncoder(buf)
		// CBR is enforced through the packet bit budget, exactly as
		// opus_encoder.c drives silk_Encode.
		if useCBR {
			ctrl.maxBits = int(bitrate) * 20 / 1000
		} else {
			ctrl.maxBits = (maxFrameBytes - 1) * 8
		}
		nBytesOut := 0
		e.encode(ctrl, pcm[off:off+frame], frame, enc, &nBytesOut, 0, -1)
		if nBytesOut <= 0 {
			t.Fatalf("frame at %d: no payload produced", off)
		}
		enc.shrink(nBytesOut)
		enc.done()
		payload := enc.payload()
		pkt := make([]byte, 1+len(payload))
		pkt[0] = silkTOC(fsKHz, false)
		copy(pkt[1:], payload)
		pkts = append(pkts, pkt)
	}
	return pkts
}

// decodePackets runs packets through the public decoder and concatenates the
// mono output.
func decodePackets(t *testing.T, pkts [][]byte) []float32 {
	t.Helper()
	cfg := Config{Channels: 1, PreSkip: 0}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	var out []float32
	for i, pkt := range pkts {
		err := dec.Decode(pkt, func(b *audio.Buffer) error {
			out = append(out, b.ChanF(0)[:b.N]...)
			return nil
		})
		if err != nil {
			t.Fatalf("packet %d: decode failed: %v", i, err)
		}
	}
	return out
}

// alignedSNR finds the best alignment within maxLag samples and reports the
// SNR of the aligned overlap, both signals normalized to float in [-1,1).
func alignedSNR(ref []int16, got []float32, maxLag int) (float64, int) {
	bestLag, bestCorr := 0, math.Inf(-1)
	n := len(got) - maxLag
	if n > 48000 {
		n = 48000
	}
	start := 24000 // skip startup transients
	for lag := 0; lag < maxLag; lag++ {
		var corr float64
		for i := start; i < start+n/2; i += 2 {
			corr += float64(ref[i]) / 32768.0 * float64(got[i+lag])
		}
		if corr > bestCorr {
			bestCorr = corr
			bestLag = lag
		}
	}
	var sig, err float64
	for i := start; i < start+n/2; i++ {
		r := float64(ref[i]) / 32768.0
		d := r - float64(got[i+bestLag])
		sig += r * r
		err += d * d
	}
	if err == 0 {
		return math.Inf(1), bestLag
	}
	return 10 * math.Log10(sig/err), bestLag
}

// TestSILKEncodeRoundTrip encodes speech-like audio in every SILK-only
// bandwidth and verifies our bit-exact decoder accepts the streams with
// plausible reconstruction quality.
func TestSILKEncodeRoundTrip(t *testing.T) {
	pcm := synthSpeech(4 * 48000)
	for _, tc := range []struct {
		fsKHz   int
		bitrate int32
		minSNR  float64
	}{
		{8, 20000, 3.0},
		{12, 28000, 3.0},
		{16, 36000, 3.0},
	} {
		pkts := encodeSILKFrames(t, pcm, tc.fsKHz, tc.bitrate, false, 10)
		out := decodePackets(t, pkts)
		wantSamples := (len(pcm) / 960) * 960
		if len(out) != wantSamples {
			t.Fatalf("fs %d: decoded %d samples, want %d", tc.fsKHz, len(out), wantSamples)
		}
		// The encoder-side signal is band-limited to fsKHz/2, so the aligned
		// SNR against the full-band original is bounded by the out-of-band
		// energy; the threshold is a plausibility floor, not a quality gate
		// (opus_compare owns quality).
		snr, lag := alignedSNR(pcm, out, 2000)
		t.Logf("fs %d kHz @ %d bps: aligned SNR %.1f dB (lag %d), %d packets, mean %d bytes",
			tc.fsKHz, tc.bitrate, snr, lag, len(pkts), meanLen(pkts))
		if snr < tc.minSNR {
			t.Errorf("fs %d: aligned SNR %.1f dB below plausibility floor %.1f", tc.fsKHz, snr, tc.minSNR)
		}
	}
}

func meanLen(pkts [][]byte) int {
	total := 0
	for _, p := range pkts {
		total += len(p)
	}
	return total / len(pkts)
}

// TestSILKEncodeCBRRate checks that CBR mode tracks the target bitrate.
func TestSILKEncodeCBRRate(t *testing.T) {
	pcm := synthSpeech(4 * 48000)
	const bitrate = 32000
	pkts := encodeSILKFrames(t, pcm, 16, bitrate, true, 8)
	mean := float64(meanLen(pkts)) * 8 * 50 // bits/packet * 50 packets/s
	if mean < 0.8*bitrate || mean > 1.2*bitrate {
		t.Errorf("CBR mean rate %.0f bps, want within 20%% of %d", mean, bitrate)
	}
}

var _ = codec.Packet{} // keep the codec import if assertions change
