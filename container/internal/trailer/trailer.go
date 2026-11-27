// Package trailer peels the non-audio structures taggers bolt onto the end of
// a file: an APEv2 tag, an ID3v1 tag, an ID3v2 tag appended after the audio, a
// Lyrics3 block, and NUL padding, stacked in any order. Every demuxer of a
// stream that taggers append to needs the audio's real end before it walks it,
// and they all need the same probe order for the same reason, so the
// recognition lives here once.
//
// The mpa demuxer bounds its frame walk with a peel and also keeps a forward
// test of its own for the bytes its frames stop at: that one asks whether
// everything after an offset is baggage, a different question with different
// answers (a truncated tag is still a tag).
package trailer

import (
	"bytes"
	"encoding/binary"

	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/srcwin"
)

// Kind names the structures, one bit each. Callers pass the set they
// recognize; Peel reports the one it found.
type Kind uint8

const (
	// APEv2 is the key/value block WavPack and Monkey's Audio carry.
	APEv2 Kind = 1 << iota
	// ID3v1 is the fixed 128-byte block starting "TAG".
	ID3v1
	// ID3v2 is a tag written after the audio. Only one carrying a footer can
	// be found from behind, which is exactly the case that puts one here.
	ID3v2
	// Padding is a run of NUL bytes. It is the one shape with no magic to
	// recognize, so only a caller that can confirm the peel afterwards should
	// ask for it; to anyone else it is indistinguishable from audio that ends
	// in zeros.
	Padding
	// Lyrics3 is a lyrics block of either version, which a tagger puts in
	// front of the ID3v1 tag. Nothing reads it: it is recognized so the tags
	// stacked in front of it are found.
	Lyrics3
)

const (
	// Max bounds peel attempts, which is trailers when every peel is a tag
	// (real files stack two, APEv2 then ID3v1 being the classic pair) but is
	// 64 KiB steps for a caller peeling NUL padding.
	Max = 8
	// maxPadding bounds one padding peel's backward scan, so a file that is
	// mostly zeros cannot pull itself into the read window.
	maxPadding = 64 << 10
	// id3v1Len is the fixed size of a trailing ID3v1 tag.
	id3v1Len = 128
	// behindWindow bounds Behind's search for a tag with junk behind it,
	// maxBehind the item bytes its candidates may walk between them, and
	// maxBehindTries the footers it examines.
	behindWindow   = 64 << 10
	maxBehind      = 16 << 20
	maxBehindTries = 64
)

// The Lyrics3 markers. A v2 block ends in a six-digit size and LYRICS200; a v1
// block ends in LYRICSEND, at most lyrics1Max bytes behind its LYRICSBEGIN.
const (
	lyricsBegin = "LYRICSBEGIN"
	lyricsEnd   = "LYRICSEND"
	lyrics200   = "LYRICS200"
	lyrics1Max  = 5100 + len(lyricsBegin)
)

// Peel recognizes the one trailer of a wanted kind ending at end and returns
// where it starts, which is where the audio before it ends. end must be at or
// below the window's data end. Recognizing nothing returns end unchanged, so a
// caller that drops the ok stays put rather than truncating to zero.
//
// floor is the lowest offset a peel may leave behind: a declared length is
// otherwise an unverified instruction to drop audio, and each caller knows
// something different about how much has to survive.
//
// The probe order is not a preference: strongest recognition first, because
// ID3v1 is three bytes at a fixed offset and every other shape here is long
// enough to spell them by accident. A Lyrics3 block is two markers, a stated
// distance apart in v2 and within a bounded reach in v1, the strongest of
// all. An APEv2 tag of exactly 131 bytes puts the
// T of "APETAGEX" where the ID3v1 probe looks, and any appended ID3v2 tag past
// 128 bytes can hold "TAG" there in a text frame or a picture. Peeling 128
// bytes out of the middle of either leaves a remnant no later probe
// recognizes, and the frames before it are dropped as trailing garbage.
// Stacked tags put ID3v1 last, where neither of the stronger probes finds
// anything, so the order costs the real case nothing.
func Peel(w *srcwin.Window, want Kind, floor, end int64) (int64, Kind, bool) {
	floor = max(floor, 0)
	// One read holds every fixed probe: each footer and marker ends at end,
	// within an ID3v1 tag's length of it.
	if at := max(end-id3v1Len, 0); at < end {
		w.Peek(at, int(end-at))
	}
	if want&Lyrics3 != 0 {
		if start, ok := peelLyrics3(w, floor, end); ok {
			return start, Lyrics3, true
		}
	}
	if want&APEv2 != 0 {
		if start, ok := peelAPEv2(w, floor, end); ok {
			return start, APEv2, true
		}
	}
	if want&ID3v2 != 0 {
		if start, ok := peelID3v2(w, floor, end); ok {
			return start, ID3v2, true
		}
	}
	if want&ID3v1 != 0 {
		if start, ok := peelID3v1(w, floor, end); ok {
			return start, ID3v1, true
		}
	}
	if want&Padding != 0 {
		if start, ok := peelPadding(w, floor, end); ok {
			return start, Padding, true
		}
	}
	return end, 0, false
}

// PeelAll repeats Peel from end until nothing is recognized or Max peels have
// come off, and returns where the audio ends. Tags come from the outermost
// APEv2 block holding any: a block whose items are all binary (a bare cover
// picture) parses to nothing, and the walk keeps looking behind it rather than
// calling the file untagged. Callers that do not surface tags should use Peel
// instead and never pay for the read.
func PeelAll(w *srcwin.Window, want Kind, floor, end int64) (int64, map[string][]string) {
	var tags map[string][]string
	for range Max {
		start, kind, ok := Peel(w, want, floor, end)
		if !ok {
			break
		}
		if kind == APEv2 && tags == nil {
			tags = apev2.Parse(w.Peek(start, int(end-start)))
		}
		end = start
	}
	return end, tags
}

// Refused reports an APEv2 footer ending at end that Peel left in place
// because its tag would start below floor, and where that is. Such a footer
// is no tag: obeying it would cut the stream inside what the caller knows has
// to survive.
func Refused(w *srcwin.Window, floor, end int64) (int64, bool) {
	e := end - apev2.FooterLen
	if e < max(floor, 0) {
		return 0, false
	}
	n, _ := apev2.Size(w.Peek(e, apev2.FooterLen))
	if n <= 0 || end-n >= floor {
		return 0, false
	}
	return end - n, true
}

// Behind finds the APEv2 tag nearest hi that ends in (lo, hi] with bytes
// behind it no peel recognizes, searching the 64 KiB below hi, and returns its
// tags and where it starts. The stream's own structure still says where the
// audio ends: this only reads a tag the junk hid.
//
// A tag carries no checksum and the search has no footer position to trust,
// so a candidate counts only when it holds together: version 1000 or 2000, a
// footer record, a start at or above lo, the header its flags announce
// agreeing on size and count, and items that walk to exactly the declared
// length and count.
func Behind(w *srcwin.Window, lo, hi int64) (map[string][]string, int64, bool) {
	lo = max(lo, 0)
	from := max(lo, hi-behindWindow)
	if hi-from < apev2.FooterLen {
		return nil, 0, false
	}
	win := w.Peek(from, int(hi-from))
	if int64(len(win)) != hi-from {
		return nil, 0, false
	}
	// read serves the search window's own bytes without moving w off them.
	read := func(off, n int64) []byte {
		if off >= from && off+n <= hi {
			return win[off-from : off-from+n]
		}
		return w.Peek(off, int(n))
	}
	budget, tries := int64(maxBehind), 0
	for next := len(win); tries < maxBehindTries; tries++ {
		i := bytes.LastIndex(win[:next], []byte("APETAGEX"))
		if i < 0 {
			break
		}
		next = i
		if i+apev2.FooterLen > len(win) {
			continue
		}
		foot := win[i : i+apev2.FooterLen]
		end := from + int64(i) + apev2.FooterLen
		n, hasHeader := apev2.Size(foot)
		version, count := binary.LittleEndian.Uint32(foot[8:]), binary.LittleEndian.Uint32(foot[16:])
		start := end - n
		if n <= 0 || (version != 1000 && version != 2000) || start < lo {
			continue
		}
		items := n - apev2.FooterLen
		if hasHeader {
			// Size and count, which the two records state alike.
			head := read(start, apev2.FooterLen)
			if !apev2.StartsTag(head) || !bytes.Equal(head[12:20], foot[12:20]) {
				continue
			}
			items -= apev2.FooterLen
		}
		if items < 0 {
			continue
		}
		if budget -= items; budget < 0 {
			break
		}
		tag := read(start, n)
		if int64(len(tag)) != n || !wholeItems(tag[n-apev2.FooterLen-items:n-apev2.FooterLen], count) {
			continue
		}
		return apev2.Parse(tag), start, true
	}
	return nil, 0, false
}

// wholeItems reports whether count items fill b exactly: each a value length,
// flags, a NUL-terminated key and the value.
func wholeItems(b []byte, count uint32) bool {
	for range count {
		if len(b) < 8 {
			return false
		}
		size := binary.LittleEndian.Uint32(b)
		k := bytes.IndexByte(b[8:], 0)
		if k < 0 || uint64(size) > uint64(len(b)-8-k-1) {
			return false
		}
		b = b[8+k+1+int(size):]
	}
	return len(b) == 0
}

// peelAPEv2 recognizes a tag whose footer ends at end.
func peelAPEv2(w *srcwin.Window, floor, end int64) (int64, bool) {
	e := end - apev2.FooterLen
	if e < floor {
		return 0, false
	}
	n, hasHeader := apev2.Size(w.Peek(e, apev2.FooterLen))
	if n <= 0 || end-n < floor {
		return 0, false
	}
	// A footer that claims a header has to have one: the claim is worth 32
	// bytes of extent, and if they are not the header, they are audio.
	if hasHeader && !apev2.StartsTag(w.Peek(end-n, apev2.FooterLen)) {
		return 0, false
	}
	return end - n, true
}

// peelLyrics3 recognizes a block ending at end: a v2 one by its size field
// and LYRICS200, confirmed by LYRICSBEGIN where the size lands, or a v1 one by
// LYRICSEND and the first LYRICSBEGIN in the bytes such a block can hold.
func peelLyrics3(w *srcwin.Window, floor, end int64) (int64, bool) {
	const foot2 = 6 + len(lyrics200)
	if e := end - int64(foot2); e >= floor {
		if b := w.Peek(e, foot2); len(b) == foot2 && string(b[6:]) == lyrics200 {
			n, digits := int64(0), true
			for _, c := range b[:6] {
				digits = digits && c >= '0' && c <= '9'
				n = n*10 + int64(c-'0')
			}
			if start := e - n; digits && n >= int64(len(lyricsBegin)) && start >= floor &&
				string(w.Peek(start, len(lyricsBegin))) == lyricsBegin {
				return start, true
			}
		}
	}
	e := end - int64(len(lyricsEnd))
	if e < floor || string(w.Peek(e, len(lyricsEnd))) != lyricsEnd {
		return 0, false
	}
	lo := max(floor, e-int64(lyrics1Max))
	i := bytes.Index(w.Peek(lo, int(e-lo)), []byte(lyricsBegin))
	if i < 0 {
		return 0, false
	}
	return lo + int64(i), true
}

func peelID3v1(w *srcwin.Window, floor, end int64) (int64, bool) {
	e := end - id3v1Len
	if e < floor || string(w.Peek(e, 3)) != "TAG" {
		return 0, false
	}
	return e, true
}

// peelID3v2 recognizes an appended tag: header, syncsafe-sized body, footer.
func peelID3v2(w *srcwin.Window, floor, end int64) (int64, bool) {
	e := end - id3.HeaderLen
	if e < floor {
		return 0, false
	}
	n := id3.SizeFromFooter(w.Peek(e, id3.HeaderLen))
	if n <= 0 || end-n < floor {
		return 0, false
	}
	// The footer alone is three magic bytes and four that merely lack a high
	// bit, so the extent it declares is confirmed against the header it
	// mirrors: without that, audio whose last ten bytes read like a footer
	// takes the frames behind it with them.
	if id3.Size(w.Peek(end-n, id3.HeaderLen)) != n {
		return 0, false
	}
	return end - n, true
}

func peelPadding(w *srcwin.Window, floor, end int64) (int64, bool) {
	n := min(int64(maxPadding), end-floor)
	if n <= 0 {
		return 0, false
	}
	tail := w.Peek(end-n, int(n))
	if int64(len(tail)) != n {
		return 0, false
	}
	i := n
	for i > 0 && tail[i-1] == 0 {
		i--
	}
	if i == n {
		return 0, false
	}
	return end - (n - i), true
}
