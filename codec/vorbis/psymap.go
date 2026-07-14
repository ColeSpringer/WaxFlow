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
// one masking threshold per MDCT line. The quality offset drives the absolute
// threshold too: the ATH anchor is a playback-level convention, and the region
// it governs (chiefly the top octaves) would otherwise be invisible to the
// quality knob, since OffsetDB only shifts the masking-derived demand. At high
// quality the encoder keeps near-threshold extremes (insurance against loud
// playback); at low quality it sheds them first.
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
		ATHOffsetDB: offsetDB,
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

// classifyPartitions assigns each residue partition a coding class: the
// cheapest rung of the precision ladder whose quantization noise stays under
// the partition's masking demand. A partition with no line above its masking
// threshold is masked and dropped (skip); noise-like partitions are capped at
// the cheap noise class (coarse quantization noise there is self-masked); an
// audible tonal partition takes the class whose step meets its demand.
//
// The demand is measured where quantization noise actually lands: a residue
// step of delta reconstructs with noise ~delta^2/12 scaled by the floor curve's
// energy at each line, so the binding line is the audible line maximizing
// curve^2/thr, not the peak signal-to-mask line (a quiet audible line under a
// high peak-envelope floor takes full-floor-scaled noise). The audibility test
// (peak per-line SNR, never a band sum) is unchanged: a real spectral line must
// never be averaged away by the huge DC threshold sharing its partition.
//
// Because the psy thresholds carry the quality knob (OffsetDB), the demand
// rises and falls with -q, so partitions migrate up the ladder at high quality
// (through fine to superfine) and down at low quality. This is what lets the
// quality setting reach bands that are already audible instead of only
// reclassifying near-masked ones.
//
// The noise cap runs only on temporally steady blocks (capNoise, decided by the
// caller). A transient's sharp attack is broadband too (flatter, even, than
// steady noise), so flatness cannot tell a coarse-tolerant noise band from a
// must-stay-sharp attack; the caller instead gates the cap on temporal steadiness
// (an attack concentrates energy into part of the block), so steady noise
// coarsens while a transient's residue stays fine regardless of block size.
func classifyPartitions(spec, curve []float32, thrLine []float64, classes []int, partSize, n2 int, capNoise bool) {
	nParts := n2 / partSize
	for p := 0; p < nParts; p++ {
		lo, hi := p*partSize, (p+1)*partSize
		var peak, demand, sumE float64
		for l := lo; l < hi; l++ {
			e := float64(spec[l]) * float64(spec[l])
			sumE += e
			if t := thrLine[l]; t > 0 {
				if snr := e / t; snr > peak {
					peak = snr
				}
				if e > t {
					// A line's demand is the floor-scaled quantization noise it
					// takes (curve^2/thr), capped at demandSlack above its own
					// audibility: a quiet line a coarse step would round to zero
					// costs at most its own energy, so it cannot honestly demand
					// the full floor-referenced precision a loud line does.
					cv := float64(curve[l])
					d := cv * cv
					if maxD := e * demandSlack; maxD < d {
						d = maxD
					}
					d /= t
					if d > demand {
						demand = d
					}
				}
			}
		}
		if peak <= 1 {
			classes[p] = classSkip
			continue
		}
		if capNoise && partitionFlatness(spec, lo, hi, sumE) >= noiseSFM {
			classes[p] = classNoise
			continue
		}
		classes[p] = classFromDemand(demand)
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

// maskResidue zeros the floor-normalized residue of every line that is BOTH
// more than globalMaskDB below the block's loudest line AND under its own psy
// masking threshold. Such a line is inaudible twice over, so coding it would
// only spend bits scattering quantization noise into otherwise-silent bands.
//
// Both conditions are required, each covering the other's failure mode. The
// block-relative floor alone (the earlier form) zeroed audible content on
// tilted real spectra, where legitimate high-frequency detail sits ~50+ dB
// below the bass peak yet well above the ear's threshold; requiring the line
// to also be psy-inaudible keeps that detail. The per-line psy test alone
// would discard a strong tone's near-frequency leakage: masked to the ear, but
// it lands in the critical band beside the tone where an objective metric with
// no cross-band spreading scores it as missing signal, so anything within the
// block floor of the peak stays coded regardless. A nil thrLine (the offline
// book generator, which has no psy model) falls back to the floor test alone.
func maskResidue(spec, resid []float32, thrLine []float64, n2 int) {
	var peak2 float64
	for l := 0; l < n2; l++ {
		if s := float64(spec[l]); s*s > peak2 {
			peak2 = s * s
		}
	}
	globalFloor := peak2 * globalMaskRatio
	for l := 0; l < n2; l++ {
		s := float64(spec[l])
		e := s * s
		if e > globalFloor {
			continue
		}
		if thrLine == nil || e <= thrLine[l] {
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

// classFromDemand maps a partition's masking demand (max curve^2/thr over its
// audible lines) to the cheapest residue class whose quantization step meets
// it. A step of delta injects ~delta^2/12 noise per unit of floor energy, so a
// class's capability is 10log10(12/delta^2), an even ~12 dB per rung: ~29 dB for
// noise (1/8), ~35 for coarse (1/16), ~47 for med (1/64), ~59 for fine (1/256),
// ~71 for super (1/1024). The boundaries sit a few dB below those capabilities,
// leaving headroom for the uniform-error model's optimism (worst-case error is
// ~4.8 dB over RMS) and the floor's interpolation wander. The lowest audible
// tonal bands ride the same cheap noise book as the noise cap, so barely-audible
// content spends the least.
func classFromDemand(demand float64) int {
	db := 10 * math.Log10(demand)
	switch {
	case db <= noiseMaxDB:
		return classNoise
	case db <= coarseMaxDB:
		return classCoarse
	case db <= medMaxDB:
		return classMed
	case db <= fineMaxDB:
		return classFine
	default:
		return classSuper
	}
}

// qualityToOffsetDB maps the libvorbis -q scale (-1..10) to the psy model's
// SNR-demand offset: higher quality lowers thresholds (positive OffsetDB), so
// more partitions clear the audibility test and are coded finely. The default
// quality (3) sits at 0 dB, the model's nominal calibration.
func qualityToOffsetDB(quality float64) float64 {
	return (quality - DefaultQuality) * 2.0
}

// Residue classes: skip a masked partition, cap a steady noise partition at
// the cheap noise book, and code an audible tonal one at the cheapest of
// coarse/med/fine/super that meets its masking demand. The ordinals ascend in
// precision, so "the more demanding of two classes" is the larger ordinal
// (deriveCoupledClasses relies on this).
const (
	classSkip   = 0
	classNoise  = 1
	classCoarse = 2
	classMed    = 3
	classFine   = 4
	classSuper  = 5
	numResClass = 6
	// The demand boundaries of the ladder, each ~3 dB under the class's
	// ~29/35/47/59 dB capability (see classFromDemand); a demand over fineMaxDB
	// takes the top (super) rung.
	noiseMaxDB  = 26.0
	coarseMaxDB = 32.0
	medMaxDB    = 44.0
	fineMaxDB   = 56.0
	// demandSlack caps a quiet audible line's demand at this factor (~12 dB)
	// over its own signal-to-mask ratio (see classifyPartitions).
	demandSlack = 16.0
	// noiseSFM is the spectral-flatness threshold above which a partition is
	// treated as noise-like and capped at the noise class. A single windowed tone
	// leaks to a flatness well under this; broadband noise sits well above it.
	noiseSFM = 0.25
)
