package musepack

import "github.com/colespringer/waxflow/audio"

// Test-only seams exposed to the external musepack_test package.

// DecoderState is the decoder's cross-packet state as a State, for the tests
// that compare a scan against a decode.
func (d *Decoder) DecoderState() State {
	var s State
	d.st.store(&s, d.cfg.StreamVersion)
	return s
}

// ResCounts decodes packets through the real Decoder and histograms every
// quantiser resolution the frames used, per channel, which is how a coverage
// test proves a fixture set reaches a sample path rather than assuming it does.
func ResCounts(cfg Config, pkts [][]byte) (map[int32]int, error) {
	d, err := NewDecoder(cfg, cfg.Format())
	if err != nil {
		return nil, err
	}
	defer d.Release()
	counts := map[int32]int{}
	d.onFrame = func(f *frameState) {
		for ch := range cfg.Channels {
			for n := 0; n <= cfg.MaxBand; n++ {
				counts[f.res[ch][n]]++
			}
		}
	}
	drop := func(*audio.Buffer) error { return nil }
	for _, p := range pkts {
		if err := d.Decode(p, drop); err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// sv7Tables lists every SV7 book for the completeness test.
var sv7Tables = []struct {
	name    string
	entries []huffEntry
}{
	{"HuffHdr", tableHuffHdr}, {"HuffSCFI", tableHuffSCFI}, {"HuffDSCF", tableHuffDSCF},
	{"HuffQ1[0]", tableHuffQ1[0]}, {"HuffQ1[1]", tableHuffQ1[1]},
	{"HuffQ2[0]", tableHuffQ2[0]}, {"HuffQ2[1]", tableHuffQ2[1]},
	{"HuffQ3[0]", tableHuffQ3[0]}, {"HuffQ3[1]", tableHuffQ3[1]},
	{"HuffQ4[0]", tableHuffQ4[0]}, {"HuffQ4[1]", tableHuffQ4[1]},
	{"HuffQ5[0]", tableHuffQ5[0]}, {"HuffQ5[1]", tableHuffQ5[1]},
	{"HuffQ6[0]", tableHuffQ6[0]}, {"HuffQ6[1]", tableHuffQ6[1]},
	{"HuffQ7[0]", tableHuffQ7[0]}, {"HuffQ7[1]", tableHuffQ7[1]},
}

// sv8Tables lists every canonical book for the completeness test.
var sv8Tables = func() []canTable {
	out := []canTable{canSCFI[0], canSCFI[1], canDSCF[0], canDSCF[1], canBands, canRes[0], canRes[1], canQ1, canQ9up}
	for i := range canQ {
		out = append(out, canQ[i][0], canQ[i][1])
	}
	return out
}()

// Books lists every Huffman book with its expanded codewords, for the
// completeness test.
func Books() map[string][]Codeword {
	out := map[string][]Codeword{}
	for _, t := range sv7Tables {
		out["sv7/"+t.name] = codewords(expand(t.entries, nil))
	}
	for _, t := range sv8Tables {
		out["sv8/"+t.name] = codewords(expand(t.entries, t.sym))
	}
	return out
}

// Codeword is one expanded book entry.
type Codeword struct {
	Code   uint32
	Length int
	Symbol int
}

func codewords(ws []codeword) []Codeword {
	out := make([]Codeword, len(ws))
	for i, w := range ws {
		out[i] = Codeword{Code: w.code, Length: int(w.length), Symbol: int(w.sym)}
	}
	return out
}

// SV7RowCounts reports how many codewords each row of every SV7 table expands
// to: the SV7 books list one codeword per row, and the expansion must agree.
func SV7RowCounts() map[string][]int {
	out := map[string][]int{}
	for _, t := range sv7Tables {
		var counts []int
		prev := uint32(0x10000)
		for _, e := range t.entries {
			shift := 16 - uint(e.length)
			counts = append(counts, int((prev-1)>>shift-uint32(e.code)>>shift+1))
			prev = uint32(e.code)
		}
		out[t.name] = counts
	}
	return out
}

// Derived tables and constants under test.
var (
	IdxTables    = [5][]int8{idx30[:], idx31[:], idx32[:], idx50[:], idx51[:]}
	Idx8Tables   = [3][125]int8{idx8_50, idx8_51, idx8_52}
	HuffQ2Var    = huffQ2Var
	Cc           = cc
	Dc           = dc
	SCFTable     = scfTable
	SCFRatio     = scfRatio
	DiOpt        = diOpt
	DiOptInt     = diOptInt
	VTaps        = vTaps
	Binomial     = binomial
	Thres        = thres
	MaxVarintLen = maxVarintBytes
)

// PRNGNext draws from a generator given as its two words and returns the draw
// and the new words.
func PRNGNext(r1, r2 uint32) (v, n1, n2 uint32) {
	p := prng{r1, r2}
	v = p.next()
	return v, p.r1, p.r2
}

// Synthesize runs one channel's filterbank over frames of subband samples and
// returns the PCM, for the test against the direct synthesis sum.
func Synthesize(frames [][36][32]float32) []float32 {
	var s synthesis
	out := make([]float32, 0, len(frames)*FrameLength)
	for i := range frames {
		buf := make([]float32, FrameLength)
		s.frame(&frames[i], buf)
		out = append(out, buf...)
	}
	return out
}

// ComputeNewV is the 32-to-64 matrixing step alone.
func ComputeNewV(s *[32]float32) [64]float32 {
	var v [64]float32
	computeNewV(s, v[:])
	return v
}

// ReadTruncated reads one of v values in truncated binary from b.
func ReadTruncated(b []byte, v uint32) (value uint32, bitsUsed int) {
	var r bitReader
	r.reset(b, len(b)*8)
	value = r.truncated(v)
	return value, r.pos
}

// ReadEnum reads a k-of-n enumeration from b.
func ReadEnum(b []byte, k, n int) (set uint32, bitsUsed int) {
	var r bitReader
	r.reset(b, len(b)*8)
	set = r.enumDec(k, n)
	return set, r.pos
}

// ReadLog reads a 0..max value in the log code from b.
func ReadLog(b []byte, max int) (value int, bitsUsed int) {
	var r bitReader
	r.reset(b, len(b)*8)
	value = r.logDec(max)
	return value, r.pos
}

// ReadGolomb reads one Golomb code from b.
func ReadGolomb(b []byte, k int) (v int32, ok bool, bitsUsed int) {
	var r bitReader
	r.reset(b, len(b)*8)
	v, ok = r.golomb(k)
	return v, ok, r.pos
}
