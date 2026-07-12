package vorbis

import (
	"math"

	"github.com/colespringer/waxflow/dsp/psy"
)

// psymap bridges the shared psychoacoustic model (dsp/psy) to the Vorbis
// encoder. The model returns masking thresholds in its own FFT energy units;
// this converts them onto the MDCT coefficient energy the encoder measures and
// turns them into the per-partition bit-allocation decisions that shape
// quantization noise to the masking curve.
//
// Thresholds are taken per MDCT line, not per residue partition: a residue
// partition is wide (32 lines), and the lowest partition spans DC to ~700 Hz,
// where the absolute threshold of hearing is enormous. Summing a band's line
// thresholds there lets the inaudible DC threshold swamp an audible mid-band
// tone in the same partition, so the encoder would drop real content. A
// per-line threshold with a peak-SNR partition test avoids that: the DC line's
// huge threshold only masks the DC line.
//
// The FFT-to-MDCT energy conversion is self-calibrating: both transforms
// measure the same signal, so their total energies define the per-block ratio,
// and no hand-tuned window/scale constant is needed.

// newPsyModel builds the long-block psychoacoustic model for one channel with
// one masking threshold per MDCT line.
func newPsyModel(rate, n2 int, offsetDB float64) (*psy.Model, error) {
	offsets := make([]int, n2+1)
	for i := range offsets {
		offsets[i] = i // one band per line
	}
	return psy.New(psy.Config{
		Rate:        rate,
		Lines:       n2,
		FFTSize:     2 * n2,
		BandOffsets: offsets,
		OffsetDB:    offsetDB,
	})
}

// lineThresholds converts the model's per-line masking thresholds into MDCT
// energy units, using the block's own FFT-to-MDCT total-energy ratio so the
// thresholds land on the same scale as the caller's MDCT coefficient energy.
func lineThresholds(res psy.Result, spec []float32, dst []float64, n2 int) {
	var sumMDCT, sumPsy float64
	for l := 0; l < n2; l++ {
		v := float64(spec[l])
		sumMDCT += v * v
		if l < len(res.Energy) {
			sumPsy += res.Energy[l]
		}
	}
	ratio := 0.0
	if sumPsy > 0 {
		ratio = sumMDCT / sumPsy
	}
	for l := 0; l < n2; l++ {
		if l < len(res.Thr) {
			dst[l] = res.Thr[l] * ratio
		} else {
			dst[l] = 0
		}
	}
}

// classifyPartitions assigns each residue partition a coding class. A partition
// with no line above its masking threshold is masked and dropped (skip); an
// audible one is coded coarse or fine. The coarse/fine split is peak-SNR-driven
// for tonal partitions but capped at coarse for noise-like ones: the peak-per-line
// SNR test (which a tone needs, so a real spectral line is never averaged away)
// otherwise mistakes a noise fluctuation for a tone and codes noise at fine
// precision, where coarse-quantization noise is fully masked by the noise itself.
// Tonality is measured by spectral flatness: a tone concentrates energy in one
// line (low flatness), noise spreads it across the partition (high flatness).
//
// The noise cap runs only on temporally steady blocks (capNoise, decided by the
// caller). A transient's sharp attack is broadband too (flatter, even, than
// steady noise), so flatness cannot tell a coarse-tolerant noise band from a
// must-stay-sharp attack; the caller instead gates the cap on temporal steadiness
// (an attack concentrates energy into part of the block), so steady noise
// coarsens while a transient's residue stays fine regardless of block size.
func classifyPartitions(spec []float32, thrLine []float64, classes []int, partSize, n2 int, capNoise bool) {
	nParts := n2 / partSize
	for p := 0; p < nParts; p++ {
		lo, hi := p*partSize, (p+1)*partSize
		var peak, sumE float64
		for l := lo; l < hi; l++ {
			e := float64(spec[l]) * float64(spec[l])
			sumE += e
			if t := thrLine[l]; t > 0 {
				if snr := e / t; snr > peak {
					peak = snr
				}
			}
		}
		if peak <= 1 {
			classes[p] = classSkip
			continue
		}
		if capNoise && partitionFlatness(spec, lo, hi, sumE) >= noiseSFM {
			classes[p] = classCoarse
			continue
		}
		classes[p] = classFromSNR(peak)
	}
}

// partitionFlatness is the spectral flatness measure (geometric mean over
// arithmetic mean of the line energies) of one partition: ~0 for a single
// dominant line (tonal), approaching 1 for a flat noise spread. A per-line floor
// relative to the partition mean keeps a few near-zero lines from collapsing the
// geometric mean of an otherwise flat band.
func partitionFlatness(spec []float32, lo, hi int, sumE float64) float64 {
	n := float64(hi - lo)
	meanE := sumE / n
	if meanE <= 0 {
		return 0
	}
	floorE := meanE * 1e-6
	sumLog := 0.0
	for l := lo; l < hi; l++ {
		e := float64(spec[l]) * float64(spec[l])
		if e < floorE {
			e = floorE
		}
		sumLog += math.Log(e)
	}
	return math.Exp(sumLog/n) / meanE
}

// maskResidue zeros the floor-normalized residue of every line more than
// globalMaskDB below the block's loudest line. Such a line sits under the ear's
// absolute floor relative to the block's dominant content, so coding it would
// only scatter quantization noise into otherwise-silent critical bands.
//
// The drop is deliberately a block-relative floor, not the per-line psy
// threshold: the coarse structure (which whole partitions to skip) already comes
// from the psy threshold via classifyPartitions, and dropping every individually
// sub-threshold line on top of that also discards a strong tone's near-frequency
// leakage. That leakage is masked to the ear, but it lands in the critical band
// beside the tone where an objective metric with no cross-band spreading scores
// it as missing signal. Coding it (down to the block floor) is what keeps tonal
// material at parity; the partition skip still drops the genuinely empty bands.
func maskResidue(spec []float32, resid []float32, n2 int) {
	var peak2 float64
	for l := 0; l < n2; l++ {
		if s := float64(spec[l]); s*s > peak2 {
			peak2 = s * s
		}
	}
	globalFloor := peak2 * globalMaskRatio
	for l := 0; l < n2; l++ {
		s := float64(spec[l])
		if s*s <= globalFloor {
			resid[l] = 0
		}
	}
}

// globalMaskRatio is the block-relative energy floor for maskResidue: a line
// below globalMaskDB relative to the block peak is inaudible and dropped.
const (
	globalMaskDB    = 50.0
	globalMaskRatio = 1e-5 // 10^(-globalMaskDB/10)
)

// classFromSNR maps a peak signal-to-mask ratio to a residue class.
func classFromSNR(snr float64) int {
	if snr <= 1 {
		return classSkip // no line rises above its masking threshold
	}
	if 10*math.Log10(snr) < coarseFineDB {
		return classCoarse
	}
	return classFine
}

// qualityToOffsetDB maps the libvorbis -q scale (-1..10) to the psy model's
// SNR-demand offset: higher quality lowers thresholds (positive OffsetDB), so
// more partitions clear the audibility test and are coded finely. The default
// quality (3) sits at 0 dB, the model's nominal calibration.
func qualityToOffsetDB(quality float64) float64 {
	return (quality - DefaultQuality) * 2.0
}

// Residue classes (4b): skip a masked partition, code an audible one coarse or
// fine. coarseFineDB is the peak SNR above the masking threshold at which fine
// coding starts.
const (
	classSkip    = 0
	classCoarse  = 1
	classFine    = 2
	numResClass  = 3
	coarseFineDB = 12.0
	// noiseSFM is the spectral-flatness threshold above which a partition is
	// treated as noise-like and capped at coarse. A single windowed tone leaks to
	// a flatness well under this; broadband noise sits well above it.
	noiseSFM = 0.25
)
