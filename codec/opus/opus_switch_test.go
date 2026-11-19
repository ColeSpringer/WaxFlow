package opus

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/internal/testutil"
)

// switchRun codes mono pcm with no look-ahead, as opus_demo does, one frame
// per call. each runs ahead of every frame with its index, and the CELT
// frames coded for each packet come back beside the packets.
func switchRun(t *testing.T, pcm []float32, opts *EncoderOptions, each func(e *Encoder, frame int)) ([][]byte, [][]celtCall) {
	t.Helper()
	f := audio.Format{Rate: SampleRate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	e, err := NewEncoder(f, opts)
	if err != nil {
		t.Fatal(err)
	}
	e.lookahead = 0
	var frame []celtCall
	e.celt.calls = &frame
	var pkts [][]byte
	var calls [][]celtCall
	emit := func(p codec.Packet) error {
		pkts = append(pkts, append([]byte(nil), p.Data...))
		calls = append(calls, slices.Clone(frame))
		frame = frame[:0]
		return nil
	}
	b := audio.Get(f, opusFrameSize)
	defer audio.Put(b)
	b.N = opusFrameSize
	for off := 0; off+opusFrameSize <= len(pcm); off += opusFrameSize {
		each(e, off/opusFrameSize)
		copy(b.ChanF(0), pcm[off:])
		if err := e.Encode(b, emit); err != nil {
			t.Fatal(err)
		}
	}
	return pkts, calls
}

// TestCELTFramesAroundAModeSwitchAreCodedUnderTheReferencesSettings pins
// what opus_encoder.c sets for each CELT frame at a switch: prediction 2
// on a frame with a CELT layer, the prefill under the frame's own start
// band and that prediction, prediction 0 for the frame behind a reset and
// for a SILK to CELT redundant pair, and a SILK-only frame coding its
// redundant frame under whatever the frame before left.
func TestCELTFramesAroundAModeSwitchAreCodedUnderTheReferencesSettings(t *testing.T) {
	pcm := testutil.StartupTone(8*opusFrameSize, 1)[0]
	plain := func(start int) celtCall { return celtCall{opusFrameSize, start, false, false} }
	s, h, c := modeSILK, modeSILK, modeCELT // h: forced SILK that a fullband frame makes hybrid
	for _, tc := range []struct {
		name      string
		bandwidth int   // forced
		modes     []int // forced, per frame
		want      map[int][]celtCall
	}{
		{"CELT to hybrid and back", bandwidthFull, []int{c, c, h, h, h, c, c, c},
			map[int][]celtCall{
				1: {plain(0)},
				// The redundant frame, the prefill, the frame behind the reset.
				2: {{240, 0, false, false}, {120, 17, false, false}, {opusFrameSize, 17, true, true}},
				3: {plain(17)},
				// The frame the switch waits on, then the prefill and the redundant frame.
				5: {plain(17), {120, 0, true, true}, {240, 0, true, true}},
				6: {plain(0)},
			}},
		{"SILK to CELT and straight back", bandwidthWide, []int{s, s, c, s, s},
			map[int][]celtCall{
				1: nil,
				2: {{120, 0, true, true}, {240, 0, true, true}},
				// No frame with a CELT layer came between: the pair's setting stands.
				3: {{240, 0, true, true}},
				4: nil,
			}},
		{"CELT to SILK", bandwidthWide, []int{c, c, s, s},
			map[int][]celtCall{1: {plain(0)}, 2: {{240, 0, false, false}}, 3: nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkts, calls := switchRun(t, pcm[:len(tc.modes)*opusFrameSize], &EncoderOptions{Bitrate: 32000},
				func(e *Encoder, frame int) { e.forcedMode, e.forcedBandwidth = tc.modes[frame], tc.bandwidth })
			for i, want := range tc.want {
				if !slices.Equal(calls[i], want) {
					t.Errorf("frame %d (%s): CELT frames coded as %v, want %v (samples, start band, intra, no pitch filter)",
						i, tocName(pkts[i][0]), calls[i], want)
				}
			}
		})
	}
}

// tocName spells a TOC byte's mode for a message.
func tocName(toc byte) string {
	return [...]string{modeSILK: "SILK", modeHybrid: "hybrid", modeCELT: "CELT"}[tocMode(toc)]
}

// switchSpeech is a voiced buzz under a syllable envelope with bursts of
// hiss between, which the analyser calls speech.
func switchSpeech(frames, _ int) [][]float32 {
	return [][]float32{synthSpeechFloat(frames)}
}

// TestModeSwitchesAgainstTheReference sweeps the bit rate through the mode
// thresholds as opus_demo -sweep does, once from 6 kbit/s in steps that
// leave it off a whole packet size, and holds the stream to libopus's:
// the same mode and bandwidth on every frame, and behind every mode switch
// two frames no worse than libopus's by more than half. The bytes are not
// compared: this CELT is not bit-exact with libopus's float build on any
// stream. It self-skips without the reference tools.
func TestModeSwitchesAgainstTheReference(t *testing.T) {
	opusDemo, _ := testutil.OpusTools(t)
	const to, bound = 40000, 1.5
	switches := map[string]int{}
	inputs := []struct {
		name string
		gen  func(frames, chans int) [][]float32
	}{{"tone", testutil.StartupTone}, {"plucked", testutil.StartupPlucked}, {"hiss", startupHiss}, {"speech", switchSpeech}}
	for _, sweep := range [][2]int{{6000, 700}, {10000, 400}} {
		from, step := sweep[0], sweep[1]
		for _, in := range inputs {
			t.Run(fmt.Sprintf("%s/from%d", in.name, from), func(t *testing.T) {
				pcm := in.gen(6*SampleRate, 1)[0]
				ints := make([]int16, len(pcm))
				for i, v := range pcm {
					ints[i] = int16(math.RoundToEven(float64(v) * 32768))
					pcm[i] = float32(ints[i]) / 32768
				}
				rate, by := from, step
				ours, _ := switchRun(t, pcm, &EncoderOptions{Bitrate: from, Complexity: 10, LSBDepth: 16}, func(e *Encoder, frame int) {
					if frame == 0 {
						return
					}
					rate += by
					if rate > to || rate < from {
						by = -by
					}
					e.bitrate = rate
				})
				lib := testutil.OpusDemoEncode(t, opusDemo, ints, SampleRate, 1, from, 10,
					"-sweep", strconv.Itoa(step), "-sweep_max", strconv.Itoa(to))
				n := min(len(ours), len(lib))
				src := [][]float32{pcm}
				decOurs, decLib := startupDecode(t, ours[:n], 1), startupDecode(t, lib[:n], 1)
				for i := range n {
					if ours[i][0]>>3 != lib[i][0]>>3 {
						t.Fatalf("frame %d: TOC configuration %d, libopus %d", i, ours[i][0]>>3, lib[i][0]>>3)
					}
					if i == 0 || i+2 > n-1 || tocMode(lib[i][0]) == tocMode(lib[i-1][0]) {
						continue
					}
					switches[tocName(lib[i-1][0])+" to "+tocName(lib[i][0])]++
					a := testutil.ErrorLevelsOf(src, decOurs, i*opusFrameSize, (i+2)*opusFrameSize).RMS
					b := testutil.ErrorLevelsOf(src, decLib, i*opusFrameSize, (i+2)*opusFrameSize).RMS
					if a > bound*b {
						t.Errorf("two frames behind the switch at frame %d (%s to %s): rms error %.5f, libopus %.5f: over %.1f times",
							i, tocName(lib[i-1][0]), tocName(lib[i][0]), a, b, bound)
					}
					if testing.Verbose() {
						t.Logf("frame %d %s to %s: rms error %.5f, libopus %.5f (%.2fx)", i, tocName(lib[i-1][0]), tocName(lib[i][0]), a, b, a/b)
					}
				}
			})
		}
	}
	for _, want := range []string{"SILK to CELT", "CELT to SILK", "SILK to hybrid", "hybrid to SILK", "hybrid to CELT", "CELT to hybrid"} {
		if switches[want] == 0 {
			t.Errorf("the sweeps never switched %s: %v", want, fmt.Sprint(switches))
		}
	}
}

// TestACBRFrameIsDecidedOnTheRateItsPacketCarries asks for 25 kbit/s, which
// rounds to a 63 byte packet, and for the 25.2 kbit/s that packet carries:
// the reference decides both on the second, so the streams are one.
func TestACBRFrameIsDecidedOnTheRateItsPacketCarries(t *testing.T) {
	pcm := [][]float32{synthSpeechFloat(SampleRate)}
	asked := startupStream(t, pcm, &EncoderOptions{Bitrate: 25000, Signal: SignalVoice}, nil)
	carried := startupStream(t, pcm, &EncoderOptions{Bitrate: 25200, Signal: SignalVoice}, nil)
	for i := range asked {
		if !slices.Equal(asked[i], carried[i]) {
			t.Fatalf("packet %d (%s) differs between a 25000 and a 25200 bit/s request", i, tocName(asked[i][0]))
		}
	}
}

// TestSixKilobitsIsRoomForSILK pins the floor under which a 20 ms frame is
// CELT whatever the content: 6 kbit/s of packet, not 9.
func TestSixKilobitsIsRoomForSILK(t *testing.T) {
	pcm := [][]float32{synthSpeechFloat(SampleRate)}
	for i, pkt := range startupStream(t, pcm, &EncoderOptions{Bitrate: 6000, Signal: SignalVoice}, nil) {
		if tocMode(pkt[0]) != modeSILK {
			t.Fatalf("packet %d of a 6 kbit/s voice stream is %s", i, tocName(pkt[0]))
		}
	}
}
