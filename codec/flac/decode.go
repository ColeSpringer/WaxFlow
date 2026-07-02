package flac

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ codec.Decoder  = (*Decoder)(nil)
	_ codec.Releaser = (*Decoder)(nil)
)

// Decoder decodes FLAC frames into planar buffers. It implements
// codec.Decoder: one packet is exactly one frame, checksum included, and
// every frame is independently decodable, so Drain and Reset are no-ops.
//
// Subframes are reconstructed in int64 scratch: side channels carry one
// bit more than the stream depth (33 significant bits at 32-bit depth),
// and LPC accumulators need up to depth+precision+log2(order) bits. The
// final samples always fit int32.
type Decoder struct {
	si  StreamInfo
	fmt audio.Format
	buf *audio.Buffer // reusable output, borrowed by emit callbacks
	res []int64       // per-channel scratch, blockSize stride
}

// NewDecoder returns a Decoder for a stream. The track format must be
// what si.PCMFormat produces; containers build both from the same
// STREAMINFO, so a mismatch is a wiring bug.
func NewDecoder(si StreamInfo, f audio.Format) (*Decoder, error) {
	if err := f.Valid(); err != nil {
		return nil, err
	}
	if want := si.PCMFormat(); f != want {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("flac: track format %v does not match STREAMINFO (want %v)", f, want))
	}
	return &Decoder{si: si, fmt: f}, nil
}

// Decode decodes one frame and emits one buffer of BlockSize frames. The
// buffer is borrowed: valid only during the callback.
func (d *Decoder) Decode(pkt []byte, emit func(*audio.Buffer) error) error {
	fi, err := ParseFrameHeader(pkt)
	if err != nil {
		return err
	}
	rate, bits := fi.Rate, fi.Bits
	if rate == 0 {
		rate = d.si.Rate
	}
	if bits == 0 {
		bits = d.si.Bits
	}
	// The pipeline's track format is fixed; a frame that disagrees with
	// STREAMINFO would need a mid-stream format change, which RFC 9639
	// permits decoders to reject.
	if rate != d.si.Rate || bits != d.si.Bits || fi.Channels != d.si.Channels {
		return malformed("frame format %dHz %dch %dbit disagrees with STREAMINFO %dHz %dch %dbit",
			rate, fi.Channels, bits, d.si.Rate, d.si.Channels, d.si.Bits)
	}
	if len(pkt) < fi.hdrLen+2 {
		return malformed("frame truncated")
	}
	if got, want := CRC16(pkt[:len(pkt)-2]), uint16(pkt[len(pkt)-2])<<8|uint16(pkt[len(pkt)-1]); got != want {
		return malformed("frame CRC-16 mismatch")
	}

	n := fi.BlockSize
	if cap(d.res) < fi.Channels*n {
		d.res = make([]int64, fi.Channels*n)
	}
	d.res = d.res[:fi.Channels*n]

	r := &bitReader{data: pkt[:len(pkt)-2], pos: fi.hdrLen}
	for c := 0; c < fi.Channels; c++ {
		bps := uint(bits)
		switch fi.assign {
		case assignLeftSide, assignMidSide:
			if c == 1 {
				bps++
			}
		case assignRightSide:
			if c == 0 {
				bps++
			}
		}
		if err := decodeSubframe(r, d.res[c*n:(c+1)*n], bps); err != nil {
			return err
		}
	}
	r.align()
	if r.err {
		return malformed("frame overruns its data")
	}

	if d.buf == nil || d.buf.Cap() < n || d.buf.Fmt != d.fmt {
		audio.Put(d.buf)
		d.buf = audio.Get(d.fmt, max(n, audio.StandardChunk))
	}
	d.buf.N = n
	d.decorrelate(fi, n)
	return emit(d.buf)
}

// decorrelate undoes inter-channel decorrelation from scratch into the
// output buffer (RFC 9639 section 4.2).
func (d *Decoder) decorrelate(fi FrameInfo, n int) {
	switch fi.assign {
	case assignLeftSide:
		left, side := d.res[:n], d.res[n:2*n]
		l, r := d.buf.ChanI(0), d.buf.ChanI(1)
		for i := 0; i < n; i++ {
			l[i] = int32(left[i])
			r[i] = int32(left[i] - side[i])
		}
	case assignRightSide:
		side, right := d.res[:n], d.res[n:2*n]
		l, r := d.buf.ChanI(0), d.buf.ChanI(1)
		for i := 0; i < n; i++ {
			l[i] = int32(right[i] + side[i])
			r[i] = int32(right[i])
		}
	case assignMidSide:
		mid, side := d.res[:n], d.res[n:2*n]
		l, r := d.buf.ChanI(0), d.buf.ChanI(1)
		for i := 0; i < n; i++ {
			m := mid[i]<<1 | side[i]&1
			l[i] = int32((m + side[i]) >> 1)
			r[i] = int32((m - side[i]) >> 1)
		}
	default:
		for c := 0; c < fi.Channels; c++ {
			src := d.res[c*n : (c+1)*n]
			dst := d.buf.ChanI(c)
			for i := 0; i < n; i++ {
				dst[i] = int32(src[i])
			}
		}
	}
}

// Drain is a no-op: FLAC has no decoder latency.
func (d *Decoder) Drain(func(*audio.Buffer) error) error { return nil }

// Reset is a no-op: every frame decodes independently.
func (d *Decoder) Reset() {}

// Release returns the output buffer to the pool (codec.Releaser). The
// decoder must not be used afterward.
func (d *Decoder) Release() {
	audio.Put(d.buf)
	d.buf = nil
}

// Subframe types (RFC 9639 section 9.2).
const (
	subConstant = 0
	subVerbatim = 1
)

// decodeSubframe reconstructs one channel into out.
func decodeSubframe(r *bitReader, out []int64, bps uint) error {
	if r.u(1) != 0 {
		return malformed("subframe padding bit set")
	}
	typ := int(r.u(6))
	wasted := uint(0)
	if r.u(1) != 0 {
		wasted = uint(r.unary()) + 1
		if wasted >= bps {
			return malformed("%d wasted bits leave no sample bits", wasted)
		}
		bps -= wasted
	}

	switch {
	case typ == subConstant:
		v := r.s(bps)
		for i := range out {
			out[i] = v
		}
	case typ == subVerbatim:
		for i := range out {
			out[i] = r.s(bps)
		}
	case typ >= 8 && typ <= 12:
		if err := decodeFixed(r, out, typ-8, bps); err != nil {
			return err
		}
	case typ >= 32:
		if err := decodeLPC(r, out, typ-31, bps); err != nil {
			return err
		}
	default:
		return malformed("reserved subframe type %d", typ)
	}
	if r.err {
		return malformed("subframe overruns its data")
	}

	if wasted > 0 {
		for i := range out {
			out[i] <<= wasted
		}
	}
	return nil
}

// decodeFixed decodes a fixed-predictor subframe of the given order.
func decodeFixed(r *bitReader, out []int64, order int, bps uint) error {
	if order > len(out) {
		return malformed("fixed order %d exceeds block size %d", order, len(out))
	}
	for i := 0; i < order; i++ {
		out[i] = r.s(bps)
	}
	if err := decodeResidual(r, out, order); err != nil {
		return err
	}
	switch order {
	case 1:
		for i := 1; i < len(out); i++ {
			out[i] += out[i-1]
		}
	case 2:
		for i := 2; i < len(out); i++ {
			out[i] += 2*out[i-1] - out[i-2]
		}
	case 3:
		for i := 3; i < len(out); i++ {
			out[i] += 3*out[i-1] - 3*out[i-2] + out[i-3]
		}
	case 4:
		for i := 4; i < len(out); i++ {
			out[i] += 4*out[i-1] - 6*out[i-2] + 4*out[i-3] - out[i-4]
		}
	}
	return nil
}

// decodeLPC decodes a linear-predictor subframe of the given order.
func decodeLPC(r *bitReader, out []int64, order int, bps uint) error {
	if order > len(out) {
		return malformed("LPC order %d exceeds block size %d", order, len(out))
	}
	for i := 0; i < order; i++ {
		out[i] = r.s(bps)
	}
	precision := uint(r.u(4)) + 1
	if precision == 16 {
		return malformed("invalid LPC precision code")
	}
	shift := r.s(5)
	if shift < 0 {
		return malformed("negative LPC shift")
	}
	var coef [32]int64
	for i := 0; i < order; i++ {
		coef[i] = r.s(precision)
	}
	if r.err {
		return malformed("subframe overruns its data")
	}
	if err := decodeResidual(r, out, order); err != nil {
		return err
	}
	c := coef[:order]
	for i := order; i < len(out); i++ {
		var sum int64
		for j, cf := range c {
			sum += cf * out[i-1-j]
		}
		out[i] += sum >> uint(shift)
	}
	return nil
}

// decodeResidual fills out[order:] with residuals from a coded residual
// section (RFC 9639 section 9.2.7).
func decodeResidual(r *bitReader, out []int64, order int) error {
	method := r.u(2)
	if method > 1 {
		return malformed("reserved residual coding method %d", method)
	}
	paramBits := uint(4 + method)
	escape := uint64(1)<<paramBits - 1

	partOrder := uint(r.u(4))
	parts := 1 << partOrder
	n := len(out)
	if n%parts != 0 {
		return malformed("block size %d not divisible into %d partitions", n, parts)
	}
	count := n >> partOrder
	if count < order {
		return malformed("first partition shorter than predictor order")
	}

	pos := order
	for p := 0; p < parts; p++ {
		want := count
		if p == 0 {
			want -= order
		}
		param := r.u(paramBits)
		if param == escape {
			raw := uint(r.u(5))
			if raw == 0 {
				for i := 0; i < want; i++ {
					out[pos+i] = 0
				}
			} else {
				for i := 0; i < want; i++ {
					out[pos+i] = r.s(raw)
				}
			}
		} else {
			k := uint(param)
			for i := 0; i < want; i++ {
				q := uint64(r.unary())
				u := q<<k | r.u(k)
				out[pos+i] = int64(u>>1) ^ -int64(u&1)
			}
		}
		if r.err {
			return malformed("residual overruns its data")
		}
		pos += want
	}
	return nil
}
