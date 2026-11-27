package adts

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/waxerr"
)

// Trailer builders: an ID3v1 tag, an APEv2 tag with its header, the
// footer-only APEv1 shape, a Lyrics3 v2 block and a footed ID3v2 tag.
func v1Tag() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAGtitle")
	return tag
}

func apeTag(t testing.TB, title string) []byte {
	t.Helper()
	b, err := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: title}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func apeV1Tag(t testing.TB, title string) []byte {
	b := apeTag(t, title)[apev2.FooterLen:]
	foot := b[len(b)-apev2.FooterLen:]
	binary.LittleEndian.PutUint32(foot[8:], 1000)
	foot[23] &^= 0x80 // no header
	return b
}

func lyricsTag() []byte { return []byte("LYRICSBEGININD0000210LYR00005hello000034LYRICS200") }

func footedID3v2() []byte {
	tag := append([]byte("ID3\x04\x00\x10\x00\x00\x00\x0A"), make([]byte, 10)...)
	return append(tag, "3DI\x04\x00\x10\x00\x00\x00\x0A"...)
}

// frameSpans returns where each frame of a clean stream starts, and its end.
func frameSpans(t testing.TB, raw []byte) (offs []int64, end int64) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := d.idx[len(d.idx)-1]
	h, ok := parseHeader(raw[last:])
	if !ok || last+int64(h.frameLen) != int64(len(raw)) {
		t.Fatal("the fixture's last frame is not flush with its end")
	}
	return slices.Clone(d.idx), int64(len(raw))
}

// delivered is every packet a demuxer yields from sample on: where each
// starts, and its bytes.
type delivered struct {
	pts  []int64
	data [][]byte
}

func readFrom(t testing.TB, d *Demuxer, sample int64) (out delivered, end error) {
	t.Helper()
	if sample > 0 {
		if _, err := d.SeekSample(context.Background(), 0, sample); err != nil {
			t.Fatal(err)
		}
	}
	var pkt container.Packet
	for {
		if err := d.ReadPacket(&pkt); err != nil {
			return out, err
		}
		out.pts = append(out.pts, pkt.PTS)
		out.data = append(out.data, bytes.Clone(pkt.Data))
	}
}

// sameOnEveryPath holds raw to ref, the same stream with nothing behind it:
// the same packets on a read and behind a seek, the same length and findings
// from a walk, tolerant; and under Strict the same verdict from the open, a
// read and a walk.
func sameOnEveryPath(t *testing.T, raw, ref []byte) {
	t.Helper()
	open := func(b []byte, strict bool) *Demuxer {
		d, err := NewDemuxer(container.BytesSource(b), &container.DemuxerOptions{Strict: strict})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d, r := open(raw, false), open(ref, false)
	for _, from := range []int64{0, 5 * 1024} {
		got, gotEnd := readFrom(t, d, from)
		want, wantEnd := readFrom(t, r, from)
		if gotEnd != io.EOF || wantEnd != io.EOF {
			t.Fatalf("from %d: reads ended with %v and %v", from, gotEnd, wantEnd)
		}
		if !slices.Equal(got.pts, want.pts) || !slices.EqualFunc(got.data, want.data, bytes.Equal) {
			t.Errorf("from %d: %d packets, want the %d of the stream alone, byte for byte", from, len(got.pts), len(want.pts))
		}
	}
	d, r = open(raw, false), open(ref, false)
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Tracks()[0], r.Tracks()[0]; got.Samples != want.Samples || !got.SamplesExact {
		t.Errorf("a walk counted %d samples (exact %v), want %d", got.Samples, got.SamplesExact, want.Samples)
	}
	if got, want := d.Warnings(), r.Warnings(); !slices.Equal(got, want) {
		t.Errorf("a walk reported %v, want %v", got, want)
	}
	// Strict: the same verdict at open, from a walk and from a read.
	strict := &container.DemuxerOptions{Strict: true}
	_, gotOpen := NewDemuxer(container.BytesSource(raw), strict)
	_, wantOpen := NewDemuxer(container.BytesSource(ref), strict)
	if errors.Is(gotOpen, waxerr.ErrMalformedInput) != errors.Is(wantOpen, waxerr.ErrMalformedInput) || (gotOpen == nil) != (wantOpen == nil) {
		t.Errorf("a strict open = %v, want %v", gotOpen, wantOpen)
	}
	if gotOpen != nil || wantOpen != nil {
		return
	}
	gotWalk, wantWalk := open(raw, true).Walk(context.Background()), open(ref, true).Walk(context.Background())
	if (gotWalk == nil) != (wantWalk == nil) || errors.Is(gotWalk, waxerr.ErrMalformedInput) != errors.Is(wantWalk, waxerr.ErrMalformedInput) {
		t.Errorf("a strict walk = %v, want %v", gotWalk, wantWalk)
	}
	got, gotEnd := readFrom(t, open(raw, true), 0)
	want, wantEnd := readFrom(t, open(ref, true), 0)
	if len(got.pts) != len(want.pts) || (gotEnd == io.EOF) != (wantEnd == io.EOF) {
		t.Errorf("a strict read delivered %d packets and ended with %v, want %d and %v", len(got.pts), gotEnd, len(want.pts), wantEnd)
	}
}

// TestTrailingTagsAreNotDamage: the tags a tagger appends to an ADTS stream
// are peeled at open. The stream reads, seeks and walks as it does with
// nothing behind it, Strict accepts it, and an APEv2 tag's fields are the
// track's tags.
func TestTrailingTagsAreNotDamage(t *testing.T) {
	clean := fixture(t, "stereo.aac")
	frameSpans(t, clean)
	for name, tc := range map[string]struct {
		tail  []byte
		title string
	}{
		"id3v1":                   {v1Tag(), ""},
		"two id3v1":               {slices.Concat(v1Tag(), v1Tag()), ""},
		"apev2, id3v1":            {slices.Concat(apeTag(t, "ape"), v1Tag()), "ape"},
		"two apev2":               {slices.Concat(apeTag(t, "inner"), apeTag(t, "outer")), "outer"},
		"apev1":                   {apeV1Tag(t, "one"), "one"},
		"apev2, lyrics3, id3v1":   {slices.Concat(apeTag(t, "ape"), lyricsTag(), v1Tag()), "ape"},
		"footed id3v2 and id3v1":  {slices.Concat(footedID3v2(), v1Tag()), ""},
		"nothing behind the tags": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			raw := slices.Concat(clean, tc.tail)
			sameOnEveryPath(t, raw, clean)
			d, err := NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Walk(context.Background()); err != nil {
				t.Fatalf("a strict walk refused a tagged stream: %v", err)
			}
			if got := d.Tags()["TITLE"]; (tc.title == "") != (got == nil) || tc.title != "" && !slices.Equal(got, []string{tc.title}) {
				t.Errorf("TITLE = %v, want %q", got, tc.title)
			}
		})
	}
}

// TestTruncatedFinalFrameBehindTags: a last frame cut short ends at the tags
// behind it and is dropped, as it is with nothing behind it. Bounded by the
// file's end the frame fit again, the tags' bytes its payload.
func TestTruncatedFinalFrameBehindTags(t *testing.T) {
	clean := fixture(t, "stereo.aac")
	frameSpans(t, clean)
	cut := clean[:len(clean)-30]
	for name, tail := range map[string][]byte{
		"id3v1":        v1Tag(),
		"apev2, id3v1": slices.Concat(apeTag(t, "ape"), v1Tag()),
	} {
		t.Run(name, func(t *testing.T) {
			sameOnEveryPath(t, slices.Concat(cut, tail), cut)
		})
	}
	// The stream alone drops one frame and says so, or the rows above pin
	// nothing.
	d, err := NewDemuxer(container.BytesSource(cut), nil)
	if err != nil {
		t.Fatal(err)
	}
	offs, _ := frameSpans(t, clean)
	if got, _ := readFrom(t, d, 0); len(got.pts) != len(offs)-1 || len(d.Warnings()) != 1 {
		t.Fatalf("the cut stream delivered %d of %d frames with %v", len(got.pts), len(offs), d.Warnings())
	}
}

// TestLoneFrameBehindTags: a single frame has no header behind it to confirm
// it, and tags there are not one. The tags come off first and the frame is
// read.
func TestLoneFrameBehindTags(t *testing.T) {
	clean := fixture(t, "stereo.aac")
	offs, _ := frameSpans(t, clean)
	one := clean[:offs[1]]
	sameOnEveryPath(t, slices.Concat(one, apeTag(t, "ape"), v1Tag()), one)
	// And cut short in front of them, it is dropped as the only frame.
	sameOnEveryPath(t, slices.Concat(one[:len(one)-5], v1Tag()), one[:len(one)-5])
}
