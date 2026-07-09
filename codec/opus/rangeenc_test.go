package opus

import "testing"

// op is one scripted entropy-coder operation. The same script drives the
// encoder and, replayed, the decoder, so a full range of symbol shapes gets
// round-tripped through both directions.
type op struct {
	kind         int
	fl, fh, ft   uint32
	bits         uint
	val          int
	icdf         []byte
	ftb          uint
	fs           uint32
	decay        int
	laplaceCoded int // value laplaceEncode actually coded (filled during encode)
}

const (
	opEncode = iota
	opBin
	opBitLogp
	opICDF
	opUint
	opRaw
	opLaplace
)

// buildScript generates a deterministic mix of coder operations.
func buildScript(n int) []op {
	var seed uint32 = 0x1234567
	rnd := func() uint32 { seed = seed*1664525 + 1013904223; return seed }
	icdfs := [][]byte{
		{2, 1, 0}, {126, 124, 119, 109, 87, 41, 19, 9, 4, 2, 0},
		{25, 23, 2, 0}, {2, 0},
	}
	ops := make([]op, 0, n)
	for i := 0; i < n; i++ {
		switch rnd() % 7 {
		case 0:
			ft := 2 + rnd()%4000
			fl := rnd() % ft
			fh := fl + 1 + rnd()%(ft-fl)
			ops = append(ops, op{kind: opEncode, fl: fl, fh: fh, ft: ft})
		case 1:
			bits := uint(1 + rnd()%14)
			ftv := uint32(1) << bits
			fl := rnd() % ftv
			fh := fl + 1 + rnd()%(ftv-fl)
			ops = append(ops, op{kind: opBin, fl: fl, fh: fh, bits: bits})
		case 2:
			ops = append(ops, op{kind: opBitLogp, val: int(rnd() & 1), bits: uint(1 + rnd()%15)})
		case 3:
			t := icdfs[rnd()%uint32(len(icdfs))]
			ops = append(ops, op{kind: opICDF, icdf: t, val: int(rnd() % uint32(len(t)-1)), ftb: 8})
		case 4:
			ft := 1 + rnd()%1_000_000
			ops = append(ops, op{kind: opUint, val: int(rnd() % ft), ft: ft + 1})
		case 5:
			bits := uint(1 + rnd()%24)
			ops = append(ops, op{kind: opRaw, val: int(rnd() & ((1 << bits) - 1)), bits: bits})
		case 6:
			// Laplace parameters resembling the coarse-energy model.
			fs := uint32(1+rnd()%250) << 7
			decay := int(rnd()%200) << 6
			v := int(rnd()%40) - 20
			ops = append(ops, op{kind: opLaplace, fs: fs, decay: decay, val: v})
		}
	}
	return ops
}

func TestRangeEncoderRoundTrip(t *testing.T) {
	ops := buildScript(5000)
	buf := make([]byte, 64*1024)
	enc := newRangeEncoder(buf)
	for i := range ops {
		o := &ops[i]
		switch o.kind {
		case opEncode:
			enc.encode(o.fl, o.fh, o.ft)
		case opBin:
			enc.encodeBin(o.fl, o.fh, o.bits)
		case opBitLogp:
			enc.encodeBitLogp(o.val, o.bits)
		case opICDF:
			enc.encodeICDF(o.val, o.icdf, o.ftb)
		case opUint:
			enc.encodeUint(uint32(o.val), o.ft)
		case opRaw:
			enc.encodeRawBits(uint32(o.val), o.bits)
		case opLaplace:
			o.laplaceCoded = enc.laplaceEncode(o.val, o.fs, o.decay)
		}
	}
	tellBits := enc.tell()
	enc.done()
	if enc.err {
		t.Fatal("encoder reported overflow on a 64 KiB buffer")
	}

	dec := newRangeDecoder(enc.payload())
	for i := range ops {
		o := &ops[i]
		switch o.kind {
		case opEncode:
			fm := dec.decode(o.ft)
			if fm < o.fl || fm >= o.fh {
				t.Fatalf("op %d encode: decoded fm=%d not in [%d,%d) ft=%d", i, fm, o.fl, o.fh, o.ft)
			}
			dec.update(o.fl, o.fh, o.ft)
		case opBin:
			fm := dec.decodeBin(o.bits)
			if fm < o.fl || fm >= o.fh {
				t.Fatalf("op %d bin: decoded fm=%d not in [%d,%d)", i, fm, o.fl, o.fh)
			}
			dec.update(o.fl, o.fh, uint32(1)<<o.bits)
		case opBitLogp:
			if got := dec.decodeBitLogp(o.bits); got != o.val {
				t.Fatalf("op %d bitLogp: got %d want %d", i, got, o.val)
			}
		case opICDF:
			if got := dec.decodeICDF(o.icdf, o.ftb); got != o.val {
				t.Fatalf("op %d icdf: got %d want %d", i, got, o.val)
			}
		case opUint:
			if got := dec.decodeUint(o.ft); got != uint32(o.val) {
				t.Fatalf("op %d uint: got %d want %d (ft=%d)", i, got, o.val, o.ft)
			}
		case opRaw:
			if got := dec.decodeRawBits(o.bits); got != uint32(o.val) {
				t.Fatalf("op %d raw: got %d want %d (bits=%d)", i, got, o.val, o.bits)
			}
		case opLaplace:
			if got := dec.laplaceDecode(o.fs, o.decay); got != o.laplaceCoded {
				t.Fatalf("op %d laplace: got %d want %d (fs=%d decay=%d in=%d)", i, got, o.laplaceCoded, o.fs, o.decay, o.val)
			}
		}
	}

	// tell() must agree between encoder and decoder at the end.
	if dt := dec.tell(); dt != tellBits {
		t.Errorf("tell mismatch: encoder %d, decoder %d", tellBits, dt)
	}
}
