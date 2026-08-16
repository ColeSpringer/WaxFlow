package aac

import "testing"

// psPutHuff emits value's codeword from a PS tree.
func psPutHuff(w *sbrTestBits, tree [][2]int16, value int) {
	for _, b := range psHuffPath(tree, value) {
		w.put(uint32(b), 1)
	}
}

// psPutHeader writes a ps_data header enabling IID (and optionally ICC)
// in the given mode.
func psPutHeader(w *sbrTestBits, iidMode int, icc bool, ext bool) {
	w.put(1, 1)               // header present
	w.put(1, 1)               // enable_iid
	w.put(uint32(iidMode), 3) // iid_mode
	if icc {
		w.put(1, 1)
		w.put(0, 3) // icc_mode 0
	} else {
		w.put(0, 1)
	}
	if ext {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
}

// TestPSDataParse pins the basic frame: one evenly-split envelope,
// frequency-coded IID deltas accumulating from zero, and start latched by
// the header.
func TestPSDataParse(t *testing.T) {
	var w sbrTestBits
	psPutHeader(&w, 0, false, false)
	w.put(0, 1) // frame_class 0
	w.put(1, 2) // one envelope
	w.put(0, 1) // dt = 0: frequency coded
	deltas := []int{0, 1, 1, -2, 0, 3, -3, 0, 1, -1}
	for _, d := range deltas {
		psPutHuff(&w, psTreeIIDDF, d)
	}
	var ps psData
	if _, ok := ps.parse(w.reader(), 1<<20); !ok {
		t.Fatal("parse failed")
	}
	if !ps.start || !ps.enableIID || ps.iidQuant != 0 || ps.nrIIDPar != 10 {
		t.Fatalf("header state = start %v iid %v quant %d nr %d", ps.start, ps.enableIID, ps.iidQuant, ps.nrIIDPar)
	}
	if ps.numEnv != 1 || ps.border[0] != -1 || ps.border[1] != 31 {
		t.Fatalf("envelopes = %d borders %v", ps.numEnv, ps.border)
	}
	want := [10]int8{0, 1, 2, 0, 0, 3, 0, 0, 1, 0}
	for b, v := range want {
		if ps.iid[0][b] != v {
			t.Errorf("iid[0][%d] = %d, want %d", b, ps.iid[0][b], v)
		}
	}
	if ps.is34 {
		t.Error("mode 0 must not select 34 bands")
	}
}

// TestPSDataDeltaTime pins the cross-frame anchor: a delta-time envelope
// reproduces the previous frame's last envelope when its deltas are zero.
func TestPSDataDeltaTime(t *testing.T) {
	var ps psData
	var w sbrTestBits
	psPutHeader(&w, 0, false, false)
	w.put(0, 1)
	w.put(1, 2)
	w.put(0, 1)
	for b := 0; b < 10; b++ {
		d := 1
		if b >= 4 {
			d = -1
		}
		psPutHuff(&w, psTreeIIDDF, d) // accumulates 1,2,3,4,3,2,1,0,-1,-2
	}
	if _, ok := ps.parse(w.reader(), 1<<20); !ok {
		t.Fatal("frame A failed")
	}
	frameA := ps.iid[0]

	var w2 sbrTestBits
	w2.put(0, 1) // no header: config persists
	w2.put(0, 1)
	w2.put(1, 2)
	w2.put(1, 1) // dt = 1: anchored on frame A's last envelope
	for b := 0; b < 10; b++ {
		psPutHuff(&w2, psTreeIIDDT, 0)
	}
	if _, ok := ps.parse(w2.reader(), 1<<20); !ok {
		t.Fatal("frame B failed")
	}
	if ps.iid[0] != frameA {
		t.Errorf("dt zeros = %v, want frame A's %v", ps.iid[0][:10], frameA[:10])
	}
	if !ps.start {
		t.Error("start must persist without a header")
	}
}

// TestPSDataEnvelopeFixup pins the virtual final envelope: a class-1
// frame whose last border stops short of the frame end grows one more
// envelope carrying the same parameters out to slot 31.
func TestPSDataEnvelopeFixup(t *testing.T) {
	var w sbrTestBits
	psPutHeader(&w, 0, false, false)
	w.put(1, 1)  // frame_class 1
	w.put(0, 2)  // one envelope
	w.put(15, 5) // border at 15: short of 31
	w.put(0, 1)
	for b := 0; b < 10; b++ {
		d := 1
		if b >= 4 {
			d = -1
		}
		psPutHuff(&w, psTreeIIDDF, d)
	}
	var ps psData
	if _, ok := ps.parse(w.reader(), 1<<20); !ok {
		t.Fatal("parse failed")
	}
	if ps.numEnv != 2 || ps.border[1] != 15 || ps.border[2] != 31 {
		t.Fatalf("numEnv %d borders %v, want the fixup envelope to 31", ps.numEnv, ps.border[:3])
	}
	if ps.iid[1] != ps.iid[0] {
		t.Error("the fixup envelope must copy the final parameters")
	}
}

// TestPSDataBorderViolation pins the damage path: non-monotone borders
// fail the frame, clear the parameters, and drop start so the element
// conceals as dual mono.
func TestPSDataBorderViolation(t *testing.T) {
	var ps psData
	var w sbrTestBits
	psPutHeader(&w, 0, false, false)
	w.put(0, 1)
	w.put(1, 2)
	w.put(0, 1)
	for b := 0; b < 10; b++ {
		d := 2
		if b >= 3 {
			d = 0
		}
		psPutHuff(&w, psTreeIIDDF, d) // 2,4,6,6,... stays in range
	}
	if _, ok := ps.parse(w.reader(), 1<<20); !ok {
		t.Fatal("setup frame failed")
	}

	var w2 sbrTestBits
	w2.put(0, 1)
	w2.put(1, 1)  // class 1
	w2.put(1, 2)  // two envelopes
	w2.put(20, 5) // border 20
	w2.put(10, 5) // then 10: non-monotone
	if _, ok := ps.parse(w2.reader(), 1<<20); ok {
		t.Fatal("non-monotone borders must fail")
	}
	if ps.start {
		t.Error("a damaged frame must drop start (dual-mono concealment)")
	}
	if ps.iid[0] != ([34]int8{}) {
		t.Error("a damaged frame must clear the delta anchors")
	}
}

// TestPSDataICCNegative pins the one-sided ICC range: a coherence run
// accumulating below zero is as illegal as one past 7 and must fail the
// frame, because the value indexes the mixing tables directly. (Found by
// the fuzzer as corpus entry 42cee124e2bc1034: a symmetric bounds check
// let -1 through.)
func TestPSDataICCNegative(t *testing.T) {
	var w sbrTestBits
	psPutHeader(&w, 0, true, false)
	w.put(0, 1)
	w.put(1, 2)
	w.put(0, 1) // iid df
	for b := 0; b < 10; b++ {
		psPutHuff(&w, psTreeIIDDF, 0)
	}
	w.put(0, 1) // icc df
	psPutHuff(&w, psTreeICCDF, -1)
	for b := 1; b < 10; b++ {
		psPutHuff(&w, psTreeICCDF, 0)
	}
	var ps psData
	if _, ok := ps.parse(w.reader(), 1<<20); ok {
		t.Fatal("a negative ICC accumulation must fail the frame")
	}
	if ps.start {
		t.Error("the damaged frame must drop start")
	}
}

// TestPSExtendedDataFillBound pins the extended-data clamp: the declared
// byte count comes from the stream, and one running past the fill
// element's end must fail the payload and stop reading at the fill
// boundary instead of handing the next element's bits to the PS parser.
func TestPSExtendedDataFillBound(t *testing.T) {
	el := newSBRElement(1, 48000, true, false)
	var w sbrTestBits
	w.put(1, 1)  // bs_extended_data present
	w.put(4, 4)  // declares 4 bytes
	w.put(2, 2)  // extension id: PS
	w.put(0, 16) // whatever follows: the fill ends inside it
	r := w.reader()
	const fillEnd = 16
	if el.parseSBRExtendedData(r, fillEnd) {
		t.Fatal("a declared size past the fill end must fail the payload")
	}
	if r.pos != fillEnd {
		t.Errorf("reader at bit %d, want clamped to the fill end %d", r.pos, fillEnd)
	}
	if el.ps.data.start {
		t.Error("PS must not latch from a block that lied about its size")
	}
	if !el.sawPS {
		t.Error("the id itself was seen and detection may record it")
	}
}

// TestPSData34Bands pins the band-mode flags across a mode change.
func TestPSData34Bands(t *testing.T) {
	var ps psData
	var w sbrTestBits
	psPutHeader(&w, 2, false, false) // iid_mode 2: 34 bands
	w.put(0, 1)
	w.put(1, 2)
	w.put(0, 1)
	for b := 0; b < 34; b++ {
		psPutHuff(&w, psTreeIIDDF, 0)
	}
	if _, ok := ps.parse(w.reader(), 1<<20); !ok {
		t.Fatal("parse failed")
	}
	if !ps.is34 || ps.is34Old {
		t.Errorf("is34/is34Old = %v/%v, want true/false on the switching frame", ps.is34, ps.is34Old)
	}
	if ps.iidQuant != 0 || ps.nrIIDPar != 34 {
		t.Errorf("quant %d nrPar %d", ps.iidQuant, ps.nrIIDPar)
	}
}

// TestPSDataIPDOPD pins the extension block: phase parameters parse under
// their byte count and enableIPDOPD latches.
func TestPSDataIPDOPD(t *testing.T) {
	var w sbrTestBits
	psPutHeader(&w, 0, false, true) // ext enabled
	w.put(0, 1)
	w.put(1, 2)
	w.put(0, 1)
	for b := 0; b < 10; b++ {
		psPutHuff(&w, psTreeIIDDF, 0)
	}
	// Extension: 6 bytes; id 0, enable, per-envelope dt+5 ipd and dt+5 opd
	// (delta 3 everywhere), reserved bit, then padding to the byte count.
	w.put(6, 4)
	w.put(0, 2) // ps_extension_id 0
	w.put(1, 1) // enable_ipdopd
	w.put(0, 1)
	for b := 0; b < 5; b++ {
		psPutHuff(&w, psTreeIPDDF, 3)
	}
	w.put(0, 1)
	for b := 0; b < 5; b++ {
		psPutHuff(&w, psTreeOPDDF, 3)
	}
	w.put(0, 1) // reserved
	w.put(0, 2) // padding to the declared byte count
	var ps psData
	if _, ok := ps.parse(w.reader(), 1<<20); !ok {
		t.Fatal("parse failed")
	}
	if !ps.enableIPDOPD || ps.nrIPDOPDPar != 5 {
		t.Fatalf("ipdopd = %v nr %d", ps.enableIPDOPD, ps.nrIPDOPDPar)
	}
	want := [5]int8{3, 6, 1, 4, 7} // mod-8 accumulation of +3 steps
	for b, v := range want {
		if ps.ipd[0][b] != v || ps.opd[0][b] != v {
			t.Errorf("phase[%d] = %d/%d, want %d", b, ps.ipd[0][b], ps.opd[0][b], v)
		}
	}
}
