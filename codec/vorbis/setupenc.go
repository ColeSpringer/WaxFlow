package vorbis

import "math/bits"

// The encode-side stream configuration and its header serialization: the exact
// inverse of parseID / parseSetup (setup.go). An encConfig both drives encoding
// (its floor/residue geometry is reused during Encode) and serializes to the
// three Vorbis headers, so the encoder and decoder are guaranteed to agree on
// geometry, book by book, field by field.
//
// The 4a configuration is deliberately minimal: long blocks only, one floor,
// one residue, one mapping, one mode. 4c adds the short block size with its own
// floor/residue/mapping/mode and bumps EncoderVersion. The header format is
// therefore a sub-phase property, not frozen here.

// encConfig is the encoder's parsed-equivalent stream configuration. It carries
// both block sizes' geometry (4c block switching): slot 0 is the short block,
// slot 1 the long, matching blockSizes. Each slot has its own floor, residue,
// and mapping because the geometry (X positions, coded band, partition count)
// scales with the block's n2. Two modes select the two slots.
type encConfig struct {
	channels   int
	rate       int
	blockSizes [2]int

	specs []bookSpec
	books []*encBook

	// Per-block-size geometry, indexed [0]=short [1]=long. Built directly (not
	// parsed) so the fitter reuses the decoder's floor1/residue structs for X
	// positions, neighbours, and partition layout.
	floors         [2]*floor1
	floorRangebits [2]int
	residues       [2]*residue
	mappings       [2]*mapping
	modes          []mode
	modeBits       int
}

// Block-size slots. long/short index encConfig's per-size arrays and match the
// blockSizes ordering (short first, as the ID header requires bs0 <= bs1).
const (
	slotShort = 0
	slotLong  = 1
)

// headerOrder is the order floors/residues/mappings are written to (and so
// indexed in) the setup header: long first, short second. So header floor 0 is
// the long floor, floor 1 the short, which is what the mapping submaps and the
// mode->mapping chain reference.
var headerOrder = [2]int{slotLong, slotShort}

// slotFor returns the geometry slot for a block size.
func (c *encConfig) slotFor(n int) int {
	if n == c.blockSizes[slotLong] {
		return slotLong
	}
	return slotShort
}

// modeForSlot returns the mode number that selects a slot. modes[0] is long,
// modes[1] short (see newEncConfig).
func modeForSlot(slot int) int {
	if slot == slotLong {
		return 0
	}
	return 1
}

// blockLog is the base-2 log of a power-of-two block size (the ID header field).
func blockLog(n int) int { return bits.Len(uint(n)) - 1 }

// newEncConfig builds the stream configuration for the given channel count and
// rate: long and short floor/residue/mapping plus the two modes that select
// them. The mux (per-channel submap routing) is trivial here; coupling adds a
// coupled mapping variant.
func newEncConfig(channels, rate int) *encConfig {
	c := &encConfig{
		channels:   channels,
		rate:       rate,
		blockSizes: [2]int{shortBlock, longBlock},
	}
	// The codebook set is immutable and identical for every encoder, so share the
	// package-level copy built once at init rather than rebuilding it here.
	c.specs = encSpecs
	c.books = encBooks
	c.floors[slotLong], c.floorRangebits[slotLong] = buildFloor1(longBlock/2, floorPartitions)
	c.floors[slotShort], c.floorRangebits[slotShort] = buildFloor1(shortBlock/2, shortFloorPartitions)
	c.residues[slotLong] = buildResidue(longBlock / 2)
	c.residues[slotShort] = buildResidue(shortBlock / 2)
	// mappings[0] routes to the long floor/residue (submap 0 -> floor 0, res 0),
	// mappings[1] to the short (floor 1, res 1). The floors/residues are written
	// to the header long-first (see setupHeader), so long is index 0 there.
	c.mappings[slotLong] = &mapping{submaps: []submap{{floor: 0, residue: 0}}, mux: make([]int, channels)}
	c.mappings[slotShort] = &mapping{submaps: []submap{{floor: 1, residue: 1}}, mux: make([]int, channels)}
	// Stereo: couple channel 0 (magnitude) with channel 1 (angle). Both channels
	// route to the one submap and are coded as per-channel (type-1) residues, so
	// each is classified on its own and a channel-agreeing input leaves the angle
	// near zero (its partitions then skip). Coupling is applied by the mapping
	// after residue decode, so magnitude and angle need not share a residue.
	if channels == 2 {
		for _, m := range c.mappings {
			m.couplingMag = []int{0}
			m.couplingAng = []int{1}
		}
	}
	// modes[0] = long (mapping 0), modes[1] = short (mapping 1).
	c.modes = []mode{{blockflag: true, mapping: 0}, {blockflag: false, mapping: 1}}
	c.modeBits = ilog(len(c.modes) - 1)
	return c
}

const (
	shortBlock = 256
	longBlock  = 2048
)

// resPartSize is the residue partition width in coefficients: also the psy
// band width, so a masking threshold maps straight to a partition.
const resPartSize = 32

// buildResidue lays out a block's residue: resPartSize partitions, six
// perceptual classes, and up to a four-pass cascade of the product-lattice
// residue books. A skipped partition carries no book (only its class symbol); a
// noise partition is one pass of the cheap noise book; the tonal classes share
// the coarse book as pass 0 and add 0..3 quarter-step refinement passes (r1/r2/
// r3), so med/fine/super reconstruct on the 1/64, 1/256, and 1/1024 grids.
//
// Both mono and coupled stereo use residue type 1 (per-channel classification).
// For a stereo pair the two channels are the coupled magnitude and angle, and
// type 1 classifies each on its own: the magnitude follows the band's masking
// allocation while the angle is coded by its own rule (a small stereo-image
// angle needs fine absolute precision that a shared class cannot give). Coupling
// is applied by the mapping after residue decode, so the reference decoder
// (ffmpeg/libvorbis) reconstructs the pair regardless of the residue framing.
func buildResidue(n2 int) *residue {
	r := &residue{
		kind:      1,
		begin:     0,
		end:       n2,
		partSize:  resPartSize,
		classes:   numResClass,
		classbook: bookResClass,
		maxPass:   4,
	}
	r.books = make([][]int, numResClass)
	r.books[classSkip] = []int{-1, -1, -1, -1, -1, -1, -1, -1}
	r.books[classNoise] = []int{bookResNoise, -1, -1, -1, -1, -1, -1, -1}
	r.books[classCoarse] = []int{bookResCoarse, -1, -1, -1, -1, -1, -1, -1}
	r.books[classMed] = []int{bookResCoarse, bookResR1, -1, -1, -1, -1, -1, -1}
	r.books[classFine] = []int{bookResCoarse, bookResR1, bookResR2, -1, -1, -1, -1, -1}
	r.books[classSuper] = []int{bookResCoarse, bookResR1, bookResR2, bookResR3, -1, -1, -1, -1}
	return r
}

// --- header serialization -------------------------------------------------

// writeString emits a header's type byte and the "vorbis" signature. The writer
// is byte-aligned here, so 8-bit writes land as raw bytes.
func writeString(w *bitWriter, typ byte, sig string) {
	w.writeBits(8, uint32(typ))
	for i := 0; i < len(sig); i++ {
		w.writeBits(8, uint32(sig[i]))
	}
}

// idHeader serializes the identification header (inverse of parseID).
func (c *encConfig) idHeader() []byte {
	var w bitWriter
	writeString(&w, 0x01, "vorbis")
	w.writeBits(32, 0) // vorbis version
	w.writeBits(8, uint32(c.channels))
	w.writeBits(32, uint32(c.rate))
	w.writeBits(32, 0) // bitrate maximum
	w.writeBits(32, 0) // bitrate nominal
	w.writeBits(32, 0) // bitrate minimum
	w.writeBits(4, uint32(blockLog(c.blockSizes[0])))
	w.writeBits(4, uint32(blockLog(c.blockSizes[1])))
	w.writeBit(1) // framing
	return w.bytes()
}

// commentHeader serializes a Vorbis comment header with the given vendor string
// and user comments ("KEY=value"). The muxer owns tags in the container path
// (phase 5); this is the standalone/deterministic default.
func commentHeader(vendor string, comments []string) []byte {
	var w bitWriter
	writeString(&w, 0x03, "vorbis")
	w.writeBits(32, uint32(len(vendor)))
	for i := 0; i < len(vendor); i++ {
		w.writeBits(8, uint32(vendor[i]))
	}
	w.writeBits(32, uint32(len(comments)))
	for _, cm := range comments {
		w.writeBits(32, uint32(len(cm)))
		for i := 0; i < len(cm); i++ {
			w.writeBits(8, uint32(cm[i]))
		}
	}
	w.writeBit(1) // framing
	return w.bytes()
}

// setupHeader serializes the setup header (inverse of parseSetup).
func (c *encConfig) setupHeader() []byte {
	var w bitWriter
	writeString(&w, 0x05, "vorbis")

	// Codebooks.
	w.writeBits(8, uint32(len(c.specs)-1))
	for i := range c.specs {
		writeCodebook(&w, c.specs[i])
	}

	// Time-domain transforms: one placeholder, value zero.
	w.writeBits(6, 0)
	w.writeBits(16, 0)

	// Floors, long (index 0) then short (index 1), both type 1. The mappings and
	// the mode->mapping->submap chain reference these indices.
	w.writeBits(6, uint32(len(headerOrder)-1))
	for _, slot := range headerOrder {
		w.writeBits(16, 1) // floor type 1
		writeFloor1(&w, c.floors[slot], c.floorRangebits[slot])
	}

	// Residues, long then short.
	w.writeBits(6, uint32(len(headerOrder)-1))
	for _, slot := range headerOrder {
		writeResidue(&w, c.residues[slot])
	}

	// Mappings, long then short, both type 0.
	w.writeBits(6, uint32(len(headerOrder)-1))
	for _, slot := range headerOrder {
		w.writeBits(16, 0) // mapping type 0
		writeMapping(&w, c.mappings[slot], c.channels)
	}

	// Modes.
	w.writeBits(6, uint32(len(c.modes)-1))
	for _, m := range c.modes {
		w.writeBit(boolBit(m.blockflag))
		w.writeBits(16, 0) // window type
		w.writeBits(16, 0) // transform type
		w.writeBits(8, uint32(m.mapping))
	}

	w.writeBit(1) // framing
	return w.bytes()
}

// codecConfig packs the three headers into the Xiph-laced blob CodecConfig
// returns (PackHeaders is defined in codecconfig.go).
func (c *encConfig) codecConfig(vendor string, comments []string) []byte {
	return PackHeaders(c.idHeader(), commentHeader(vendor, comments), c.setupHeader())
}

// writeFloor1 serializes a floor-1 configuration (inverse of parseFloor1). The
// caller has already written the floor type.
func writeFloor1(w *bitWriter, f *floor1, rangebits int) {
	w.writeBits(5, uint32(len(f.partitionClass)))
	maxClass := -1
	for _, cls := range f.partitionClass {
		w.writeBits(4, uint32(cls))
		if cls > maxClass {
			maxClass = cls
		}
	}
	for cls := 0; cls <= maxClass; cls++ {
		w.writeBits(3, uint32(f.classDims[cls]-1))
		w.writeBits(2, uint32(f.classSubclasses[cls]))
		if f.classSubclasses[cls] > 0 {
			w.writeBits(8, uint32(f.classMasterbook[cls]))
		}
		for _, b := range f.classSubbooks[cls] {
			w.writeBits(8, uint32(b+1)) // -1 (unused) => 0
		}
	}
	w.writeBits(2, uint32(f.multiplier-1))
	w.writeBits(4, uint32(rangebits))
	// Post X values in xs order (xs[0]=0 and xs[1]=1<<rangebits are implicit).
	for i := 2; i < len(f.xs); i++ {
		w.writeBits(uint(rangebits), uint32(f.xs[i]))
	}
}

// writeResidue serializes a residue configuration (inverse of parseResidue,
// including the type field parseResidue reads first).
func writeResidue(w *bitWriter, r *residue) {
	w.writeBits(16, uint32(r.kind))
	w.writeBits(24, uint32(r.begin))
	w.writeBits(24, uint32(r.end))
	w.writeBits(24, uint32(r.partSize-1))
	w.writeBits(6, uint32(r.classes-1))
	w.writeBits(8, uint32(r.classbook))
	// Cascade bitmap per class, from which passes carry a book.
	cascade := make([]int, r.classes)
	for i := 0; i < r.classes; i++ {
		for j := 0; j < 8; j++ {
			if r.books[i][j] >= 0 {
				cascade[i] |= 1 << uint(j)
			}
		}
		low := cascade[i] & 7
		high := cascade[i] >> 3
		w.writeBits(3, uint32(low))
		if high > 0 {
			w.writeBit(1)
			w.writeBits(5, uint32(high))
		} else {
			w.writeBit(0)
		}
	}
	for i := 0; i < r.classes; i++ {
		for j := 0; j < 8; j++ {
			if cascade[i]&(1<<uint(j)) != 0 {
				w.writeBits(8, uint32(r.books[i][j]))
			}
		}
	}
}

// writeMapping serializes a mapping (inverse of parseMapping). The caller has
// already written the mapping type.
func writeMapping(w *bitWriter, m *mapping, channels int) {
	submaps := len(m.submaps)
	if submaps > 1 {
		w.writeBit(1)
		w.writeBits(4, uint32(submaps-1))
	} else {
		w.writeBit(0)
	}
	if len(m.couplingMag) > 0 {
		w.writeBit(1)
		w.writeBits(8, uint32(len(m.couplingMag)-1))
		magBits := ilog(channels - 1)
		for i := range m.couplingMag {
			w.writeBits(uint(magBits), uint32(m.couplingMag[i]))
			w.writeBits(uint(magBits), uint32(m.couplingAng[i]))
		}
	} else {
		w.writeBit(0)
	}
	w.writeBits(2, 0) // reserved
	if submaps > 1 {
		for ch := 0; ch < channels; ch++ {
			w.writeBits(4, uint32(m.mux[ch]))
		}
	}
	for _, sm := range m.submaps {
		w.writeBits(8, 0) // time-config placeholder
		w.writeBits(8, uint32(sm.floor))
		w.writeBits(8, uint32(sm.residue))
	}
}
