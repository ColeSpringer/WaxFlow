package aac

import (
	"errors"
	"math"
	"sort"
)

// SBR frequency band tables (ISO/IEC 14496-3 4.6.18.3): the master table
// derived from the header's start/stop/scale fields, the high- and
// low-resolution envelope tables cut from it at the crossover band, the
// noise-floor table, the limiter table, and the transposer patches. All
// borders are absolute QMF subband indices (0..64). Derivation failures
// are errors, not panics: a hostile header drops the SBR data (the element
// conceals) rather than crashing the decode.

var errSBRFreq = errors.New("aac: unusable SBR frequency configuration")

// sbrFreqTables is everything derived from one header at one SBR rate.
type sbrFreqTables struct {
	k0, k2  int
	nMaster int
	fMaster []int // nMaster+1 borders

	nHigh, nLow int
	fHigh, fLow []int
	kx, m       int

	nQ     int
	fNoise []int // nQ+1 borders

	nL   int
	fLim []int // nL+1 borders

	patchNum   []int // subbands per patch
	patchStart []int // source start subband per patch
}

// sbrStartK0 resolves the start band k0 from the SBR (output) rate and
// bs_start_freq. A rate with no offset row reports failure.
func sbrStartK0(fs, bsStartFreq int) (int, bool) {
	var t int
	switch {
	case fs < 32000:
		t = 3000
	case fs < 64000:
		t = 4000
	default:
		t = 5000
	}
	startMin := (t*128 + fs/2) / fs
	row := -1
	switch {
	case fs == 16000:
		row = 0
	case fs == 22050:
		row = 1
	case fs == 24000:
		row = 2
	case fs == 32000:
		row = 3
	case fs >= 44100 && fs <= 64000:
		row = 4
	case fs > 64000:
		row = 5
	}
	if row < 0 {
		return 0, false
	}
	return startMin + sbrOffsetRows[row][bsStartFreq], true
}

// sbrStopK2 resolves the stop band k2 from the SBR rate, bs_stop_freq, and
// the already-resolved k0.
func sbrStopK2(fs, bsStopFreq, k0 int) int {
	switch {
	case bsStopFreq == 14:
		return min(64, 2*k0)
	case bsStopFreq == 15:
		return min(64, 3*k0)
	}
	stopMin := (2*sbrStopBase(fs)*128 + fs/2) / fs
	if stopMin >= 64 {
		return 64
	}
	// Thirteen warped steps from stopMin to 64, their widths sorted
	// ascending; k2 accumulates the first bs_stop_freq of them.
	dk := make([]int, 13)
	ratio := 64.0 / float64(stopMin)
	prev := stopMin
	for i := range 13 {
		next := int(math.Round(float64(stopMin) * math.Pow(ratio, float64(i+1)/13)))
		dk[i] = next - prev
		prev = next
	}
	sort.Ints(dk)
	k2 := stopMin
	for i := 0; i < bsStopFreq && i < 13; i++ {
		k2 += dk[i]
	}
	return min(64, k2)
}

func sbrStopBase(fs int) int {
	switch {
	case fs < 32000:
		return 3000
	case fs < 64000:
		return 4000
	default:
		return 5000
	}
}

// deriveFreqTables builds every band table for a header at the SBR rate.
func deriveFreqTables(h sbrHeader, fs int) (sbrFreqTables, error) {
	var t sbrFreqTables
	k0, ok := sbrStartK0(fs, h.startFreq)
	if !ok || k0 < 1 || k0 > 32 {
		return t, errSBRFreq
	}
	k2 := sbrStopK2(fs, h.stopFreq, k0)
	if k2 <= k0 {
		return t, errSBRFreq
	}
	// The spec bounds the SBR range by rate: 48 bands below 44100, 35 at
	// 44100, 32 at and above 48000.
	maxBands := 48
	switch {
	case fs >= 48000:
		maxBands = 32
	case fs == 44100:
		maxBands = 35
	}
	if k2-k0 > maxBands {
		return t, errSBRFreq
	}
	t.k0, t.k2 = k0, k2

	if h.freqScale == 0 {
		if err := masterLinear(&t, h.alterScale != 0); err != nil {
			return t, err
		}
	} else {
		if err := masterWarped(&t, h.freqScale, h.alterScale != 0); err != nil {
			return t, err
		}
	}
	if t.nMaster < 1 || h.xoverBand >= t.nMaster {
		return t, errSBRFreq
	}
	for i := 0; i < t.nMaster; i++ {
		if t.fMaster[i+1] <= t.fMaster[i] {
			return t, errSBRFreq
		}
	}
	if t.fMaster[t.nMaster] > 64 {
		return t, errSBRFreq
	}

	t.nHigh = t.nMaster - h.xoverBand
	t.fHigh = t.fMaster[h.xoverBand:]
	t.kx = t.fHigh[0]
	t.m = t.fHigh[t.nHigh] - t.kx
	if t.kx > 32 || t.kx+t.m > 64 {
		return t, errSBRFreq
	}

	t.nLow = t.nHigh - t.nHigh/2
	t.fLow = make([]int, t.nLow+1)
	if t.nHigh%2 == 0 {
		for i := range t.fLow {
			t.fLow[i] = t.fHigh[2*i]
		}
	} else {
		t.fLow[0] = t.fHigh[0]
		for i := 1; i <= t.nLow; i++ {
			t.fLow[i] = t.fHigh[2*i-1]
		}
	}

	// Noise bands: bs_noise_bands per octave of the SBR range, at least one,
	// at most five, spread evenly over the low-resolution table.
	nQ := int(math.Round(float64(h.noiseBands) * math.Log2(float64(k2)/float64(t.kx))))
	t.nQ = min(5, max(1, nQ))
	if t.nQ > t.nLow {
		t.nQ = t.nLow
	}
	if t.nQ < 1 {
		return t, errSBRFreq
	}
	t.fNoise = make([]int, t.nQ+1)
	t.fNoise[0] = t.fLow[0]
	prev := 0
	for k := 1; k <= t.nQ; k++ {
		prev += (t.nLow - prev) / (t.nQ - k + 1)
		t.fNoise[k] = t.fLow[prev]
	}

	if err := derivePatches(&t, fs); err != nil {
		return t, err
	}
	deriveLimiter(&t, h.limiterBands)
	return t, nil
}

// masterLinear is the bs_freq_scale == 0 master table: equal-width bands of
// dk (1 or 2), with any remainder distributed one subband at a time.
func masterLinear(t *sbrFreqTables, alterScale bool) error {
	dk := 1
	if alterScale {
		dk = 2
	}
	numBands := 2 * int(math.Round(float64(t.k2-t.k0)/float64(2*dk)))
	if numBands < 1 || numBands > 63 {
		return errSBRFreq
	}
	vDk := make([]int, numBands)
	for i := range vDk {
		vDk[i] = dk
	}
	k2Diff := t.k2 - (t.k0 + numBands*dk)
	if k2Diff != 0 {
		incr, k := 1, 0
		if k2Diff > 0 {
			incr, k = -1, numBands-1
		}
		for k2Diff != 0 {
			if k < 0 || k >= numBands {
				return errSBRFreq
			}
			vDk[k] -= incr
			k += incr
			k2Diff += incr
		}
	}
	t.nMaster = numBands
	t.fMaster = make([]int, numBands+1)
	t.fMaster[0] = t.k0
	for i := 0; i < numBands; i++ {
		if vDk[i] < 1 {
			return errSBRFreq
		}
		t.fMaster[i+1] = t.fMaster[i] + vDk[i]
	}
	return nil
}

// masterWarped is the bs_freq_scale 1..3 master table: log-spaced bands,
// split into two regions at 2*k0 when the range is wide, the upper region
// warped by 1.3 when bs_alter_scale is set.
func masterWarped(t *sbrFreqTables, freqScale int, alterScale bool) error {
	bands := 14 - 2*freqScale // 12, 10, 8 bands per octave
	warp := 1.0
	if alterScale {
		warp = 1.3
	}
	k0, k2 := t.k0, t.k2
	twoRegions := false
	k1 := k2
	if float64(k2)/float64(k0) > 2.2449 {
		twoRegions = true
		k1 = 2 * k0
	}
	vk0, err := warpedRegion(k0, k1, bands, 1.0)
	if err != nil {
		return err
	}
	if !twoRegions {
		t.nMaster = len(vk0) - 1
		t.fMaster = vk0
		return nil
	}
	numBands1 := 2 * int(math.Round(float64(bands)*math.Log2(float64(k2)/float64(k1))/(2*warp)))
	if numBands1 < 1 {
		return errSBRFreq
	}
	vDk1 := make([]int, numBands1)
	prev := k1
	ratio := float64(k2) / float64(k1)
	for i := range vDk1 {
		next := int(math.Round(float64(k1) * math.Pow(ratio, float64(i+1)/float64(numBands1))))
		vDk1[i] = next - prev
		prev = next
	}
	sort.Ints(vDk1)
	// Keep band widths monotonic across the region seam: widen the first
	// upper band to the widest lower one, narrowing the last upper band by
	// the same amount, bounded to keep the ordering.
	maxDk0 := 0
	for i := 1; i < len(vk0); i++ {
		maxDk0 = max(maxDk0, vk0[i]-vk0[i-1])
	}
	if vDk1[0] < maxDk0 {
		change := min(maxDk0-vDk1[0], (vDk1[numBands1-1]-vDk1[0])/2)
		vDk1[0] += change
		vDk1[numBands1-1] -= change
	}
	t.nMaster = len(vk0) - 1 + numBands1
	t.fMaster = make([]int, 0, t.nMaster+1)
	t.fMaster = append(t.fMaster, vk0...)
	acc := k1
	for i := range vDk1 {
		if vDk1[i] < 1 {
			return errSBRFreq
		}
		acc += vDk1[i]
		t.fMaster = append(t.fMaster, acc)
	}
	return nil
}

// warpedRegion lays numBands log-spaced borders from k0 to k1, widths
// sorted ascending, and returns the border vector.
func warpedRegion(k0, k1, bands int, warp float64) ([]int, error) {
	numBands := 2 * int(math.Round(float64(bands)*math.Log2(float64(k1)/float64(k0))/(2*warp)))
	if numBands < 1 || numBands > 63 {
		return nil, errSBRFreq
	}
	vDk := make([]int, numBands)
	prev := k0
	ratio := float64(k1) / float64(k0)
	for i := range vDk {
		next := int(math.Round(float64(k0) * math.Pow(ratio, float64(i+1)/float64(numBands))))
		vDk[i] = next - prev
		prev = next
	}
	sort.Ints(vDk)
	out := make([]int, numBands+1)
	out[0] = k0
	for i := range vDk {
		if vDk[i] < 1 {
			return nil, errSBRFreq
		}
		out[i+1] = out[i] + vDk[i]
	}
	return out, nil
}

// derivePatches builds the transposer patch list (4.6.18.6.3): contiguous
// runs of source subbands below k0 copied upward, each patch bounded by the
// goal subband (2.048 MHz / fs) and the master borders.
func derivePatches(t *sbrFreqTables, fs int) error {
	goalSb := int(math.Round(2048000.0 / float64(fs)))
	msb, usb := t.k0, t.kx
	k := t.nMaster
	if goalSb < t.kx+t.m {
		k = 0
		for i := 0; i < len(t.fMaster) && t.fMaster[i] < goalSb; i++ {
			k = i + 1
		}
	}
	t.patchNum = t.patchNum[:0]
	t.patchStart = t.patchStart[:0]
	for iter := 0; ; iter++ {
		if iter > 12 {
			// A conforming derivation terminates in a handful of rounds; a
			// crafted header that cycles must not spin.
			return errSBRFreq
		}
		j := min(k, t.nMaster) + 1
		var sb, odd int
		for {
			j--
			if j < 0 {
				return errSBRFreq
			}
			sb = t.fMaster[j]
			odd = (sb - 2 + t.k0) & 1
			if sb <= t.k0-1+msb-odd || j == 0 {
				break
			}
		}
		num := max(sb-usb, 0)
		start := t.k0 - odd - num
		if num > 0 && start < 0 {
			return errSBRFreq
		}
		if num > 0 {
			t.patchNum = append(t.patchNum, num)
			t.patchStart = append(t.patchStart, start)
			usb = sb
			msb = sb
		} else {
			msb = t.kx
		}
		if t.fMaster[k]-sb < 3 {
			k = t.nMaster
		}
		if sb == t.kx+t.m {
			break
		}
		if len(t.patchNum) > 5 {
			return errSBRFreq
		}
	}
	if len(t.patchNum) > 1 && t.patchNum[len(t.patchNum)-1] < 3 {
		t.patchNum = t.patchNum[:len(t.patchNum)-1]
		t.patchStart = t.patchStart[:len(t.patchStart)-1]
	}
	if len(t.patchNum) == 0 {
		return errSBRFreq
	}
	return nil
}

// deriveLimiter builds the limiter band table: the low-resolution borders
// merged with the patch borders, then thinned until every band spans at
// least 0.49/limBands octaves, patch borders surviving preferentially.
func deriveLimiter(t *sbrFreqTables, limiterBands int) {
	if limiterBands == 0 {
		t.fLim = []int{t.kx, t.kx + t.m}
		t.nL = 1
		return
	}
	limBands := [4]float64{0, 1.2, 2, 3}[limiterBands]
	patchBorder := map[int]bool{t.kx: true, t.kx + t.m: true}
	borders := append([]int(nil), t.fLow...)
	acc := t.kx
	for _, n := range t.patchNum {
		acc += n
		patchBorder[acc] = true
		borders = append(borders, acc)
	}
	sort.Ints(borders)
	for k := 1; k < len(borders); {
		if borders[k] == borders[k-1] {
			borders = append(borders[:k], borders[k+1:]...)
			continue
		}
		oct := math.Log2(float64(borders[k]) / float64(borders[k-1]))
		if oct*limBands < 0.49 {
			switch {
			case !patchBorder[borders[k]]:
				borders = append(borders[:k], borders[k+1:]...)
			case !patchBorder[borders[k-1]]:
				borders = append(borders[:k-1], borders[k:]...)
				if k > 1 {
					k--
				}
			default:
				k++
			}
			continue
		}
		k++
	}
	t.fLim = borders
	t.nL = len(borders) - 1
}
