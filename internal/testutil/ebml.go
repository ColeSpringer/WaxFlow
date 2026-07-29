package testutil

import "testing"

// A minimal EBML reader for tests outside container/mka, which cannot reach
// that package's parser. Tests inside container/mka should use the real one, so
// they check the muxer against the thing that reads it.

// EBMLElement is one parsed element: its ID and the bounds of its body within
// the buffer it was read from.
type EBMLElement struct {
	ID    uint32
	Start int
	End   int
}

// ebmlVint decodes one variable-length integer. keepMarker leaves the
// length-descriptor bits in, which is how element IDs are compared; otherwise
// they are stripped and unknown reports the all-ones run-to-parent-end size.
func ebmlVint(b []byte, keepMarker bool) (val uint64, n int, unknown, ok bool) {
	if len(b) == 0 {
		return 0, 0, false, false
	}
	w := 0
	for i := 0; i < 8; i++ {
		if b[0]&(0x80>>i) != 0 {
			w = i + 1
			break
		}
	}
	if w == 0 || w > len(b) {
		return 0, 0, false, false
	}
	if keepMarker {
		for _, x := range b[:w] {
			val = val<<8 | uint64(x)
		}
		return val, w, false, true
	}
	first := b[0] & (0xFF >> w)
	allOnes := first == (0xFF >> w)
	val = uint64(first)
	for _, x := range b[1:w] {
		val = val<<8 | uint64(x)
		if x != 0xFF {
			allOnes = false
		}
	}
	return val, w, allOnes, true
}

// EBMLAt parses the element header at off; an unknown or overrunning size runs
// to the end of buf. It fails the test rather than returning an error.
func EBMLAt(t testing.TB, buf []byte, off int) EBMLElement {
	t.Helper()
	if off < 0 || off >= len(buf) {
		t.Fatalf("ebml: element offset %d outside a %d-byte buffer", off, len(buf))
	}
	id, idLen, _, ok := ebmlVint(buf[off:], true)
	if !ok || id > 0xFFFFFFFF {
		t.Fatalf("ebml: bad element ID at %d", off)
	}
	if off+idLen >= len(buf) {
		t.Fatalf("ebml: element at %d has no size field", off)
	}
	size, sizeLen, unknown, ok := ebmlVint(buf[off+idLen:], false)
	if !ok {
		t.Fatalf("ebml: bad element size at %d", off)
	}
	e := EBMLElement{ID: uint32(id), Start: off + idLen + sizeLen, End: len(buf)}
	if !unknown && size <= uint64(len(buf)-e.Start) {
		e.End = e.Start + int(size)
	}
	return e
}

// EBMLChildren walks the elements packed in buf. The progress guarantee lives
// here rather than in every caller.
func EBMLChildren(t testing.TB, buf []byte, fn func(EBMLElement)) {
	t.Helper()
	for off := 0; off < len(buf); {
		e := EBMLAt(t, buf, off)
		fn(e)
		if e.End <= off {
			t.Fatalf("ebml: element %#x at %d made no progress", e.ID, off)
		}
		off = e.End
	}
}

// EBMLSegment returns a Matroska file's Segment body and whether its size was
// declared rather than left unknown (the streaming form).
func EBMLSegment(t testing.TB, file []byte) (body []byte, definite bool) {
	t.Helper()
	head := EBMLAt(t, file, 0)
	if head.ID != EBMLIDHeader {
		t.Fatalf("ebml: no EBML header (id %#x)", head.ID)
	}
	seg := EBMLAt(t, file, head.End)
	if seg.ID != EBMLIDSegment {
		t.Fatalf("ebml: no Segment after the EBML header (id %#x)", seg.ID)
	}
	// The size vint sits between the 4-byte Segment ID and its data.
	_, _, unknown, _ := ebmlVint(file[head.End+4:], false)
	return file[seg.Start:seg.End], !unknown
}

// EBMLFind returns the first child of buf with the given ID.
func EBMLFind(t testing.TB, buf []byte, id uint32) (body []byte, found bool) {
	t.Helper()
	EBMLChildren(t, buf, func(e EBMLElement) {
		if e.ID == id && !found {
			body, found = buf[e.Start:e.End], true
		}
	})
	return body, found
}

// EBMLSeekTargets returns the SeekID of every Seek entry in a SeekHead body.
func EBMLSeekTargets(t testing.TB, seekHead []byte) []uint32 {
	t.Helper()
	var out []uint32
	EBMLChildren(t, seekHead, func(e EBMLElement) {
		if e.ID != EBMLIDSeek {
			return
		}
		// Child offsets are relative to the body walked, so index that body.
		entry := seekHead[e.Start:e.End]
		EBMLChildren(t, entry, func(c EBMLElement) {
			if c.ID != EBMLIDSeekID {
				return
			}
			var v uint32
			for _, x := range entry[c.Start:c.End] {
				v = v<<8 | uint32(x)
			}
			out = append(out, v)
		})
	})
	return out
}

// The element IDs these helpers name, in their on-wire form.
const (
	EBMLIDHeader   = 0x1A45DFA3
	EBMLIDSegment  = 0x18538067
	EBMLIDSeekHead = 0x114D9B74
	EBMLIDSeek     = 0x4DBB
	EBMLIDSeekID   = 0x53AB
	EBMLIDInfo     = 0x1549A966
	EBMLIDDuration = 0x4489
	EBMLIDTracks   = 0x1654AE6B
	EBMLIDCues     = 0x1C53BB6B
	EBMLIDCluster  = 0x1F43B675
)
