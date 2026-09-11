//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
)

// The coverage matrix of docs/notes/wma-voice-oracle-corpus.md section 4,
// measured there with the analysis pass's own instrumented decoder and pinned
// here against this decoder's path counters. Sample comparison cannot say that
// a walk took the same branches the same number of times; this can, and it is
// what turns "the corpus reaches the window pulse coder" from a claim into a
// count.
//
// A dash in the note is a zero here, and the zeros are the point: they are the
// deficit list docs/quality-gates.md carries, so a cell that started reaching
// one of them would fail this test rather than quietly widen the coverage.
type coverage struct {
	packets, superframes, frames, blocks  int
	residualLSP, independentLSP           int
	lspInterp                             int
	sampleCount, lastSampleCount          int
	spilloverZero, spilloverNonZero       int
	superframeCounts                      []int
	staleCarry                            int
	frameType                             [17]int
	asymBlocks, asymRuns                  int
	asymPhases                            []int
	pitchReset                            int
	blockPitchFirst, blockPitchDelta      int
	hammingPhase                          [4]int
	convArm                               [4]int
	gainWeight1, gainWeight2, gainWeight4 int
	awRange24                             int
	silenceGains                          int
	kalmanOK, kalmanFail                  int
	denoiseRuns                           int
}

var coverages = map[string]coverage{
	"voice-8000-4k": {
		packets: 18, superframes: 67, frames: 201, blocks: 558,
		residualLSP: 67, lspInterp: 23, sampleCount: 1, lastSampleCount: 183,
		spilloverZero: 1, spilloverNonZero: 17, superframeCounts: []int{1, 2, 3, 5, 7, 9},
		frameType:  [17]int{40, 62, 15, 2, 2, 2, 26, 15, 0, 0, 0, 9, 19, 0, 0, 0, 9},
		asymBlocks: 210, asymRuns: 1978, asymPhases: []int{1, 2, 3, 4, 5, 6, 7, 8},
		pitchReset:      3,
		blockPitchFirst: 37, blockPitchDelta: 147,
		hammingPhase: [4]int{97, 26, 34, 27}, convArm: [4]int{100, 43, 41, 0},
		gainWeight1: 72, gainWeight2: 284, gainWeight4: 38,
		awRange24: 15, silenceGains: 1,
		kalmanOK: 197, kalmanFail: 1, denoiseRuns: 322,
	},
	"voice-8000-5k": {
		packets: 18, superframes: 67, frames: 201, blocks: 688,
		residualLSP: 67, lspInterp: 23, sampleCount: 1, lastSampleCount: 183,
		spilloverZero: 1, spilloverNonZero: 17, superframeCounts: []int{1, 2, 3, 5, 7, 8, 9},
		frameType:  [17]int{40, 62, 4, 2, 10, 5, 26, 6, 0, 0, 0, 3, 3, 0, 0, 1, 39},
		asymBlocks: 180, asymRuns: 1388, asymPhases: []int{1, 2, 3, 4, 5, 6, 7, 8},
		pitchReset:      7,
		blockPitchFirst: 46, blockPitchDelta: 298,
		hammingPhase: [4]int{204, 27, 81, 32}, convArm: [4]int{119, 154, 71, 0},
		gainWeight1: 320, gainWeight2: 172, gainWeight4: 32,
		awRange24: 4, silenceGains: 1,
		kalmanOK: 198, kalmanFail: 0, denoiseRuns: 322,
	},
	"voice-8000-8k": {
		packets: 16, superframes: 67, frames: 201, blocks: 968,
		residualLSP: 67, lspInterp: 23, sampleCount: 1, lastSampleCount: 183,
		spilloverZero: 2, spilloverNonZero: 14, superframeCounts: []int{1, 2, 3, 4, 8, 9},
		frameType:       [17]int{40, 60, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 101},
		blockPitchFirst: 101, blockPitchDelta: 707,
		hammingPhase: [4]int{624, 33, 110, 41}, convArm: [4]int{320, 370, 118, 0},
		gainWeight1: 808, silenceGains: 1,
		kalmanOK: 201, kalmanFail: 1, denoiseRuns: 322,
	},
	"voice-11025-10k": {
		packets: 10, superframes: 92, frames: 276, blocks: 768,
		residualLSP: 92, lspInterp: 27, sampleCount: 1, lastSampleCount: 237,
		spilloverZero: 6, spilloverNonZero: 4, superframeCounts: []int{5, 7, 9, 10, 11, 13},
		staleCarry:      3,
		frameType:       [17]int{56, 84, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 136, 0, 0, 0},
		blockPitchFirst: 136, blockPitchDelta: 408,
		hammingPhase: [4]int{409, 20, 83, 32}, convArm: [4]int{204, 264, 76, 0},
		gainWeight2: 544, silenceGains: 1,
		kalmanOK: 271, kalmanFail: 1, denoiseRuns: 440,
	},
	"voice-16000-12k": {
		packets: 15, superframes: 133, frames: 399, blocks: 980,
		residualLSP: 133, lspInterp: 28, sampleCount: 1, lastSampleCount: 367,
		spilloverZero: 8, spilloverNonZero: 7, superframeCounts: []int{1, 6, 10, 14, 15, 17},
		staleCarry:      4,
		frameType:       [17]int{82, 185, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 132, 0, 0, 0},
		blockPitchFirst: 132, blockPitchDelta: 396,
		hammingPhase: [4]int{377, 27, 101, 23}, convArm: [4]int{246, 236, 46, 0},
		gainWeight2: 528, silenceGains: 1,
		kalmanOK: 264, kalmanFail: 0, denoiseRuns: 634,
	},
	"voice-16000-16k": {
		packets: 14, superframes: 133, frames: 399, blocks: 980,
		residualLSP: 133, lspInterp: 28, sampleCount: 1, lastSampleCount: 367,
		spilloverZero: 11, spilloverNonZero: 3, superframeCounts: []int{5, 7, 8, 10, 15},
		staleCarry:      6,
		frameType:       [17]int{82, 185, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 132, 0, 0, 0},
		blockPitchFirst: 132, blockPitchDelta: 396,
		hammingPhase: [4]int{377, 27, 101, 23}, convArm: [4]int{246, 236, 46, 0},
		gainWeight2: 528, silenceGains: 1,
		kalmanOK: 264, kalmanFail: 0, denoiseRuns: 634,
	},
	"voice-22050-20k": {
		packets: 10, superframes: 183, frames: 549, blocks: 1467,
		residualLSP: 183, lspInterp: 30, sampleCount: 1, lastSampleCount: 473,
		spilloverZero: 6, spilloverNonZero: 4, superframeCounts: []int{9, 14, 16, 20, 23, 24},
		staleCarry:      5,
		frameType:       [17]int{115, 192, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 242, 0, 0, 0},
		blockPitchFirst: 242, blockPitchDelta: 726,
		hammingPhase: [4]int{585, 43, 277, 63}, convArm: [4]int{410, 462, 96, 0},
		gainWeight2: 968, silenceGains: 1,
		kalmanOK: 474, kalmanFail: 10, denoiseRuns: 868,
	},
}

func TestCorpusReachesWhatTheNotesMeasured(t *testing.T) {
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			want, ok := coverages[c.name]
			if !ok {
				t.Fatalf("no coverage row for %s", c.name)
			}
			p := decodePaths(t, c.name)
			eq := func(name string, got, w int) {
				t.Helper()
				if got != w {
					t.Errorf("%s = %d, the note measured %d", name, got, w)
				}
			}
			eq("media objects", p.Packets, want.packets)
			eq("superframes", p.Superframes, want.superframes)
			eq("frames", p.Frames, want.frames)
			eq("blocks", p.Blocks, want.blocks)
			eq("residual LSP superframes", p.ResidualLSP, want.residualLSP)
			eq("independent LSP frames", p.IndependentLSP, want.independentLSP)
			eq("distinct LSP interpolation indices", countTrue(p.LSPInterp[:]), want.lspInterp)
			eq("LSP stabiliser, first clamp", p.StabFirst, 0)
			eq("LSP stabiliser, last clamp", p.StabLast, 0)
			eq("LSP stabiliser, reorder", p.StabSort, 0)
			eq("superframes with a sample count", p.SampleCount, want.sampleCount)
			eq("the sample count it carries", p.LastSampleCount, want.lastSampleCount)
			eq("statistics fields", p.Statistics, 0)
			eq("packets with spillover zero", p.SpilloverZero, want.spilloverZero)
			eq("packets with spillover non-zero", p.SpilloverNonZero, want.spilloverNonZero)
			eq("superframe count escapes", p.CountEscape, 0)
			eq("carries above the reference's cache", p.StaleCarry, want.staleCarry)
			if got := trueSet(p.SuperframeCounts[:]); !slices.Equal(got, want.superframeCounts) {
				t.Errorf("superframes per packet %v, the note measured %v", got, want.superframeCounts)
			}
			for ft, n := range p.FrameType {
				eq("frame type "+strconv.Itoa(ft), n, want.frameType[ft])
			}
			eq("asymmetric-codebook blocks", p.AsymBlocks, want.asymBlocks)
			eq("asymmetric interpolation runs", p.AsymRuns, want.asymRuns)
			if got := trueSet(p.AsymPhase[:]); !slices.Equal(got, want.asymPhases) {
				t.Errorf("asymmetric interpolation phases %v, the note measured %v", got, want.asymPhases)
			}
			eq("pitch resets by the five percent rule", p.PitchReset, want.pitchReset)
			eq("pitches clamped to maxPitch-1", p.PitchClamp, 0)
			eq("Hamming first-block pitch fields", p.BlockPitchFirst, want.blockPitchFirst)
			eq("Hamming delta pitch fields", p.BlockPitchDelta, want.blockPitchDelta)
			for i, n := range p.HammingPhase {
				eq("Hamming phase "+strconv.Itoa(i), n, want.hammingPhase[i])
			}
			for i, n := range p.ConvArm {
				eq("pitch conversion arm "+strconv.Itoa(i+1), n, want.convArm[i])
			}
			eq("gain predictor weight 1", p.GainWeight[1], want.gainWeight1)
			eq("gain predictor weight 2", p.GainWeight[2], want.gainWeight2)
			eq("gain predictor weight 4", p.GainWeight[4], want.gainWeight4)
			eq("window pulse range 24", p.AWRange24, want.awRange24)
			eq("window pulse range 16", p.AWRange16, 0)
			eq("window extended index", p.AWExtended, 0)
			eq("window zero-count branch", p.AWZeroCount, 0)
			eq("window concealment", p.AWConceal, 0)
			eq("distinct silence gain indices", countTrue(p.SilenceGain[:]), want.silenceGains)
			eq("postfilter Kalman succeeded", p.KalmanOK, want.kalmanOK)
			eq("postfilter Kalman failed", p.KalmanFail, want.kalmanFail)
			eq("postfilter denoise ran", p.DenoiseRuns, want.denoiseRuns)
			eq("DC removal filter", p.DCRemoval, 0)
		})
	}
}

// TestDemuxerCellReachesTheRowsItChanges is the same pin on the one-second
// cell the ASF differential carries, which is the only fixture here with NO
// silence frames at all.
func TestDemuxerCellReachesTheRowsItChanges(t *testing.T) {
	track, pkts := demuxerCell(t)
	dec := newDecoder(t, track)
	defer dec.Release()
	drain(t, dec, pkts)
	p := dec.Paths()
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"media objects", p.Packets, 4},
		{"superframes", p.Superframes, 34},
		{"frames", p.Frames, 102},
		{"blocks", p.Blocks, 304},
		{"silence frames", p.FrameType[0], 0},
		{"frame type 1", p.FrameType[1], 52},
		{"frame type 13", p.FrameType[13], 50},
		{"the sample count it carries", p.LastSampleCount, 160},
		{"packets with spillover non-zero", p.SpilloverNonZero, 3},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, the note measured %d", tc.name, tc.got, tc.want)
		}
	}
}

func decodePaths(t testing.TB, name string) wmavoice.Paths {
	t.Helper()
	track, pkts := demux(t, corpusPath(t, name))
	dec := newDecoder(t, track)
	defer dec.Release()
	drain(t, dec, pkts)
	return dec.Paths()
}

func drain(t testing.TB, dec *wmavoice.Decoder, pkts [][]byte) {
	t.Helper()
	emit := func(*audio.Buffer) error { return nil }
	for i, p := range pkts {
		if err := dec.Decode(p, emit); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func countTrue(v []bool) int {
	n := 0
	for _, ok := range v {
		if ok {
			n++
		}
	}
	return n
}

func trueSet(v []bool) []int {
	var out []int
	for i, ok := range v {
		if ok {
			out = append(out, i)
		}
	}
	return out
}
