// Package psy is the shared psychoacoustic model behind the lossy
// encoders. It follows the ISO 11172-3 Annex D model 2 structure, the
// same analysis ISO 13818-7 Annex B adapts for AAC, so one
// implementation serves both encoders: a Hann-windowed FFT of the block,
// two-frame polar prediction giving per-line unpredictability, energy
// and weighted unpredictability accumulated into roughly third-bark
// partitions, a two-slope spreading convolution, tonality mapped to a
// tone-masking-noise / noise-masking-tone SNR demand, and the resulting
// masking threshold distributed onto the caller's scalefactor bands.
//
// The model is resolution-agnostic: the caller states its transform
// geometry (Lines coefficients per block, band offsets in those units)
// and the analysis FFT size, and the partition and band tables are
// derived from the bark scale at construction rather than ported from
// the per-rate spec tables. Thresholds come back in FFT energy units;
// each encoder owns one calibration constant mapping them onto its own
// transform's energy scale (window shapes and normalizations differ,
// the ratio is constant per geometry).
//
// Levels are anchored by convention: a full-scale sine is taken to play
// back at 96 dB SPL, which places the absolute threshold of hearing.
// Input is one block of full-scale float PCM (|x| <= 1).
//
// A Model carries per-channel state (prediction history, the pre-echo
// threshold memory), so callers construct one Model per channel and one
// per analysis geometry. It is not safe for concurrent use.
package psy

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/waxerr"
)

// Version is the model's algorithm revision. Encoder version strings
// (the cache key inputs, ADR-0004) must compose it in, so retuning the
// model invalidates exactly the streams it changes.
const Version = "psy-1"

// Model constants. tmn and nmt are the classic masking demands: a tonal
// masker hides noise 29 dB down, a noise masker hides a tone 6 dB down.
// rpelev caps how fast the threshold may rise between consecutive
// blocks, the model 2 pre-echo control. athSPL anchors full scale.
const (
	tmnDB   = 29.0
	nmtDB   = 6.0
	rpelev  = 2.0
	athSPL  = 96.0
	maxPart = 1.0 / 3.0 // maximum partition width in bark
)

// Config declares one analysis geometry.
type Config struct {
	// Rate is the sample rate in Hz.
	Rate int
	// Lines is the caller's transform resolution: coefficients per
	// block (576 for an MP3 granule, 1024 for an AAC long window, 128
	// for an AAC short window).
	Lines int
	// FFTSize is the analysis window length, a power of two. It need
	// not match 2*Lines: MP3 analyzes a 576-line granule with a
	// 1024-point FFT, the model 2 arrangement.
	FFTSize int
	// BandOffsets are the scalefactor band boundaries in transform-line
	// units, starting at 0 and ending at Lines (the sfb edge tables
	// both encoders already carry).
	BandOffsets []int
	// NoPredict replaces two-frame prediction with FixedC as every
	// line's unpredictability, the standard short-block simplification
	// (prediction across 8 discontiguous sub-windows is meaningless).
	NoPredict bool
	// FixedC is the unpredictability used under NoPredict, in [0,1].
	// 0.4 is the customary short-block value.
	FixedC float64
	// OffsetDB shifts every band's SNR demand: positive values lower
	// thresholds (more bits, higher quality). This is the encoders'
	// quality/rate tuning knob.
	OffsetDB float64
	// ATHOffsetDB shifts the absolute-threshold floor: positive values
	// lower it (more of the near-inaudible spectral extremes are kept).
	// The 96 dB SPL full-scale anchor is a playback-level convention, so
	// content near the absolute threshold is a judgment call the quality
	// setting should own: at high quality an encoder buys insurance
	// against louder playback, at low quality it sheds barely-audible
	// extremes first. Zero keeps the anchored threshold as-is (the
	// existing encoders' behavior).
	ATHOffsetDB float64
}

// Result is one block's analysis. The slices are owned by the Model and
// valid only until the next Analyze call; callers needing them longer
// copy them out.
type Result struct {
	// Thr is the masking threshold per band in FFT energy units: the
	// most quantization noise energy the band tolerates inaudibly,
	// after spreading, tonality, pre-echo control, and the absolute
	// threshold floor.
	Thr []float64
	// Energy is the block's FFT energy per band, for stereo decisions
	// and rate estimation.
	Energy []float64
	// PE is the block's perceptual entropy in bits: the information
	// the block carries above its masking threshold. High PE flags
	// hard content (transients, dense spectra) for bit reservoirs and
	// window switching.
	PE float64
}

// partition is one roughly third-bark group of FFT lines.
type partition struct {
	lo, hi int     // FFT line span [lo, hi)
	bval   float64 // center bark
	minDB  float64 // minimum SNR demand
	qthr   float64 // absolute hearing threshold energy
}

// Model is one channel's analysis state for one geometry.
type Model struct {
	cfg    Config
	win    []float64 // Hann window
	fft    *fftPlan
	parts  []partition
	spread []float64 // normalized spreading matrix, parts x parts
	// band mapping: for each band, the partitions overlapping it with
	// their width fractions, and the FFT line span for band energy.
	bandParts [][]bandPart
	bandLines [][2]int
	// scratch, sized at New
	xw, re, im  []float64
	e, c        []float64 // per line
	pe, pc      []float64 // per partition: energy, weighted unpredictability
	thrP        []float64
	prevThr     []float64
	thr, energy []float64 // per band (the Result views)
	// prediction history for t-1 and t-2: magnitude and the phase's unit
	// vector (cos, sin). Storing the vector instead of the angle keeps the
	// two-frame polar prediction free of trigonometry: the predicted
	// phase 2*f1-f2 comes from double- and difference-angle identities.
	rPrev, cPrev, sPrev [2][]float64
	frames              int
}

type bandPart struct {
	p    int
	frac float64
}

// New builds a Model for the geometry. See Config for field semantics.
func New(cfg Config) (*Model, error) {
	if cfg.Rate <= 0 || cfg.Lines <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "psy: rate and lines must be positive")
	}
	if cfg.FFTSize < 64 || cfg.FFTSize&(cfg.FFTSize-1) != 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("psy: FFT size %d is not a power of two >= 64", cfg.FFTSize))
	}
	if n := len(cfg.BandOffsets); n < 2 || cfg.BandOffsets[0] != 0 || cfg.BandOffsets[n-1] != cfg.Lines {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"psy: band offsets must run 0..Lines")
	}
	for i := 1; i < len(cfg.BandOffsets); i++ {
		if cfg.BandOffsets[i] <= cfg.BandOffsets[i-1] {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				"psy: band offsets must be strictly increasing")
		}
	}
	if cfg.NoPredict && !(cfg.FixedC >= 0 && cfg.FixedC <= 1) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "psy: FixedC outside [0,1]")
	}

	m := &Model{cfg: cfg, fft: newFFTPlan(cfg.FFTSize)}
	n := cfg.FFTSize
	m.win = make([]float64, n)
	for i := range m.win {
		m.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*(float64(i)+0.5)/float64(n))
	}

	m.buildPartitions()
	m.buildSpreading()
	m.buildBandMap()

	half := n / 2
	m.xw = make([]float64, n)
	m.re = make([]float64, n)
	m.im = make([]float64, n)
	m.e = make([]float64, half)
	m.c = make([]float64, half)
	np := len(m.parts)
	m.pe = make([]float64, np)
	m.pc = make([]float64, np)
	m.thrP = make([]float64, np)
	m.prevThr = make([]float64, np)
	nb := len(cfg.BandOffsets) - 1
	m.thr = make([]float64, nb)
	m.energy = make([]float64, nb)
	if !cfg.NoPredict {
		for h := range m.rPrev {
			m.rPrev[h] = make([]float64, half)
			m.cPrev[h] = make([]float64, half)
			m.sPrev[h] = make([]float64, half)
		}
	}
	return m, nil
}

// Bands returns the band count the geometry produces.
func (m *Model) Bands() int { return len(m.cfg.BandOffsets) - 1 }

// Reset clears prediction history and pre-echo memory, for use after
// seeks or stream splices.
func (m *Model) Reset() {
	m.frames = 0
	for h := range m.rPrev {
		if m.rPrev[h] != nil {
			clear(m.rPrev[h])
			clear(m.cPrev[h])
			clear(m.sPrev[h])
		}
	}
	clear(m.prevThr)
}

// bark converts frequency in Hz to the bark scale.
func bark(f float64) float64 {
	return 13*math.Atan(0.00076*f) + 3.5*math.Atan((f/7500)*(f/7500))
}

// athDB is the absolute threshold of hearing in dB SPL (Terhardt's
// approximation), clamped to keep the extremes finite.
func athDB(f float64) float64 {
	if f < 20 {
		return 120
	}
	k := f / 1000
	v := 3.64*math.Pow(k, -0.8) - 6.5*math.Exp(-0.6*(k-3.3)*(k-3.3)) + 1e-3*k*k*k*k
	return math.Min(math.Max(v, -12), 120)
}

// buildPartitions groups FFT lines into partitions no wider than a
// third of a bark (single lines where the resolution is coarser).
func (m *Model) buildPartitions() {
	half := m.cfg.FFTSize / 2
	df := float64(m.cfg.Rate) / float64(m.cfg.FFTSize)
	// Full-scale sine energy at its FFT peak line under the Hann
	// window: amplitude N/4, so energy (N/4)^2. The ATH anchor.
	fsLine := float64(m.cfg.FFTSize) / 4
	fsEnergy := fsLine * fsLine

	athScale := math.Pow(10, -m.cfg.ATHOffsetDB/10)
	lo := 0
	for lo < half {
		hi := lo + 1
		bLo := bark(float64(lo) * df)
		for hi < half && bark(float64(hi+1)*df)-bLo <= maxPart {
			hi++
		}
		center := (float64(lo) + float64(hi)) / 2 * df
		bval := bark(center)
		qthr := 0.0
		for w := lo; w < hi; w++ {
			qthr += math.Pow(10, (athDB(float64(w)*df)-athSPL)/10) * fsEnergy * athScale
		}
		m.parts = append(m.parts, partition{
			lo: lo, hi: hi, bval: bval,
			minDB: minvalDB(bval),
			qthr:  qthr,
		})
		lo = hi
	}
}

// minvalDB is the per-partition floor on the SNR demand: low bands keep
// a substantial demand even for noise-like content (the model 2 minval
// tables have this shape), fading out above 12 bark.
func minvalDB(bval float64) float64 {
	if bval <= 12 {
		return 24.5
	}
	return math.Max(0, 24.5-(bval-12)*(24.5/6))
}

// spreadDB is the model 2 two-slope spreading function: dz is maskee
// bark minus masker bark. Roughly -25 dB/bark below the masker and
// -10 dB/bark above it.
func spreadDB(dz float64) float64 {
	x := 1.05 * dz
	d := x - 0.5
	extra := 8 * math.Min(d*d-2*d, 0)
	y := 15.811389 + 7.5*(x+0.474) - 17.5*math.Sqrt(1+(x+0.474)*(x+0.474))
	if y < -100 {
		return -1000
	}
	return extra + y
}

// buildSpreading precomputes the partition-to-partition spreading
// matrix with each maskee row normalized to unit gain, so a flat
// spectrum convolves to itself and thresholds stay in energy units.
func (m *Model) buildSpreading() {
	np := len(m.parts)
	m.spread = make([]float64, np*np)
	for i := 0; i < np; i++ {
		row := m.spread[i*np : (i+1)*np]
		sum := 0.0
		for j := 0; j < np; j++ {
			s := math.Pow(10, spreadDB(m.parts[i].bval-m.parts[j].bval)/10)
			row[j] = s
			sum += s
		}
		for j := range row {
			row[j] /= sum
		}
	}
}

// buildBandMap precomputes the partition overlap fractions and FFT line
// spans for each caller band. Bands and partitions live on different
// grids; frequency is the common axis.
func (m *Model) buildBandMap() {
	nb := len(m.cfg.BandOffsets) - 1
	m.bandParts = make([][]bandPart, nb)
	m.bandLines = make([][2]int, nb)
	// A transform line l covers [l, l+1) * rate/(2*Lines) Hz; an FFT
	// line w covers [w, w+1) * rate/FFTSize Hz. Work in FFT line units:
	// scale converts transform offsets onto the FFT grid.
	scale := float64(m.cfg.FFTSize) / (2 * float64(m.cfg.Lines))
	half := float64(m.cfg.FFTSize / 2)
	for b := 0; b < nb; b++ {
		loF := math.Min(float64(m.cfg.BandOffsets[b])*scale, half)
		hiF := math.Min(float64(m.cfg.BandOffsets[b+1])*scale, half)
		li := int(loF)
		hi := int(math.Ceil(hiF))
		m.bandLines[b] = [2]int{li, hi}
		for p := range m.parts {
			pLo, pHi := float64(m.parts[p].lo), float64(m.parts[p].hi)
			ov := math.Min(hiF, pHi) - math.Max(loF, pLo)
			if ov <= 0 {
				continue
			}
			m.bandParts[b] = append(m.bandParts[b], bandPart{p: p, frac: ov / (pHi - pLo)})
		}
	}
}

// Analyze runs the model over one block. x must hold exactly FFTSize
// samples: the block the caller is about to transform, in the same
// alignment it documents (the calibration constant absorbs any fixed
// offset). The returned slices are valid until the next call.
func (m *Model) Analyze(x []float32) (Result, error) {
	n := m.cfg.FFTSize
	if len(x) != n {
		return Result{}, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("psy: block is %d samples, geometry wants %d", len(x), n))
	}
	for i := 0; i < n; i++ {
		m.xw[i] = float64(x[i]) * m.win[i]
		m.im[i] = 0
	}
	copy(m.re, m.xw)
	m.fft.transform(m.re, m.im)

	half := n / 2
	cur, old := m.frames%2, (m.frames+1)%2
	for w := 0; w < half; w++ {
		re, im := m.re[w], m.im[w]
		e := re*re + im*im
		r := math.Sqrt(e)
		m.e[w] = e
		if m.cfg.NoPredict {
			m.c[w] = m.cfg.FixedC
			continue
		}
		// The phase is carried as its unit vector; a silent line keeps the
		// zero angle (cos 1, sin 0), matching atan2(0, 0).
		cf, sf := 1.0, 0.0
		if r > 0 {
			cf, sf = re/r, im/r
		}
		if m.frames < 2 {
			m.c[w] = 0.4
		} else {
			r1, c1, s1 := m.rPrev[cur][w], m.cPrev[cur][w], m.sPrev[cur][w]
			c2, s2 := m.cPrev[old][w], m.sPrev[old][w]
			rp := 2*r1 - m.rPrev[old][w]
			// Predicted phase 2*f1 - f2 by double- and difference-angle
			// identities on the stored unit vectors (no trigonometry).
			c11 := 2*c1*c1 - 1
			s11 := 2 * s1 * c1
			cp := c11*c2 + s11*s2
			sp := s11*c2 - c11*s2
			dx := r*cf - rp*cp
			dy := r*sf - rp*sp
			den := r + math.Abs(rp)
			if den > 0 {
				m.c[w] = math.Min(math.Sqrt(dx*dx+dy*dy)/den, 1)
			} else {
				m.c[w] = 0.4
			}
		}
		m.rPrev[old][w] = r
		m.cPrev[old][w] = cf
		m.sPrev[old][w] = sf
	}
	m.frames++

	// Partition accumulation: energy and energy-weighted
	// unpredictability.
	for p := range m.parts {
		e, c := 0.0, 0.0
		for w := m.parts[p].lo; w < m.parts[p].hi; w++ {
			e += m.e[w]
			c += m.c[w] * m.e[w]
		}
		m.pe[p] = e
		m.pc[p] = c
	}

	// Spreading convolution, tonality, SNR demand, pre-echo control,
	// absolute floor.
	np := len(m.parts)
	pe := 0.0
	for i := 0; i < np; i++ {
		row := m.spread[i*np : (i+1)*np]
		ecb, ctb := 0.0, 0.0
		for j := 0; j < np; j++ {
			ecb += row[j] * m.pe[j]
			ctb += row[j] * m.pc[j]
		}
		cb := 1.0
		if ecb > 0 {
			cb = math.Min(math.Max(ctb/ecb, 1e-10), 1)
		}
		tb := math.Min(math.Max(-0.299-0.43*math.Log(cb), 0), 1)
		snr := math.Max(m.parts[i].minDB, tmnDB*tb+nmtDB*(1-tb)) + m.cfg.OffsetDB
		nb := ecb * math.Pow(10, -snr/10)
		if m.frames > 1 {
			nb = math.Min(nb, rpelev*m.prevThr[i])
		}
		thr := math.Max(m.parts[i].qthr, nb)
		m.thrP[i] = thr
		m.prevThr[i] = thr
		if m.pe[i] > thr {
			w := float64(m.parts[i].hi - m.parts[i].lo)
			pe += w * math.Log2(m.pe[i]/thr)
		}
	}

	// Distribute onto the caller's bands.
	for b := range m.thr {
		t := 0.0
		for _, bp := range m.bandParts[b] {
			t += m.thrP[bp.p] * bp.frac
		}
		m.thr[b] = t
		e := 0.0
		for w := m.bandLines[b][0]; w < m.bandLines[b][1]; w++ {
			e += m.e[w]
		}
		m.energy[b] = e
	}
	return Result{Thr: m.thr, Energy: m.energy, PE: pe}, nil
}
