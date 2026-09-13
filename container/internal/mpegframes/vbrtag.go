package mpegframes

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec/mp3"
)

// VBRInfo is a parsed Xing, Info, or VBRI metadata frame: the fields a
// demuxer consumes today. The tags carry more (byte counts, TOCs, a CBR
// marker) that nothing reads yet; parsing skips over it.
type VBRInfo struct {
	// Frames is the audio frame count, 0 when absent.
	Frames int64
	// Delay and Padding are the LAME gapless fields (encoder samples),
	// -1 when the tag carries no LAME extension.
	Delay, Padding int64
}

// ParseVBRTag inspects a frame's payload for a Xing/Info or VBRI tag. h is
// the frame's parsed header, frame its whole bytes.
func ParseVBRTag(h mp3.Header, frame []byte) (VBRInfo, bool) {
	tag := VBRInfo{Delay: -1, Padding: -1}

	off := mp3.HeaderLen + h.SideInfoLen()
	if h.Protected {
		off += 2
	}
	if len(frame) >= off+8 {
		magic := string(frame[off : off+4])
		if magic == "Xing" || magic == "Info" {
			flags := binary.BigEndian.Uint32(frame[off+4:])
			p := off + 8
			take := func(n int) []byte {
				if p+n > len(frame) {
					p = len(frame)
					return nil
				}
				b := frame[p : p+n]
				p += n
				return b
			}
			if flags&1 != 0 {
				if b := take(4); b != nil {
					tag.Frames = int64(binary.BigEndian.Uint32(b))
				}
			}
			if flags&2 != 0 {
				take(4) // stream byte count
			}
			if flags&4 != 0 {
				take(100) // TOC: coarse byte hints; the exact index supersedes it
			}
			if flags&8 != 0 {
				take(4) // quality
			}
			// LAME extension: encoder string, then the gapless delay and
			// padding packed in three bytes at its offset 21.
			if enc := take(9); enc != nil {
				switch string(enc[:4]) {
				case "LAME", "Lavc", "Lavf", "WaxF":
					if p+12+3 <= len(frame) {
						b := frame[p+12:]
						tag.Delay = int64(b[0])<<4 | int64(b[1])>>4
						tag.Padding = int64(b[1]&0xF)<<8 | int64(b[2])
					}
				}
			}
			return tag, true
		}
	}

	// VBRI (Fraunhofer): fixed 32 bytes after the header.
	off = mp3.HeaderLen + 32
	if len(frame) >= off+26 && string(frame[off:off+4]) == "VBRI" {
		tag.Frames = int64(binary.BigEndian.Uint32(frame[off+14:]))
		return tag, true
	}
	return VBRInfo{}, false
}
