package mp4

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Muxer-side movie metadata: the moov's udta box holding iTunes-style
// ilst tags (with optional cover art), Nero chpl chapters, and the
// iTunSMPB gapless atom. Everything here is built once at Begin, so the
// output stays deterministic; the only later writes are the fixed-width
// iTunSMPB back-patches at End on a seekable destination.

// ilstText maps canonical tag keys onto their iTunes text atoms, in the
// fixed order the atoms are written (map iteration would break byte
// determinism). Multi-valued keys join with "; ": an ilst data atom is a
// single string.
var ilstText = []struct{ key, atom string }{
	{"TITLE", "\xa9nam"},
	{"ARTIST", "\xa9ART"},
	{"ALBUM", "\xa9alb"},
	{"ALBUMARTIST", "aART"},
	{"COMPOSER", "\xa9wrt"},
	{"GENRE", "\xa9gen"},
	{"RECORDINGDATE", "\xa9day"},
	{"COMMENT", "\xa9cmt"},
	{"LYRICS", "\xa9lyr"},
}

// ilstFreeform lists the canonical keys written as iTunes freeform
// (----:com.apple.iTunes:KEY) atoms: the ReplayGain fields players read
// from M4A. iTunSMPB is freeform too but is muxer-owned, not caller tag
// data.
var ilstFreeform = []string{
	"REPLAYGAIN_TRACK_GAIN",
	"REPLAYGAIN_TRACK_PEAK",
	"REPLAYGAIN_ALBUM_GAIN",
	"REPLAYGAIN_ALBUM_PEAK",
}

// smpb is the iTunSMPB payload template: twelve space-prefixed fixed-width
// hex fields (the iTunes spelling). Field 2 is the encoder delay, field 3
// the trailing padding, field 4 the 64-bit trimmed sample count; the rest
// are always zero. The widths are fixed, so End can patch values into a
// written header without moving a byte.
const (
	smpbDelayOff   = 10 // " 00000000 " then eight delay digits
	smpbPaddingOff = 19
	smpbLengthOff  = 28
)

func smpbPayload(delay, length int64) string {
	return fmt.Sprintf(" 00000000 %08X %08X %016X"+
		" 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000",
		uint32(delay), uint32(0), uint64(length))
}

// udtaBox assembles the moov user-data box: meta/ilst when there are tags,
// art, or an iTunSMPB payload, plus chpl when there are chapters. smpb is
// the pre-built iTunSMPB freeform atom (nil for none). Returns nil when
// there is nothing to say.
func udtaBox(tags []container.Tag, chapters []container.Chapter, art *container.Picture, smpb []byte) []byte {
	ilst := ilstBox(tags, art, smpb)
	chpl := chplBox(chapters)
	if ilst == nil && chpl == nil {
		return nil
	}
	var parts [][]byte
	if ilst != nil {
		hdlr := makeFullBox("hdlr", 0, 0,
			u32(0),
			[]byte("mdir"), []byte("appl"),
			u32(0), u32(0),
			[]byte{0})
		parts = append(parts, makeFullBox("meta", 0, 0, hdlr, makeBox("ilst", ilst)))
	}
	if chpl != nil {
		parts = append(parts, chpl)
	}
	return makeBox("udta", parts...)
}

// ilstBox builds the ilst payload (concatenated item atoms), nil when empty.
func ilstBox(tags []container.Tag, art *container.Picture, smpb []byte) []byte {
	vals := make(map[string][]string, len(tags))
	for _, t := range tags {
		if t.Value != "" {
			vals[t.Key] = append(vals[t.Key], t.Value)
		}
	}
	var out []byte
	for _, m := range ilstText {
		if vs := vals[m.key]; len(vs) > 0 {
			out = append(out, itemAtom(m.atom, dataAtom(1, []byte(strings.Join(vs, "; "))))...)
		}
	}
	if b := numberPairAtom("trkn", vals["TRACKNUMBER"], vals["TRACKTOTAL"], 8); b != nil {
		out = append(out, b...)
	}
	if b := numberPairAtom("disk", vals["DISCNUMBER"], vals["DISCTOTAL"], 6); b != nil {
		out = append(out, b...)
	}
	for _, key := range ilstFreeform {
		if vs := vals[key]; len(vs) > 0 {
			out = append(out, freeformAtom(key, vs[0])...)
		}
	}
	if art != nil && len(art.Data) > 0 {
		flag := uint32(13) // JPEG
		if strings.Contains(art.MIME, "png") {
			flag = 14
		}
		out = append(out, itemAtom("covr", dataAtom(flag, art.Data))...)
	}
	out = append(out, smpb...)
	return out
}

// itemAtom wraps a data atom in its ilst item box.
func itemAtom(atom string, data []byte) []byte {
	return makeBox(atom, data)
}

// dataAtom builds the ilst value carrier: type flag (1 UTF-8 text, 0 raw
// binary, 13 JPEG, 14 PNG), a zero locale, then the payload.
func dataAtom(flag uint32, payload []byte) []byte {
	return makeBox("data", u32(flag), u32(0), payload)
}

// numberPairAtom renders trkn/disk: 16-bit index and total inside a raw
// binary data atom (trkn carries a trailing reserved pair, disk does not).
// Unparsable or absent numbers yield nil rather than a zero atom.
func numberPairAtom(atom string, nums, totals []string, size int) []byte {
	n := firstUint16(nums)
	t := firstUint16(totals)
	if n == 0 && t == 0 {
		return nil
	}
	payload := make([]byte, size)
	payload[2], payload[3] = byte(n>>8), byte(n)
	payload[4], payload[5] = byte(t>>8), byte(t)
	return itemAtom(atom, dataAtom(0, payload))
}

func firstUint16(vs []string) uint16 {
	if len(vs) == 0 {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(vs[0]), 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// freeformAtom builds an iTunes freeform (----) item: mean, name, then a
// UTF-8 data atom.
func freeformAtom(name, value string) []byte {
	return makeBox("----",
		makeFullBox("mean", 0, 0, []byte("com.apple.iTunes")),
		makeFullBox("name", 0, 0, []byte(name)),
		dataAtom(1, []byte(value)))
}

// chplBox renders a Nero chapter list (version 1 with the reserved word,
// the form shipping encoders write and both our demuxer and ffmpeg read).
// Times are 100-nanosecond units; the one-byte count caps the list at 255
// entries and the one-byte title length caps each title at 255 bytes,
// truncated on a rune boundary.
func chplBox(chapters []container.Chapter) []byte {
	if len(chapters) == 0 {
		return nil
	}
	if len(chapters) > 255 {
		chapters = chapters[:255]
	}
	body := []byte{byte(len(chapters))}
	for _, ch := range chapters {
		start := ch.Start.Nanoseconds() / 100
		if start < 0 {
			start = 0
		}
		title := truncateRunes(ch.Title, 255)
		body = append(body, u64(uint64(start))...)
		body = append(body, byte(len(title)))
		body = append(body, title...)
	}
	return makeFullBox("chpl", 1, 0, u32(0), body)
}

// truncateRunes cuts s to at most max bytes without splitting a rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// FreeformPatcher is the file access PatchFreeform needs (os.File).
type FreeformPatcher interface {
	io.ReaderAt
	io.WriterAt
}

// maxPatchScanBytes caps the header scan for PatchFreeform: a moov
// bigger than this is not something the muxer writes (metadata values
// are bounded upstream), so the cap only guards a corrupt size field.
const maxPatchScanBytes = 64 << 20

// patchScanLen resolves how much of the file PatchFreeform must scan:
// the muxer writes ftyp then moov at the head, so their two size fields
// name the exact extent holding every ilst atom, however large the
// metadata grew. A layout this muxer did not write falls back to the
// cap.
func patchScanLen(f FreeformPatcher) int {
	var hdr [8]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil || string(hdr[4:]) != "ftyp" {
		return maxPatchScanBytes
	}
	ftypLen := int64(be32(hdr[:]))
	if _, err := f.ReadAt(hdr[:], ftypLen); err != nil || string(hdr[4:]) != "moov" {
		return maxPatchScanBytes
	}
	n := ftypLen + int64(be32(hdr[:]))
	if n <= 8 || n > maxPatchScanBytes {
		return maxPatchScanBytes
	}
	return int(n)
}

// PatchFreeform replaces the value of the freeform ilst tag key in a
// finished file, in place. placeholder must be the exact value the muxer
// wrote at Begin and value must be the same length, so no byte moves:
// this is how measured ReplayGain values land in a fragmented MP4 after
// the encode, which no tag rewriter can restructure. The written atom
// bytes are matched whole, so a payload that happens to spell the
// placeholder can never redirect the patch.
func PatchFreeform(f FreeformPatcher, key, placeholder, value string) error {
	if len(value) != len(placeholder) {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("mp4: freeform patch %q: value %q does not match placeholder width %d", key, value, len(placeholder)))
	}
	atom := freeformAtom(key, placeholder)
	head := make([]byte, patchScanLen(f))
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "mp4: freeform patch read", err)
	}
	i := bytes.Index(head[:n], atom)
	if i < 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: freeform tag %q not found for patch", key))
	}
	off := int64(i + len(atom) - len(placeholder))
	if _, err := f.WriteAt([]byte(value), off); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: freeform patch write", err)
	}
	return nil
}
