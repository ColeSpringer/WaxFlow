package aac

import "testing"

// sbrPayloadOpts parameterizes the test payload builder. envDelta and
// noiseDelta apply at band 1 of the dF runs (all other deltas are zero),
// which is enough to steer a run into or out of the valid range.
type sbrPayloadOpts struct {
	envStart   uint32 // 7-bit start value
	envDelta   int
	noiseStart uint32 // 5-bit start value
	noiseDelta int
	ext        []byte // bs_extended_data contents, nil for none
}

// writeSCESBRData appends one SCE sbr_data body (a flat FIXFIX frame with
// dF-coded values), optionally trailed by a bs_extended_data block.
func writeSCESBRData(w *sbrTestBits, tbl *sbrFreqTables, o sbrPayloadOpts) {
	w.put(0, 1) // bs_data_extra
	w.put(0, 2) // grid: fixfix
	w.put(0, 2) // numEnv = 1
	w.put(0, 1) // freqRes low
	w.put(0, 1) // dtdf env: dF
	w.put(0, 1) // dtdf noise: dF
	for i := 0; i < tbl.nQ; i++ {
		w.put(0, 2) // invf off
	}
	// Envelope: single-envelope FIXFIX forces 1.5 dB, plain tables, 7-bit start.
	w.put(o.envStart, 7)
	for i := 1; i < tbl.nLow; i++ {
		d := 0
		if i == 1 {
			d = o.envDelta
		}
		for _, bit := range huffPath(sbrHuffFEnv15[:], d) {
			w.put(uint32(bit), 1)
		}
	}
	// Noise: 5-bit start plus deltas from the 3 dB env table.
	w.put(o.noiseStart, 5)
	for i := 1; i < tbl.nQ; i++ {
		d := 0
		if i == 1 {
			d = o.noiseDelta
		}
		for _, bit := range huffPath(sbrHuffFEnv30[:], d) {
			w.put(uint32(bit), 1)
		}
	}
	w.put(0, 1) // bs_add_harmonic_flag
	if o.ext == nil {
		w.put(0, 1) // bs_extended_data absent
	} else {
		w.put(1, 1)
		w.put(uint32(len(o.ext)), 4)
		for _, b := range o.ext {
			w.put(uint32(b), 8)
		}
	}
}

// sceSBRPayload builds one SCE sbr_data payload, byte-aligned.
func sceSBRPayload(t *testing.T, tbl *sbrFreqTables, o sbrPayloadOpts) []byte {
	t.Helper()
	var w sbrTestBits
	writeSCESBRData(&w, tbl, o)
	w.w.align()
	return append([]byte(nil), w.w.buf...)
}

// sceSBRFill builds a whole fill-element extension payload: EXT_SBR_DATA,
// no header, then the sbr_data body. Feed to parseFill with its length.
func sceSBRFill(t *testing.T, tbl *sbrFreqTables, o sbrPayloadOpts) []byte {
	t.Helper()
	var w sbrTestBits
	w.put(extSBRData, 4)
	w.put(0, 1) // bs_header_flag absent
	writeSCESBRData(&w, tbl, o)
	w.w.align()
	return append([]byte(nil), w.w.buf...)
}

// TestESBRKeepOut is the executable form of the eSBR/PS keep-out rule: a
// bs_extended_data block is consumed whole by its declared length, its
// extension ids never routed to a parser. The test pins the two halves of
// that which are observable today: the parsed frame is bit-identical with
// and without an extension whose contents would derail any parser that
// read them, and the reader lands exactly past the declared length (so the
// skip is by length, not by content). If an edit starts interpreting these
// ids, the differential suites are what notice the output change; this
// test notices the parsing change.
func TestESBRKeepOut(t *testing.T) {
	h := sbrHeader{
		ampRes: 1, startFreq: 12, stopFreq: 9,
		freqScale: 2, alterScale: 1, noiseBands: 3,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	parse := func(payload []byte) (*sbrElement, int, bool) {
		el := newSBRElement(1, 48000)
		el.applyHeader(h)
		if !el.haveTbl {
			t.Fatal("no tables")
		}
		el.beginFrame()
		r := newBitReader(payload)
		ok := el.parseSBRData(r)
		return el, r.pos, ok
	}

	base := sbrPayloadOpts{envStart: 60, noiseStart: 30}
	plain := sceSBRPayload(t, tables48k(t), base)
	elPlain, _, okPlain := parse(plain)
	if !okPlain {
		t.Fatal("the plain payload must parse")
	}

	for _, tc := range []struct {
		name string
		ext  []byte
	}{
		// bs_extension_id rides the top two bits of the first byte if
		// anything reads it: 0b11 is an eSBR-family id, 0b10 is
		// EXTENSION_ID_PS. The trailing bits are huffman garbage that would
		// derail a parser that consumed them.
		{"esbr id", []byte{0xDE, 0xAD, 0xBE}},
		{"ps id", []byte{0x95, 0x55, 0x57}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExt := base
			withExt.ext = tc.ext
			payload := sceSBRPayload(t, tables48k(t), withExt)
			elExt, pos, okExt := parse(payload)
			if !okExt {
				t.Fatal("a payload with an extension must still parse: skip it by length")
			}
			if a, b := &elPlain.ch[0].frame, &elExt.ch[0].frame; *a != *b {
				t.Error("the extension changed the parsed frame; extended data must be skipped whole")
			}
			// The reader must land exactly past the declared block (modulo
			// the builder's byte-align padding): short means something
			// consumed by content, past means the length was misread.
			if pos > len(payload)*8 || pos <= len(payload)*8-8 {
				t.Errorf("reader at bit %d after parsing %d payload bits", pos, len(payload)*8)
			}
		})
	}
}

func tables48k(t *testing.T) *sbrFreqTables {
	t.Helper()
	h := sbrHeader{
		ampRes: 1, startFreq: 12, stopFreq: 9,
		freqScale: 2, alterScale: 1, noiseBands: 3,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	tbl, err := deriveFreqTables(h, 48000)
	if err != nil {
		t.Fatal(err)
	}
	return &tbl
}
