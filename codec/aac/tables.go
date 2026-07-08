package aac

import "math"

// Clean-room provenance (ADR-0001): the AAC-LC decode logic in this package
// is written from ISO/IEC 14496-3 and Bosi/Goldberg (Tier A). AAC reference
// decoders (faad, ffmpeg) are Tier B and were not opened while implementing
// the decode path. The one exception is tables_hcb.go, the normative
// parameter tables (Huffman codeword/length data and scalefactor-band
// boundaries) extracted as a black-box artifact per the ADR-0001 provision
// that permits parameter tables produced in a separate analysis pass; it
// carries data only, no decoder logic. THIRD-PARTY-NOTICES.md records it.

// Window sequences (ISO 14496-3 4.5.2.3.2).
const (
	onlyLong   = 0
	longStart  = 1
	eightShort = 2
	longStop   = 3
)

// Window shapes.
const (
	shapeSine = 0
	shapeKBD  = 1
)

// Special codebooks in section data.
const (
	zeroHCB       = 0  // all-zero spectrum, no data
	escHCB        = 11 // codebook 11 carries escape-coded values
	reservedHCB   = 12 // reserved, never valid in a bitstream
	noiseHCB      = 13 // perceptual noise substitution
	intensityHCB2 = 14
	intensityHCB  = 15
)

// Per-codebook structural facts (index [cb-1] for codebooks 1..11), from
// the spec's codebook definitions. dim is the tuple size, mod the base of
// the index-to-value decomposition, off the value offset, and unsigned
// marks books whose magnitudes are coded with separate sign bits.
var (
	hcbDim      = [11]int{4, 4, 4, 4, 2, 2, 2, 2, 2, 2, 2}
	hcbMod      = [11]int{3, 3, 3, 3, 9, 9, 8, 8, 13, 13, 17}
	hcbOff      = [11]int{1, 1, 0, 0, 4, 4, 0, 0, 0, 0, 0}
	hcbUnsigned = [11]bool{false, false, true, true, false, false, true, true, true, true, true}
)

// samplingIndex returns the 4-bit samplingFrequencyIndex for a rate, or -1.
func samplingIndex(rate int) int {
	for i, r := range sampleRates {
		if r == rate {
			return i
		}
	}
	return -1
}

// swbCountLong / swbCountShort report the number of scalefactor bands.
func swbCountLong(rateIdx int) int  { return len(swbOffsetLong[rateIdx]) - 1 }
func swbCountShort(rateIdx int) int { return len(swbOffsetShort[rateIdx]) - 1 }

// tnsMaxBandsLong / tnsMaxBandsShort cap the TNS-filtered region per
// samplingFrequencyIndex (ISO 14496-3 Table 4.140).
var (
	tnsMaxBandsLong  = [13]int{31, 31, 34, 40, 42, 51, 46, 46, 42, 42, 42, 39, 39}
	tnsMaxBandsShort = [13]int{9, 9, 10, 14, 14, 14, 14, 14, 14, 14, 14, 14, 14}
)

// Window tables, generated once at init: full 2N windows for sine and KBD,
// long (2048) and short (256). w[shape].
var (
	longWindow  [2][2048]float64
	shortWindow [2][256]float64
)

func init() {
	buildWindow(longWindow[shapeSine][:], sineHalf(1024))
	buildWindow(shortWindow[shapeSine][:], sineHalf(128))
	buildWindow(longWindow[shapeKBD][:], kbdHalf(1024, 4.0))
	buildWindow(shortWindow[shapeKBD][:], kbdHalf(128, 6.0))
}

// buildWindow forms a symmetric 2N window from an N-sample rising half:
// the left half rises with the half-window, the right half falls with its
// mirror.
func buildWindow(w []float64, half []float64) {
	n := len(half)
	for i := 0; i < n; i++ {
		w[i] = half[i]
		w[2*n-1-i] = half[i]
	}
}

// sineHalf returns the rising half of a sine window of full length 2N.
func sineHalf(n int) []float64 {
	half := make([]float64, n)
	for i := range half {
		half[i] = math.Sin(math.Pi / float64(2*n) * (float64(i) + 0.5))
	}
	return half
}

// kbdHalf returns the rising half of a Kaiser-Bessel-derived window of full
// length 2N with the given alpha (ISO 14496-3 4.6.14).
func kbdHalf(n int, alpha float64) []float64 {
	w := make([]float64, n+1)
	denom := besselI0(math.Pi * alpha)
	for i := 0; i <= n; i++ {
		x := (2*float64(i)/float64(n) - 1)
		w[i] = besselI0(math.Pi*alpha*math.Sqrt(1-x*x)) / denom
	}
	// Cumulative sums, then the derived window is the normalized sqrt. The
	// ISO denominator runs the full kernel 0..n inclusive (n+1 terms).
	var total float64
	for i := 0; i <= n; i++ {
		total += w[i]
	}
	half := make([]float64, n)
	var running float64
	for i := 0; i < n; i++ {
		running += w[i]
		half[i] = math.Sqrt(running / total)
	}
	return half
}

// besselI0 is the modified Bessel function of the first kind, order 0.
func besselI0(x float64) float64 {
	var sum, term float64 = 1, 1
	xx := x * x / 4
	for k := 1; k < 64; k++ {
		term *= xx / float64(k*k)
		sum += term
		if term < 1e-18*sum {
			break
		}
	}
	return sum
}
