package testutil

// ZeroMiddle returns a copy of raw with n bytes at its middle overwritten by
// zeros. On a frame run it is damage past the head: the first frames parse,
// the walk that reaches the middle skips the zeros, and only a read or a
// walk that gets there finds it, which is what the live-warning cells test.
func ZeroMiddle(raw []byte, n int) []byte {
	out := append([]byte(nil), raw...)
	mid := len(out) / 2
	clear(out[mid:min(mid+n, len(out))])
	return out
}
