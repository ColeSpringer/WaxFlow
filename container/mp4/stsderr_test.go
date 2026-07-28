package mp4

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// lcASC44k is a plain AAC-LC AudioSpecificConfig: AOT 2, sfIdx 4 (44100),
// chanCfg 2. mainASC44k differs in one field, AOT 1 (Main), which the
// decoder does not implement.
//
// Object type is the refusal to test against because it is the stable one.
// Channel configuration would do the job today (7 and 8-15 are refused),
// but which counts decode is the thing most likely to move, and this file
// is about the plumbing that carries a codec-layer reason up to the
// caller, not about which reasons exist.
var (
	lcASC44k   = []byte{0x12, 0x10}
	mainASC44k = []byte{0x0A, 0x10}
)

// muxAACLC writes a minimal fragmented MP4 declaring asc. The packet
// payloads are not real AAC; nothing here decodes them.
func muxAACLC(t *testing.T, asc []byte) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	track := container.Track{Codec: codec.AACLC, CodecConfig: asc, Fmt: f,
		Samples: 2048, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := range 2 {
		pkt := codec.Packet{Data: bytes.Repeat([]byte{byte(i + 1)}, 64),
			PTS: int64(i * 1024), Dur: 1024, Sync: true}
		if err := m.WritePacket(container.Packet{Track: 0, Packet: pkt}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(codec.Trailer{Samples: 2048}); err != nil {
		t.Fatalf("End: %v", err)
	}
	return out.Bytes()
}

// childBoxes splits a box body into its children, headers included, so a
// test can rebuild the parent with one inserted.
func childBoxes(t *testing.T, body []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for off := 0; off+8 <= len(body); {
		size := int(be32(body[off:]))
		if size < 8 || off+size > len(body) {
			t.Fatalf("child box at %d declares size %d of %d bytes", off, size, len(body))
		}
		out = append(out, body[off:off+size])
		off += size
	}
	return out
}

// replaceOnce swaps old for new, requiring exactly one occurrence so a
// patch cannot silently land somewhere else in the file.
func replaceOnce(t *testing.T, data, old, new []byte) []byte {
	t.Helper()
	if n := bytes.Count(data, old); n != 1 {
		t.Fatalf("pattern %x occurs %d times, want exactly 1", old, n)
	}
	return bytes.Replace(data, old, new, 1)
}

// twoAudioTracks builds a file carrying two sound tracks: one whose stsd
// cannot be parsed, ahead of a good one. The muxer is single-track, so the
// broken track is a patched copy of the good trak spliced into the moov,
// which is also the shape of the real files this covers (an alternate
// audio track a player is expected to skip past).
func twoAudioTracks(t *testing.T) []byte {
	t.Helper()
	file := muxAACLC(t, lcASC44k)

	top := childBoxes(t, file)
	var rebuilt []byte
	patched := false
	for _, b := range top {
		if string(b[4:8]) != "moov" {
			rebuilt = append(rebuilt, b...)
			continue
		}
		kids := childBoxes(t, b[8:])
		var parts [][]byte
		for _, k := range kids {
			if string(k[4:8]) == "trak" && !patched {
				patched = true
				broken := replaceOnce(t, append([]byte(nil), k...), lcASC44k, mainASC44k)
				// A second track_ID: two traks claiming ID 1 would be
				// malformed in a way that muddies what the test proves.
				tkhd := findBoxForTest(t, broken[8:], "tkhd")
				// FullBox(4) creation(4) modification(4) track_ID(4)
				binary.BigEndian.PutUint32(tkhd[12:], 2)
				parts = append(parts, broken)
			}
			parts = append(parts, k)
		}
		rebuilt = append(rebuilt, makeBox("moov", parts...)...)
	}
	if !patched {
		t.Fatal("no trak box to duplicate")
	}
	return rebuilt
}

// TestStsdErrorSurfacesWhenNothingSelectable pins the message an
// unsupported-codec MP4 produces. parseStsd is what sets t.codec, so its
// failure used to leave the track looking like one with no sample
// description at all and the codec layer's reason was replaced by
// "found: unknown" -- which says nothing an operator can act on.
func TestStsdErrorSurfacesWhenNothingSelectable(t *testing.T) {
	file := muxAACLC(t, lcASC44k)
	file = replaceOnce(t, file, lcASC44k, mainASC44k)

	_, err := NewDemuxer(container.BytesSource(file), nil)
	if err == nil {
		t.Fatal("a file whose only audio track has an unparseable stsd opened")
	}
	if !strings.Contains(err.Error(), "audio object type 1 is not AAC-LC") {
		t.Errorf("error = %q, want the codec layer's reason verbatim", err)
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
		t.Errorf("code = %q, want unsupported-format", got)
	}
}

// TestStsdErrorKeepsTheCodecNameList covers the mixed case: when the file
// also carries a sound track whose codec we can name but not decode, the
// deferred reason must ride alongside that list rather than replace it.
// Both halves are actionable and neither implies the other.
func TestStsdErrorKeepsTheCodecNameList(t *testing.T) {
	file := twoAudioTracks(t)
	// Re-point the surviving track at a codec we name but do not decode,
	// so nothing is selectable and both reports have something to say. The
	// broken trak leads, so the last sample entry is the good one's.
	last := bytes.LastIndex(file, []byte("mp4a"))
	if last < 0 {
		t.Fatal("no mp4a sample entry")
	}
	copy(file[last:], "ac-3")

	_, err := NewDemuxer(container.BytesSource(file), nil)
	if err == nil {
		t.Fatal("a file with no decodable audio track opened")
	}
	for _, want := range []string{"audio object type 1 is not AAC-LC", "ac-3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestStsdErrorFatalOnADecodableTrack pins the arm that keeps the
// deferral honest. parseStsd's setters assign t.codec only once they have
// succeeded, but walkBoxes rejects a malformed box on its own, so a first
// sample entry that parsed cleanly followed by a truncated second one
// leaves the track with a codec AND an error. That is damaged audio we
// would otherwise decode, and it must fail the open rather than defer.
func TestStsdErrorFatalOnADecodableTrack(t *testing.T) {
	file := muxAACLC(t, lcASC44k)

	// A second sample entry appended after the good one, declaring far more
	// bytes than the box has left. The first entry still parses and sets
	// the codec; walkBoxes then rejects this one on the size alone, without
	// the callback ever seeing it.
	bogus := []byte{0, 0, 0x7F, 0xFF, 'm', 'p', '4', 'a'}
	typeOff := bytes.Index(file, []byte("stsd"))
	if typeOff < 4 {
		t.Fatal("no stsd box")
	}
	stsdOff := typeOff - 4
	at := stsdOff + int(be32(file[stsdOff:]))

	out := append(append([]byte(nil), file[:at]...), bogus...)
	out = append(out, file[at:]...)
	growAncestors(out, 0, len(out), at, len(bogus))
	// size(4) type(4) version+flags(4) then entry_count: two entries now.
	binary.BigEndian.PutUint32(out[stsdOff+12:], 2)

	_, err := NewDemuxer(container.BytesSource(out), nil)
	if err == nil {
		t.Fatal("a sound track whose stsd walk failed after setting a codec opened")
	}
}

// growAncestors adds n to the size of every box spanning offset at, from
// the outermost in, so a splice at at leaves the box tree consistent.
// Exactly one box per level can span a given offset, so each level grows
// once and recurses.
func growAncestors(out []byte, start, end, at, n int) {
	for off := start; off+8 <= end; {
		size := int(be32(out[off:]))
		if size < 8 || off+size > end {
			return // not a box header: a full box's payload, or the tail
		}
		if off < at && off+size >= at {
			binary.BigEndian.PutUint32(out[off:], uint32(size+n))
			growAncestors(out, off+8, off+size+n, at, n)
			return
		}
		off += size
	}
}

// TestStsdErrorDoesNotRejectGoodSibling is the other direction, and why
// the error is deferred rather than propagated: a broken sound track must
// not reject a file that also carries one we can decode.
func TestStsdErrorDoesNotRejectGoodSibling(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(twoAudioTracks(t)), nil)
	if err != nil {
		t.Fatalf("a file with a good audio track beside a broken one was rejected: %v", err)
	}

	tracks := d.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(tracks))
	}
	if tracks[0].Codec != codec.AACLC || tracks[0].Fmt.Rate != 44100 {
		t.Fatalf("selected %v at %d Hz, want the good AAC-LC track at 44100",
			tracks[0].Codec, tracks[0].Fmt.Rate)
	}
	// It plays, not just opens: the deferral must not have cost the good
	// track its sample map.
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if len(pkt.Data) == 0 {
		t.Fatal("first packet is empty")
	}
}
