package wv_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/wv"
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

// lyrics3v2 renders a Lyrics3 v2 block; id3v1 a bare ID3v1 tag.
func lyrics3v2() []byte {
	body := "LYRICSBEGININD0000210LYR00005hello"
	return []byte(body + "000034LYRICS200")
}

func id3v1() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAGtitle")
	return tag
}

// lyricsStack is the fixture with the stack a tagger leaves behind it: an
// APEv2 tag, a Lyrics3 block, an ID3v1 tag.
func lyricsStack(t testing.TB) []byte {
	raw := fixture(t, "sine-s16.wv")
	return append(append(append(append([]byte(nil), raw...), apeTag(t, "behind the lyrics")...), lyrics3v2()...), id3v1()...)
}

// footerIntoFirstBlock is the fixture with a bare APEv2 footer behind it
// whose size reaches back to offset 100, inside the first block.
func footerIntoFirstBlock(t testing.TB) []byte {
	clean := fixture(t, "sine-s16.wv")
	foot := apeTag(t, "x")
	foot = foot[len(foot)-apev2.FooterLen:]
	foot[23] &^= 0x80 // no header
	binary.LittleEndian.PutUint32(foot[12:], uint32(len(clean)+apev2.FooterLen-100))
	return append(append([]byte(nil), clean...), foot...)
}

// tagBehindJunk is the fixture, an APEv2 tag, and bytes that are no tag.
func tagBehindJunk(t testing.TB) []byte {
	raw := fixture(t, "sine-s16.wv")
	return append(append(append([]byte(nil), raw...), apeTag(t, "behind the junk")...), bytes.Repeat([]byte{0xAA}, 300)...)
}

// readFrom seeks d to sample, when it is not 0, and reads to the end,
// returning where the read ended on the timeline.
func readFrom(t *testing.T, d *wv.Demuxer, sample int64) int64 {
	t.Helper()
	end := int64(0)
	if sample > 0 {
		landed, err := d.SeekSample(context.Background(), 0, sample)
		if err != nil {
			t.Fatal(err)
		}
		if landed > sample {
			t.Fatalf("a seek to %d landed at %d", sample, landed)
		}
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return end
		}
		if err != nil {
			t.Fatal(err)
		}
		end = pkt.PTS + pkt.Dur
	}
}

// TestLyrics3BetweenTheTags: a Lyrics3 block in its usual place, between the
// APEv2 tag and the ID3v1 one, is a trailer like them. The tag in front of it
// is read, and the open, a read, a seek and a strict read all end at the
// stream's own 15435 samples with nothing reported.
func TestLyrics3BetweenTheTags(t *testing.T) {
	raw := lyricsStack(t)
	for _, strict := range []bool{false, true} {
		d, err := wv.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: %v", strict, err)
		}
		if got := d.Tags()["TITLE"]; len(got) != 1 || got[0] != "behind the lyrics" {
			t.Errorf("strict=%v: TITLE = %v", strict, got)
		}
		if tr := d.Tracks()[0]; tr.Samples != 15435 || !tr.SamplesExact {
			t.Errorf("strict=%v: the open states %d samples, exact=%v", strict, tr.Samples, tr.SamplesExact)
		}
		if end := readFrom(t, d, 0); end != 15435 {
			t.Errorf("strict=%v: a read ended at %d", strict, end)
		}
		if end := readFrom(t, d, 9000); end != 15435 {
			t.Errorf("strict=%v: a read behind a seek ended at %d", strict, end)
		}
		if w := d.Warnings(); len(w) != 0 {
			t.Errorf("strict=%v: warnings %v", strict, w)
		}
	}
}

// TestTagBehindJunkIsRead: bytes that are no tag behind an APEv2 tag hide it
// from the peel. The tag is read all the same; the junk and the tag stay
// trailing bytes that are no block, so the samples and the warning are what
// they are with no tag there.
func TestTagBehindJunkIsRead(t *testing.T) {
	junk := bytes.Repeat([]byte{0xAA}, 300)
	clean := fixture(t, "sine-s16.wv")
	raw := tagBehindJunk(t)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Tags()["TITLE"]; len(got) != 1 || got[0] != "behind the junk" {
		t.Errorf("TITLE = %v", got)
	}
	if end := readFrom(t, d, 0); end != 15435 {
		t.Errorf("a read ended at %d", end)
	}
	if end := readFrom(t, d, 9000); end != 15435 {
		t.Errorf("a read behind a seek ended at %d", end)
	}
	ws := d.Warnings()
	if len(ws) != 1 || ws[0].Offset != int64(len(clean)) || !strings.Contains(ws[0].Msg, "trailing bytes are not a block") {
		t.Errorf("warnings = %v, want the trailing bytes at %d", ws, len(clean))
	}
	// Junk alone still reads no tag.
	d, err = wv.NewDemuxer(container.BytesSource(append(append([]byte(nil), clean...), junk...)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tags := d.Tags(); tags != nil {
		t.Errorf("tags = %v from junk", tags)
	}
}

// TestFooterIntoTheFirstBlockIsNotATag: an APEv2 footer whose size puts its
// tag inside the first block is refused rather than obeyed, which would cut
// the stream there. Every sample arrives on a read and behind a seek, the
// footer is reported, and Strict refuses the file at open.
func TestFooterIntoTheFirstBlockIsNotATag(t *testing.T) {
	clean := fixture(t, "sine-s16.wv")
	raw := footerIntoFirstBlock(t)

	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("the open obeyed the footer: %v", err)
	}
	if end := readFrom(t, d, 0); end != 15435 {
		t.Errorf("a read ended at %d", end)
	}
	if end := readFrom(t, d, 9000); end != 15435 {
		t.Errorf("a read behind a seek ended at %d", end)
	}
	ws := d.Warnings()
	if len(ws) == 0 || ws[0].Offset != int64(len(clean)) || !strings.Contains(ws[0].Msg, "APEv2 footer places its tag at offset 100") {
		t.Errorf("warnings = %v, want the footer named at %d", ws, len(clean))
	}
	if _, err := wv.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a strict open = %v, want the footer refused as malformed", err)
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
	raw := append(fixture(t, "sine-s16.wv"), overlappingTag()...)
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if end := readFrom(t, d, 0); end != 15435 || d.Tags() != nil {
		t.Errorf("a read ended at %d with tags %v", end, d.Tags())
	}
}
