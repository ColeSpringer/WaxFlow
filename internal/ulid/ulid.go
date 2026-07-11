// Package ulid mints ULIDs: 26-character Crockford base32 identifiers
// packing a 48-bit millisecond timestamp ahead of 80 bits of entropy,
// so ids sort lexicographically in creation order. There is no
// monotonicity guarantee within a millisecond; 80 bits of fresh entropy
// per id keeps collisions out of reach without one.
package ulid

import (
	"crypto/rand"
	"fmt"
	"io"
	"time"
)

// Len is the length of a rendered ULID.
const Len = 26

// alphabet is Crockford base32: uppercase, no I, L, O, or U.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// maxMillis bounds the 48-bit timestamp field (reached in the year 10889).
const maxMillis = 1<<48 - 1

// New returns a fresh ULID using the current time and crypto/rand.
func New() (string, error) {
	return Make(time.Now(), rand.Reader)
}

// Make renders a ULID from an explicit time and entropy source. It reads
// exactly 10 bytes from entropy. New is the production path; Make exists
// so tests can pin both inputs.
func Make(t time.Time, entropy io.Reader) (string, error) {
	ms := t.UnixMilli()
	if ms < 0 || ms > maxMillis {
		return "", fmt.Errorf("ulid: time out of range: %v", t)
	}
	var e [10]byte
	if _, err := io.ReadFull(entropy, e[:]); err != nil {
		return "", fmt.Errorf("ulid: reading entropy: %w", err)
	}

	// The 128-bit value, big-endian: 48 timestamp bits then 80 entropy
	// bits, split across two words. hi holds the top 64 bits (timestamp
	// plus the first two entropy bytes), lo the bottom 64.
	hi := uint64(ms)<<16 | uint64(e[0])<<8 | uint64(e[1])
	lo := uint64(e[2])<<56 | uint64(e[3])<<48 | uint64(e[4])<<40 |
		uint64(e[5])<<32 | uint64(e[6])<<24 | uint64(e[7])<<16 |
		uint64(e[8])<<8 | uint64(e[9])

	// Peel 5 bits at a time off the low end, shifting the 128-bit value
	// right as we go. 26 characters cover 130 bits; the 2 bits of slack
	// land in the first character, which therefore only ever reads 0-7.
	var b [Len]byte
	for i := Len - 1; i >= 0; i-- {
		b[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(b[:]), nil
}

// inAlphabet is a byte-indexed membership table for the Crockford digits.
var inAlphabet = func() [256]bool {
	var t [256]bool
	for i := range len(alphabet) {
		t[alphabet[i]] = true
	}
	return t
}()

// Valid reports whether s is a well-formed ULID: 26 characters of the
// uppercase Crockford alphabet whose value fits in 128 bits (so the
// first character is 0-7). Ids are minted here and never normalized,
// so lowercase is rejected, as are the excluded letters I, L, O, U.
func Valid(s string) bool {
	if len(s) != Len || s[0] > '7' {
		return false
	}
	for i := range len(s) {
		if !inAlphabet[s[i]] {
			return false
		}
	}
	return true
}
