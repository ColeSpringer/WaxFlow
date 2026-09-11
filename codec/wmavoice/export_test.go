//go:build !wmavoicetablesgen

package wmavoice

// Internals the package's black-box tests read. Everything here is a view of
// something the decoder already computes; nothing exists only for a test.

// Geom mirrors the derived geometry of note 2 so a test can pin the note's
// four-row table field by field.
type Geom struct {
	MinPitch, MaxPitch, PitchRange, PitchBits int
	History                                   int
	Conv                                      [4]int
	DeltaPitchHalf, DeltaPitchBits            int
	BlockPitchRange, BlockPitchBits           int
	SpilloverBits                             int
}

func Geometry(c Config) Geom {
	g := c.geom()
	return Geom{
		MinPitch: g.minPitch, MaxPitch: g.maxPitch, PitchRange: g.pitchRange,
		PitchBits: g.pitchBits, History: g.history, Conv: g.conv,
		DeltaPitchHalf: g.deltaPitchHalf, DeltaPitchBits: g.deltaPitchBits,
		BlockPitchRange: g.blockPitchRange, BlockPitchBits: g.blockPitchBits,
		SpilloverBits: g.spilloverBits,
	}
}

func SpilloverBits(c Config) int { return c.geom().spilloverBits }
func QuantiserB(c Config) bool   { return c.lspQuantiserB() }
func DefaultB(c Config) bool     { return c.meanIndex() == 1 }

// TreeClasses inverts the symbol-to-frame-type map back into the seventeen
// class fields the extradata carries, which is the form the note states.
func TreeClasses(c Config) [17]int {
	var out [17]int
	for slot, ft := range c.Tree {
		if ft >= 0 {
			out[ft] = slot / 3
		}
	}
	return out
}

// Paths is the decoder's own count of the bitstream branches it took, which is
// what pins the corpus note's coverage matrix against this walk rather than
// only against its samples.
type Paths = pathCounts

func (d *Decoder) Paths() Paths { return d.paths }

// Superframe geometry, so a test can speak in the units the notes do.
const (
	SuperframeSamples   = superframeSamples
	FrameSamples        = frameSamples
	FramesPerSuperframe = framesPerSuperframe
	ReferenceCarryBits  = referenceCarryBits
)

// Transform access for the closed-form checks of note 10.5.
type Transforms = transforms

func NewTransforms() *Transforms { return newTransforms() }

func (t *Transforms) ForwardDFT(dst *[dftCoefs]float32, src *[dftLen]float32) { t.forwardDFT(dst, src) }
func (t *Transforms) InverseDFT(dst *[dftLen]float32, src *[dftCoefs]float32) { t.inverseDFT(dst, src) }
func (t *Transforms) PhaseRef65(src *[trigLen]float32) float32                { return t.phaseRef65(src) }

func (t *Transforms) DCTI(dst, src *[trigLen]float32) { t.dctI(dst, src) }
func (t *Transforms) DSTI(dst, src *[trigLen]float32) { t.dstI(dst, src) }

const (
	DFTLen   = dftLen
	DFTCoefs = dftCoefs
	TrigLen  = trigLen
)

// LSPToLPC is the expansion of note 5.7, so a test can check it against a
// known LSF set before anything downstream is trusted.
func LSPToLPC(lpc []float32, lsp []float64) {
	pa := make([]float64, len(lsp)/2+1)
	qa := make([]float64, len(lsp)/2+1)
	lspToLPC(lpc, lsp, pa, qa)
}

// Stabilise is note 5.6, with the flags saying which clamp fired.
func Stabilise(lsf []float64) (floored, capped, sorted bool) { return stabilise(lsf) }

// Codebook sizes, so a test can prove every index a bitstream field can carry
// addresses inside its own stage.
func CodebookLens() map[string]int {
	return map[string]int{
		"dqLSP10i":  len(dqLSP10i),
		"dqLSP10r":  len(dqLSP10r),
		"dqLSP16i1": len(dqLSP16i1),
		"dqLSP16i2": len(dqLSP16i2),
		"dqLSP16i3": len(dqLSP16i3),
		"dqLSP16r1": len(dqLSP16r1),
		"dqLSP16r2": len(dqLSP16r2),
		"dqLSP16r3": len(dqLSP16r3),
	}
}

// TypeRuns exposes the canonical frame type code so a test can prove the Kraft
// sum is exactly 1 and that every symbol decodes back to itself.
func TypeRuns() ([22]int, [maxTypeCodeLen + 1]struct {
	First         uint64
	Symbol, Count int
}) {
	var out [maxTypeCodeLen + 1]struct {
		First         uint64
		Symbol, Count int
	}
	for i, r := range typeRuns {
		out[i].First, out[i].Symbol, out[i].Count = r.first, r.symbol, r.count
	}
	return typeCodeLens, out
}

// MaxMagnitude is the bound on an emitted sample, past which a superframe is
// refused as a synthesis that has run away.
const MaxMagnitude = maxMagnitude

// FrameCounter is the comfort-noise generator's input, which a refused
// superframe must still advance when it was decoded in full.
func (d *Decoder) FrameCounter() int { return d.frameCounter }
