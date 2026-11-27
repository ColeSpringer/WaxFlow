package apen_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/internal/testutil"
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

// lyricsStack is the multi-frame fixture with the stack a tagger leaves
// behind it: an APEv2 tag, a Lyrics3 v2 block, an ID3v1 tag.
func lyricsStack(t testing.TB) []byte {
	v1 := make([]byte, 128)
	copy(v1, "TAGtitle")
	raw := append(append([]byte(nil), fixture(t, "seek.ape")...), apeTag(t, "behind the lyrics")...)
	raw = append(raw, "LYRICSBEGININD0000210LYR00005hello000034LYRICS200"...)
	return append(raw, v1...)
}

// tagBehindJunk is a fixture, an APEv2 tag, and bytes that are no tag.
func tagBehindJunk(t testing.TB, name string) []byte {
	raw := append(append([]byte(nil), fixture(t, name)...), apeTag(t, "behind the junk")...)
	return append(raw, bytes.Repeat([]byte{0xAA}, 300)...)
}

// footerIntoFirstFrame is a fixture with a bare APEv2 footer behind it whose
// size reaches back to ten bytes into the first frame.
func footerIntoFirstFrame(t testing.TB, name string) []byte {
	clean := fixture(t, name)
	h, err := ape.ParseHeader(clean)
	if err != nil {
		t.Fatal(err)
	}
	foot := apeTag(t, "x")
	foot = foot[len(foot)-apev2.FooterLen:]
	foot[23] &^= 0x80 // no header
	binary.LittleEndian.PutUint32(foot[12:], uint32(int64(len(clean)+apev2.FooterLen)-h.FrameDataOffset-10))
	return append(append([]byte(nil), clean...), foot...)
}

// frames reads d from sample to the end and returns each frame's start and
// coded bytes, the bytes a decoder is handed.
func frames(t *testing.T, d *apen.Demuxer, sample int64) (starts []int64, coded [][]byte) {
	t.Helper()
	if _, err := d.SeekSample(context.Background(), 0, sample); err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return starts, coded
		}
		if err != nil {
			t.Fatal(err)
		}
		_, skip, n, data, err := ape.ParseFrameHeader(pkt.Data)
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, pkt.PTS)
		coded = append(coded, append([]byte(nil), data[skip:skip+n]...))
	}
}

// sameFrames holds a tagged file's frames to the clean file's, from the start
// and behind a seek: the same positions and the same coded bytes.
func sameFrames(t *testing.T, d, clean *apen.Demuxer) {
	t.Helper()
	for _, from := range []int64{0, clean.Tracks()[0].Samples / 2} {
		gotAt, got := frames(t, d, from)
		wantAt, want := frames(t, clean, from)
		if len(got) != len(want) {
			t.Fatalf("from %d: %d frames, want %d", from, len(got), len(want))
		}
		for i := range want {
			if gotAt[i] != wantAt[i] || !bytes.Equal(got[i], want[i]) {
				t.Fatalf("from %d: frame %d at %d holds %d bytes, want %d bytes at %d", from, i, gotAt[i], len(got[i]), len(want[i]), wantAt[i])
			}
		}
	}
}

// TestLyrics3BetweenTheTags: a Lyrics3 block between the APEv2 tag and the
// ID3v1 one is a trailer like them. The tag in front of it is read, and the
// open, a read, a seek and a strict open all see the clean file's frames with
// nothing reported.
func TestLyrics3BetweenTheTags(t *testing.T) {
	raw := lyricsStack(t)
	clean := open(t, fixture(t, "seek.ape"))
	for _, strict := range []bool{false, true} {
		d, err := apen.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: strict})
		if err != nil {
			t.Fatalf("strict=%v: %v", strict, err)
		}
		if got := d.Tags()["TITLE"]; len(got) != 1 || got[0] != "behind the lyrics" {
			t.Errorf("strict=%v: TITLE = %v", strict, got)
		}
		if got, want := d.Tracks()[0].Samples, clean.Tracks()[0].Samples; got != want {
			t.Errorf("strict=%v: the open states %d samples, want %d", strict, got, want)
		}
		sameFrames(t, d, clean)
		if w := d.Warnings(); len(w) != 0 {
			t.Errorf("strict=%v: warnings %v", strict, w)
		}
	}
}

// TestTagBehindJunkIsRead: bytes that are no tag behind an APEv2 tag hide it
// from the peel. The tag is read all the same. The frames before the last are
// the clean file's; the last still runs to the end of the data, as it does
// with junk alone, and the disagreement with the descriptor is still
// reported.
func TestTagBehindJunkIsRead(t *testing.T) {
	for _, name := range []string{"seek.ape", "sine-s16.ape"} {
		raw := tagBehindJunk(t, name)
		d := open(t, raw)
		if got := d.Tags()["TITLE"]; len(got) != 1 || got[0] != "behind the junk" {
			t.Errorf("%s: TITLE = %v", name, got)
		}
		clean := open(t, fixture(t, name))
		if got, want := d.Tracks()[0].Samples, clean.Tracks()[0].Samples; got != want {
			t.Errorf("%s: the open states %d samples, want %d", name, got, want)
		}
		gotAt, got := frames(t, d, 0)
		wantAt, want := frames(t, clean, 0)
		if len(got) != len(want) {
			t.Fatalf("%s: %d frames, want %d", name, len(got), len(want))
		}
		for i := range want {
			last := i == len(want)-1
			if gotAt[i] != wantAt[i] || !bytes.HasPrefix(got[i], want[i]) || (!last && len(got[i]) != len(want[i])) {
				t.Fatalf("%s: frame %d differs from the clean file's", name, i)
			}
		}
		ws := d.Warnings()
		if len(ws) != 1 || !strings.Contains(ws[0].Msg, "bytes of frame data") {
			t.Errorf("%s: warnings = %v, want the descriptor's disagreement", name, ws)
		}
		// Junk alone still reads no tag.
		junk := append(append([]byte(nil), fixture(t, name)...), bytes.Repeat([]byte{0xAA}, 300)...)
		if tags := open(t, junk).Tags(); tags != nil {
			t.Errorf("%s: tags = %v from junk", name, tags)
		}
	}
}

// TestFooterIntoTheFirstFrameIsNotATag: an APEv2 footer whose size puts its
// tag inside the first frame is refused rather than obeyed, which would cut
// the stream there, in a file of one frame as in one of several: the frame
// decodes whole, so what claims its bytes is no tag. The open succeeds, every
// frame before the last is the clean file's on a read and behind a seek, the
// footer is reported, and Strict refuses the file at open.
func TestFooterIntoTheFirstFrameIsNotATag(t *testing.T) {
	for _, name := range []string{"seek.ape", "sine-s16.ape"} {
		raw := footerIntoFirstFrame(t, name)
		d, err := apen.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("%s: the open obeyed the footer: %v", name, err)
		}
		clean := open(t, fixture(t, name))
		// The first frame decodes whole, so the file is not cut: the length
		// is the header's, and nothing is counted truncated.
		if d.Tracks()[0].Samples != clean.Tracks()[0].Samples || d.TruncatedSamples(0) != 0 {
			t.Errorf("%s: the open states %d samples, %d truncated", name, d.Tracks()[0].Samples, d.TruncatedSamples(0))
		}
		for _, from := range []int64{0, clean.Tracks()[0].Samples / 2} {
			gotAt, got := frames(t, d, from)
			wantAt, want := frames(t, clean, from)
			if len(got) != len(want) {
				t.Fatalf("%s from %d: %d frames, want %d", name, from, len(got), len(want))
			}
			for i := range want {
				last := i == len(want)-1
				if gotAt[i] != wantAt[i] || !bytes.HasPrefix(got[i], want[i]) || (!last && len(got[i]) != len(want[i])) {
					t.Fatalf("%s from %d: frame %d differs from the clean file's", name, from, i)
				}
			}
		}
		ws := d.Warnings()
		if len(ws) == 0 || !strings.Contains(ws[0].Msg, "APEv2 footer places its tag at offset") {
			t.Errorf("%s: warnings = %v, want the footer named", name, ws)
		}
		if _, err := apen.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
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

// TestABlockThatPassesForATagIsRead: bytes behind the frames that pass for a
// tag and are none read as the junk they are. The open once panicked on them.
func TestABlockThatPassesForATagIsRead(t *testing.T) {
	for _, name := range []string{"seek.ape", "sine-s16.ape"} {
		d := open(t, append(fixture(t, name), overlappingTag()...))
		clean := open(t, fixture(t, name))
		if _, samples := walk(t, d); samples != clean.Tracks()[0].Samples || d.Tags() != nil {
			t.Errorf("%s: walked %d samples with tags %v", name, samples, d.Tags())
		}
	}
}

// TestAByteCountPastTheFileKeepsTheFloor: the descriptor's frame data count
// is 64 bits the file states. One near the top of the range is held at the
// data's end before anything is added to it, so the floor a trailing tag must
// clear stays a real offset: a footer whose tag would start at the file's
// first byte is not obeyed, and the frame is still read.
func TestAByteCountPastTheFileKeepsTheFloor(t *testing.T) {
	clean := fixture(t, "sine-s16.ape")
	h, err := ape.ParseHeader(clean)
	if err != nil || h.TotalFrames != 1 {
		t.Fatalf("the fixture has %d frames (%v), want one", h.TotalFrames, err)
	}
	raw := bytes.Clone(clean)
	binary.LittleEndian.PutUint64(raw[24:], 1<<63-1)
	foot := apeTag(t, "x")
	foot = foot[len(foot)-apev2.FooterLen:]
	foot[23] &^= 0x80 // no header
	binary.LittleEndian.PutUint32(foot[12:], uint32(len(raw)+apev2.FooterLen))
	raw = append(raw, foot...)

	d, err := apen.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("the open obeyed the footer: %v", err)
	}
	want := open(t, clean).Tracks()[0].Samples
	if _, samples := walk(t, d); samples != want || d.Tracks()[0].Samples != want {
		t.Errorf("walked %d samples of the %d the open states, want %d", samples, d.Tracks()[0].Samples, want)
	}
	// The floor is the data's end, so nothing behind the frame is taken for
	// a tag, and the count that no file could hold is what is reported.
	if ws := d.Warnings(); len(ws) != 1 || !strings.Contains(ws[0].Msg, "bytes of frame data") {
		t.Errorf("warnings = %v, want the descriptor's count alone", ws)
	}
}

// TestACutFirstFrameBehindTags: tags behind a file cut inside its first frame
// start below where a whole first frame ends. They are tags all the same, and
// the frame's CRC says so: it does not decode with a whole frame's bytes, or,
// where the cut took only bytes the coder never reads, it decodes without
// theirs too. So they come off, the cut is seen, and the open, a read, a seek
// and a strict open answer as they do for the cut file with nothing behind
// it, whether that file still decodes or not. The tag's fields are read.
func TestACutFirstFrameBehindTags(t *testing.T) {
	v1 := make([]byte, 128)
	copy(v1, "TAGtitle")
	stack := slices.Concat(apeTag(t, "behind the cut"), []byte("LYRICSBEGININD0000210LYR00005hello000034LYRICS200"), v1)
	type cutFile struct {
		name string
		cut  []byte
	}
	cuts := []cutFile{
		{"one frame, 50 bytes gone", cutBy(t, "sine-s16.ape", 50)},
		{"one frame, 1000 bytes gone", cutBy(t, "sine-s16.ape", 1000)},
		{"three frames, cut inside the first", fixture(t, "seek.ape")[:9000]},
	}
	// The last bytes of a frame, where a cut may leave it decoding.
	kept := 0
	for n := 1; n <= 6; n++ {
		cuts = append(cuts, cutFile{fmt.Sprintf("one frame, %d bytes gone", n), cutBy(t, "sine-s16.ape", n)})
	}
	for _, tc := range cuts {
		bare, err := apen.NewDemuxer(container.BytesSource(tc.cut), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := bare.Tracks()[0]
		wantFrames, wantSamples := walk(t, bare)
		if len(bare.Warnings()) == 0 {
			t.Fatalf("%s: the cut file alone reads clean; the rows pin nothing", tc.name)
		}
		if wantFrames > 0 {
			kept++
		}
		for tail, tags := range map[string][]byte{"id3v1": v1, "apev2, lyrics3, id3v1": stack} {
			raw := slices.Concat(tc.cut, tags)
			d, err := apen.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatalf("%s, %s: %v", tc.name, tail, err)
			}
			if got := d.Tracks()[0]; got.Samples != want.Samples || got.SamplesExact != want.SamplesExact || d.TruncatedSamples(0) != bare.TruncatedSamples(0) {
				t.Errorf("%s, %s: the open states %d samples, %d truncated; the cut file alone %d and %d",
					tc.name, tail, got.Samples, d.TruncatedSamples(0), want.Samples, bare.TruncatedSamples(0))
			}
			if !slices.Equal(d.Warnings(), bare.Warnings()) {
				t.Errorf("%s, %s: warnings %v, the cut file alone %v", tc.name, tail, d.Warnings(), bare.Warnings())
			}
			if frames, samples := walk(t, d); frames != wantFrames || samples != wantSamples {
				t.Errorf("%s, %s: a read delivered %d frames, the cut file alone %d", tc.name, tail, frames, wantFrames)
			}
			if landed, err := d.SeekSample(context.Background(), 0, 1000); err != nil || landed != 0 {
				t.Errorf("%s, %s: a seek landed at %d: %v", tc.name, tail, landed, err)
			}
			if frames, _ := walk(t, d); frames != wantFrames {
				t.Errorf("%s, %s: a read behind a seek delivered %d frames, the cut file alone %d", tc.name, tail, frames, wantFrames)
			}
			if got := d.Tags()["TITLE"]; (tail == "id3v1") != (got == nil) {
				t.Errorf("%s, %s: TITLE = %v", tc.name, tail, got)
			}
			_, strictErr := apen.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
			_, bareErr := apen.NewDemuxer(container.BytesSource(tc.cut), &container.DemuxerOptions{Strict: true})
			if !errors.Is(strictErr, waxerr.ErrMalformedInput) || strictErr.Error() != bareErr.Error() {
				t.Errorf("%s, %s: a strict open = %v, the cut file alone %v", tc.name, tail, strictErr, bareErr)
			}
		}
	}
	if kept == 0 {
		t.Error("no cut of 1 to 6 bytes left the frame decoding: the slack rows pin nothing")
	}
}

// TestAHugeFirstFrameIsNotStagedForTheTrial: a first frame whose stated
// extent is past the size a frame may have is refused by the seek table's
// check. A footer pointing below that extent does not get it read and
// decoded first: the open reads what it reads without the footer.
func TestAHugeFirstFrameIsNotStagedForTheTrial(t *testing.T) {
	clean := fixture(t, "sine-s16.ape")
	h, err := ape.ParseHeader(clean)
	if err != nil || h.TotalFrames != 1 {
		t.Fatalf("the fixture has %d frames (%v), want one", h.TotalFrames, err)
	}
	const more = 4 << 20
	huge := append(bytes.Clone(clean), make([]byte, more)...)
	binary.LittleEndian.PutUint64(huge[24:], uint64(h.FrameDataBytes+more))
	foot := apeTag(t, "x")
	foot = foot[len(foot)-apev2.FooterLen:]
	foot[23] &^= 0x80 // no header
	binary.LittleEndian.PutUint32(foot[12:], 1032)
	var reads [2]int64
	for i, raw := range [][]byte{huge, slices.Concat(huge[:len(huge)-apev2.FooterLen], foot)} {
		src := &testutil.CountingSource{Src: container.BytesSource(raw)}
		_, err := apen.NewDemuxer(src, nil)
		if err == nil || !strings.Contains(err.Error(), "exceeds the") {
			t.Fatalf("open = %v, want the frame refused for its size", err)
		}
		reads[i] = src.Bytes
	}
	if reads[1] > reads[0]+4096 || reads[1] > 64<<10 {
		t.Errorf("the open read %d bytes with a footer below the frame's end and %d without", reads[1], reads[0])
	}
}

// cutBy is a fixture less its last n bytes.
func cutBy(t testing.TB, name string, n int) []byte {
	raw := fixture(t, name)
	return raw[:len(raw)-n]
}

// hugeFirstFrame is the one-frame fixture with 64 KiB of zeros behind it that
// its header counts as frame data, and a bare footer at the end whose tag
// would start inside them.
func hugeFirstFrame(t testing.TB) []byte {
	clean := fixture(t, "sine-s16.ape")
	h, err := ape.ParseHeader(clean)
	if err != nil {
		t.Fatal(err)
	}
	const more = 64 << 10
	raw := append(bytes.Clone(clean), make([]byte, more)...)
	binary.LittleEndian.PutUint64(raw[24:], uint64(h.FrameDataBytes+more))
	foot := apeTag(t, "x")
	foot = foot[len(foot)-apev2.FooterLen:]
	foot[23] &^= 0x80 // no header
	binary.LittleEndian.PutUint32(foot[12:], 1032)
	return append(raw[:len(raw)-apev2.FooterLen], foot...)
}
