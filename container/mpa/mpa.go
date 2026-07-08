// Package mpa demuxes the MP3 elementary stream: a bare sequence of
// Layer III frames, usually wrapped in ID3 tags, often led by a Xing,
// Info, or VBRI metadata frame.
//
// MP3 frames carry no position, so sample-exact seeking needs an exact
// frame index: the demuxer builds one lazily as it walks (each frame
// header gives the next frame's offset) and reuses it across seeks. The
// walk tolerates real-world mess with bounded resyncs and structured
// warnings, per the package container invariants.
//
// Seek landings back off from the target far enough that the codec's bit
// reservoir (up to 511 bytes of backward reference) and filterbank
// history converge before the target frame: the frames in between decode
// (or silence out) and are discarded by format.Media's pre-roll, so
// post-seek output is bit-identical to a linear decode.
package mpa

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec/mp3"
)

// Match reports whether head begins with an MP3 elementary stream: a
// parsable Layer III header confirmed by a second one right behind it
// (the sync word alone false-positives on arbitrary binaries, which is
// also why this driver sniffs last). A leading ID3v2 tag was already
// skipped by the caller (format's sniff) or by NewDemuxer.
func Match(head []byte) bool {
	for off := 0; off < maxLeadingJunk && off+mp3.HeaderLen <= len(head); off++ {
		h, err := mp3.ParseHeader(head[off:])
		if err != nil || h.Size() == 0 {
			continue
		}
		next := off + h.Size()
		if next+mp3.HeaderLen > len(head) {
			// The confirming frame lies past the sniff window; a lone
			// candidate at the very start is convincing enough, junk
			// deeper in is not.
			return off == 0
		}
		if n, err := mp3.ParseHeader(head[next:]); err == nil && h.Kin(n) {
			return true
		}
	}
	return false
}

// maxLeadingJunk bounds the Match scan; the tolerant demuxer itself
// scans further (maxResync) once the driver is chosen.
const maxLeadingJunk = 8 << 10

// MatchNeed is the sniff window Match wants: room for leading junk plus
// two maximum-size frames.
const MatchNeed = maxLeadingJunk + 2*1441 + mp3.HeaderLen

// decoderDelay is the fixed Layer III decoder latency in samples
// (528 plus 1) that gapless trims add to the encoder delay signaled in
// the LAME tag; every mainstream decoder applies the same constant, so
// trimmed output lines up across implementations.
const decoderDelay = 529

// vbrTag is a parsed Xing, Info, or VBRI metadata frame: the fields the
// demuxer consumes today. The tags carry more (byte counts, TOCs, a CBR
// marker) that nothing reads yet; parsing skips over it.
type vbrTag struct {
	frames int64 // audio frame count, 0 when absent
	// delay and padding are the LAME gapless fields (encoder samples),
	// -1 when the tag carries no LAME extension.
	delay, padding int64
}

// parseVBRTag inspects the first frame's payload for a Xing/Info or VBRI
// tag. h is the frame's parsed header, frame its whole bytes.
func parseVBRTag(h mp3.Header, frame []byte) (vbrTag, bool) {
	tag := vbrTag{delay: -1, padding: -1}

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
					tag.frames = int64(binary.BigEndian.Uint32(b))
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
						tag.delay = int64(b[0])<<4 | int64(b[1])>>4
						tag.padding = int64(b[1]&0xF)<<8 | int64(b[2])
					}
				}
			}
			return tag, true
		}
	}

	// VBRI (Fraunhofer): fixed 32 bytes after the header.
	off = mp3.HeaderLen + 32
	if len(frame) >= off+26 && string(frame[off:off+4]) == "VBRI" {
		tag.frames = int64(binary.BigEndian.Uint32(frame[off+14:]))
		return tag, true
	}
	return vbrTag{}, false
}
