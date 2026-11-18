package mp3_test

import (
	"errors"
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/waxerr"
)

// TestFailedHeaderParseAllocatesNothing: every way ParseHeader refuses a
// header answers a malformed error, worded as it always was, without
// allocating; a scan calls it at every byte it resyncs over.
func TestFailedHeaderParseAllocatesNothing(t *testing.T) {
	for _, tc := range []struct {
		hdr  []byte
		want string
	}{
		{[]byte{0xff, 0xfb, 0x90}, "mp3: truncated frame header"},
		{[]byte{0xfe, 0xfb, 0x90, 0x00}, "mp3: missing frame sync"},
		{[]byte{0xff, 0xeb, 0x90, 0x00}, "mp3: reserved MPEG version"},
		{[]byte{0xff, 0xf9, 0x90, 0x00}, "mp3: not Layer III (layer bits 0)"},
		{[]byte{0xff, 0xfd, 0x90, 0x00}, "mp3: not Layer III (layer bits 2)"},
		{[]byte{0xff, 0xff, 0x90, 0x00}, "mp3: not Layer III (layer bits 3)"},
		{[]byte{0xff, 0xfb, 0xf0, 0x00}, "mp3: forbidden bit rate index"},
		{[]byte{0xff, 0xfb, 0x9c, 0x00}, "mp3: reserved sample rate"},
		{[]byte{0xff, 0xfb, 0x90, 0x02}, "mp3: reserved emphasis"},
	} {
		_, err := mp3.ParseHeader(tc.hdr)
		if err == nil || err.Error() != tc.want || !errors.Is(err, waxerr.ErrMalformedInput) {
			t.Errorf("% x: ParseHeader answered %v, want a malformed %q", tc.hdr, err, tc.want)
			continue
		}
		if n := testing.AllocsPerRun(100, func() { _, _ = mp3.ParseHeader(tc.hdr) }); n != 0 {
			t.Errorf("% x: a failed parse allocates %v times", tc.hdr, n)
		}
	}
	good := []byte{0xff, 0xfb, 0x90, 0x00}
	if _, err := mp3.ParseHeader(good); err != nil {
		t.Fatalf("% x: %v", good, err)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = mp3.ParseHeader(good) }); n != 0 {
		t.Errorf("a parsed header allocates %v times", n)
	}
}

// TestHeaderRefusalCannotBeEditedByACaller: a caller that edits the error it
// got from ParseHeader changes nothing the next caller is told.
func TestHeaderRefusalCannotBeEditedByACaller(t *testing.T) {
	bad := []byte{0xfe, 0xfb, 0x90, 0x00}
	_, err := mp3.ParseHeader(bad)
	if e, ok := errors.AsType[*waxerr.Error](err); ok {
		e.Code, e.Msg = waxerr.CodeInternal, "edited"
	}
	_, err = mp3.ParseHeader(bad)
	if err.Error() != "mp3: missing frame sync" || waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
		t.Errorf("after a caller's edit ParseHeader answers %q (%s), want the malformed missing sync", err, waxerr.CodeOf(err))
	}
}
