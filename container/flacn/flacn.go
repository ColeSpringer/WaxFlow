// Package flacn demuxes native FLAC framing (RFC 9639): the fLaC marker,
// metadata blocks, and the self-framing audio stream. Frames become
// codec packets whose boundaries are confirmed, not guessed: a boundary
// is accepted only when the next sync candidate parses as a header
// consistent with STREAMINFO, carries the expected frame or sample
// number, and the bytes before it checksum the whole candidate frame
// (CRC-16). Seeking uses the SEEKTABLE when one exists and bisection on
// the frame headers' own position numbers otherwise; either way the
// demuxer lands on a frame boundary at or before the target and
// format.Media pre-rolls the rest, sample-exact.
package flacn

// Match reports whether head begins with the fLaC stream marker. It is
// the format sniff-table entry.
func Match(head []byte) bool {
	return len(head) >= 4 && string(head[:4]) == "fLaC"
}
