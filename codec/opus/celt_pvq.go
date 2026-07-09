package opus

import "math"

// CELT PVQ (pyramid vector quantization) shape decoding, RFC 6716 section
// 4.3.4. Ported from libopus cwrs.c and vq.c. A band's
// unit-norm shape is coded as an index into the set of integer vectors with K
// pulses summing (in absolute value) to K; cwrsi enumerates that set. The U(n,k)
// combinatorial counts are computed from the recurrence
// u[n][k] = u[n-1][k] + u[n][k-1] + u[n-1][k-1] rather than a static table.
//
// All arithmetic is the float build: the fixed-point Q-shifts are no-ops, so
// normalise_residual is X = iy·gain/‖iy‖ and the spread rotation is a plain
// Givens rotation.

// unext advances the U() recurrence one row: u becomes the next row given base
// case ui0 (libopus cwrs.c unext).
func unext(u []uint32, ln int, ui0 uint32) {
	j := 1
	for ; j < ln; j++ {
		ui1 := u[j] + u[j-1] + ui0
		u[j-1] = ui0
		ui0 = ui1
	}
	u[j-1] = ui0
}

// uprev steps the U() recurrence back one row (libopus cwrs.c uprev).
func uprev(u []uint32, ui0 uint32) {
	ln := len(u)
	j := 1
	for ; j < ln; j++ {
		ui1 := u[j] - u[j-1] - ui0
		u[j-1] = ui0
		ui0 = ui1
	}
	u[j-1] = ui0
}

// ncwrsURow fills u[0..k+1] with row n of U() and returns V(n,k)=U(n,k)+U(n,k+1),
// the number of PVQ codewords (libopus cwrs.c ncwrs_urow). u must have length
// ≥ k+2.
func ncwrsURow(n, k int, u []uint32) uint32 {
	ln := k + 2
	u[0] = 0
	u[1] = 1
	for i := 2; i < ln; i++ {
		u[i] = uint32(i<<1) - 1
	}
	for i := 2; i < n; i++ {
		unext(u[1:], k+1, 1)
	}
	return u[k] + u[k+1]
}

// cwrsi decodes index i into the pulse vector y (length n, K=k pulses), using
// row n of U() in u (destructively modified). Returns Σ y² (libopus cwrs.c).
func cwrsi(n, k int, i uint32, y []int, u []uint32) float32 {
	var yy float32
	for j := 0; j < n; j++ {
		p := u[k+1]
		s := 0
		if i >= p {
			s = -1
		}
		i -= p & uint32(s)
		yj := k
		p = u[k]
		for p > i {
			k--
			p = u[k]
		}
		i -= p
		yj -= k
		val := (yj + s) ^ s
		y[j] = val
		yy += float32(val) * float32(val)
		uprev(u[:k+2], 0)
	}
	return yy
}

// decodePulses range-decodes a PVQ codeword index and expands it to y. u is
// caller scratch of length ≥ k+2.
func decodePulses(y []int, n, k int, d *rangeDecoder, u []uint32) float32 {
	v := ncwrsURow(n, k, u)
	i := d.decodeUint(v)
	return cwrsi(n, k, i, y, u)
}

// expRotation1 applies one Givens rotation pass across X with the given stride
// (libopus vq.c exp_rotation1; float build, scale shifts elided).
func expRotation1(X []float32, length, stride int, c, s float32) {
	ms := -s
	for i := 0; i < length-stride; i++ {
		x1 := X[i]
		x2 := X[i+stride]
		X[i+stride] = c*x2 + s*x1
		X[i] = c*x1 + ms*x2
	}
	for i := length - 2*stride - 1; i >= 0; i-- {
		x1 := X[i]
		x2 := X[i+stride]
		X[i+stride] = c*x2 + s*x1
		X[i] = c*x1 + ms*x2
	}
}

var spreadFactor = [3]int{15, 10, 5}

// expRotation applies (dir<0) or removes (dir>0) the pre-rotation that spreads a
// band's energy to reduce tonal artifacts (libopus vq.c exp_rotation). B is the
// number of interleaved blocks.
func expRotation(X []float32, n, dir, B, K, spread int) {
	if 2*K >= n || spread == spreadNone {
		return
	}
	factor := spreadFactor[spread-1]
	gain := float32(n) / float32(n+factor*K)
	theta := 0.5 * gain * gain
	c := float32(math.Cos(0.5 * math.Pi * float64(theta)))
	s := float32(math.Cos(0.5 * math.Pi * float64(1-theta))) // sin(π/2·theta)

	stride2 := 0
	length := n
	if length >= 8*B {
		stride2 = 1
		for (stride2*stride2+stride2)*B+(B>>2) < length {
			stride2++
		}
	}
	length /= B
	for i := 0; i < B; i++ {
		seg := X[i*length:]
		if dir < 0 {
			if stride2 != 0 {
				expRotation1(seg, length, stride2, s, c)
			}
			expRotation1(seg, length, 1, c, s)
		} else {
			expRotation1(seg, length, 1, c, -s)
			if stride2 != 0 {
				expRotation1(seg, length, stride2, s, -c)
			}
		}
	}
}

// normaliseResidual scales the integer pulse vector iy to unit norm times gain
// (libopus vq.c normalise_residual; float build).
func normaliseResidual(iy []int, X []float32, n int, ryy, gain float32) {
	g := gain / float32(math.Sqrt(float64(ryy)))
	for i := 0; i < n; i++ {
		X[i] = float32(iy[i]) * g
	}
}

// extractCollapseMask reports, as a bitmask over the B interleaved blocks, which
// blocks received at least one pulse (libopus vq.c). Used by anti-collapse.
func extractCollapseMask(iy []int, N, B int) uint32 {
	if B <= 1 {
		return 1
	}
	N0 := N / B
	var mask uint32
	for i := 0; i < B; i++ {
		var tmp int
		for j := 0; j < N0; j++ {
			tmp |= iy[i*N0+j]
		}
		if tmp != 0 {
			mask |= 1 << uint(i)
		}
	}
	return mask
}

// algUnquant decodes a band's shape into X: PVQ index → pulses → unit-norm
// vector → spread rotation (libopus vq.c alg_unquant). iy and u are caller
// scratch (length ≥ n and ≥ K+2). Returns the collapse mask.
func algUnquant(X []float32, n, K, spread, B int, d *rangeDecoder, gain float32, iy []int, u []uint32) uint32 {
	ryy := decodePulses(iy, n, K, d, u)
	normaliseResidual(iy, X, n, ryy, gain)
	expRotation(X, n, -1, B, K, spread)
	return extractCollapseMask(iy, n, B)
}

func iabs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// opPVQSearch finds the K-pulse integer vector iy that best matches the band
// shape X (maximizing the projection X·y/‖y‖), returning Σy². It is a clean-room
// port of libopus vq.c op_pvq_search_c (float build): a pyramid pre-projection
// when K is large, then a greedy pulse-by-pulse refinement. X is consumed (its
// sign is stripped); the caller resynthesizes from iy.
func opPVQSearch(X []float32, iy []int, K, N int) float32 {
	y := make([]float32, N)
	signx := make([]int, N)
	var sum, xy, yy float32
	for j := 0; j < N; j++ {
		if X[j] < 0 {
			signx[j] = 1
		} else {
			signx[j] = 0
		}
		X[j] = absf(X[j])
		iy[j] = 0
		y[j] = 0
	}
	pulsesLeft := K
	if K > N>>1 {
		for j := 0; j < N; j++ {
			sum += X[j]
		}
		// If X is too small, replace it with a single pulse to avoid NaNs and
		// over-allocation (64 approximates infinity).
		if !(sum > 1e-15 && sum < 64) {
			X[0] = 1.0
			for j := 1; j < N; j++ {
				X[j] = 0
			}
			sum = 1.0
		}
		// K+0.8 guarantees at most K pulses after the floor.
		rcp := (float32(K) + 0.8) / sum
		for j := 0; j < N; j++ {
			iy[j] = int(math.Floor(float64(rcp * X[j])))
			y[j] = float32(iy[j])
			yy += y[j] * y[j]
			xy += X[j] * y[j]
			y[j] *= 2
			pulsesLeft -= iy[j]
		}
	}
	// Should not happen outside silence, but keep the pulse count exact.
	if pulsesLeft > N+3 {
		tmp := float32(pulsesLeft)
		yy += tmp * tmp
		yy += tmp * y[0]
		iy[0] += pulsesLeft
		pulsesLeft = 0
	}
	for i := 0; i < pulsesLeft; i++ {
		yy += 1
		// Position 0 out of the loop; the branch below is usually not taken.
		Rxy := xy + X[0]
		Ryy := yy + y[0]
		Rxy = Rxy * Rxy
		bestDen := Ryy
		bestNum := Rxy
		bestID := 0
		for j := 1; j < N; j++ {
			Rxy = xy + X[j]
			Ryy = yy + y[j]
			Rxy = Rxy * Rxy
			// Maximize Rxy/sqrt(Ryy) via a cross-multiply, no division.
			if bestDen*Rxy > Ryy*bestNum {
				bestDen = Ryy
				bestNum = Rxy
				bestID = j
			}
		}
		xy += X[bestID]
		yy += y[bestID]
		y[bestID] += 2
		iy[bestID]++
	}
	for j := 0; j < N; j++ {
		iy[j] = (iy[j] ^ -signx[j]) + signx[j]
	}
	return yy
}

// icwrs returns the PVQ codeword index for the pulse vector y (length n, k
// pulses) and V(n,k), using u as scratch of length ≥ k+2 (libopus cwrs.c
// SMALL_FOOTPRINT icwrs). It is the exact inverse of cwrsi.
func icwrs(n, k int, y []int, u []uint32) (i, nc uint32) {
	u[0] = 0
	for m := 1; m <= k+1; m++ {
		u[m] = uint32(m<<1) - 1
	}
	run := iabs(y[n-1]) // running |sum|, from icwrs1 on the last element
	if y[n-1] < 0 {
		i = 1
	}
	j := n - 2
	i += u[run]
	run += iabs(y[j])
	if y[j] < 0 {
		i += u[run+1]
	}
	for j > 0 {
		j--
		unext(u, k+2, 0)
		i += u[run]
		run += iabs(y[j])
		if y[j] < 0 {
			i += u[run+1]
		}
	}
	return i, u[run] + u[run+1]
}

// encodePulses range-codes the PVQ index for y (n samples, k pulses), the
// inverse of decodePulses. u is caller scratch of length ≥ k+2.
func encodePulses(y []int, n, k int, e *rangeEncoder, u []uint32) {
	i, nc := icwrs(n, k, y, u)
	e.encodeUint(i, nc)
}

// algQuant searches and encodes a band's shape: spread-rotate, PVQ pulse search,
// then codeword encode. When resynth is set it also rebuilds the reconstructed
// unit vector into X (needed only when a later stage folds from it); a normal
// encode leaves resynth off, matching libopus at moderate complexity. Clean-room
// port of libopus vq.c alg_quant (float build). iy and u are caller scratch.
func algQuant(X []float32, n, K, spread, B int, e *rangeEncoder, gain float32, iy []int, u []uint32, resynth bool) uint32 {
	expRotation(X, n, 1, B, K, spread)
	yy := opPVQSearch(X, iy, K, n)
	cm := extractCollapseMask(iy, n, B)
	encodePulses(iy, n, K, e, u)
	if resynth {
		normaliseResidual(iy, X, n, yy, gain)
		expRotation(X, n, -1, B, K, spread)
	}
	return cm
}

// stereoItheta estimates a band's mid/side (or two-half) split angle in Q30
// (libopus vq.c stereo_itheta, float build): the normalized atan2 of the side
// energy over the mid energy. The result feeds compute_theta's quantizer.
func stereoItheta(X, Y []float32, stereo, N int) int {
	var Emid, Eside float32
	if stereo != 0 {
		for i := 0; i < N; i++ {
			m := X[i] + Y[i]
			s := X[i] - Y[i]
			Emid += m * m
			Eside += s * s
		}
	} else {
		for i := 0; i < N; i++ {
			Emid += X[i] * X[i]
			Eside += Y[i] * Y[i]
		}
	}
	if Emid+Eside < 1e-18 {
		return 0
	}
	mid := math.Sqrt(float64(Emid))
	side := math.Sqrt(float64(Eside))
	// atan2p_norm(side, mid) = atan2(side,mid)·2/π in [0,1]; scale to Q30.
	return int(math.Floor(0.5 + 1073741824.0*(math.Atan2(side, mid)*(2.0/math.Pi))))
}

// renormaliseVector rescales X to gain·X/‖X‖ (libopus vq.c renormalise_vector;
// float build). Used by band folding and the theta split.
func renormaliseVector(X []float32, N int, gain float32) {
	E := float32(1e-15)
	for i := 0; i < N; i++ {
		E += X[i] * X[i]
	}
	g := gain / float32(math.Sqrt(float64(E)))
	for i := 0; i < N; i++ {
		X[i] *= g
	}
}
