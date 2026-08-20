// Package trailer peels the non-audio structures taggers bolt onto the end of
// a file: an APEv2 tag, an ID3v1 tag, an ID3v2 tag appended after the audio,
// and NUL padding, stacked in any order. The apen, wv, and flacn demuxers all
// need the audio's real end before they walk it, and they all need the same
// probe order for the same reason, so the recognition lives here once.
//
// The mpa demuxer deliberately stays off this package. It does not peel from
// behind: it walks forward from the offset where frames stopped parsing and
// asks whether everything after it is baggage, which is a different question
// with different answers (a truncated tag is still a tag) and its own list of
// shapes.
package trailer

import (
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
// enough to spell them by accident. An APEv2 tag of exactly 131 bytes puts the
// T of "APETAGEX" where the ID3v1 probe looks, and any appended ID3v2 tag past
// 128 bytes can hold "TAG" there in a text frame or a picture. Peeling 128
// bytes out of the middle of either leaves a remnant no later probe
// recognizes, and the frames before it are dropped as trailing garbage.
// Stacked tags put ID3v1 last, where neither of the stronger probes finds
// anything, so the order costs the real case nothing.
func Peel(w *srcwin.Window, want Kind, floor, end int64) (int64, Kind, bool) {
	floor = max(floor, 0)
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
			tags = apev2.Parse(w.BytesAt(start, int(end-start)))
		}
		end = start
	}
	return end, tags
}

// peelAPEv2 recognizes a tag whose footer ends at end.
func peelAPEv2(w *srcwin.Window, floor, end int64) (int64, bool) {
	e := end - apev2.FooterLen
	if e < floor {
		return 0, false
	}
	n, hasHeader := apev2.Size(w.BytesAt(e, apev2.FooterLen))
	if n <= 0 || end-n < floor {
		return 0, false
	}
	// A footer that claims a header has to have one: the claim is worth 32
	// bytes of extent, and if they are not the header, they are audio.
	if hasHeader && !apev2.StartsTag(w.BytesAt(end-n, apev2.FooterLen)) {
		return 0, false
	}
	return end - n, true
}

func peelID3v1(w *srcwin.Window, floor, end int64) (int64, bool) {
	e := end - id3v1Len
	if e < floor || string(w.BytesAt(e, 3)) != "TAG" {
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
	n := id3.SizeFromFooter(w.BytesAt(e, id3.HeaderLen))
	if n <= 0 || end-n < floor {
		return 0, false
	}
	// The footer alone is three magic bytes and four that merely lack a high
	// bit, so the extent it declares is confirmed against the header it
	// mirrors: without that, audio whose last ten bytes read like a footer
	// takes the frames behind it with them.
	if id3.Size(w.BytesAt(end-n, id3.HeaderLen)) != n {
		return 0, false
	}
	return end - n, true
}

func peelPadding(w *srcwin.Window, floor, end int64) (int64, bool) {
	n := min(int64(maxPadding), end-floor)
	if n <= 0 {
		return 0, false
	}
	tail := w.BytesAt(end-n, int(n))
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
