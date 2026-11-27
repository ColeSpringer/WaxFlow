// Package flacn demuxes native FLAC framing (RFC 9639): the fLaC marker,
// metadata blocks, and the self-framing audio stream. Frames become
// codec packets whose boundaries are confirmed, not guessed: a boundary
// is accepted only when the next sync candidate parses as a header
// consistent with STREAMINFO, carries the expected frame or sample
// number, and the bytes before it checksum the whole candidate frame
// (CRC-16). A frame whose bytes never check is damage the read steps
// over, as libFLAC and ffmpeg do: it resyncs on the next frame that
// checks, which its own number places, and reports the hole once. The
// samples it took go to the reader as container.Packet.Lost, which a
// format.Media fills with silence.
// Seeking uses the SEEKTABLE when one exists and bisection on the frame
// headers' own position numbers otherwise; either way the demuxer lands
// on a frame boundary at or before the target and format.Media pre-rolls
// the rest, sample-exact.
package flacn

import (
	"bytes"

	"github.com/colespringer/waxflow/codec/flac"
)

// Match reports whether head begins with the fLaC stream marker. It is
// the format sniff-table entry.
func Match(head []byte) bool {
	return len(head) >= 4 && string(head[:4]) == "fLaC"
}

// FindStart returns the offset of the first stream start window holds: the
// marker, the STREAMINFO block a stream must open with, parsed, and behind
// it what a stream goes on with, the header of a block of a defined type or,
// where STREAMINFO is the last block, a frame's sync code. It is how a stream
// behind stray bytes is told from four letters in them.
func FindStart(window []byte) (int, bool) {
	const need = 8 + flac.StreamInfoLen
	for i := 0; ; i++ {
		j := bytes.Index(window[i:], []byte("fLaC"))
		if j < 0 {
			return 0, false
		}
		i += j
		b := window[i:]
		if len(b) < need+2 || b[4]&0x7F != 0 || int(b[5])<<16|int(b[6])<<8|int(b[7]) != flac.StreamInfoLen {
			continue
		}
		if _, err := flac.ParseStreamInfo(b[8:need]); err != nil {
			continue
		}
		if next := b[need:]; b[4]&0x80 != 0 {
			if next[0] != 0xFF || next[1]&0xFE != 0xF8 {
				continue
			}
		} else if typ := next[0] & 0x7F; typ < 1 || typ > 6 {
			continue
		}
		return i, true
	}
}
