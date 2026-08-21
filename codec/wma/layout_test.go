//go:build !wmatablesgen

package wma

// The exponent band layout and the noise-gain book, pinned directly.
//
// Both are reached by every stream and by no differential. FFmpeg's WMA
// encoder emits a completely FLAT exponent curve -- measured across the
// eighteen cells of the analysis corpus and on tone, low-passed, high-passed,
// two-tone and near-silent sources, 0 of 1216 exponent decodes had two different band
// values -- so exp[i]/maxExponent is 1 everywhere and the band layout cancels
// out of the reconstruction entirely. It also never sets a noise-fill flag, so
// the noise-gain book is never read from a corpus file at all. A differential
// therefore pins the band COUNT, which is how many scalefactor codes get read,
// and nothing else about either.
//
// So they are checked here against the rules as written down, by a second
// implementation that shares no code with the decoder's.

import (
	"math"
	"testing"
)

// ruleBands is the band layout restated from docs/notes/wma-bitstream.md
// section 6, independently of expBandWidths: v1 places band ends at
// round(blockLen*2*edge/rate); v2 rounds the same quantity to the NEAREST
// multiple of four. It returns edges, not widths.
func ruleBands(v2 bool, blockLen, rate int) []int {
	var out []int
	prev := 0
	for _, edge := range criticalFreqs {
		var end int
		if v2 {
			q := float64(blockLen) * 2 * float64(edge) / float64(rate)
			end = int(math.Round(q/4)) * 4
		} else {
			end = int(math.Round(float64(blockLen) * 2 * float64(edge) / float64(rate)))
		}
		end = min(end, blockLen)
		if end > prev {
			out = append(out, end)
			prev = end
		}
		if end >= blockLen {
			break
		}
	}
	return out
}

func widthsToEdges(w []int) []int {
	out, at := make([]int, len(w)), 0
	for i, v := range w {
		at += v
		out[i] = at
	}
	return out
}

func TestBandLayoutFollowsTheRule(t *testing.T) {
	rates := []int{8000, 11025, 16000, 22050, 32000, 44100, 48000, 50000}
	for _, v2 := range []bool{false, true} {
		for _, rate := range rates {
			c := Config{V2: v2, Rate: rate, Channels: 1, BitRate: 64000, Flags2: flagExpVLC}
			for k := range 5 {
				blockLen := c.FrameLen() >> k
				if blockLen < 128 {
					break
				}
				// The tabulated rows are a coarsening rather than the formula,
				// and tables_test.go pins them against their own rate; they
				// get their own check below.
				if v2 && rate >= 22050 && c.frameLenBits()-7-k < 3 {
					continue
				}
				widths := c.expBandWidths(k, blockLen)
				got := widthsToEdges(widths)
				want := ruleBands(v2, blockLen, rate)
				if len(got) != len(want) {
					t.Fatalf("v2=%v %dHz block %d: %d bands, want %d", v2, rate, blockLen, len(got), len(want))
				}
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("v2=%v %dHz block %d: edge %d at %d, want %d\n got  %v\n want %v",
							v2, rate, blockLen, i, got[i], want[i], got, want)
					}
				}
				if !v2 {
					continue
				}
				for i, w := range widths {
					if w%4 != 0 {
						t.Errorf("v2 %dHz block %d: band %d is %d wide, not a multiple of four", rate, blockLen, i, w)
					}
				}
			}
		}
	}
}

// TestBandLayoutRoundsToNearest is the test above made non-vacuous. Rounding
// the v2 quantity DOWN instead of to nearest keeps the band count identical at
// every rate, so it changes no bit layout and no differential can see it, and
// it still moves most of the edges. This states that the two really do differ,
// so a reader knows the check above is doing work.
func TestBandLayoutRoundsToNearest(t *testing.T) {
	moved := 0
	for _, rate := range []int{8000, 11025, 16000, 22050, 32000, 44100, 48000} {
		c := Config{V2: true, Rate: rate, Channels: 1, BitRate: 64000, Flags2: flagExpVLC}
		blockLen := c.FrameLen()
		got := widthsToEdges(c.expBandWidths(0, blockLen))
		// The round-down spelling, which the notes name as a wrong answer that
		// gives a wrong band layout on every v2 stream.
		var down []int
		prev := 0
		for _, edge := range criticalFreqs {
			end := min(blockLen*2*int(edge)/(4*rate)<<2, blockLen)
			if end > prev {
				down = append(down, end)
				prev = end
			}
			if end >= blockLen {
				break
			}
		}
		if len(down) != len(got) {
			t.Fatalf("%dHz: rounding down changes the band count (%d against %d); it would not be silent",
				rate, len(down), len(got))
		}
		for i := range got {
			if got[i] != down[i] {
				moved++
			}
		}
	}
	if moved < 50 {
		t.Errorf("only %d edges move between the two roundings; the distinction is not being tested", moved)
	}
}

// TestBandLayoutTakesTheTabulatedRows: the three v2 tables apply exactly where
// the notes say and nowhere else, picked from the RAW sample rate, so 48 kHz
// uses the 44100 one.
func TestBandLayoutTakesTheTabulatedRows(t *testing.T) {
	for _, tc := range []struct {
		rate  int
		table *[3][]uint8
	}{
		{22050, &expBands22050}, {32000, &expBands32000},
		{44100, &expBands44100}, {48000, &expBands44100},
	} {
		c := Config{V2: true, Rate: tc.rate, Channels: 1, BitRate: 64000, Flags2: flagExpVLC | flagVarBlkLen}
		rows := 0
		for k := range 5 {
			blockLen := c.FrameLen() >> k
			if blockLen < 128 {
				break
			}
			row := c.frameLenBits() - 7 - k
			if row < 0 || row >= 3 {
				continue
			}
			rows++
			got, want := c.expBandWidths(k, blockLen), tc.table[row]
			if len(got) != len(want) {
				t.Fatalf("%dHz block %d (row %d): %d bands, table has %d", tc.rate, blockLen, row, len(got), len(want))
			}
			for i := range got {
				if got[i] != int(want[i]) {
					t.Fatalf("%dHz block %d: band %d is %d, table says %d", tc.rate, blockLen, i, got[i], want[i])
				}
			}
		}
		if rows != 3 {
			t.Errorf("%dHz reached %d tabulated rows, want 3", tc.rate, rows)
		}
	}
	// The largest block size never takes a table, whatever the rate.
	c := Config{V2: true, Rate: 44100, Channels: 1, BitRate: 64000, Flags2: flagExpVLC | flagVarBlkLen}
	full := widthsToEdges(c.expBandWidths(0, c.FrameLen()))
	if want := ruleBands(true, c.FrameLen(), 44100); len(full) != len(want) {
		t.Errorf("the full-length block took a tabulated row")
	}
	// 22.05 kHz is the floor: below it every block size is computed.
	c = Config{V2: true, Rate: 16000, Channels: 1, BitRate: 64000, Flags2: flagExpVLC | flagVarBlkLen}
	got := c.expBandWidths(2, c.FrameLen()>>2)
	if want := ruleBands(true, c.FrameLen()>>2, 16000); len(got) != len(want) {
		t.Error("16 kHz took a tabulated row; the tables start at 22.05 kHz")
	}
}

// TestNoiseGainBookUsesTheListedOrder closes the loop tables_test.go opens.
// That test walks the listing and pins the codewords it produces; this one
// feeds those codewords to the book the DECODER built and requires the symbol
// back. Without it a decoder that sorted the book canonically -- the textbook
// build, which disagrees with this one everywhere -- would pass every table
// check and mis-decode every noise gain after the first in a block.
func TestNoiseGainBookUsesTheListedOrder(t *testing.T) {
	maxLen := uint8(0)
	for _, e := range hgainHuff {
		maxLen = max(maxLen, e[1])
	}
	book := hgainBook()
	var acc uint32
	for i, e := range hgainHuff {
		code := acc >> (maxLen - e[1])
		acc += 1 << (maxLen - e[1])
		// Left-justify the codeword into a buffer with room to spare, so the
		// reader's fixed-width peek has something to read.
		var buf [8]byte
		v := uint64(code) << (64 - e[1])
		for j := range buf {
			buf[j] = byte(v >> (56 - 8*j))
		}
		var r bitReader
		r.reset(buf[:])
		got := book.decode(&r)
		if got != i {
			t.Fatalf("entry %d (symbol %d, code %0*b) decoded as %d", i, e[0], int(e[1]), code, got)
		}
		if r.pos != int(e[1]) {
			t.Fatalf("entry %d consumed %d bits, its codeword is %d", i, r.pos, e[1])
		}
		if delta, want := hgainDelta(got), int(e[0])-18; delta != want {
			t.Fatalf("entry %d: delta %d, want %d", i, delta, want)
		}
	}
}

// TestExponentReuseTakesTheRatio pins the reuse indexing. A block shorter or
// longer than the one whose exponents it reuses indexes them through the ratio
// between the two lengths; using the current length alone gives the right
// answer only when the curve happens to be flat, which is the one case an
// ffmpeg corpus contains and so the one case a differential would confirm.
func TestExponentReuseTakesTheRatio(t *testing.T) {
	src := make([]float32, 2048)
	for i := range src {
		src[i] = float32(i + 1)
	}
	for _, tc := range []struct{ expLen, blockLen int }{
		{2048, 2048}, {2048, 1024}, {2048, 256}, {512, 1024}, {256, 2048}, {128, 128},
	} {
		dst := make([]float32, tc.blockLen)
		resampleExponents(dst, src, tc.expLen)
		for i := range dst {
			// The rule as written: src[i*expLen/blockLen].
			if want := src[i*tc.expLen/tc.blockLen]; dst[i] != want {
				t.Fatalf("expLen %d block %d: dst[%d] = %v, want %v",
					tc.expLen, tc.blockLen, i, dst[i], want)
			}
		}
		// And it must actually differ from ignoring the ratio, or the case is
		// not being tested.
		if tc.expLen != tc.blockLen {
			same := true
			for i := range dst {
				if dst[i] != src[i] {
					same = false
					break
				}
			}
			if same {
				t.Errorf("expLen %d block %d: the ratio changed nothing", tc.expLen, tc.blockLen)
			}
		}
	}
}
