// Package id3 parses the byte length of an ID3v2 tag, from its header at the
// front of a stream or from the footer of one appended to the back. The MP3
// and ADTS demuxers skip a leading tag on the way to the first audio frame and
// the trailer package peels an appended one off the end; sharing one parser
// keeps those hostile-input paths in sync.
package id3

// HeaderLen is the fixed size of an ID3v2 header, and of the footer that
// mirrors it.
const HeaderLen = 10

// Size returns the total byte length of an ID3v2 tag starting at b, or 0
// when b does not begin with one. b should hold at least the 10-byte
// ID3v2 header; a short or non-syncsafe header reports 0 rather than guess.
func Size(b []byte) int64 {
	if len(b) < HeaderLen || string(b[:3]) != "ID3" {
		return 0
	}
	n := syncsafe(b)
	if n < 0 {
		return 0
	}
	n += HeaderLen
	if b[5]&0x10 != 0 {
		n += HeaderLen // footer
	}
	return n
}

// SizeFromFooter returns the total byte length of an ID3v2 tag whose 10-byte
// footer is b, or 0 when b is not one. Only a tag carrying a footer can be
// found from behind, which is the placement that requires one.
//
// The footer is 28 bits of recognition and the length behind it is an
// instruction to drop that many bytes, so a caller has to confirm the extent:
// Size of the bytes it reaches back to must agree.
func SizeFromFooter(b []byte) int64 {
	if len(b) < HeaderLen || string(b[:3]) != "3DI" {
		return 0
	}
	n := syncsafe(b)
	if n < 0 {
		return 0
	}
	return n + 2*HeaderLen // the header it mirrors, and itself
}

// syncsafe decodes the 28-bit length at b[6:10], or -1 when those bytes are
// not syncsafe and so are not a length at all.
func syncsafe(b []byte) int64 {
	for _, x := range b[6:10] {
		if x&0x80 != 0 {
			return -1 // treat as absent rather than guess
		}
	}
	return int64(b[6])<<21 | int64(b[7])<<14 | int64(b[8])<<7 | int64(b[9])
}
