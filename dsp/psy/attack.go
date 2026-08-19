package psy

// AttackDetector flags transient energy jumps for window switching. It
// splits each block into sub-windows and compares every sub-window's
// energy against the previous sub-window's, carried across blocks, so
// an attack right at a block boundary is still caught and a second
// attack during the first one's decay still reads as a jump. (An
// earlier slow-decaying running-level reference held clicks 20-50 ms
// apart under the ratio: the level was still carrying the first
// click's energy when the second arrived. Drum rolls, hi-hat
// subdivisions, and applause live at that spacing, and the reference
// swap only ever adds detections: the running level never sat below
// the previous sub-window's energy.)
//
// The refractory window is what separates rhythm from pitch under the
// sharper reference: jumps closer than refractory sub-windows to the
// previous jump reset the clock but are not reported, so a pulse train
// at pitch rate (a 100 Hz buzz is 10 ms of spacing) reads as one onset
// and then sustained content, while a roll or applause at 20 ms and up
// reports every stroke. One detector per channel; not safe for
// concurrent use.
//
// Consumers version this behavior through their own encoder revisions
// (the AAC and Vorbis encoders), not psy.Version, which covers the
// masking model.
type AttackDetector struct {
	ratio      float64
	floor      float64
	refractory int
	prev       float64 // previous sub-window energy
	since      int     // sub-windows since the last detected jump
}

// sinceCap saturates the jump clock: anything this far back is "long
// ago" for every plausible refractory.
const sinceCap = 1 << 30

// NewAttackDetector builds a detector firing when a sub-window holds
// ratio times the previous one's energy and the last jump is at least
// refractory sub-windows back (0 reports every jump). ratio <= 1 falls
// back to the customary 8x (a 9 dB step). The floor is fixed at
// -60 dBFS mean square so silence noise never counts as reference or
// attack.
func NewAttackDetector(ratio float64, refractory int) *AttackDetector {
	if ratio <= 1 {
		ratio = 8
	}
	return &AttackDetector{ratio: ratio, floor: 1e-6, refractory: refractory, since: sinceCap}
}

// Reset clears the carried reference and jump clock, for seeks and
// splices.
func (d *AttackDetector) Reset() {
	d.prev = 0
	d.since = sinceCap
}

// Scan examines one block split into sub equal sub-windows and reports
// the first attack and which sub-window holds it. The carried
// reference updates whether or not an attack fires, so consecutive
// loud blocks flag once, on the jump.
func (d *AttackDetector) Scan(x []float32, sub int) (attack bool, pos int) {
	if sub < 1 || len(x) < sub {
		return false, 0
	}
	w := len(x) / sub
	prev := d.prev
	for i := 0; i < sub; i++ {
		e := 0.0
		for _, v := range x[i*w : (i+1)*w] {
			e += float64(v) * float64(v)
		}
		e /= float64(w)
		// The reference never drops below the floor, so an onset out of
		// true silence still registers as a jump.
		ref := prev
		if ref < d.floor {
			ref = d.floor
		}
		if e > d.ratio*ref {
			if !attack && d.since >= d.refractory {
				attack, pos = true, i
			}
			d.since = 0
		} else if d.since < sinceCap {
			d.since++
		}
		prev = e
	}
	d.prev = prev
	return attack, pos
}
