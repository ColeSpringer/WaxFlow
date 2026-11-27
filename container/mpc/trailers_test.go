package mpc_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/waxerr"
)

// apeTag renders an APEv2 block holding one title.
func apeTag(t testing.TB, title string) []byte {
	t.Helper()
	b, err := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: title}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// lyricsStack is a fixture with the stack a tagger leaves behind it: an
// APEv2 tag, a Lyrics3 v2 block, an ID3v1 tag.
func lyricsStack(t testing.TB, name string) []byte {
	v1 := make([]byte, 128)
	copy(v1, "TAGtitle")
	raw := append(append([]byte(nil), fixture(t, name)...), apeTag(t, "behind the lyrics")...)
	raw = append(raw, "LYRICSBEGININD0000210LYR00005hello000034LYRICS200"...)
	return append(raw, v1...)
}

// tagBehindJunk is a fixture, an APEv2 tag, and bytes that are no tag.
func tagBehindJunk(t testing.TB, name string) []byte {
	raw := append(append([]byte(nil), fixture(t, name)...), apeTag(t, "behind the junk")...)
	return append(raw, bytes.Repeat([]byte{0xAA}, 300)...)
}

// footerIntoFirstFrame is a fixture with a bare APEv2 footer behind it whose
// size reaches back to offset 60: behind the stream header of either fixture
// and in front of where its first frame or audio packet ends.
func footerIntoFirstFrame(t testing.TB, name string) []byte {
	clean := fixture(t, name)
	foot := apeTag(t, "x")
	foot = foot[len(foot)-apev2.FooterLen:]
	foot[23] &^= 0x80 // no header
	binary.LittleEndian.PutUint32(foot[12:], uint32(len(clean)+apev2.FooterLen-60))
	return append(append([]byte(nil), clean...), foot...)
}

// sameSamples holds d to the clean stream on every path: the length the open
// states, a decode from the start, and a decode behind a seek.
func sameSamples(t *testing.T, name string, d, clean *mpc.Demuxer) {
	t.Helper()
	got, want := d.Tracks()[0], clean.Tracks()[0]
	if got.Samples != want.Samples || got.SamplesExact != want.SamplesExact || got.Delay != want.Delay {
		t.Errorf("%s: the open states %d samples (exact %v), want %d (exact %v)", name, got.Samples, got.SamplesExact, want.Samples, want.SamplesExact)
	}
	if !slices.Equal(decodeAll(t, d), decodeAll(t, clean)) {
		t.Errorf("%s: a read decodes other samples than the clean stream", name)
	}
	target := (want.Delay + want.Samples) / 2
	at, err := d.SeekSample(context.Background(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	cleanAt, err := clean.SeekSample(context.Background(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	if at != cleanAt || !slices.Equal(decodeAll(t, d), decodeAll(t, clean)) {
		t.Errorf("%s: a seek to %d landed at %d and decodes other samples than the clean stream from %d", name, target, at, cleanAt)
	}
}

// TestLyrics3BetweenTheTags: a Lyrics3 block between the APEv2 tag and the
// ID3v1 one is a trailer like them, in both framings. The tag in front of it
// is read, and the open, a read, a seek and a strict open all deliver the
// clean stream with nothing reported.
func TestLyrics3BetweenTheTags(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		for _, strict := range []bool{false, true} {
			d := open(t, lyricsStack(t, name), strict)
			if got := d.Tags()["TITLE"]; len(got) != 1 || got[0] != "behind the lyrics" {
				t.Errorf("%s strict=%v: TITLE = %v", name, strict, got)
			}
			sameSamples(t, name, d, open(t, fixture(t, name), strict))
			if w := d.Warnings(); len(w) != 0 {
				t.Errorf("%s strict=%v: warnings %v", name, strict, w)
			}
		}
	}
}

// TestTagBehindJunkIsRead: bytes that are no tag behind an APEv2 tag hide it
// from the peel. The tag is read all the same, and the stream's own structure
// still ends the audio: the samples are the clean stream's on every path.
func TestTagBehindJunkIsRead(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		d := open(t, tagBehindJunk(t, name), false)
		if got := d.Tags()["TITLE"]; len(got) != 1 || got[0] != "behind the junk" {
			t.Errorf("%s: TITLE = %v", name, got)
		}
		sameSamples(t, name, d, open(t, fixture(t, name), false))
		// Junk alone still reads no tag.
		junk := append(append([]byte(nil), fixture(t, name)...), bytes.Repeat([]byte{0xAA}, 300)...)
		if got := open(t, junk, false).Tags()["TITLE"]; got != nil {
			t.Errorf("%s: TITLE = %v from junk", name, got)
		}
	}
}

// TestFooterIntoTheFirstFrameIsNotATag: an APEv2 footer whose size puts its
// tag inside the first frame is refused rather than obeyed, which would cut
// the stream there. The clean stream's samples arrive on every path, the
// footer is reported, and Strict refuses the file at open.
func TestFooterIntoTheFirstFrameIsNotATag(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		raw := footerIntoFirstFrame(t, name)
		d := open(t, raw, false)
		sameSamples(t, name, d, open(t, fixture(t, name), false))
		if ws := d.Warnings(); !hasWarning(ws, "APEv2 footer places its tag at offset 60") {
			t.Errorf("%s: warnings = %v, want the footer named", name, ws)
		}
		if _, err := mpc.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
			t.Errorf("%s: a strict open = %v, want the footer refused as malformed", name, err)
		}
	}
}

// overlappingTag is 56 bytes that pass for an APEv2 tag with a header, the
// footer record inside the header's reserved bytes and stating a size under
// its own length: no tag, in a shape a search once took for one.
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
	return append(b, bytes.Repeat([]byte{0xAA}, 10)...)
}

// TestABlockThatPassesForATagIsRead: bytes behind the stream that pass for a
// tag and are none read as the junk they are. The open once panicked on them.
func TestABlockThatPassesForATagIsRead(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		d := open(t, append(fixture(t, name), overlappingTag()...), false)
		sameSamples(t, name, d, open(t, fixture(t, name), false))
		if got := d.Tags()["TITLE"]; got != nil {
			t.Errorf("%s: TITLE = %v", name, got)
		}
	}
}
