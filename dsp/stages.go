package dsp

import (
	"fmt"
	"io"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/mix"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

// scratchBox lazily owns one pooled scratch buffer, re-fetched when the
// requested shape changes (which in practice happens once per chain).
type scratchBox struct{ buf *audio.Buffer }

func (b *scratchBox) get(f audio.Format, frames int) *audio.Buffer {
	if b.buf == nil || b.buf.Fmt != f || b.buf.Cap() != frames {
		audio.Put(b.buf)
		b.buf = audio.Get(f, frames)
	}
	return b.buf
}

func (b *scratchBox) release() {
	audio.Put(b.buf)
	b.buf = nil
}

// pull1to1 services the frame-aligned stages (convert, mix, quantize,
// widen): read one upstream chunk sized exactly to dst and mirror its
// metadata, so Pos and Discont pass through untouched per ADR-0006.
func pull1to1(up Stage, box *scratchBox, dst *audio.Buffer) (*audio.Buffer, error) {
	dst.N = 0
	in := box.get(up.Format(), dst.Cap())
	if err := up.ReadChunk(in); err != nil {
		return nil, err
	}
	dst.N = in.N
	dst.Pos = in.Pos
	dst.Discont = in.Discont
	return in, nil
}

// convertStage crosses the int-to-float domain boundary: samples map to
// a [-1, 1] nominal range as v / 2^(depth-1). Depths through 24 bits
// convert exactly; deeper samples round to float32's 24-bit mantissa
// (package doc), where positive full scale can land on exactly 1.0; the
// quantizer's clamp absorbs that on the way back to int. No stage
// converts implicitly: chains insert this one explicitly (plan
// section 7).
type convertStage struct {
	up    Stage
	fmt   audio.Format
	scale float32
	box   scratchBox
}

func (s *convertStage) Format() audio.Format { return s.fmt }
func (s *convertStage) release()             { s.box.release() }

func (s *convertStage) ReadChunk(dst *audio.Buffer) error {
	in, err := pull1to1(s.up, &s.box, dst)
	if err != nil {
		return err
	}
	for c := 0; c < s.fmt.Channels; c++ {
		src := in.ChanI(c)
		out := dst.ChanF(c)
		for i := range src {
			out[i] = float32(src[i]) * s.scale
		}
	}
	return nil
}

// widenStage grows int samples to a wider depth by a left shift, the
// exact (and dither-free) path for pure bit-depth increases.
type widenStage struct {
	up    Stage
	fmt   audio.Format
	shift uint
	box   scratchBox
}

func (s *widenStage) Format() audio.Format { return s.fmt }
func (s *widenStage) release()             { s.box.release() }

func (s *widenStage) ReadChunk(dst *audio.Buffer) error {
	in, err := pull1to1(s.up, &s.box, dst)
	if err != nil {
		return err
	}
	for c := 0; c < s.fmt.Channels; c++ {
		src := in.ChanI(c)
		out := dst.ChanI(c)
		for i := range src {
			out[i] = src[i] << s.shift
		}
	}
	return nil
}

// mixStage applies the channel matrix. Frame-aligned and stateless.
type mixStage struct {
	up     Stage
	fmt    audio.Format
	matrix *mix.Matrix
	box    scratchBox
	srcV   [][]float32
	dstV   [][]float32
}

func (s *mixStage) Format() audio.Format { return s.fmt }
func (s *mixStage) release()             { s.box.release() }

func (s *mixStage) ReadChunk(dst *audio.Buffer) error {
	in, err := pull1to1(s.up, &s.box, dst)
	if err != nil {
		return err
	}
	if s.srcV == nil {
		s.srcV = make([][]float32, s.matrix.In())
		s.dstV = make([][]float32, s.matrix.Out())
	}
	for c := range s.srcV {
		s.srcV[c] = in.ChanF(c)
	}
	for c := range s.dstV {
		s.dstV[c] = dst.ChanF(c)
	}
	s.matrix.Apply(s.dstV, s.srcV, in.N)
	return nil
}

// gainStage scales in place on the caller's buffer: same format in and
// out, so it needs no scratch at all.
type gainStage struct {
	up  Stage
	fmt audio.Format
	g   float32
}

func (s *gainStage) Format() audio.Format { return s.fmt }

func (s *gainStage) ReadChunk(dst *audio.Buffer) error {
	if err := s.up.ReadChunk(dst); err != nil {
		return err
	}
	for c := 0; c < s.fmt.Channels; c++ {
		gain.Apply(dst.ChanF(c), s.g)
	}
	return nil
}

// quantizeStage crosses float-to-int with the dithering quantizer. The
// dither is keyed by the chunk's absolute Pos, not by a running stream, so
// a segment requantizes identically wherever playback lands and whatever
// preceded it (deterministic mode). A Discont resets the shaping history,
// which is the only state left.
type quantizeStage struct {
	up  Stage
	fmt audio.Format
	q   *dither.Quantizer
	box scratchBox
}

func (s *quantizeStage) Format() audio.Format { return s.fmt }
func (s *quantizeStage) release()             { s.box.release() }

func (s *quantizeStage) ReadChunk(dst *audio.Buffer) error {
	in, err := pull1to1(s.up, &s.box, dst)
	if err != nil {
		return err
	}
	if in.Discont {
		s.q.Reset()
	}
	for c := 0; c < s.fmt.Channels; c++ {
		s.q.Quantize(dst.ChanI(c), in.ChanF(c), c, in.Pos)
	}
	return nil
}

// kernelOps adapts a buffering float kernel (resampler, limiter) to the
// shared pump loop.
type kernelOps interface {
	process(dst, src [][]float32) (produced, consumed int)
	drain(dst [][]float32) int
	// anchor resets kernel state for a segment starting at input
	// position pos and returns the segment's first output position.
	anchor(pos int64) int64
}

type resampleOps struct{ k *resample.Resampler }

func (o resampleOps) process(dst, src [][]float32) (int, int) { return o.k.Process(dst, src) }
func (o resampleOps) drain(dst [][]float32) int               { return o.k.Drain(dst) }
func (o resampleOps) anchor(pos int64) int64 {
	outPos, phase := o.k.OffsetFor(pos)
	o.k.Reset(phase)
	return outPos
}

type limiterOps struct{ k *gain.Limiter }

func (o limiterOps) process(dst, src [][]float32) (int, int) { return o.k.Process(dst, src) }
func (o limiterOps) drain(dst [][]float32) int               { return o.k.Drain(dst) }
func (o limiterOps) anchor(pos int64) int64 {
	o.k.Reset()
	return pos
}
func (o limiterOps) Horizon() time.Duration { return o.k.Horizon() }

type compressorOps struct{ k *gain.Compressor }

func (o compressorOps) process(dst, src [][]float32) (int, int) { return o.k.Process(dst, src) }
func (o compressorOps) drain(dst [][]float32) int               { return o.k.Drain(dst) }
func (o compressorOps) anchor(pos int64) int64 {
	o.k.Reset()
	return pos
}
func (o compressorOps) Horizon() time.Duration { return o.k.Horizon() }

// pumpStage drives a buffering kernel whose output is not frame-aligned
// with its input (the resampler changes the count, the limiter delays).
// It owns the position bookkeeping the kernels are too low-level to see:
// anchoring on the first chunk and after every discontinuity, advancing
// Pos by frames produced, and never letting one output chunk span a
// splice. A Discont drains the kernel to its exact end-of-stream output
// before the reset, so the pre-splice segment is chunking-independent;
// what a seek discards is decided upstream, never by backlog here.
//
// The stage is float-domain only, by chain construction.
type pumpStage struct {
	up    Stage
	fmt   audio.Format
	inFmt audio.Format
	ops   kernelOps

	box  scratchBox
	off  int // frames of the scratch chunk already fed to the kernel
	srcV [][]float32
	dstV [][]float32

	outPos      int64 // output-timeline position of the next frame out
	anchorPos   int64 // pending anchor (deferred across a chunk flush)
	needAnchor  bool
	splice      bool // a Discont chunk is stashed; drain the kernel first
	markDiscont bool // stamp the next emitted chunk
	started     bool
	eof         bool
}

func newPump(up Stage, out audio.Format, ops kernelOps) *pumpStage {
	return &pumpStage{up: up, fmt: out, inFmt: up.Format(), ops: ops}
}

func (s *pumpStage) Format() audio.Format { return s.fmt }
func (s *pumpStage) release()             { s.box.release() }

// horizon reports the kernel's settle horizon, 0 for a kernel that
// declares none (an FIR window, which the primeSeconds floor covers).
func (s *pumpStage) horizon() time.Duration {
	if st, ok := s.ops.(Settler); ok {
		return st.Horizon()
	}
	return 0
}

func (s *pumpStage) ReadChunk(dst *audio.Buffer) error {
	if s.srcV == nil {
		s.srcV = make([][]float32, s.inFmt.Channels)
		s.dstV = make([][]float32, s.fmt.Channels)
	}
	dst.N = 0
	produced := 0
	var chunkPos int64
	discont := false

	mark := func(p int) {
		if p > 0 && produced == 0 {
			chunkPos = s.outPos
			discont = s.markDiscont
			s.markDiscont = false
		}
		produced += p
		s.outPos += int64(p)
	}

pump:
	for produced < dst.Cap() {
		if s.needAnchor {
			s.outPos = s.ops.anchor(s.anchorPos)
			s.markDiscont = true
			s.needAnchor = false
		}
		for c := range s.dstV {
			s.dstV[c] = dst.F[c*dst.Stride+produced : c*dst.Stride+dst.Cap()]
		}

		switch in := s.box.buf; {
		case s.splice:
			// A Discont chunk is stashed in the scratch. Finish the
			// pre-splice segment first: drain the kernel to its exact
			// end-of-stream output, so the segment's length and samples
			// never depend on how much backlog the window happened to
			// hold (which varies with upstream chunking).
			if p := s.ops.drain(s.dstV); p > 0 {
				mark(p)
				break
			}
			s.splice = false
			if produced > 0 {
				// Anchor on the next call: one chunk never spans a
				// discontinuity.
				s.needAnchor = true
				break pump
			}
			s.outPos = s.ops.anchor(s.anchorPos)
			s.markDiscont = true

		case in != nil && s.off < in.N:
			for c := range s.srcV {
				s.srcV[c] = in.F[c*in.Stride+s.off : c*in.Stride+in.N]
			}
			p, consumed := s.ops.process(s.dstV, s.srcV)
			mark(p)
			s.off += consumed

		case s.eof:
			p := s.ops.drain(s.dstV)
			if p == 0 {
				break pump
			}
			mark(p)

		default:
			in := s.box.get(s.inFmt, dst.Cap())
			err := s.up.ReadChunk(in)
			if err == io.EOF {
				s.eof = true
				continue
			}
			if err != nil {
				return err
			}
			s.off = 0
			switch {
			case !s.started:
				s.started = true
				s.outPos = s.ops.anchor(in.Pos)
				s.markDiscont = in.Discont
			case in.Discont:
				// The stashed chunk waits untouched while the splice
				// case above drains and re-anchors.
				s.anchorPos = in.Pos
				s.splice = true
			}
		}
	}
	if produced == 0 {
		return io.EOF
	}
	dst.N = produced
	dst.Pos = chunkPos
	dst.Discont = discont
	return nil
}

// framerStage re-chunks the stream to exactly size frames per chunk (the
// encoder-native frame length), short only at end of stream or right
// before a discontinuity: a chunk never spans a splice, and the chunk
// that starts one carries the Discont mark.
type framerStage struct {
	up   Stage
	fmt  audio.Format
	size int

	box         scratchBox
	off         int // frames of the scratch chunk already delivered
	pendDiscont bool
	eof         bool
}

func (s *framerStage) Format() audio.Format { return s.fmt }
func (s *framerStage) release()             { s.box.release() }

func (s *framerStage) ReadChunk(dst *audio.Buffer) error {
	if dst.Cap() < s.size {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("dsp: framer emits %d-frame chunks, buffer holds %d", s.size, dst.Cap()))
	}
	dst.N = 0
	for dst.N < s.size {
		in := s.box.buf
		if in == nil || s.off >= in.N {
			if s.eof {
				break
			}
			in = s.box.get(s.fmt, max(s.size, audio.StandardChunk))
			err := s.up.ReadChunk(in)
			if err == io.EOF {
				s.eof = true
				in.N = 0
				break
			}
			if err != nil {
				return err
			}
			s.off = 0
			if in.Discont {
				s.pendDiscont = true
				if dst.N > 0 {
					break // flush pre-splice frames; scratch waits intact
				}
			}
		}
		if dst.N == 0 {
			dst.Pos = in.Pos + int64(s.off)
			dst.Discont = s.pendDiscont
			s.pendDiscont = false
		}
		n := min(s.size-dst.N, in.N-s.off)
		audio.CopyFrames(dst, dst.N, in, s.off, n)
		dst.N += n
		s.off += n
	}
	if dst.N == 0 {
		return io.EOF
	}
	return nil
}
