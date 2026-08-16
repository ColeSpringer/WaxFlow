package loudness

import (
	"fmt"
	"math"

	"github.com/colespringer/waxflow/waxerr"
)

// PeakMeter measures the oversampled true peak and nothing else, for
// always-on taps that cannot pay for the full Meter. It shares Meter's
// interpolator, so the two read identical peaks from identical samples.
// Not safe for concurrent use.
type PeakMeter struct {
	channels int
	tp       *truePeak // nil above 192 kHz: the sample grid is dense enough
	max      float64
	flushed  bool
}

// NewPeakMeter returns a peak meter for the rate and channel count. It
// takes no layout: peaks are per-channel magnitudes, nothing is weighted.
func NewPeakMeter(rate, channels int) (*PeakMeter, error) {
	if rate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("loudness: meter rate %d must be positive", rate))
	}
	if channels <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("loudness: meter channel count %d must be positive", channels))
	}
	return &PeakMeter{channels: channels, tp: newTruePeak(rate, channels)}, nil
}

// Process consumes one chunk of planar float32 PCM, +-1.0 full scale. A
// wrong channel count panics (the dither.Quantize convention: callers are
// wiring, not user input). Process after Flush resumes as a fresh segment,
// the drain having left silence in the history.
func (m *PeakMeter) Process(chans [][]float32) {
	m.check(len(chans))
	m.flushed = false
	for c, ch := range chans {
		for _, s := range ch {
			m.push(c, float64(s))
		}
	}
}

// ProcessInt consumes one chunk of planar right-justified integer PCM of
// the given depth, normalized by 2^(bits-1) so full scale is 1.0 (the
// audio package's domain contract). Same contract as Process.
func (m *PeakMeter) ProcessInt(chans [][]int32, bits int) {
	m.check(len(chans))
	m.flushed = false
	scale := 1 / math.Ldexp(1, bits-1)
	for c, ch := range chans {
		for _, s := range ch {
			m.push(c, float64(s)*scale)
		}
	}
}

func (m *PeakMeter) check(n int) {
	if n != m.channels {
		panic("loudness: chunk channel count differs from the meter's")
	}
}

func (m *PeakMeter) push(c int, x float64) {
	if m.tp != nil {
		if p := m.tp.push(c, x); p > m.max {
			m.max = p
		}
	} else if a := math.Abs(x); a > m.max {
		m.max = a
	}
}

// Flush drains the interpolator tail, so a peak within the final half
// window is counted; the reading is complete only after it. Idempotent,
// and not terminal: Process may follow (see its doc).
func (m *PeakMeter) Flush() {
	if m.flushed {
		return
	}
	m.flushed = true
	if m.tp != nil {
		if p := m.tp.drain(); p > m.max {
			m.max = p
		}
	}
}

// Peak returns the largest true-peak magnitude seen: linear, 1.0 full
// scale, 0 silence; dBTP is 20*log10 of it.
func (m *PeakMeter) Peak() float64 { return m.max }
