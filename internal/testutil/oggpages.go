package testutil

import (
	"encoding/binary"
	"testing"
)

// Ogg page surgery for tests that need a stream starting somewhere other than
// granule 0. Both helpers take one logical stream whose audio packets each end
// on the page they start on, which is what this tree's Ogg muxer writes, and
// recompute every checksum they touch, so the result is a stream any reader
// accepts.

// oggPageData is one parsed page.
type oggPageData struct {
	flags   byte
	granule int64
	serial  uint32
	seq     uint32
	lacing  []byte
	body    []byte
}

// completes counts the packets that end on the page.
func (p oggPageData) completes() int {
	n := 0
	for _, l := range p.lacing {
		if l < 255 {
			n++
		}
	}
	return n
}

func parseOggPages(t testing.TB, raw []byte) []oggPageData {
	t.Helper()
	var pages []oggPageData
	for off := 0; off < len(raw); {
		if off+27 > len(raw) || string(raw[off:off+4]) != "OggS" {
			t.Fatalf("no Ogg page at offset %d", off)
		}
		lacing := off + 27 + int(raw[off+26])
		if lacing > len(raw) {
			t.Fatalf("the page at %d is truncated", off)
		}
		size := 0
		for _, l := range raw[off+27 : lacing] {
			size += int(l)
		}
		if lacing+size > len(raw) {
			t.Fatalf("the page at %d runs past the stream", off)
		}
		p := oggPageData{
			flags:   raw[off+5],
			granule: int64(binary.LittleEndian.Uint64(raw[off+6:])),
			serial:  binary.LittleEndian.Uint32(raw[off+14:]),
			seq:     binary.LittleEndian.Uint32(raw[off+18:]),
			lacing:  raw[off+27 : lacing],
			body:    raw[lacing : lacing+size],
		}
		if len(pages) > 0 && p.serial != pages[0].serial {
			t.Fatalf("the page at %d belongs to a second logical stream", off)
		}
		pages = append(pages, p)
		off = lacing + size
	}
	return pages
}

func appendOggPage(out []byte, p oggPageData) []byte {
	start := len(out)
	out = append(out, "OggS"...)
	out = append(out, 0, p.flags)
	out = binary.LittleEndian.AppendUint64(out, uint64(p.granule))
	out = binary.LittleEndian.AppendUint32(out, p.serial)
	out = binary.LittleEndian.AppendUint32(out, p.seq)
	out = append(out, 0, 0, 0, 0, byte(len(p.lacing)))
	out = append(out, p.lacing...)
	out = append(out, p.body...)
	binary.LittleEndian.PutUint32(out[start+22:], oggCRC(out[start:]))
	return out
}

// oggAudioStart is the index of the first page past the stream's header
// packets, given how many there are.
func oggAudioStart(t testing.TB, pages []oggPageData, headers int) int {
	t.Helper()
	done := 0
	for i, p := range pages {
		if done >= headers {
			return i
		}
		if done += p.completes(); done > headers {
			t.Fatalf("page %d ends header and audio packets both", i)
		}
	}
	t.Fatalf("the stream has no audio pages past its %d header packets", headers)
	return 0
}

// OggFirstAudioPage is the index of the page raw's first audio packet
// completes on, the page whose granule says where the stream starts, given
// how many header packets come before it.
func OggFirstAudioPage(t testing.TB, raw []byte, headers int) int {
	t.Helper()
	done := 0
	for i, p := range parseOggPages(t, raw) {
		if done += p.completes(); done > headers {
			return i
		}
	}
	t.Fatalf("no audio packet completes past the %d header packets", headers)
	return 0
}

// OggPage is one page of a logical Ogg stream.
type OggPage struct {
	Flags   byte
	Granule int64
	Lacing  []byte
	Body    []byte
}

// OggPages parses raw, one logical Ogg stream, into its pages.
func OggPages(t testing.TB, raw []byte) []OggPage {
	t.Helper()
	var out []OggPage
	for _, p := range parseOggPages(t, raw) {
		out = append(out, OggPage{Flags: p.flags, Granule: p.granule, Lacing: p.lacing, Body: p.body})
	}
	return out
}

// OggGranule is the granule position of raw's page'th page (from 0, header
// pages included).
func OggGranule(t testing.TB, raw []byte, page int) int64 {
	t.Helper()
	pages := parseOggPages(t, raw)
	if page >= len(pages) {
		t.Fatalf("the stream has %d pages, not %d", len(pages), page+1)
	}
	return pages[page].granule
}

// DamageOggPage returns raw with one byte of its page'th page's body flipped
// and the checksum left as it was, so a reader finds the page damaged.
func DamageOggPage(t testing.TB, raw []byte, page int) []byte {
	t.Helper()
	pages := parseOggPages(t, raw)
	if page >= len(pages) || len(pages[page].body) == 0 {
		t.Fatalf("the stream has no page %d with a body", page)
	}
	out := append([]byte(nil), raw...)
	off := 0
	for _, p := range pages[:page] {
		off += 27 + len(p.lacing) + len(p.body)
	}
	p := pages[page]
	out[off+27+len(p.lacing)+len(p.body)/2] ^= 0x5A
	return out
}

// StripOggGranules returns raw with every page from its first audio page on
// stating granule -1, the checksums recomputed: a stream that says nowhere
// where its packets are. headers is the codec's count of header packets.
func StripOggGranules(t testing.TB, raw []byte, headers int) []byte {
	t.Helper()
	pages := parseOggPages(t, raw)
	audio := oggAudioStart(t, pages, headers)
	var out []byte
	for i, p := range pages {
		if i >= audio {
			p.granule = -1
		}
		out = appendOggPage(out, p)
	}
	return out
}

// TruncateOggAfter returns raw's pages up to and including page, as a
// download cut short there leaves it: no page ends the stream.
func TruncateOggAfter(t testing.TB, raw []byte, page int) []byte {
	t.Helper()
	pages := parseOggPages(t, raw)
	if page >= len(pages) {
		t.Fatalf("the stream has %d pages, not %d", len(pages), page+1)
	}
	var out []byte
	for _, p := range pages[:page+1] {
		p.flags &^= 0x04
		out = appendOggPage(out, p)
	}
	return out
}

// ShiftOggGranules returns raw with by added to the granule position of every
// audio page that completes a packet: a stream starting at granule by rather
// than 0 (RFC 7845 section 4.5, Vorbis I section A.2). headers is the codec's
// count of header packets, which keep granule 0.
func ShiftOggGranules(t testing.TB, raw []byte, headers int, by int64) []byte {
	t.Helper()
	pages := parseOggPages(t, raw)
	audio := oggAudioStart(t, pages, headers)
	var out []byte
	for i, p := range pages {
		if i >= audio && p.granule != -1 {
			if p.granule += by; p.granule < 0 {
				t.Fatalf("page %d would carry granule %d", i, p.granule)
			}
		}
		out = appendOggPage(out, p)
	}
	return out
}

// SetOggGranule returns raw with the granule position of its page'th page (from
// 0, header pages included) set to granule, the page checksum recomputed.
func SetOggGranule(t testing.TB, raw []byte, page int, granule int64) []byte {
	t.Helper()
	pages := parseOggPages(t, raw)
	if page >= len(pages) {
		t.Fatalf("the stream has %d pages, not %d", len(pages), page+1)
	}
	pages[page].granule = granule
	var out []byte
	for _, p := range pages {
		out = appendOggPage(out, p)
	}
	return out
}

// RepageOggStart returns raw with its first n audio packets moved onto one
// page of granule position granule, the layout Vorbis I section A.2 asks of a
// stream that starts anywhere but 0: the second audio packet ends the first
// audio page. The packets come off the front of the pages that held them, and
// what is left of the last of those keeps its granule, since it still ends with
// the packet it ended with. The audio pages are renumbered.
func RepageOggStart(t testing.TB, raw []byte, headers, n int, granule int64) []byte {
	t.Helper()
	pages := parseOggPages(t, raw)
	audio := oggAudioStart(t, pages, headers)
	var out []byte
	for _, p := range pages[:audio] {
		out = appendOggPage(out, p)
	}
	first := oggPageData{granule: granule, serial: pages[0].serial, seq: pages[audio].seq}
	var rest []oggPageData
	taken := 0
	for i := audio; i < len(pages); i++ {
		p := pages[i]
		if taken == n {
			rest = append(rest, p)
			continue
		}
		if p.flags&0x01 != 0 || p.lacing[len(p.lacing)-1] == 255 {
			t.Fatalf("page %d carries a packet that spans pages", i)
		}
		seg, off := 0, 0
		for seg < len(p.lacing) && taken < n {
			start, size := seg, 0
			for {
				l := p.lacing[seg]
				seg++
				size += int(l)
				if l < 255 {
					break
				}
			}
			first.lacing = append(first.lacing, p.lacing[start:seg]...)
			first.body = append(first.body, p.body[off:off+size]...)
			off += size
			taken++
		}
		if seg < len(p.lacing) {
			p.lacing, p.body = p.lacing[seg:], p.body[off:]
			rest = append(rest, p)
		} else {
			first.flags |= p.flags & 0x04 // the end of stream, if it went with them
		}
	}
	if taken < n || len(first.lacing) > 255 {
		t.Fatalf("cannot put %d audio packets on one page (%d found, %d lacing values)",
			n, taken, len(first.lacing))
	}
	out = appendOggPage(out, first)
	for i, p := range rest {
		p.seq = first.seq + uint32(i) + 1
		out = appendOggPage(out, p)
	}
	return out
}
