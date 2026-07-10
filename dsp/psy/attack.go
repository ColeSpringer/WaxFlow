package psy

// AttackDetector flags transient energy jumps for window switching. It
// splits each block into sub-windows and compares every sub-window's
// energy against the running level carried in from earlier audio, so an
// attack right at a block boundary is still caught. One detector per
// channel; not safe for concurrent use.
type AttackDetector struct {
	ratio float64
	floor float64
	prev  float64
}

// NewAttackDetector builds a detector firing when a sub-window holds
// ratio times the preceding level. ratio <= 1 falls back to the
// customary 8x (a 9 dB step). The floor is fixed at -60 dBFS mean
// square so silence noise never counts as level or attack.
func NewAttackDetector(ratio float64) *AttackDetector {
	if ratio <= 1 {
		ratio = 8
	}
	return &AttackDetector{ratio: ratio, floor: 1e-6}
}

// Reset clears the carried level, for seeks and splices.
func (d *AttackDetector) Reset() { d.prev = 0 }

// Scan examines one block split into sub equal sub-windows and reports
// the first attack and which sub-window holds it. The carried level
// updates whether or not an attack fires, so consecutive loud blocks
// flag once, on the jump.
func (d *AttackDetector) Scan(x []float32, sub int) (attack bool, pos int) {
	if sub < 1 || len(x) < sub {
		return false, 0
	}
	w := len(x) / sub
	level := d.prev
	for i := 0; i < sub; i++ {
		e := 0.0
		for _, v := range x[i*w : (i+1)*w] {
			e += float64(v) * float64(v)
		}
		e /= float64(w)
		// The reference never drops below the floor, so an onset out of
		// true silence still registers as a jump.
		ref := level
		if ref < d.floor {
			ref = d.floor
		}
		if !attack && e > d.ratio*ref {
			attack, pos = true, i
		}
		if e > level {
			level = e
		} else {
			// Decay the reference toward quiet passages so an attack
			// after a fade is still a jump against its local context.
			level = level*0.5 + e*0.5
		}
	}
	d.prev = level
	return attack, pos
}
