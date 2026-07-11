package ogg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	waxlabel "github.com/colespringer/waxlabel"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// Canned-packet parameters for the tag tests, the shape
// TestMuxerPageBatching uses.
const (
	tagTestPktLen  = 240
	tagTestDur     = 960
	tagTestPreSkip = 120
	tagTestTrim    = 100
)

// muxCannedOpus muxes a deterministic run of canned Opus packets through
// a muxer built with opts, returning the stream, the payloads written,
// and the end-trim sample count End stamped.
func muxCannedOpus(t *testing.T, opts *MuxerOptions, packets int) (stream []byte, want [][]byte, samples int64) {
	t.Helper()
	var out bytes.Buffer
	m := NewMuxer(&out, opts)
	track := container.Track{Codec: codec.Opus, CodecConfig: muxOpusHead(tagTestPreSkip)}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < packets; i++ {
		data := make([]byte, tagTestPktLen)
		data[0] = byte(31<<3 | 0x4) // TOC: CELT FB 20 ms, stereo, code 0
		for j := 1; j < tagTestPktLen; j++ {
			data[j] = byte(i + j)
		}
		want = append(want, data)
		if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: data, Dur: tagTestDur, Sync: true}}); err != nil {
			t.Fatal(err)
		}
	}
	samples = int64(packets*tagTestDur - tagTestPreSkip - tagTestTrim)
	if err := m.End(codec.Trailer{Samples: samples, Delay: tagTestPreSkip}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), want, samples
}

// demuxed is one packet as the demuxer surfaced it.
type demuxed struct {
	data     []byte
	pts, dur int64
}

// demuxAll parses stream with the package demuxer, returning the track
// and every packet in order.
func demuxAll(t *testing.T, stream []byte) (container.Track, []demuxed) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkts []demuxed
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, demuxed{data: append([]byte(nil), pkt.Data...), pts: pkt.PTS, dur: pkt.Dur})
	}
	return d.Tracks()[0], pkts
}

// opusTagsComments parses the stream's OpusTags page (always the second
// page; the cap keeps the header on one) and returns the vendor and user
// comments, failing on any residue after the declared count.
func opusTagsComments(t *testing.T, stream []byte) (vendor string, comments []string) {
	t.Helper()
	offs := pageOffsets(stream)
	if len(offs) < 2 {
		t.Fatalf("%d pages, want at least the two headers", len(offs))
	}
	p := stream[offs[1]:]
	nseg := int(p[26])
	payLen := 0
	for _, s := range p[27 : 27+nseg] {
		payLen += int(s)
	}
	pay := p[27+nseg : 27+nseg+payLen]
	if len(pay) < 12 || string(pay[:8]) != "OpusTags" {
		t.Fatal("second page does not carry OpusTags")
	}
	vl := int(binary.LittleEndian.Uint32(pay[8:]))
	vendor = string(pay[12 : 12+vl])
	rest := pay[12+vl:]
	count := int(binary.LittleEndian.Uint32(rest))
	rest = rest[4:]
	for i := 0; i < count; i++ {
		if len(rest) < 4 {
			t.Fatalf("comment %d length truncated", i)
		}
		l := int(binary.LittleEndian.Uint32(rest))
		rest = rest[4:]
		if len(rest) < l {
			t.Fatalf("comment %d body truncated", i)
		}
		comments = append(comments, string(rest[:l]))
		rest = rest[l:]
	}
	if len(rest) != 0 {
		t.Fatalf("%d bytes after the declared comments; the count field disagrees with the content", len(rest))
	}
	return vendor, comments
}

// TestMuxOpusTagsRoundTrip checks the OpusTags user comments carry the
// caller's tags without disturbing the audio: the demuxer sees the same
// packets, timing, and trims as a no-tags run, and waxlabel reads the
// fields back.
func TestMuxOpusTagsRoundTrip(t *testing.T) {
	const packets = 60
	tags := []container.Tag{
		{Key: "TITLE", Value: "Opus Title"},
		{Key: "ARTIST", Value: "Opus Artist"},
	}
	tagged, want, samples := muxCannedOpus(t, &MuxerOptions{Tags: tags}, packets)
	plain, _, _ := muxCannedOpus(t, nil, packets)

	vendor, comments := opusTagsComments(t, tagged)
	if vendor != "WaxFlow" {
		t.Errorf("vendor %q, want WaxFlow", vendor)
	}
	wantComments := []string{"TITLE=Opus Title", "ARTIST=Opus Artist"}
	if len(comments) != len(wantComments) {
		t.Fatalf("comments %q, want %q", comments, wantComments)
	}
	for i := range wantComments {
		if comments[i] != wantComments[i] {
			t.Errorf("comment %d = %q, want %q", i, comments[i], wantComments[i])
		}
	}

	trkT, pktsT := demuxAll(t, tagged)
	trkP, pktsP := demuxAll(t, plain)
	if trkT.Samples != samples || trkT.Delay != tagTestPreSkip {
		t.Errorf("tagged track samples %d delay %d, want %d and %d",
			trkT.Samples, trkT.Delay, samples, tagTestPreSkip)
	}
	if trkT.Samples != trkP.Samples || trkT.Delay != trkP.Delay {
		t.Errorf("tags changed the track timing: (%d, %d) with tags, (%d, %d) without",
			trkT.Samples, trkT.Delay, trkP.Samples, trkP.Delay)
	}
	if len(pktsT) != packets || len(pktsP) != packets {
		t.Fatalf("demuxed %d tagged and %d plain packets, want %d", len(pktsT), len(pktsP), packets)
	}
	for i := range pktsT {
		if !bytes.Equal(pktsT[i].data, want[i]) {
			t.Fatalf("packet %d differs from what was written", i)
		}
		if pktsT[i].pts != pktsP[i].pts || pktsT[i].dur != pktsP[i].dur {
			t.Fatalf("packet %d timing (%d, %d) with tags, (%d, %d) without",
				i, pktsT[i].pts, pktsT[i].dur, pktsP[i].pts, pktsP[i].dur)
		}
	}

	doc, err := waxlabel.Parse(t.Context(), container.BytesSource(tagged))
	if err != nil {
		t.Fatalf("waxlabel.Parse: %v", err)
	}
	fields := doc.Fields()
	if fields.Title != "Opus Title" {
		t.Errorf("waxlabel TITLE %q, want %q", fields.Title, "Opus Title")
	}
	if len(fields.Artists) != 1 || fields.Artists[0] != "Opus Artist" {
		t.Errorf("waxlabel ARTIST %q, want %q", fields.Artists, "Opus Artist")
	}
}

// TestMuxOpusTagsCap pins the header cap: a comment that would push the
// OpusTags packet past maxTagsPageBytes is dropped, later comments that
// still fit survive, the count field matches the comments actually
// written, and the stream still parses.
func TestMuxOpusTagsCap(t *testing.T) {
	t.Run("oversized dropped", func(t *testing.T) {
		tags := []container.Tag{
			{Key: "TITLE", Value: "Kept"},
			{Key: "LYRICS", Value: strings.Repeat("x", maxTagsPageBytes)},
			{Key: "ALBUM", Value: "After The Break"},
		}
		stream, _, samples := muxCannedOpus(t, &MuxerOptions{Tags: tags}, 20)
		_, comments := opusTagsComments(t, stream)
		if len(comments) != 2 || comments[0] != "TITLE=Kept" || comments[1] != "ALBUM=After The Break" {
			t.Errorf("comments %q, want the oversized comment alone dropped", comments)
		}
		trk, pkts := demuxAll(t, stream)
		if trk.Samples != samples || len(pkts) != 20 {
			t.Errorf("capped stream demuxed %d packets with %d samples, want 20 and %d",
				len(pkts), trk.Samples, samples)
		}
	})
	t.Run("invalid keys skipped", func(t *testing.T) {
		tags := []container.Tag{
			{Key: "TIT=LE", Value: "split corruptor"},
			{Key: "caf\xc3\xa9", Value: "non-ascii"},
			{Key: "", Value: "empty"},
			{Key: "ALBUM", Value: "Kept"},
		}
		stream, _, _ := muxCannedOpus(t, &MuxerOptions{Tags: tags}, 20)
		_, comments := opusTagsComments(t, stream)
		if len(comments) != 1 || comments[0] != "ALBUM=Kept" {
			t.Errorf("comments %q, want only the valid key", comments)
		}
	})
	t.Run("large comment kept", func(t *testing.T) {
		big := strings.Repeat("y", 40000) // under the cap, over the page byte target
		tags := []container.Tag{
			{Key: "TITLE", Value: "Kept"},
			{Key: "LYRICS", Value: big},
		}
		stream, _, _ := muxCannedOpus(t, &MuxerOptions{Tags: tags}, 20)
		_, comments := opusTagsComments(t, stream)
		if len(comments) != 2 || comments[1] != "LYRICS="+big {
			t.Fatalf("%d comments; a comment under the cap must survive whole", len(comments))
		}
		_, pkts := demuxAll(t, stream)
		if len(pkts) != 20 {
			t.Errorf("demuxed %d packets, want 20", len(pkts))
		}
	})
}
