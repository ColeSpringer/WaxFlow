package aac

import "testing"

// sbrTestBits builds test payloads on the package's bitWriter.
type sbrTestBits struct {
	w bitWriter
}

func (b *sbrTestBits) put(v uint32, n uint) { b.w.writeBits(n, uint64(v)) }

func (b *sbrTestBits) reader() *bitReader {
	b.w.align()
	return newBitReader(append([]byte(nil), b.w.buf...))
}

// huffPath returns the bit path that decodes to value in a pair table, or
// nil. It lets tests emit real codewords without hardcoding any.
func huffPath(tab [][2]int8, value int) []byte {
	var walk func(idx int, path []byte) []byte
	walk = func(idx int, path []byte) []byte {
		for bit := 0; bit < 2; bit++ {
			v := tab[idx][bit]
			p := append(append([]byte(nil), path...), byte(bit))
			if v < 0 {
				if int(v)+64 == value {
					return p
				}
				continue
			}
			if got := walk(int(v), p); got != nil {
				return got
			}
		}
		return nil
	}
	return walk(0, nil)
}

// TestParseGridClasses pins the border and sinusoid-ramp derivation for
// each frame class, including the pointer edge that bit: FIXVAR/VARVAR
// with pointer 1 places lA at numEnv (a ramp ending at the frame edge),
// not "absent".
func TestParseGridClasses(t *testing.T) {
	// FIXFIX, 2 envelopes: tE 0/8/16, two noise floors split at tE[1].
	var w sbrTestBits
	w.put(0, 2) // fixfix
	w.put(1, 2) // numEnv = 2
	w.put(1, 1) // freqRes high
	var g sbrGrid
	if !parseGrid(w.reader(), &g) {
		t.Fatal("fixfix parse failed")
	}
	if g.numEnv != 2 || g.tE[0] != 0 || g.tE[1] != 8 || g.tE[2] != 16 || g.lA != -1 {
		t.Errorf("fixfix: %+v", g)
	}
	if g.numNoise != 2 || g.tQ[1] != 8 {
		t.Errorf("fixfix noise grid: %+v", g)
	}

	// FIXVAR, 2 envelopes ending at 16, one relative border of width 4,
	// pointer 1: lA = numEnv.
	w = sbrTestBits{}
	w.put(1, 2) // fixvar
	w.put(0, 2) // absBord = 16
	w.put(1, 2) // numRel = 1 -> numEnv 2
	w.put(1, 2) // width (2*1+2) = 4 -> tE[1] = 12
	w.put(1, 2) // pointer = 1 (ceil(log2(3)) = 2 bits)
	w.put(1, 1) // freqRes env1 (reversed order)
	w.put(0, 1) // freqRes env0
	g = sbrGrid{}
	if !parseGrid(w.reader(), &g) {
		t.Fatal("fixvar parse failed")
	}
	if g.numEnv != 2 || g.tE[1] != 12 || g.tE[2] != 16 {
		t.Errorf("fixvar borders: %+v", g)
	}
	if g.lA != 2 {
		t.Errorf("fixvar pointer 1: lA = %d, want numEnv (2)", g.lA)
	}
	if g.freqRes[0] != 0 || g.freqRes[1] != 1 {
		t.Errorf("fixvar freqRes order: %v", g.freqRes[:2])
	}

	// VARFIX starting at 2 with one relative border: pointer 1 leaves lA
	// absent (VARFIX places lA only for pointer > 1).
	w = sbrTestBits{}
	w.put(2, 2) // varfix
	w.put(2, 2) // tE[0] = 2
	w.put(1, 2) // numRel = 1
	w.put(2, 2) // width = 6 -> tE[1] = 8
	w.put(1, 2) // pointer = 1
	w.put(1, 1) // freqRes env0
	w.put(0, 1) // freqRes env1
	g = sbrGrid{}
	if !parseGrid(w.reader(), &g) {
		t.Fatal("varfix parse failed")
	}
	if g.numEnv != 2 || g.tE[0] != 2 || g.tE[1] != 8 || g.tE[2] != 16 || g.lA != -1 {
		t.Errorf("varfix: %+v", g)
	}
}

// TestParseEnvelopeBalancePreDoubled pins the balance-channel delta
// convention this decoder once got wrong: the balance huffman tables carry
// deltas already in doubled units, so the parser must add them raw. Only
// the start value scales by two. (The plain channel's tables step in
// single units and take no scaling either.)
func TestParseEnvelopeBalancePreDoubled(t *testing.T) {
	h := sbrHeader{
		ampRes: 1, startFreq: 12, stopFreq: 9,
		freqScale: 2, alterScale: 1, noiseBands: 3,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	tbl, err := deriveFreqTables(h, 48000)
	if err != nil {
		t.Fatal(err)
	}

	// The balance freq tables must carry even values only; the plain tables
	// must include odd steps. This is the structural fact the parse relies on.
	for _, v := range []int{2, -2} {
		if huffPath(sbrHuffFEnvBal30[:], v) == nil {
			t.Fatalf("balance table has no %+d delta codeword", v)
		}
	}
	if huffPath(sbrHuffFEnvBal30[:], 1) != nil {
		t.Fatal("balance table decodes an odd delta; it must be pre-doubled")
	}
	if huffPath(sbrHuffFEnv30[:], 1) == nil {
		t.Fatal("plain 3dB table has no +1 delta")
	}

	// One dF-coded balance envelope at low resolution: start 12 (doubled to
	// 24), then deltas +2 and -2 taken verbatim from the table.
	var w sbrTestBits
	w.put(12, 5) // bs_env_start_value_balance (ampRes 1: 5 bits)
	for _, d := range []int{2, -2, 0, 2} {
		for _, bit := range huffPath(sbrHuffFEnvBal30[:], d) {
			w.put(uint32(bit), 1)
		}
	}
	fd := sbrFrameData{ampRes: 1}
	fd.grid = sbrGrid{numEnv: 1, freqRes: [5]int{0}, numNoise: 1}
	fd.grid.tE[0], fd.grid.tE[1] = 0, 16
	var prevEnv [64]int32
	prevRes := 0
	parseEnvelope(w.reader(), &tbl, &fd, &prevEnv, &prevRes, true)
	want := []int32{24, 26, 24, 24, 26}
	for b, v := range want {
		if fd.env[0][b] != v {
			t.Errorf("balance env[%d] = %d, want %d (row %v)", b, fd.env[0][b], v, fd.env[0][:5])
		}
	}
}

// TestMiddleBorder pins the noise-grid split rules per class, matching the
// reference decoder's table.
func TestMiddleBorder(t *testing.T) {
	cases := []struct {
		class, pointer, numEnv, want int
	}{
		{fixfix, 0, 2, 1},
		{fixfix, 0, 4, 2},
		{fixvar, 0, 2, 1}, // numEnv - max(p-1, 1)
		{fixvar, 1, 3, 2}, // numEnv - 1
		{fixvar, 3, 4, 2}, // numEnv - (p-1)
		{varfix, 0, 2, 1}, // 1
		{varfix, 1, 3, 2}, // numEnv - 1
		{varfix, 3, 4, 2}, // p - 1
		{varvar, 2, 3, 2}, // numEnv - (p-1)
	}
	for _, tc := range cases {
		if got := middleBorder(tc.class, tc.pointer, tc.numEnv); got != tc.want {
			t.Errorf("middleBorder(class %d, p %d, n %d) = %d, want %d",
				tc.class, tc.pointer, tc.numEnv, got, tc.want)
		}
	}
}
