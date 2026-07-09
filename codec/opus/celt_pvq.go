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
