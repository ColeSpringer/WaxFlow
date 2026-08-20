package trailer

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/internal/srcwin"
)

// audio stands in for the stream a trailer is bolted onto. It ends in no NUL
// and spells none of the magics at the offsets the probes look, so anything
// peeled off a file built from it came from the trailer.
var audio = bytes.Repeat([]byte("audio!!!"), 64)

func window(t testing.TB, b []byte) *srcwin.Window {
	t.Helper()
	w := srcwin.New(container.BytesSource(b), int64(len(b)), "trailer test")
	return &w
}

// tagged returns audio with tail appended, and the offset where tail starts.
func tagged(tail []byte) ([]byte, int64) {
	b := append(append([]byte(nil), audio...), tail...)
	return b, int64(len(audio))
}

// apeFooterOnly renders the shape the reference tagger writes: items and a
// footer, no header.
func apeFooterOnly() []byte {
	full := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "footer only"}})
	body := full[apev2.FooterLen : len(full)-apev2.FooterLen]
	foot := append([]byte(nil), full[len(full)-apev2.FooterLen:]...)
	foot[23] &^= 0x80 // the has-header flag, top bit of the little-endian flags
	return append(append([]byte(nil), body...), foot...)
}

func id3v1() []byte {
	tag := make([]byte, id3v1Len)
	copy(tag, "TAG")
	copy(tag[3:], "some title")
	return tag
}

// appendedID3v2 renders a tag written after the audio: header, body, and the
// footer that is the only thing making it findable from behind.
func appendedID3v2(body int) []byte {
	size := []byte{byte(body >> 21 & 0x7f), byte(body >> 14 & 0x7f), byte(body >> 7 & 0x7f), byte(body & 0x7f)}
	out := append([]byte{'I', 'D', '3', 4, 0, 0x10}, size...)
	out = append(out, make([]byte, body)...)
	out = append(out, '3', 'D', 'I', 4, 0, 0x10)
	return append(out, size...)
}

// TestPeelRecognizesEveryTrailer covers each shape a tagger leaves behind.
func TestPeelRecognizesEveryTrailer(t *testing.T) {
	cases := map[string]struct {
		want Kind
		tail []byte
	}{
		"apev2, footer only": {APEv2, apeFooterOnly()},
		"apev2 with header":  {APEv2, apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "x"}})},
		"id3v1":              {ID3v1, id3v1()},
		"appended id3v2":     {ID3v2, appendedID3v2(300)},
		"nul padding":        {Padding, make([]byte, 300)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b, want := tagged(c.tail)
			start, kind, ok := Peel(window(t, b), c.want, 1, int64(len(b)))
			if !ok {
				t.Fatalf("recognized nothing in a %d-byte trailer", len(c.tail))
			}
			if start != want || kind != c.want {
				t.Fatalf("Peel = %d, %v; want %d, %v", start, kind, want, c.want)
			}
		})
	}
}

// TestNoTrailerIsNotPeeled is the other half: audio alone must survive every
// probe, or the recognitions above prove nothing.
func TestNoTrailerIsNotPeeled(t *testing.T) {
	if start, kind, ok := Peel(window(t, audio), APEv2|ID3v1|ID3v2|Padding, 1, int64(len(audio))); ok {
		t.Fatalf("peeled %d bytes of audio as a %v trailer", int64(len(audio))-start, kind)
	}
}

// TestPaddingIsOptIn pins the difference between the callers: flacn re-checks
// the frame checksum after every peel and can afford a run of NULs, which has
// no magic to recognize; a caller that peels once cannot tell it from audio
// that happens to end in zeros, so it must not be handed one it did not ask
// for.
func TestPaddingIsOptIn(t *testing.T) {
	b, want := tagged(make([]byte, 300))
	if start, kind, ok := Peel(window(t, b), APEv2|ID3v1|ID3v2, 1, int64(len(b))); ok {
		t.Fatalf("peeled %d bytes as %v without being asked for padding", int64(len(b))-start, kind)
	}
	if start, _, ok := Peel(window(t, b), Padding, 1, int64(len(b))); !ok || start != want {
		t.Fatalf("Peel = %d, %v; want %d, true", start, ok, want)
	}
}

// TestTagThatSpellsID3v1 pins the probe order. "APETAGEX" spells TAG at bytes
// three to five, so an APEv2 tag of exactly 131 bytes puts that T where the
// ID3v1 probe looks: probing ID3v1 first peels 128 bytes out of the middle of
// the tag and leaves three bytes of it standing where the audio should end.
// The tag is sized to that length deliberately and the size is asserted, so a
// value edited later cannot quietly stop covering the case.
func TestTagThatSpellsID3v1(t *testing.T) {
	tag := apev2.Build([]apev2.Tag{{Key: "TITLE",
		Value: "01234567890123456789012345678901234567890123456789012"}})
	if len(tag) != 131 {
		t.Fatalf("the fixture tag renders to %d bytes; this test needs exactly 131", len(tag))
	}
	b, want := tagged(tag)
	start, kind, ok := Peel(window(t, b), APEv2|ID3v1, 1, int64(len(b)))
	if !ok || start != want || kind != APEv2 {
		t.Fatalf("Peel = %d, %v, %v; want %d, APEv2, true", start, kind, ok, want)
	}
}

// TestFloorBoundsThePeel checks the caller's floor: a peel may land on it but
// never below it. It is what keeps a declared length from being an instruction
// to drop audio.
func TestFloorBoundsThePeel(t *testing.T) {
	b, want := tagged(id3v1())
	if start, _, ok := Peel(window(t, b), ID3v1, want+1, int64(len(b))); ok {
		t.Fatalf("peeled to %d, below a floor of %d", start, want+1)
	}
	if start, _, ok := Peel(window(t, b), ID3v1, want, int64(len(b))); !ok || start != want {
		t.Fatalf("Peel = %d, %v; want %d, true (the floor itself is reachable)", start, ok, want)
	}
}

// TestAPEv2HeaderMustBeThere is the confirmation a declared length needs: a
// footer claiming a header is claiming 32 bytes more than its items, and if
// they are not the header, they are audio.
func TestAPEv2HeaderMustBeThere(t *testing.T) {
	full := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "x"}})
	b, _ := tagged(full[apev2.FooterLen:]) // the header dropped, the claim kept
	if start, _, ok := Peel(window(t, b), APEv2, 1, int64(len(b))); ok {
		t.Fatalf("peeled to %d on a footer whose header is not there", start)
	}
}

// TestAPEv2HeaderIsNotAFooter covers a file that ends where a tag begins: the
// 32 bytes are a header, so the tag starts here rather than ending here and
// there is nothing behind it to peel.
func TestAPEv2HeaderIsNotAFooter(t *testing.T) {
	full := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "x"}})
	b, _ := tagged(full[:apev2.FooterLen])
	if start, _, ok := Peel(window(t, b), APEv2, 1, int64(len(b))); ok {
		t.Fatalf("peeled to %d on an APEv2 header read as a footer", start)
	}
}

// TestPeelAllStacksAndReadsTheOutermostTag walks a file a tagger wrote twice
// and then bolted an ID3v1 onto. The tags come from the outermost APEv2 block,
// the one written last.
func TestPeelAllStacksAndReadsTheOutermostTag(t *testing.T) {
	inner := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "inner"}})
	outer := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "outer"}})
	b, want := tagged(append(append(append([]byte(nil), inner...), outer...), id3v1()...))

	start, tags := PeelAll(window(t, b), APEv2|ID3v1, 1, int64(len(b)))
	if start != want {
		t.Fatalf("PeelAll left the audio ending at %d, want %d", start, want)
	}
	if got := tags["TITLE"]; len(got) != 1 || got[0] != "outer" {
		t.Fatalf("TITLE = %v, want [outer]", got)
	}
}

// TestPeelAllStopsAtMax bounds the walk: stacked tags are a real shape, an
// unbounded run of them is a crafted file.
func TestPeelAllStopsAtMax(t *testing.T) {
	tail := bytes.Repeat(id3v1(), Max+2)
	b, audioEnd := tagged(tail)
	start, _ := PeelAll(window(t, b), ID3v1, 1, int64(len(b)))
	if want := audioEnd + 2*id3v1Len; start != want {
		t.Fatalf("PeelAll stopped at %d, want %d (%d tags of %d bytes)", start, want, Max, id3v1Len)
	}
}

// TestPaddingPeelIsBounded caps one peel's backward scan, so a file that is
// mostly zeros cannot pull itself into the read window.
func TestPaddingPeelIsBounded(t *testing.T) {
	b, _ := tagged(make([]byte, maxPadding+100))
	start, _, ok := Peel(window(t, b), Padding, 1, int64(len(b)))
	if want := int64(len(b)) - maxPadding; !ok || start != want {
		t.Fatalf("Peel = %d, %v; want %d, true", start, ok, want)
	}
}

// TestNonSyncsafeID3v2FooterIsNotATag guards the size field: four bytes with
// the high bit set are not an ID3v2 length, and reading them as one would drop
// audio the tag never covered.
func TestNonSyncsafeID3v2FooterIsNotATag(t *testing.T) {
	tail := appendedID3v2(300)
	tail[len(tail)-2] |= 0x80
	b, _ := tagged(tail)
	if start, _, ok := Peel(window(t, b), ID3v2, 1, int64(len(b))); ok {
		t.Fatalf("peeled to %d on a footer whose size is not syncsafe", start)
	}
}

// TestID3v2FooterMustMatchItsHeader is the confirmation the shape needs.
// "3DI" plus four bytes under 0x80 is 28 bits of recognition, and the length
// behind it is an instruction to drop that many bytes of audio, so the header
// the footer mirrors has to be at the other end of the extent it declares.
// Here there is no tag at all: the audio's last ten bytes merely read like a
// footer claiming 148 bytes.
func TestID3v2FooterMustMatchItsHeader(t *testing.T) {
	b, _ := tagged([]byte{'3', 'D', 'I', 4, 0, 0x10, 0, 0, 1, 0})
	if start, _, ok := Peel(window(t, b), ID3v2, 1, int64(len(b))); ok {
		t.Fatalf("peeled to %d, dropping %d bytes of audio behind a footer with no tag",
			start, int64(len(b))-start)
	}
}

// TestID3v2ThatSpellsID3v1 pins the probe order for the other collision.
// ID3v1 is recognized by three bytes at a fixed offset, so an appended ID3v2
// tag long enough to reach that offset and holding "TAG" there (a text frame,
// a picture, a stacked-tag remnant) is peeled 128 bytes out of the middle
// unless the stronger recognition goes first, leaving the tag's own header
// standing where the audio should end.
func TestID3v2ThatSpellsID3v1(t *testing.T) {
	tail := appendedID3v2(300)
	copy(tail[len(tail)-id3v1Len:], "TAG")
	b, want := tagged(tail)
	start, kind, ok := Peel(window(t, b), ID3v1|ID3v2, 1, int64(len(b)))
	if !ok || start != want || kind != ID3v2 {
		t.Fatalf("Peel = %d, %v, %v; want %d, ID3v2, true", start, kind, ok, want)
	}
}
