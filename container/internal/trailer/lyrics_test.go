package trailer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/colespringer/waxflow/container/internal/apev2"
)

// lyrics3v2 renders a v2 block: the fields, their six-digit size, the marker.
func lyrics3v2() []byte {
	body := []byte(lyricsBegin + "IND0000210LYR00005hello")
	return append(body, fmt.Sprintf("%06d%s", len(body), lyrics200)...)
}

func lyrics3v1() []byte { return []byte(lyricsBegin + "hello world" + lyricsEnd) }

// TestPeelRecognizesLyrics3 covers both versions where a tagger puts them, in
// front of the ID3v1 tag, and alone at the end of the file.
func TestPeelRecognizesLyrics3(t *testing.T) {
	for name, block := range map[string][]byte{"v2": lyrics3v2(), "v1": lyrics3v1()} {
		t.Run(name, func(t *testing.T) {
			b, want := tagged(block)
			start, kind, ok := Peel(window(t, b), Lyrics3, 1, int64(len(b)))
			if !ok || start != want || kind != Lyrics3 {
				t.Fatalf("Peel = %d, %v, %v; want %d, Lyrics3, true", start, kind, ok, want)
			}
			if start, kind, ok := Peel(window(t, b), APEv2|ID3v1|ID3v2, 1, int64(len(b))); ok {
				t.Fatalf("peeled to %d as %v without being asked for Lyrics3", start, kind)
			}
		})
	}
}

// TestPeelAllReadsTheTagBehindLyrics3 is the stack a tagger writes: the block
// sits between the APEv2 tag and the ID3v1 one, and the tag in front of it is
// the one whose fields surface.
func TestPeelAllReadsTheTagBehindLyrics3(t *testing.T) {
	// The tag renders to 131 bytes, which with the block peeled puts the T of
	// its preamble where the ID3v1 probe looks: the probe order still holds.
	tag := buildTag(t, apev2.Tag{Key: "TITLE",
		Value: "01234567890123456789012345678901234567890123456789012"})
	for name, block := range map[string][]byte{"v2": lyrics3v2(), "v1": lyrics3v1()} {
		t.Run(name, func(t *testing.T) {
			tail := append(append(append([]byte(nil), tag...), block...), id3v1()...)
			b, want := tagged(tail)
			start, tags := PeelAll(window(t, b), APEv2|ID3v1|ID3v2|Lyrics3, 1, int64(len(b)))
			if start != want {
				t.Fatalf("PeelAll left the audio ending at %d, want %d", start, want)
			}
			if got := tags["TITLE"]; len(got) != 1 {
				t.Fatalf("TITLE = %v, want the tag behind the block", got)
			}
		})
	}
}

// TestLyrics3NeedsBothMarkers holds the recognition to its confirmation: a
// size that lands on no LYRICSBEGIN, a size that is not six digits, a block
// that would start below the floor, and a v1 end marker with no begin marker
// in reach are all audio.
func TestLyrics3NeedsBothMarkers(t *testing.T) {
	wrongSize := lyrics3v2()
	copy(wrongSize[len(wrongSize)-15:], "000030")
	notDigits := lyrics3v2()
	copy(notDigits[len(notDigits)-15:], "00003x")
	far := append([]byte(lyricsBegin), bytes.Repeat([]byte{'x'}, lyrics1Max)...)
	far = append(far, lyricsEnd...)
	for name, block := range map[string][]byte{
		"v2 size lands elsewhere": wrongSize,
		"v2 size is not digits":   notDigits,
		"v1 with no begin marker": []byte("hello world" + lyricsEnd),
		"v1 begin out of reach":   far,
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := tagged(block)
			if start, _, ok := Peel(window(t, b), Lyrics3, 1, int64(len(b))); ok {
				t.Fatalf("peeled to %d", start)
			}
		})
	}
	b, at := tagged(lyrics3v2())
	if start, _, ok := Peel(window(t, b), Lyrics3, at+1, int64(len(b))); ok {
		t.Fatalf("peeled to %d, below a floor of %d", start, at+1)
	}
}

// TestRefusedNamesAFooterBelowTheFloor: a footer whose tag would start below
// the floor is left in place by the peel and reported, with where it points.
func TestRefusedNamesAFooterBelowTheFloor(t *testing.T) {
	foot := apeFooterOnly(t)
	foot = foot[len(foot)-apev2.FooterLen:]
	binary.LittleEndian.PutUint32(foot[12:], 300) // items and footer: 300 bytes back
	b, _ := tagged(foot)
	end := int64(len(b))
	if start, _, ok := Peel(window(t, b), APEv2, end-200, end); ok {
		t.Fatalf("peeled to %d, below the floor", start)
	}
	if at, ok := Refused(window(t, b), end-200, end); !ok || at != end-300 {
		t.Fatalf("Refused = %d, %v; want %d, true", at, ok, end-300)
	}
	if at, ok := Refused(window(t, b), end-300, end); ok {
		t.Fatalf("Refused = %d for a tag that starts on the floor", at)
	}
	if at, ok := Refused(window(t, audio), 1, int64(len(audio))); ok {
		t.Fatalf("Refused = %d with no footer there", at)
	}
}

// TestBehindFindsTheTagJunkHides: bytes no peel recognizes behind a tag hide
// it from the peel, and the search reads it, in either shape, nearest the end
// first.
func TestBehindFindsTheTagJunkHides(t *testing.T) {
	junk := bytes.Repeat([]byte{0xAA}, 300)
	inner := buildTag(t, apev2.Tag{Key: "TITLE", Value: "inner"})
	outer := buildTag(t, apev2.Tag{Key: "TITLE", Value: "outer"})
	for name, c := range map[string]struct {
		tail  []byte
		title string
		at    int // the found tag's offset in tail
	}{
		"with a header":   {append(append([]byte(nil), outer...), junk...), "outer", 0},
		"footer only":     {append(apeFooterOnly(t), junk...), "footer only", 0},
		"nearest the end": {append(append(append(append([]byte(nil), inner...), junk...), outer...), junk...), "outer", len(inner) + len(junk)},
	} {
		t.Run(name, func(t *testing.T) {
			b, audioEnd := tagged(c.tail)
			w := window(t, b)
			if end, tags := PeelAll(w, APEv2|ID3v1|ID3v2|Lyrics3, 1, int64(len(b))); end != int64(len(b)) || tags != nil {
				t.Fatalf("PeelAll = %d, %v: the junk is not a trailer", end, tags)
			}
			tags, start, ok := Behind(w, 1, int64(len(b)))
			if !ok || start != audioEnd+int64(c.at) {
				t.Fatalf("Behind = %d, %v; want %d, true", start, ok, audioEnd+int64(c.at))
			}
			if got := tags["TITLE"]; len(got) != 1 || got[0] != c.title {
				t.Fatalf("TITLE = %v, want [%s]", got, c.title)
			}
		})
	}
}

// TestBehindNeedsATagThatHoldsTogether: with no footer position to trust, a
// preamble alone is nothing. Each case breaks one thing the search checks.
func TestBehindNeedsATagThatHoldsTogether(t *testing.T) {
	junk := bytes.Repeat([]byte{0xAA}, 300)
	full := buildTag(t, apev2.Tag{Key: "TITLE", Value: "x"})
	edit := func(f func(tag []byte)) []byte {
		tag := append([]byte(nil), full...)
		f(tag)
		return append(tag, junk...)
	}
	foot := len(full) - apev2.FooterLen
	for name, tail := range map[string][]byte{
		"an unknown version":       edit(func(b []byte) { binary.LittleEndian.PutUint32(b[foot+8:], 3000) }),
		"a count the items lack":   edit(func(b []byte) { binary.LittleEndian.PutUint32(b[foot+16:], 2) }),
		"a size the items overrun": edit(func(b []byte) { binary.LittleEndian.PutUint32(b[foot+12:], uint32(len(full)-apev2.FooterLen-1)) }),
		"a header that disagrees":  edit(func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 2) }),
		"a header that is missing": edit(func(b []byte) { copy(b, "notatag!") }),
		"a header and no footer":   append(append([]byte(nil), full[:apev2.FooterLen]...), junk...),
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := tagged(tail)
			if tags, start, ok := Behind(window(t, b), 1, int64(len(b))); ok {
				t.Fatalf("Behind = %v at %d", tags, start)
			}
		})
	}
	// A tag that holds together is still refused below the caller's floor.
	b, at := tagged(append(append([]byte(nil), full...), junk...))
	if _, start, ok := Behind(window(t, b), at+1, int64(len(b))); ok {
		t.Fatalf("Behind found a tag at %d, below %d", start, at+1)
	}
}

// overlappingTag is 56 bytes that pass for a tag with a header: a header
// record, and a footer record that starts in its reserved bytes and states a
// size of 24, under a footer's own length. Each record names the other.
func overlappingTag() []byte {
	b := make([]byte, 56)
	copy(b, "APETAGEX")
	binary.LittleEndian.PutUint32(b[20:], 1<<29|1<<31)
	copy(b[24:], "APETAGEX")
	binary.LittleEndian.PutUint32(b[32:], 2000)
	binary.LittleEndian.PutUint32(b[36:], 24)
	binary.LittleEndian.PutUint32(b[40:], 1)
	binary.LittleEndian.PutUint32(b[44:], 1<<31)
	copy(b[12:20], b[36:44])
	return b
}

// TestAFooterSmallerThanItselfIsNoTag: a size field under the footer's own 32
// bytes describes no tag, so neither the search nor the peel takes one, at the
// end of the file or behind bytes that are no tag. The search once sliced the
// items of such a block at a negative length.
func TestAFooterSmallerThanItselfIsNoTag(t *testing.T) {
	for name, tail := range map[string][]byte{
		"at the end":  overlappingTag(),
		"behind junk": append(overlappingTag(), bytes.Repeat([]byte{0xAA}, 10)...),
	} {
		b, _ := tagged(tail)
		if tags, start, ok := Behind(window(t, b), 1, int64(len(b))); ok {
			t.Errorf("%s: Behind = %v at %d", name, tags, start)
		}
		if end, tags := PeelAll(window(t, b), APEv2|ID3v1|ID3v2|Lyrics3, 1, int64(len(b))); end != int64(len(b)) || tags != nil {
			t.Errorf("%s: PeelAll = %d, %v", name, end, tags)
		}
	}
}

// TestBehindIsBounded: the search reads the 64 KiB below its end and examines
// a bounded number of preambles, so neither a tag further back nor a run of
// preambles is chased.
func TestBehindIsBounded(t *testing.T) {
	full := buildTag(t, apev2.Tag{Key: "TITLE", Value: "x"})
	b, _ := tagged(append(append([]byte(nil), full...), bytes.Repeat([]byte{0xAA}, behindWindow)...))
	if _, start, ok := Behind(window(t, b), 1, int64(len(b))); ok {
		t.Fatalf("Behind found a tag at %d, past its window", start)
	}
	run := bytes.Repeat([]byte("APETAGEX"), maxBehindTries+1)
	b, _ = tagged(append(append(append([]byte(nil), full...), run...), 0xAA))
	if _, start, ok := Behind(window(t, b), 1, int64(len(b))); ok {
		t.Fatalf("Behind found a tag at %d, past its candidates", start)
	}
}
