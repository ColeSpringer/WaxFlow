package opus

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/internal/testutil"
)

// encodeAll drives the full Opus encoder over pcm and returns the packets
// and per-packet final ranges.
func encodeAll(t *testing.T, e *Encoder, pcm []float32) (pkts [][]byte, ranges []uint32) {
	t.Helper()
	buf := audio.Buffer{Fmt: e.fmt, N: opusFrameSize}
	_ = buf
	for off := 0; off+opusFrameSize <= len(pcm); off += opusFrameSize {
		b := audio.Get(e.fmt, opusFrameSize)
		copy(b.ChanF(0), pcm[off:off+opusFrameSize])
		b.N = opusFrameSize
		err := e.Encode(b, func(p codec.Packet) error {
			pkt := append([]byte(nil), p.Data...)
			pkts = append(pkts, pkt)
			ranges = append(ranges, e.FinalRange())
			return nil
		})
		audio.Put(b)
		if err != nil {
			t.Fatal(err)
		}
	}
	return pkts, ranges
}

// tocMode reads the coding mode from a packet's TOC byte.
func tocMode(toc byte) int {
	config := int(toc >> 3)
	switch {
	case config < 12:
		return modeSILK
	case config < 16:
		return modeHybrid
	default:
		return modeCELT
	}
}

func synthSpeechFloat(n int) []float32 {
	s16 := synthSpeech(n)
	out := make([]float32, n)
	for i, v := range s16 {
		out[i] = float32(v) / 32768
	}
	return out
}

// encodeAllStereo drives the full Opus encoder over planar stereo input.
func encodeAllStereo(t *testing.T, e *Encoder, left, right []float32) (pkts [][]byte, ranges []uint32) {
	t.Helper()
	for off := 0; off+opusFrameSize <= len(left); off += opusFrameSize {
		b := audio.Get(e.fmt, opusFrameSize)
		copy(b.ChanF(0), left[off:off+opusFrameSize])
		copy(b.ChanF(1), right[off:off+opusFrameSize])
		b.N = opusFrameSize
		err := e.Encode(b, func(p codec.Packet) error {
			pkts = append(pkts, append([]byte(nil), p.Data...))
			ranges = append(ranges, e.FinalRange())
			return nil
		})
		audio.Put(b)
		if err != nil {
			t.Fatal(err)
		}
	}
	return pkts, ranges
}

// TestOpusEncoderStereoSILKHybrid reference-verifies the stereo SILK and
// hybrid encode paths (mono is covered by TestSILKEncoderReferenceDecode and
// TestOpusEncoderModeSelection; stereo exercises LR-to-MS prediction coding
// and the stereo hybrid stitch). Each stream goes through the reference
// libopus decoder with per-packet range-coder final-state verification, must
// land in the expected mode, and must decode with our own decoder to the
// same sample count.
func TestOpusEncoderStereoSILKHybrid(t *testing.T) {
	opusDemo, _ := testutil.OpusTools(t)
	const n = 3 * 48000
	left := synthSpeechFloat(n)
	// A plausibly-recorded second channel: attenuated, slightly delayed, with
	// its own noise floor, so the stereo predictors have real work.
	right := make([]float32, n)
	rng := uint32(99)
	for i := range right {
		d := i - 17
		if d < 0 {
			d = 0
		}
		rng = rng*1664525 + 1013904223
		right[i] = 0.8*left[d] + 0.01*(float32(int32(rng))/float32(1<<31))
	}

	for _, tc := range []struct {
		name    string
		bitrate int
		force   int // modeNone = analyser-driven
		want    int
	}{
		// The stereo SILK case is forced and runs at 12 kb/s: at the AUDIO
		// application libopus itself prefers hybrid for stereo speech at any
		// higher rate (verified against opus_demo), and above 12k the
		// bandwidth selection lands past wideband, which SILK-only cannot
		// code. The case exercises exactly the code under test: LR-to-MS
		// conversion, stereo predictor coding, and the mid/side layering.
		{"stereo-12k-silk", 12000, modeSILK, modeSILK},
		{"stereo-48k-hybrid", 48000, modeNone, modeHybrid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: SampleRate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
			e, err := NewEncoder(f, &EncoderOptions{Bitrate: tc.bitrate, Signal: SignalVoice})
			if err != nil {
				t.Fatal(err)
			}
			e.forcedMode = tc.force
			pkts, ranges := encodeAllStereo(t, e, left, right)

			counts := map[int]int{}
			for _, p := range pkts[len(pkts)/2:] {
				counts[tocMode(p[0])]++
			}
			t.Logf("%s: modes over 2nd half: silk=%d hybrid=%d celt=%d",
				tc.name, counts[modeSILK], counts[modeHybrid], counts[modeCELT])
			half := len(pkts) / 2
			if counts[tc.want] < half*3/4 {
				t.Errorf("mode %d chosen for %d/%d packets, want >= 3/4", tc.want, counts[tc.want], half)
			}

			// Reference decode with per-packet range verification.
			bitPath := filepath.Join(t.TempDir(), "stereo.bit")
			if err := testutil.WriteOpusBitstream(bitPath, pkts, ranges); err != nil {
				t.Fatal(err)
			}
			refOut := testutil.OpusDemoDecode(t, opusDemo, bitPath, 48000, 2)
			if len(refOut) != len(pkts)*opusFrameSize*2 {
				t.Fatalf("reference decoded %d samples, want %d", len(refOut), len(pkts)*opusFrameSize*2)
			}

			// Our decoder accepts the stream and agrees on length.
			cfg := Config{Channels: 2}
			dec, err := NewDecoder(cfg, cfg.Format())
			if err != nil {
				t.Fatal(err)
			}
			decoded := 0
			for i, pkt := range pkts {
				err := dec.Decode(pkt, func(b *audio.Buffer) error {
					decoded += b.N
					return nil
				})
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
			}
			if decoded != len(pkts)*opusFrameSize {
				t.Fatalf("our decode %d frames, want %d", decoded, len(pkts)*opusFrameSize)
			}
		})
	}
}

// TestOpusEncoderModeSelection checks that the analysis-driven decision
// lands in the expected mode per content and rate, and that every stream
// passes the reference decoder's per-packet final-range verification.
func TestOpusEncoderModeSelection(t *testing.T) {
	opusDemo, _ := testutil.OpusTools(t)
	speech := synthSpeechFloat(3 * 48000)
	music := make([]float32, 3*48000)
	for i := range music {
		ti := float64(i) / 48000
		music[i] = float32(0.25*math.Sin(2*math.Pi*523*ti) +
			0.2*math.Sin(2*math.Pi*659*ti) +
			0.18*math.Sin(2*math.Pi*880*ti) +
			0.1*math.Sin(2*math.Pi*1320*ti+0.5*math.Sin(2*math.Pi*3*ti)))
	}

	for _, tc := range []struct {
		name    string
		pcm     []float32
		bitrate int
		signal  Signal
		want    int
	}{
		// The voice hint pins the estimate like OPUS_SET_SIGNAL(VOICE): the
		// synthetic speech here is harmonic enough that the MLP calls it
		// music, which is a property of the corpus, not the decision logic
		// (the real-speech gates cover the analyser's own decision).
		{"speech-12k-silk", speech, 12000, SignalVoice, modeSILK},
		{"speech-32k-hybrid", speech, 32000, SignalVoice, modeHybrid},
		{"music-96k-celt", music, 96000, SignalAuto, modeCELT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audio.Format{Rate: SampleRate, Channels: 1, Type: audio.Float, BitDepth: 32}
			e, err := NewEncoder(f, &EncoderOptions{Bitrate: tc.bitrate, Signal: tc.signal})
			if err != nil {
				t.Fatal(err)
			}
			pkts, ranges := encodeAll(t, e, tc.pcm)

			// Count modes over the second half (the decision needs some
			// analysis history to settle).
			counts := map[int]int{}
			for _, p := range pkts[len(pkts)/2:] {
				counts[tocMode(p[0])]++
			}
			t.Logf("%s: modes over 2nd half: silk=%d hybrid=%d celt=%d",
				tc.name, counts[modeSILK], counts[modeHybrid], counts[modeCELT])
			half := len(pkts) / 2
			if counts[tc.want] < half*3/4 {
				t.Errorf("mode %d chosen for %d/%d packets, want >= 3/4", tc.want, counts[tc.want], half)
			}

			// Reference decode with per-packet range verification.
			bitPath := filepath.Join(t.TempDir(), "modes.bit")
			if err := testutil.WriteOpusBitstream(bitPath, pkts, ranges); err != nil {
				t.Fatal(err)
			}
			refOut := testutil.OpusDemoDecode(t, opusDemo, bitPath, 48000, 1)
			if len(refOut) != len(pkts)*opusFrameSize {
				t.Fatalf("reference decoded %d samples, want %d", len(refOut), len(pkts)*opusFrameSize)
			}

			// Our decoder accepts the stream too.
			cfg := Config{Channels: 1}
			dec, err := NewDecoder(cfg, cfg.Format())
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for i, pkt := range pkts {
				err := dec.Decode(pkt, func(b *audio.Buffer) error {
					n += b.N
					return nil
				})
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
			}
			if n != len(pkts)*opusFrameSize {
				t.Fatalf("our decode %d samples, want %d", n, len(pkts)*opusFrameSize)
			}
		})
	}
}

// TestOpusEncoderModeSwitch forces mode transitions mid-stream and checks
// the redundancy-carrying switch packets satisfy the reference decoder's
// final-range verification in both directions.
func TestOpusEncoderModeSwitch(t *testing.T) {
	opusDemo, _ := testutil.OpusTools(t)
	pcm := synthSpeechFloat(3 * 48000)
	f := audio.Format{Rate: SampleRate, Channels: 1, Type: audio.Float, BitDepth: 32}
	e, err := NewEncoder(f, &EncoderOptions{Bitrate: 32000, Signal: SignalVoice})
	if err != nil {
		t.Fatal(err)
	}

	var pkts [][]byte
	var ranges []uint32
	frames := 0
	for off := 0; off+opusFrameSize <= len(pcm); off += opusFrameSize {
		// SILK/hybrid for 1s, CELT for 1s, then back: both switch directions
		// exercise the redundant frames and the prefills.
		switch {
		case frames < 50:
			e.forcedMode = modeSILK
		case frames < 100:
			e.forcedMode = modeCELT
		default:
			e.forcedMode = modeSILK
		}
		b := audio.Get(f, opusFrameSize)
		copy(b.ChanF(0), pcm[off:off+opusFrameSize])
		b.N = opusFrameSize
		err := e.Encode(b, func(p codec.Packet) error {
			pkts = append(pkts, append([]byte(nil), p.Data...))
			ranges = append(ranges, e.FinalRange())
			return nil
		})
		audio.Put(b)
		if err != nil {
			t.Fatal(err)
		}
		frames++
	}

	// The stream must actually contain both a SILK-carrying mode and CELT
	// (forced SILK at 32 kb/s upgrades to hybrid via the bandwidth rule).
	sawLP, sawCELT := false, false
	for _, p := range pkts {
		switch tocMode(p[0]) {
		case modeSILK, modeHybrid:
			sawLP = true
		case modeCELT:
			sawCELT = true
		}
	}
	if !sawLP || !sawCELT {
		t.Fatalf("stream missing a mode: silk/hybrid=%v celt=%v", sawLP, sawCELT)
	}

	bitPath := filepath.Join(t.TempDir(), "switch.bit")
	if err := testutil.WriteOpusBitstream(bitPath, pkts, ranges); err != nil {
		t.Fatal(err)
	}
	refOut := testutil.OpusDemoDecode(t, opusDemo, bitPath, 48000, 1)
	if len(refOut) != len(pkts)*opusFrameSize {
		t.Fatalf("reference decoded %d samples, want %d", len(refOut), len(pkts)*opusFrameSize)
	}

	cfg := Config{Channels: 1}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, pkt := range pkts {
		err := dec.Decode(pkt, func(b *audio.Buffer) error {
			n += b.N
			return nil
		})
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if n != len(pkts)*opusFrameSize {
		t.Fatalf("our decode %d samples, want %d", n, len(pkts)*opusFrameSize)
	}
	t.Logf("mode-switch stream: %d packets verified against libopus (final ranges incl. redundancy)", len(pkts))
}
