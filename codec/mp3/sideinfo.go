package mp3

// Side information parsing (ISO 11172-3 section 2.4.1.7 and 13818-3
// section 2.4.1.7): the per-granule, per-channel parameters between the
// frame header and the main data.

// Block types (block_type field). blockShort granules switch the hybrid
// filterbank to three short windows; blockStart and blockStop are the
// transition windows.
const (
	blockNormal = 0
	blockStart  = 1
	blockShort  = 2
	blockStop   = 3
)

// grInfo is one granule-channel's side information.
type grInfo struct {
	part23Len    int // scalefactor plus Huffman bits
	bigValues    int
	globalGain   int
	scfCompress  int
	blockType    int
	mixed        bool
	tableSelect  [3]int
	subblockGain [3]int
	regionCount  [2]int
	preflag      bool
	scfScale     int
	count1Table  bool
}

// sideInfo is a frame's parsed side information.
type sideInfo struct {
	mainDataBegin int
	scfsi         [2][4]bool // MPEG-1 only: share scalefactor groups with granule 0
	gr            [2][2]grInfo
}

// parseSideInfo reads the side information from b (exactly
// h.SideInfoLen() bytes). Structural violations (reserved block type,
// big_values past the spectrum) are damage: the frame decodes to
// silence, so the error here is just a marker, not user-facing.
func parseSideInfo(h Header, b []byte, si *sideInfo) bool {
	r := bitReader{data: b}
	granules := 2
	if h.Version == MPEG1 {
		si.mainDataBegin = int(r.bits(9))
		if h.Channels == 1 {
			r.bits(5) // private bits
		} else {
			r.bits(3)
		}
		for ch := 0; ch < h.Channels; ch++ {
			for g := 0; g < 4; g++ {
				si.scfsi[ch][g] = r.bit() == 1
			}
		}
	} else {
		granules = 1
		si.mainDataBegin = int(r.bits(8))
		r.bits(uint(h.Channels)) // private bits
	}

	for gri := 0; gri < granules; gri++ {
		for ch := 0; ch < h.Channels; ch++ {
			g := &si.gr[gri][ch]
			*g = grInfo{}
			g.part23Len = int(r.bits(12))
			g.bigValues = int(r.bits(9))
			if g.bigValues > 288 {
				return false
			}
			g.globalGain = int(r.bits(8))
			if h.Version == MPEG1 {
				g.scfCompress = int(r.bits(4))
			} else {
				g.scfCompress = int(r.bits(9))
			}
			if r.bit() == 1 { // window switching
				g.blockType = int(r.bits(2))
				if g.blockType == blockNormal {
					return false // reserved combination
				}
				g.mixed = r.bit() == 1
				g.tableSelect[0] = int(r.bits(5))
				g.tableSelect[1] = int(r.bits(5))
				// No region 2, and region boundaries are implicit: 8 width
				// entries for pure short blocks, 7 otherwise (the ISO
				// window-switching defaults, counted on the granule's own
				// band table).
				g.subblockGain[0] = int(r.bits(3))
				g.subblockGain[1] = int(r.bits(3))
				g.subblockGain[2] = int(r.bits(3))
				g.regionCount[0] = 7
				if g.blockType == blockShort && !g.mixed {
					g.regionCount[0] = 8
				}
				g.regionCount[1] = 36 // rest: past any table's entry count
			} else {
				g.blockType = blockNormal
				g.tableSelect[0] = int(r.bits(5))
				g.tableSelect[1] = int(r.bits(5))
				g.tableSelect[2] = int(r.bits(5))
				g.regionCount[0] = int(r.bits(4))
				g.regionCount[1] = int(r.bits(3))
			}
			if h.Version == MPEG1 {
				g.preflag = r.bit() == 1
			} else {
				g.preflag = g.scfCompress >= 500
			}
			g.scfScale = int(r.bit())
			g.count1Table = r.bit() == 1
		}
	}
	return !r.err
}
