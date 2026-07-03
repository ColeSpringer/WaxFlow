package gain

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/dsp/internal/firwin"
	"github.com/colespringer/waxflow/waxerr"
)

// LimiterVersion is the limiter node's revision for cache keys: bump on
// any change to detection, smoothing constants, or the clamp (plan
// section 10).
const LimiterVersion = "limiter-1"

// DefaultCeilingDB is the default limiter ceiling in dBTP. -1 dB is the
// EBU R128 s1 headroom convention and leaves margin for the 4x detector's
// small underestimate of true inter-sample peaks.
const DefaultCeilingDB = -1.0

// Limiter is a look-ahead true-peak limiter: peaks are detected on a 4x
// oversampled estimate (BS.1770-4 style polyphase interpolation), the
// gain ramps down over a 5 ms look-ahead window before each peak arrives
// and releases over 50 ms after it passes, all channels sharing one gain
// so the stereo image cannot shift. A final sample-domain clamp at the
// ceiling turns the residual smoothing error (about 2 percent worst case)
// into a hard guarantee.
//
// Latency is fully compensated: output sample n is input sample n times
// its gain, so positions map one to one and an impulse stays exactly
// where it was. When the signal never exceeds the ceiling the gain holds
// at exactly 1 and the limiter is bit-transparent.
//
// The limiter is deterministic and not safe for concurrent use.
type Limiter struct {
	channels int
	ceil     float64
	look     int // look-ahead window W in frames
	aAtk     float64
	aRel     float64

	// 4x interpolator phases 1..3 (phase 0 is the sample itself). Fixed
	// size so the detect loop's bounds are compile-time constants.
	interp [3][interpTaps]float32

	// Pending samples occupy buf[c][:have]; buf[c][0] is absolute input
	// index start. peaks is index-aligned with buf. All cursors are
	// absolute input indices, so window math survives compaction.
	buf   [][]float32
	peaks []float32
	start int64 // absolute index of buf[c][0]
	base  int64 // absolute index of the next frame to emit
	have  int
	pk    int64 // absolute index up to which peaks are computed (exclusive)

	deque    []maxEntry // sliding-max candidates, values decreasing
	dqHead   int
	pushed   int64 // absolute index up to which peaks were offered to the deque
	g        float64
	draining bool
}

type maxEntry struct {
	idx int64
	v   float32
}

// interpTaps is the per-phase tap count of the 4x true-peak interpolator
// (16 taps reach 8 input samples on either side of the evaluation point).
const interpTaps = 16

// NewLimiter returns a limiter for one stream. ceilingDB is the true-peak
// ceiling in dBTP, at most 0; pass DefaultCeilingDB unless the caller has
// a reason not to.
func NewLimiter(rate, channels int, ceilingDB float64) (*Limiter, error) {
	if rate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain: limiter rate %d must be positive", rate))
	}
	if channels < 1 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain: limiter channel count %d must be positive", channels))
	}
	if ceilingDB > 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("gain: limiter ceiling %+.2f dB is above full scale", ceilingDB))
	}
	l := &Limiter{
		channels: channels,
		ceil:     FromDB(ceilingDB),
		look:     max(rate/200, 32), // 5 ms
	}
	// Attack settles to within e^-4 (2 percent) of the required gain by
	// the time the peak arrives; release takes 50 ms to recover.
	l.aAtk = 1 - math.Exp(-4/float64(l.look))
	l.aRel = 1 - math.Exp(-1/(0.050*float64(rate)))
	l.designInterp()

	capFrames := l.look + interpTaps + 4096
	l.buf = make([][]float32, channels)
	for c := range l.buf {
		l.buf[c] = make([]float32, capFrames)
	}
	l.peaks = make([]float32, capFrames)
	l.Reset()
	return l, nil
}

// Reset clears all state for a new stream segment.
func (l *Limiter) Reset() {
	l.start, l.base, l.have, l.pk, l.pushed = 0, 0, 0, 0, 0
	l.deque = l.deque[:0]
	l.dqHead = 0
	l.g = 1
	l.draining = false
}

// Process consumes frames from src and produces limited frames into dst,
// per channel, in lockstep, returning the counts written and consumed.
// Output lags input by the look-ahead window until Drain flushes the
// tail, so a Process call can produce less than it consumes (or nothing).
func (l *Limiter) Process(dst, src [][]float32) (produced, consumed int) {
	if l.draining {
		panic("gain: limiter Process after Drain")
	}
	if len(dst) != l.channels || len(src) != l.channels {
		panic("gain: limiter channel count mismatch")
	}
	space := len(dst[0])
	for {
		// Append what fits after sliding out the emitted prefix.
		l.compact()
		take := min(len(l.buf[0])-l.have, len(src[0])-consumed)
		for c := range l.buf {
			copy(l.buf[c][l.have:], src[c][consumed:consumed+take])
		}
		l.have += take
		consumed += take

		l.detect(false)
		produced += l.emit(dst, produced, space, false)
		if produced >= space || consumed >= len(src[0]) {
			return produced, consumed
		}
	}
}

// Drain flushes the delayed tail after the final Process call. Call
// repeatedly with non-empty dst slices until it returns 0.
func (l *Limiter) Drain(dst [][]float32) (produced int) {
	if len(dst) != l.channels {
		panic("gain: limiter channel count mismatch")
	}
	l.draining = true
	l.detect(true)
	return l.emit(dst, 0, len(dst[0]), true)
}

// compact drops emitted frames from the buffer front. The emit cursor is
// the low-water mark: emission trails detection by the look-ahead window
// (base <= pk-look-1), and detection reads at most interpTaps/2-1 samples
// behind pk, so nothing below base is ever read again.
func (l *Limiter) compact() {
	drop := int(l.base - l.start)
	if drop <= 0 {
		return
	}
	for c := range l.buf {
		copy(l.buf[c], l.buf[c][drop:l.have])
	}
	copy(l.peaks, l.peaks[drop:l.have])
	l.have -= drop
	l.start = l.base
}

// detect advances peak computation. The peak at index j reads samples
// j-7..j+8 through the interpolator, so it needs 8 samples of future;
// draining treats past-the-end samples as silence.
//
// The steady-state path re-slices the window to the interpolator's
// fixed length, so the tap loops carry no bounds checks; only the first
// half-window of a stream and the draining tail take the guarded path.
func (l *Limiter) detect(draining bool) {
	const half = interpTaps / 2
	end := l.have - half // last index with a full future window, exclusive
	if draining {
		end = l.have
	}
	for j := int(l.pk - l.start); j < end; j++ {
		var peak float32
		for c := range l.buf {
			buf := l.buf[c]
			if v := abs32(buf[j]); v > peak {
				peak = v
			}
			if lo := j - half + 1; lo >= 0 && j+half < l.have {
				win := (*[interpTaps]float32)(buf[lo : lo+interpTaps])
				for p := range l.interp {
					phase := &l.interp[p]
					var acc float32
					for t := 0; t < interpTaps; t++ {
						acc += phase[t] * win[t]
					}
					if a := abs32(acc); a > peak {
						peak = a
					}
				}
			} else {
				peak = l.detectEdge(buf, j, peak)
			}
		}
		l.peaks[j] = peak
		l.pk++
	}
}

// detectEdge is detect's guarded path for windows that spill past the
// buffered samples: the stream head (history is silence) and the
// draining tail (the future is silence).
func (l *Limiter) detectEdge(buf []float32, j int, peak float32) float32 {
	const half = interpTaps / 2
	for p := range l.interp {
		phase := &l.interp[p]
		var acc float32
		for t := 0; t < interpTaps; t++ {
			if i := j - half + 1 + t; i >= 0 && i < l.have {
				acc += phase[t] * buf[i]
			}
		}
		if a := abs32(acc); a > peak {
			peak = a
		}
	}
	return peak
}

// emit produces limited output frames starting at dst offset off. Each
// output needs the peak window [n, n+look] fully detected; draining
// shrinks the window at the stream tail.
func (l *Limiter) emit(dst [][]float32, off, space int, draining bool) (produced int) {
	ceil := float32(l.ceil)
	for off+produced < space {
		n := l.base
		winEnd := n + int64(l.look)
		if !draining && l.pk <= winEnd {
			return produced
		}
		if draining && n >= l.pk {
			return produced
		}

		// Offer newly detected peaks up to the window end to the deque,
		// then drop entries that fell out of the window front.
		for ; l.pushed < min(winEnd+1, l.pk); l.pushed++ {
			v := l.peaks[l.pushed-l.start]
			for len(l.deque) > l.dqHead && l.deque[len(l.deque)-1].v <= v {
				l.deque = l.deque[:len(l.deque)-1]
			}
			l.deque = append(l.deque, maxEntry{l.pushed, v})
		}
		for l.dqHead < len(l.deque) && l.deque[l.dqHead].idx < n {
			l.dqHead++
		}
		if l.dqHead > 0 && l.dqHead*2 > len(l.deque) {
			n := copy(l.deque, l.deque[l.dqHead:])
			l.deque = l.deque[:n]
			l.dqHead = 0
		}

		required := 1.0
		if env := float64(l.deque[l.dqHead].v); env > l.ceil {
			required = l.ceil / env
		}
		if required < l.g {
			l.g += (required - l.g) * l.aAtk
		} else {
			l.g += (required - l.g) * l.aRel
		}

		i := int(n - l.start)
		g := float32(l.g)
		for c := range l.buf {
			v := l.buf[c][i] * g
			if v > ceil {
				v = ceil
			} else if v < -ceil {
				v = -ceil
			}
			dst[c][off+produced] = v
		}
		produced++
		l.base++
	}
	return produced
}

// designInterp builds the three fractional phases of the 4x Kaiser
// windowed-sinc interpolator (the integer phase is the sample itself).
// Modest attenuation is plenty for a peak detector; the ceiling's
// headroom absorbs the estimate error.
func (l *Limiter) designInterp() {
	const beta = 3.67 // Kaiser for ~42 dB stopband at this length
	half := interpTaps / 2
	i0 := firwin.BesselI0(beta)
	for p := 1; p <= 3; p++ {
		c := &l.interp[p-1]
		frac := float64(p) / 4
		var sum float64
		for t := 0; t < interpTaps; t++ {
			// Tap t weighs sample j-half+1+t for the value at j+frac.
			x := frac - float64(t-half+1)
			w := firwin.BesselI0(beta*math.Sqrt(1-(x/float64(half))*(x/float64(half)))) / i0
			c[t] = float32(firwin.Sinc(x) * w)
			sum += float64(c[t])
		}
		for t := range c {
			c[t] = float32(float64(c[t]) / sum) // unity DC gain per phase
		}
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
