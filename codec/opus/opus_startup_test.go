package opus

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/internal/testutil"
)

// startupStream encodes pcm the way the engine drives the encoder (constant
// bit rate, one 20 ms buffer per call, then Finish) and returns the packets.
// each, when set, runs ahead of every frame with that frame's index.
func startupStream(t *testing.T, pcm [][]float32, opts *EncoderOptions, each func(e *Encoder, next int)) [][]byte {
	t.Helper()
	chans := len(pcm)
	f := audio.Format{Rate: SampleRate, Channels: chans, Layout: audio.DefaultLayout(chans), Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, opts)
	if err != nil {
		t.Fatal(err)
	}
	var pkts [][]byte
	emit := func(p codec.Packet) error {
		pkts = append(pkts, append([]byte(nil), p.Data...))
		if each != nil {
			each(enc, len(pkts))
		}
		return nil
	}
	if each != nil {
		each(enc, 0)
	}
	b := audio.Get(f, opusFrameSize)
	defer audio.Put(b)
	for off := 0; off < len(pcm[0]); off += opusFrameSize {
		b.N = min(opusFrameSize, len(pcm[0])-off)
		for c := range chans {
			copy(b.ChanF(c), pcm[c][off:])
		}
		if err := enc.Encode(b, emit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	return pkts
}

// startupDecode decodes one continuous packet run and drops the pre-skip, so
// output sample i is source sample i.
func startupDecode(t *testing.T, pkts [][]byte, chans int) [][]float32 {
	t.Helper()
	cfg := Config{Channels: chans, PreSkip: EncoderDelay}
	dec, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]float32, chans)
	for _, p := range pkts {
		err := dec.Decode(p, func(b *audio.Buffer) error {
			for c := range chans {
				out[c] = append(out[c], b.ChanF(c)...)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for c := range out {
		out[c] = out[c][EncoderDelay:]
	}
	return out
}

// startupInput is one of the start-up tests' inputs.
type startupInput struct {
	name string
	gen  func(frames, chans int) [][]float32
}

var startupInputs = []startupInput{{"tone", testutil.StartupTone}, {"plucked", testutil.StartupPlucked}}

// startupCells runs one check over the low rates, the channel counts and
// the inputs the start-up tests cover. Each input is two periods and a
// second long.
func startupCells(t *testing.T, run func(t *testing.T, pcm [][]float32, bitrate int)) {
	startupCellsOf(t, startupInputs, run)
}

func startupCellsOf(t *testing.T, inputs []startupInput, run func(t *testing.T, pcm [][]float32, bitrate int)) {
	rates := []int{24, 32, 48}
	if raceEnabled {
		rates = rates[1:2] // the race detector has one rate's worth to find
	}
	for _, in := range inputs {
		for _, chans := range []int{1, 2} {
			pcm := in.gen(2*testutil.StartupPeriod+SampleRate, chans)
			for _, kbps := range rates {
				t.Run(fmt.Sprintf("%s/%dch/%dk", in.name, chans, kbps), func(t *testing.T) {
					run(t, pcm, kbps*1000)
				})
			}
		}
	}
}

// startupSteady is where a start-up stretch is measured again in steady
// state: two periods on.
const startupSteady = 2 * testutil.StartupPeriod

// TestALowRateStreamStartsAtItsSteadyStateError holds the first 500 ms of a
// stream to the error of the same samples two periods on.
func TestALowRateStreamStartsAtItsSteadyStateError(t *testing.T) {
	startupCells(t, func(t *testing.T, pcm [][]float32, bitrate int) {
		const head = SampleRate / 2
		dec := startupDecode(t, startupStream(t, pcm, &EncoderOptions{Bitrate: bitrate}, nil), len(pcm))
		testutil.HoldToSteadyState(t, "first 500 ms",
			testutil.ErrorLevelsOf(pcm, dec, 0, head),
			testutil.ErrorLevelsOf(pcm, dec, startupSteady, startupSteady+head))
	})
}

// startupLeads are what a stream opens on ahead of its sound. Silence to a
// listener: exact zeros, 16-bit dither at the lengths around a second, an
// offset, noise at -100 dB. And a floor that is sound to the analyser, as
// a live recording or a tape transfer opens: noise at the swing of shaped
// dither, and white noise at -70 and -50 dBFS.
var startupLeads = []struct {
	name   string
	frames int
	amp    float64 // of uniform noise
	offset float32
}{
	{"zeros", 15, 0, 0},
	{"dither/40", 40, 1.0 / 32768, 0},
	{"dither/50", 50, 1.0 / 32768, 0},
	{"dither/100", 100, 1.0 / 32768, 0},
	{"offset", 100, 0, 1e-30},
	{"offset16", 100, 0, 0.01},
	{"noise", 100, 1e-5, 0},
	{"shaped dither", 100, 3.7 / 32768, 0}, // a swing of 7.4 steps: sound
	{"bed/-70", 100, 5.5e-4, 0},
	{"bed/-50", 100, 5.5e-3, 0},
}

// startupSilence is frames of such a lead-in.
func startupSilence(frames, chans int, amp float64, offset float32) [][]float32 {
	out := make([][]float32, chans)
	seed := uint32(7)
	for c := range out {
		out[c] = make([]float32, frames*opusFrameSize)
		for i := range out[c] {
			seed = seed*1664525 + 1013904223
			out[c][i] = offset + float32(amp*(float64(seed>>8)/(1<<23)-1))
		}
	}
	return out
}

// startupBehind puts lead ahead of pcm, keeping pcm's length.
func startupBehind(lead, pcm [][]float32) [][]float32 {
	out := make([][]float32, len(pcm))
	for c := range pcm {
		out[c] = append(append([]float32(nil), lead[c]...), pcm[c][:len(pcm[c])-len(lead[c])]...)
	}
	return out
}

// TestASoundBehindALeadInStartsAtItsSteadyStateError puts each lead-in
// ahead of the input and holds the first 500 ms of the input to the same
// samples a period on.
func TestASoundBehindALeadInStartsAtItsSteadyStateError(t *testing.T) {
	startupCells(t, func(t *testing.T, pcm [][]float32, bitrate int) {
		const head = SampleRate / 2
		for _, l := range startupLeads {
			late := startupBehind(startupSilence(l.frames, len(pcm), l.amp, l.offset), pcm)
			lead := l.frames * opusFrameSize
			dec := startupDecode(t, startupStream(t, late, &EncoderOptions{Bitrate: bitrate}, nil), len(late))
			testutil.HoldToSteadyState(t, "first 500 ms of sound behind "+l.name,
				testutil.ErrorLevelsOf(late, dec, lead, lead+head),
				testutil.ErrorLevelsOf(late, dec, lead+testutil.StartupPeriod, lead+testutil.StartupPeriod+head))
		}
	})
}

// TestAStreamShorterThanTheLookAheadStartsAsALongOneDoes holds streams of
// 6 to 49 frames, which give the analyser less than it reads ahead, to the
// error a long stream has over the same frames.
func TestAStreamShorterThanTheLookAheadStartsAsALongOneDoes(t *testing.T) {
	startupCells(t, func(t *testing.T, pcm [][]float32, bitrate int) {
		opts := &EncoderOptions{Bitrate: bitrate}
		long := startupDecode(t, startupStream(t, pcm, opts, nil), len(pcm))
		for _, frames := range []int{6, 11, 14, 18, 25, 35, startLookahead - 10} {
			short := frames * opusFrameSize
			head := make([][]float32, len(pcm))
			for c := range pcm {
				head[c] = pcm[c][:short]
			}
			got := startupDecode(t, startupStream(t, head, opts, nil), len(pcm))
			testutil.HoldToSteadyState(t, fmt.Sprintf("a stream of %d frames", frames),
				testutil.ErrorLevelsOf(pcm, got, 0, short), testutil.ErrorLevelsOf(pcm, long, 0, short))
		}
	})
}

// TestAPickupOfNoiseNeedsTheWholeLookAhead puts 440 ms of noise ahead of
// the input, as a breath or a cymbal leads into a song. Read half a second
// ahead the stream is called speech; the whole second calls it music. The
// first 500 ms of the tone are held to the same samples a period on.
func TestAPickupOfNoiseNeedsTheWholeLookAhead(t *testing.T) {
	startupCellsOf(t, startupInputs[:1], func(t *testing.T, pcm [][]float32, bitrate int) {
		const pickup, head = 22, SampleRate / 2
		late := startupBehind(startupSilence(pickup, len(pcm), 0.05, 0), pcm)
		lead := pickup * opusFrameSize
		dec := startupDecode(t, startupStream(t, late, &EncoderOptions{Bitrate: bitrate}, nil), len(late))
		testutil.HoldToSteadyState(t, "first 500 ms behind the pickup",
			testutil.ErrorLevelsOf(late, dec, lead, lead+head),
			testutil.ErrorLevelsOf(late, dec, lead+testutil.StartupPeriod, lead+testutil.StartupPeriod+head))
	})
}

// startupModes spells the modes of n packets from at.
func startupModes(pkts [][]byte, at, n int) string {
	var b []byte
	for _, p := range pkts[at : at+n] {
		b = append(b, "SHC"[tocMode(p[0])])
	}
	return string(b)
}

// TestASoundBehindABeepAndAPauseStartsAsItDoesAlone puts a 60 ms beep and
// two seconds of silence ahead of noise, which the analyser calls speech,
// and of the tone, which it calls music: either is coded behind the pause
// in the modes it is coded in alone.
func TestASoundBehindABeepAndAPauseStartsAsItDoesAlone(t *testing.T) {
	const beep, pause, run = 3, 100, 30
	for _, chans := range []int{1, 2} {
		sounds := map[string][][]float32{
			"noise": startupSilence(150, chans, 0.05, 0),
			"tone":  testutil.StartupTone(150*opusFrameSize, chans),
		}
		for name, sound := range sounds {
			for _, quiet := range []float64{0, 1.0 / 32768} {
				for _, kbps := range []int{24, 32, 48} {
					t.Run(fmt.Sprintf("%s/%dch/%dk/pause%g", name, chans, kbps, quiet*32768), func(t *testing.T) {
						opts := &EncoderOptions{Bitrate: kbps * 1000}
						alone := startupModes(startupStream(t, sound, opts, nil), 0, run)
						in := make([][]float32, chans)
						lead := startupSilence(pause, chans, quiet, 0)
						for c := range in {
							in[c] = append(append(append([]float32(nil), testutil.StartupTone(beep*opusFrameSize, chans)[c]...), lead[c]...), sound[c]...)
						}
						behind := startupModes(startupStream(t, in, opts, nil), beep+pause, run)
						if behind != alone {
							t.Errorf("behind the pause the first %d frames are coded %s, alone %s", run, behind, alone)
						}
					})
				}
			}
		}
	}
}

// TestOppositeChannelsAreSound feeds a stereo stream whose channels cancel
// in a downmix: once a second is held, every call emits its frame, as for
// any sound.
func TestOppositeChannelsAreSound(t *testing.T) {
	pcm := testutil.StartupTone(4*SampleRate, 2)
	for i, v := range pcm[0] {
		pcm[1][i] = -v
	}
	f := audio.Format{Rate: SampleRate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: 32000})
	if err != nil {
		t.Fatal(err)
	}
	b := audio.Get(f, opusFrameSize)
	defer audio.Put(b)
	b.N = opusFrameSize
	for call := 0; (call+1)*opusFrameSize <= len(pcm[0]); call++ {
		for c := range 2 {
			copy(b.ChanF(c), pcm[c][call*opusFrameSize:])
		}
		n := 0
		if err := enc.Encode(b, func(codec.Packet) error { n++; return nil }); err != nil {
			t.Fatal(err)
		}
		want := 1
		if call < startLookahead {
			want = 0
		}
		if n != want {
			t.Fatalf("call %d emitted %d packets, want %d", call, n, want)
		}
	}
}

// startupLowRates are the rates where an unknown stream would be SILK by
// the reference's prior, and one where it would be CELT.
var startupLowRates = []int{12000, 16000, 24000}

// TestAToneTheAnalyserCannotCallIsCELT holds a tone to CELT where the call
// is not used: in a stream of 30 frames, and in a long one with 300 ms of
// zeros in its first second. Coded as speech it would stay in SILK to the
// end at these rates.
func TestAToneTheAnalyserCannotCallIsCELT(t *testing.T) {
	tone := testutil.StartupTone(4*SampleRate, 1)
	gapped := [][]float32{append([]float32(nil), tone[0]...)}
	clear(gapped[0][10*opusFrameSize : 25*opusFrameSize])
	for _, rate := range startupLowRates {
		for name, pcm := range map[string][][]float32{"30 frames": {tone[0][:30*opusFrameSize]}, "a gap in the first second": gapped} {
			pkts := startupStream(t, pcm, &EncoderOptions{Bitrate: rate}, nil)
			n := len(pcm[0]) / opusFrameSize
			if got := startupModes(pkts, 0, n); strings.Trim(got, "C") != "" {
				t.Errorf("%d bit/s, %s: coded %s", rate, name, got)
			}
		}
	}
}

// TestSpeechTheAnalyserCouldNotCallAtFirstEndsInSILK gives noise, which
// the analyser calls speech, a gap in its first second. With 100 ms of
// zeros the call stands and the stream is SILK or hybrid from its first
// frame; with 300 ms it starts as an unknown, and is SILK or hybrid by its
// last second.
func TestSpeechTheAnalyserCouldNotCallAtFirstEndsInSILK(t *testing.T) {
	for _, rate := range startupLowRates {
		for _, gap := range []int{5, 15} {
			pcm := startupSilence(200, 1, 0.05, 0)
			clear(pcm[0][10*opusFrameSize : (10+gap)*opusFrameSize])
			pkts := startupStream(t, pcm, &EncoderOptions{Bitrate: rate}, nil)
			from := 0
			if gap == 15 {
				from = 150
			}
			if got := startupModes(pkts, from, 200-from); strings.Contains(got[:10], "C") || strings.Contains(got[10+gap:], "C") {
				t.Errorf("%d bit/s, a gap of %d frames: frames %d on coded %s", rate, gap, from, got)
			}
		}
	}
}

// TestSilenceIsUnderFourSixteenBitSteps pins what is silence: zeros, flat
// dither and an offset are; a tone at the swing of the corpora's quietest
// frame, shaped dither, and channels that cancel are sound.
func TestSilenceIsUnderFourSixteenBitSteps(t *testing.T) {
	tone := func(swingDB float64) []float32 {
		x := make([]float32, opusFrameSize)
		for i := range x {
			x[i] = float32(0.5 * math.Pow(10, swingDB/20) * math.Sin(2*math.Pi*440*float64(i)/SampleRate))
		}
		return x
	}
	loud := tone(-6)
	away := make([]float32, opusFrameSize)
	for i, v := range loud {
		away[i] = -v
	}
	for _, tc := range []struct {
		name string
		x    [][]float32
		want bool
	}{
		{"zeros", [][]float32{make([]float32, opusFrameSize)}, true},
		{"flat dither", startupSilence(1, 2, 1.0/32768, 0), true},
		{"an offset", startupSilence(1, 1, 0, 0.3), true},
		{"3.9 steps", [][]float32{tone(20 * math.Log10(3.9/32768))}, true},
		{"4.1 steps", [][]float32{tone(20 * math.Log10(4.1/32768))}, false},
		{"the quietest corpus frame, -73.4 dBFS", [][]float32{tone(-73.4)}, false},
		{"shaped dither", startupSilence(1, 1, 3.7/32768, 0), false},
		{"channels that cancel", [][]float32{loud, away}, false},
		{"one channel silent", [][]float32{make([]float32, opusFrameSize), loud}, false},
	} {
		if got := quiet(tc.x, 0, opusFrameSize, len(tc.x)); got != tc.want {
			t.Errorf("%s: silence is %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAHalfSecondPauseIsNotAnOnset runs a tone, half a second of zeros and
// the tone again: every call emits its frame, and the analyser carries its
// history across, where a pause of a second starts it afresh.
func TestAHalfSecondPauseIsNotAnOnset(t *testing.T) {
	for _, tc := range []struct {
		pause int
		fresh bool
	}{{25, false}, {50, true}} {
		pcm := testutil.StartupTone(250*opusFrameSize, 1)
		clear(pcm[0][100*opusFrameSize : (100+tc.pause)*opusFrameSize])
		f := audio.Format{Rate: SampleRate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
		enc, err := NewEncoder(f, &EncoderOptions{Bitrate: 32000})
		if err != nil {
			t.Fatal(err)
		}
		b := audio.Get(f, opusFrameSize)
		b.N = opusFrameSize
		for call := range 250 {
			copy(b.ChanF(0), pcm[0][call*opusFrameSize:])
			n := 0
			if err := enc.Encode(b, func(codec.Packet) error { n++; return nil }); err != nil {
				t.Fatal(err)
			}
			if want := min(1, max(0, call-startLookahead+1)); n != want {
				t.Fatalf("pause of %d frames: call %d emitted %d packets, want %d", tc.pause, call, n, want)
			}
		}
		audio.Put(b)
		// The analyser has read all 250 frames: 100 of sound ahead of the pause.
		if fresh := enc.scout.count < 200-tc.pause; fresh != tc.fresh {
			t.Errorf("pause of %d frames: the analyser has %d frames of history, started afresh %v, want %v",
				tc.pause, enc.scout.count, fresh, tc.fresh)
		}
	}
}

// startupSpeechLike is noise with a little of the tone in it: the analyser
// calls it speech, and reads the second ahead at 0.16 to 0.29 music.
func startupSpeechLike(frames int) [][]float32 {
	pcm := startupSilence(frames, 1, 0.05, 0)
	for i, v := range testutil.StartupTone(frames*opusFrameSize, 1)[0] {
		pcm[0][i] += 0.08 * v
	}
	return pcm
}

// TestSteadySpeechHasNoCELTFrame holds the music-ahead rule off sound the
// second ahead reads as speech, at the rate where a weak reading would
// already choose CELT.
func TestSteadySpeechHasNoCELTFrame(t *testing.T) {
	pkts := startupStream(t, startupSpeechLike(300), &EncoderOptions{Bitrate: 44000}, nil)
	if got := startupModes(pkts, 0, 300); strings.Contains(got, "C") {
		t.Errorf("coded %s", got)
	}
}

// startupBuzz is a pitched buzz under a syllable envelope, which the
// analyser calls speech even when it is warm from music.
func startupBuzz(frames int) []float32 {
	out := make([]float32, frames*opusFrameSize)
	seed := uint32(5)
	for i := range out {
		x := float64(i) / SampleRate
		f0 := 110 + 30*math.Sin(2*math.Pi*0.7*x)
		var v float64
		for h := 1; h <= 20; h++ {
			v += math.Sin(2*math.Pi*f0*float64(h)*x) / float64(h)
		}
		seed = seed*1664525 + 1013904223
		w := float64(seed>>8)/(1<<23) - 1
		out[i] = float32((0.08*v + 0.02*w) * math.Pow(0.5+0.5*math.Sin(2*math.Pi*3.3*x), 2))
	}
	return out
}

// TestMusicIntoSpeechKeepsTheMusicInCELT runs the tone into that buzz with
// nothing between. The second ahead reads as speech before the music has
// ended; what is coming may move the mode to CELT and never out of it, so
// the music is CELT to its last frame.
func TestMusicIntoSpeechKeepsTheMusicInCELT(t *testing.T) {
	pcm := [][]float32{append(testutil.StartupTone(150*opusFrameSize, 1)[0], startupBuzz(150)...)}
	for _, kbps := range []int{24, 32, 48} {
		pkts := startupStream(t, pcm, &EncoderOptions{Bitrate: kbps * 1000}, nil)
		if got := startupModes(pkts, 0, 150); strings.Trim(got, "C") != "" {
			t.Errorf("%d kbit/s: the music is coded %s", kbps, got)
		}
	}
}

// TestTheFrameIsReadAsAnAnalyserStandingAtItWouldReadIt feeds a plain
// analyser one frame a call beside the encoder, as the reference's is fed.
// Once the first second's look-ahead has run out, the info each frame is
// decided on, and the count beside it, are that analyser's: the reader
// sees no further than the frame, whatever the scout has analysed.
func TestTheFrameIsReadAsAnAnalyserStandingAtItWouldReadIt(t *testing.T) {
	const frames = 160
	pcm := testutil.StartupPlucked(frames*opusFrameSize, 2)
	f := audio.Format{Rate: SampleRate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: 96000})
	if err != nil {
		t.Fatal(err)
	}
	var plain tonalityAnalysisState
	var want []analysisInfo
	var counts []int
	b := audio.Get(f, opusFrameSize)
	defer audio.Put(b)
	b.N = opusFrameSize
	for call := range frames {
		x := [][]float32{pcm[0][call*opusFrameSize:][:opusFrameSize], pcm[1][call*opusFrameSize:][:opusFrameSize]}
		var info analysisInfo
		plain.runAnalysis(x, opusFrameSize, opusFrameSize, 0, -2, 2, 24, &info)
		want, counts = append(want, info), append(counts, plain.count)
		copy(b.ChanF(0), x[0])
		copy(b.ChanF(1), x[1])
		if err := enc.Encode(b, func(codec.Packet) error { return nil }); err != nil {
			t.Fatal(err)
		}
		// At 96 kbit/s every frame is CELT, which is handed the info.
		k := int(enc.outFrames) - 1
		if k <= startLookahead {
			continue
		}
		if enc.celt.analysis != want[k] || enc.past.count != counts[k] {
			t.Fatalf("frame %d: decided on info %+v with count %d, a plain analyser gives %+v with count %d",
				k, enc.celt.analysis, enc.past.count, want[k], counts[k])
		}
	}
}

// TestAVoiceHintIsNotMovedByMusicAhead puts speech-like sound ahead of the
// tone under the voice hint: the hint holds SILK or hybrid through both.
func TestAVoiceHintIsNotMovedByMusicAhead(t *testing.T) {
	pcm := [][]float32{append(startupSpeechLike(100)[0], testutil.StartupTone(150*opusFrameSize, 1)[0]...)}
	pkts := startupStream(t, pcm, &EncoderOptions{Bitrate: 32000, Signal: SignalVoice}, nil)
	if got := startupModes(pkts, 0, 250); strings.Contains(got, "C") {
		t.Errorf("coded %s", got)
	}
}

// TestMusicAheadMovesAFrameIntoCELTAndNeverOut pins the rule's guards: the
// threshold from both sides, one direction only, no move under a hint, and
// none at a rate where music itself is SILK.
func TestMusicAheadMovesAFrameIntoCELTAndNeverOut(t *testing.T) {
	const voice, music = 64000, 10000
	for _, tc := range []struct {
		name    string
		mode    int
		signal  Signal
		reading float32
		rate    int32
		want    bool
	}{
		{"just under the threshold", modeSILK, SignalAuto, 0.49, 30000, false},
		{"just over it", modeSILK, SignalAuto, 0.51, 30000, true},
		{"hybrid moves as SILK does", modeHybrid, SignalAuto, 0.9, 30000, true},
		{"a frame in CELT with speech ahead stays", modeCELT, SignalAuto, 0, 30000, false},
		{"a frame in CELT with music ahead is not touched", modeCELT, SignalAuto, 1, 30000, false},
		{"no reading", modeSILK, SignalAuto, -1, 30000, false},
		{"under a voice hint", modeSILK, SignalVoice, 1, 30000, false},
		{"under a music hint", modeSILK, SignalMusic, 1, 30000, false},
		{"where music itself is SILK", modeSILK, SignalAuto, 1, 9000, false},
	} {
		e := &Encoder{mode: tc.mode, signal: tc.signal}
		if _, got := e.musicAheadCall(tc.reading, tc.rate, voice, music); got != tc.want {
			t.Errorf("%s: moved to CELT %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTheAnalyserStandsAWholeLookAheadAhead pins the scout's reach: with
// 120 frames fed, 71 are coded and all 120 are analysed.
func TestTheAnalyserStandsAWholeLookAheadAhead(t *testing.T) {
	pcm := testutil.StartupTone(120*opusFrameSize, 1)
	f := audio.Format{Rate: SampleRate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	enc, err := NewEncoder(f, &EncoderOptions{Bitrate: 32000})
	if err != nil {
		t.Fatal(err)
	}
	b := &audio.Buffer{Fmt: f, F: pcm[0], Stride: len(pcm[0]), N: len(pcm[0])}
	if err := enc.Encode(b, func(codec.Packet) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if enc.outFrames != 120-startLookahead || enc.scout.count != 120 {
		t.Errorf("%d frames coded and %d analysed, want %d and 120", enc.outFrames, enc.scout.count, 120-startLookahead)
	}
}

// startupHiss is faint noise tilted toward the top of the band, where the
// CELT layer of a hybrid frame does its coding.
func startupHiss(frames, chans int) [][]float32 {
	out := make([][]float32, chans)
	seed := uint32(99)
	for c := range out {
		out[c] = make([]float32, frames)
		var p1, p2 float64
		for i := range out[c] {
			seed = seed*1664525 + 1013904223
			n := float64(seed>>8)/(1<<23) - 1
			x := float64(i) / SampleRate
			out[c][i] = float32(0.0003 * (n - 2*p1 + p2) * (0.3 + 0.7*math.Abs(math.Sin(2*math.Pi*x/0.3))))
			p2, p1 = p1, n
		}
	}
	return out
}

// TestTheFrameBehindAModeSwitchIsCodedAtItsSteadyStateError switches a
// stream from CELT to SILK and holds the two frames behind the switch to
// the error of a stream that was in SILK throughout. The decoder resets its
// CELT state at the switch, so a frame predicted from the one before
// decodes at the wrong level.
func TestTheFrameBehindAModeSwitchIsCodedAtItsSteadyStateError(t *testing.T) {
	for _, chans := range []int{1, 2} {
		pcm := startupHiss(SampleRate, chans)
		for _, kbps := range []int{24, 48} {
			t.Run(fmt.Sprintf("%dch/%dk", chans, kbps), func(t *testing.T) {
				opts := &EncoderOptions{Bitrate: kbps * 1000}
				force := func(until int) func(e *Encoder, next int) {
					return func(e *Encoder, next int) {
						e.forcedMode = modeSILK
						if next < until {
							e.forcedMode = modeCELT
						}
					}
				}
				steady := startupDecode(t, startupStream(t, pcm, opts, force(0)), chans)
				for at := 5; at < 25; at += 2 {
					got := startupDecode(t, startupStream(t, pcm, opts, force(at)), chans)
					from, to := at*opusFrameSize, (at+2)*opusFrameSize
					testutil.HoldToSteadyState(t, fmt.Sprintf("two frames behind a switch at frame %d", at),
						testutil.ErrorLevelsOf(pcm, got, from, to), testutil.ErrorLevelsOf(pcm, steady, from, to))
				}
			})
		}
	}
}

// TestLowRateStartupAgainstTheReference compares what a start costs with
// what it costs libopus at the same settings, at the head of a stream and
// behind two seconds of dither, of noise at -100 dB and of a noise bed at
// -50 dBFS: the rms error of
// the first 500 ms of sound over the error of the same samples a period
// on, both streams decoded by this decoder. It self-skips without the
// reference tools.
func TestLowRateStartupAgainstTheReference(t *testing.T) {
	opusDemo, _ := testutil.OpusTools(t)
	const bound = 1.25
	startupCells(t, func(t *testing.T, pcm [][]float32, bitrate int) {
		const head = SampleRate / 2
		chans := len(pcm)
		for _, l := range startupLeads {
			if l.name != "zeros" && l.name != "dither/100" && l.name != "noise" && l.name != "bed/-50" {
				continue
			}
			late := startupBehind(startupSilence(l.frames, chans, l.amp, l.offset), pcm)
			if l.name == "zeros" {
				late = pcm // the head of a stream
			}
			lead := len(late[0]) - len(pcm[0]) + l.frames*opusFrameSize
			if l.name == "zeros" {
				lead = 0
			}
			// The reference reads 16-bit input, so both sides code that.
			ints := make([]int16, 0, chans*len(late[0]))
			src := make([][]float32, chans)
			for c := range src {
				src[c] = make([]float32, len(late[c]))
			}
			for i := range late[0] {
				for c := range chans {
					v := int16(math.RoundToEven(float64(late[c][i]) * 32768))
					ints = append(ints, v)
					src[c][i] = float32(v) / 32768
				}
			}
			cost := func(pkts [][]byte) float64 {
				dec := startupDecode(t, pkts, chans)
				return testutil.ErrorLevelsOf(src, dec, lead, lead+head).RMS /
					testutil.ErrorLevelsOf(src, dec, lead+testutil.StartupPeriod, lead+testutil.StartupPeriod+head).RMS
			}
			ours := cost(startupStream(t, src, &EncoderOptions{Bitrate: bitrate, LSBDepth: 16}, nil))
			lib := cost(testutil.OpusDemoEncode(t, opusDemo, ints, SampleRate, chans, bitrate, DefaultComplexity))
			if !(ours <= bound*lib) {
				t.Errorf("behind %s the first 500 ms cost %.2f times the steady error, libopus %.2f: over %.2f times libopus", l.name, ours, lib, bound)
			}
			if testing.Verbose() {
				t.Logf("behind %s, start over steady: ours %.2f, libopus %.2f", l.name, ours, lib)
			}
		}
	})
}
