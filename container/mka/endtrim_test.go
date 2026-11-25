package mka

// An end trim longer than the last block. A DiscardPadding trims the block it
// rides on, so the muxer spreads such a trim over the final blocks and the
// demuxer counts the trims that run to the end as one.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// The end-trim fixture: an Opus track of 480-sample packets (TOC 0x00).
const (
	endTrimBlocks  = 20
	endTrimDur     = 480
	endTrimPreSkip = 312
	endTrimRaw     = endTrimBlocks * endTrimDur
)

func endTrimTrack() container.Track {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint16(head[10:], endTrimPreSkip)
	binary.LittleEndian.PutUint32(head[12:], 48000)
	return container.Track{
		Codec: codec.Opus, CodecConfig: head, Delay: endTrimPreSkip, Samples: -1, Default: true,
		Fmt: audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
	}
}

// muxEndTrim muxes the fixture's packets, each stating the trim own gives it
// and stamped as a demuxer stamps it, and ends on trim.
func muxEndTrim(t testing.TB, track container.Track, webm, seekable bool, own map[int]int64, trim int64) []byte {
	t.Helper()
	buf, ws := &bytes.Buffer{}, &memWS{}
	var w io.Writer = buf
	if seekable {
		w = ws
	}
	m := NewMuxer(w, &MuxerOptions{WebM: webm})
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	var pos int64
	for i := range endTrimBlocks {
		pkt := container.Packet{Padding: own[i], Packet: codec.Packet{
			Data: []byte{0x00, byte(i), 0x55}, PTS: pos, Dur: endTrimDur,
		}}
		pos += endTrimDur - own[i]
		if err := m.WritePacket(pkt); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(codec.Trailer{Samples: endTrimRaw - endTrimPreSkip - trim, Delay: endTrimPreSkip, Padding: trim}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if seekable {
		return ws.Buf
	}
	return buf.Bytes()
}

// foretold is the fixture's track as Begin is told of an end trim: by its
// Padding, by a length, or not at all.
func foretold(how string, trim int64) container.Track {
	track := endTrimTrack()
	switch how {
	case "padding":
		track.Padding = trim
	case "length":
		track.Samples = endTrimRaw - endTrimPreSkip - trim
	}
	return track
}

// spread is trim laid over the fixture's blocks from the last one back.
func spread(trim int64) []int64 {
	pads := make([]int64, endTrimBlocks)
	for i := endTrimBlocks - 1; i >= 0 && trim > 0; i-- {
		pads[i] = min(trim, endTrimDur)
		trim -= pads[i]
	}
	return pads
}

// strictTrims reads file under a strict demuxer, which refuses a trim larger
// than its block, and returns each packet's trim and the walked track.
func strictTrims(t *testing.T, file []byte) ([]int64, container.Track) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	var pads []int64
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a strict read refuses the file: %v", err)
		}
		pads = append(pads, pkt.Padding)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatalf("a strict walk refuses the file: %v", err)
	}
	return pads, d.Tracks()[0]
}

// TestEndTrimSpreadsOverTheFinalBlocks: an end trim Begin was told of, by a
// Padding or by a length, is written as one DiscardPadding per final block,
// none over its block, and reads back as the track's end trim.
func TestEndTrimSpreadsOverTheFinalBlocks(t *testing.T) {
	for _, how := range []string{"padding", "length"} {
		for _, trim := range []int64{481, 960, 1000, 1441} {
			for _, seekable := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/seekable=%v", how, trim, seekable), func(t *testing.T) {
					file := muxEndTrim(t, foretold(how, trim), false, seekable, nil, trim)
					pads, tr := strictTrims(t, file)
					if want := spread(trim); !slices.Equal(pads, want) {
						t.Errorf("the blocks trim %v, want %v", pads[endTrimBlocks-4:], want[endTrimBlocks-4:])
					}
					if tr.Padding != trim || tr.MidPadding != 0 || len(tr.MidTrims) != 0 {
						t.Errorf("the walk found Padding %d, MidPadding %d, MidTrims %v; want %d, 0 and none",
							tr.Padding, tr.MidPadding, tr.MidTrims, trim)
					}
					if want := int64(endTrimRaw - endTrimPreSkip - trim); tr.Samples != want || !tr.SamplesExact {
						t.Errorf("the walk settled %d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, want)
					}
				})
			}
		}
	}
}

// statedRun is packets stating a 1000-sample end trim between them, as a copy
// of a file that spread one hands them over.
var statedRun = map[int]int64{endTrimBlocks - 3: 40, endTrimBlocks - 2: endTrimDur, endTrimBlocks - 1: endTrimDur}

// TestEndTrimCountsWhatItsPacketsStated: packets that state the end trim
// between them are written as they came, and the trailer's total is not laid
// over them a second time. The file is the one a spread of the same trim
// writes, block timestamps included, whatever Begin was told.
func TestEndTrimCountsWhatItsPacketsStated(t *testing.T) {
	const trim = 1000
	spreadFile := muxEndTrim(t, foretold("padding", trim), false, false, nil, trim)
	for _, how := range []string{"none", "padding"} {
		t.Run(how, func(t *testing.T) {
			file := muxEndTrim(t, foretold(how, trim), false, false, statedRun, trim)
			pads, tr := strictTrims(t, file)
			if want := spread(trim); !slices.Equal(pads, want) {
				t.Errorf("the blocks trim %v, want %v", pads[endTrimBlocks-4:], want[endTrimBlocks-4:])
			}
			if tr.Padding != trim || tr.MidPadding != 0 {
				t.Errorf("the walk found Padding %d, MidPadding %d; want %d, 0", tr.Padding, tr.MidPadding, trim)
			}
			if !bytes.Equal(file, spreadFile) {
				t.Error("the copy of a spread trim is not the file the spread wrote")
			}
		})
	}
	lengthFile := muxEndTrim(t, foretold("length", trim), false, false, statedRun, trim)
	if !bytes.Equal(lengthFile, muxEndTrim(t, foretold("length", trim), false, false, nil, trim)) {
		t.Error("beside a length, the copy of a spread trim is not the file the spread wrote")
	}
}

// TestWebMStatesATrailingRunOnItsLastBlock: WebM carries one DiscardPadding,
// so packets that state the end trim between them leave with it whole on the
// last block: the file a WebM copy of the same trim wrote before any spread.
func TestWebMStatesATrailingRunOnItsLastBlock(t *testing.T) {
	const trim = 1000
	want := make([]int64, endTrimBlocks)
	want[endTrimBlocks-1] = trim
	for _, how := range []string{"none", "padding", "length"} {
		t.Run(how, func(t *testing.T) {
			file := muxEndTrim(t, foretold(how, trim), true, false, statedRun, trim)
			d, err := NewDemuxer(container.BytesSource(file), nil)
			if err != nil {
				t.Fatal(err)
			}
			var pads []int64
			var pkt container.Packet
			for d.ReadPacket(&pkt) == nil {
				pads = append(pads, pkt.Padding)
			}
			if !slices.Equal(pads, want) {
				t.Errorf("the blocks trim %v, want the last alone to state %d", pads[endTrimBlocks-4:], trim)
			}
			sum := sha256.Sum256(file)
			name := fmt.Sprintf("webm=true/%s/%d/seekable=false", how, trim)
			if got := fmt.Sprintf("%x", sum[:8]); got != endTrimPins[name] {
				t.Errorf("the file is %s, want %s: the one whose packets state nothing", got, endTrimPins[name])
			}
		})
	}
	// Trimmed blocks audio follows are inner trims, and leave as they came.
	own := map[int]int64{4: 100, 5: endTrimDur, 6: endTrimDur}
	pads, _ := strictTrims(t, muxEndTrim(t, endTrimTrack(), true, false, own, 96))
	if pads[4] != 100 || pads[5] != endTrimDur || pads[6] != endTrimDur || pads[endTrimBlocks-1] != 96 {
		t.Errorf("the blocks trim %v, %v, %v and %v; want 100, %d, %d and 96",
			pads[4], pads[5], pads[6], pads[endTrimBlocks-1], endTrimDur, endTrimDur)
	}
}

// TestEndTrimOfAnAllPaddingTrack: a length of 0 is a stated length, so a
// track that is all padding spreads its trim over every block.
func TestEndTrimOfAnAllPaddingTrack(t *testing.T) {
	track := endTrimTrack()
	track.Samples = 0
	const trim = endTrimRaw - endTrimPreSkip
	pads, tr := strictTrims(t, muxEndTrim(t, track, false, false, nil, trim))
	if want := spread(trim); !slices.Equal(pads, want) {
		t.Errorf("the blocks trim %v, want %v", pads, want)
	}
	if tr.Samples != 0 || tr.Padding != trim {
		t.Errorf("the walk settled %d samples and Padding %d, want 0 and %d", tr.Samples, tr.Padding, trim)
	}
}

// TestEndTrimHoldsAtMostMaxHeldPackets: the queue is bounded, so a trim that
// reaches further back is spread over the last maxHeld blocks and the rest
// stays on the last one, oversized, as a trim nothing foretold does.
func TestEndTrimHoldsAtMostMaxHeldPackets(t *testing.T) {
	const blocks = 4 * maxHeld
	track := endTrimTrack()
	track.Padding = blocks * endTrimDur
	var buf bytes.Buffer
	m := NewMuxer(&buf, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	for i := range blocks {
		pkt := container.Packet{Packet: codec.Packet{Data: []byte{0x00, byte(i)}, PTS: int64(i) * endTrimDur, Dur: endTrimDur}}
		if err := m.WritePacket(pkt); err != nil {
			t.Fatal(err)
		}
		if n := len(m.held) - m.head; n > maxHeld || len(m.held) > 3*maxHeld {
			t.Fatalf("%d packets held in a queue of %d after packet %d, want at most %d", n, len(m.held), i, maxHeld)
		}
	}
	if err := m.End(codec.Trailer{Samples: 0, Delay: endTrimPreSkip, Padding: track.Padding}); err != nil {
		t.Fatal(err)
	}
	d, err := NewDemuxer(container.BytesSource(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for i := 0; d.ReadPacket(&pkt) == nil; i++ {
		want := int64(endTrimDur)
		switch {
		case i < blocks-maxHeld:
			want = 0
		case i == blocks-1:
			want += (blocks - maxHeld) * endTrimDur
		}
		if pkt.Padding != want {
			t.Fatalf("block %d trims %d, want %d", i, pkt.Padding, want)
		}
	}
}

// TestEndTrimIsTheRunNothingPlaysAfter: a partly trimmed block plays its
// head, so a trimmed block before it is an inner trim, and only a block
// trimmed whole joins the end trim of the ones before it.
func TestEndTrimIsTheRunNothingPlaysAfter(t *testing.T) {
	last := endTrimBlocks - 1
	for _, tc := range []struct {
		name           string
		own            map[int]int64
		trailer        int64
		wantP, wantMid int64
	}{
		{"a partly trimmed block after a partly trimmed one", map[int]int64{last - 1: 120}, 100, 100, 120},
		{"a partly trimmed block after one trimmed whole", map[int]int64{last - 1: endTrimDur}, 100, 100, endTrimDur},
		{"a block trimmed whole after a partly trimmed one", map[int]int64{last - 1: 120}, 120 + endTrimDur, 120 + endTrimDur, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tr := strictTrims(t, muxEndTrim(t, endTrimTrack(), false, false, tc.own, tc.trailer))
			if tr.Padding != tc.wantP || tr.MidPadding != tc.wantMid {
				t.Errorf("the walk found Padding %d, MidPadding %d; want %d, %d", tr.Padding, tr.MidPadding, tc.wantP, tc.wantMid)
			}
		})
	}
}

// TestTrimmedBlocksAudioFollowsStayInner: blocks trimmed whole with audio
// after them (a spliced cut's pre-roll) are inner trims, however many run
// together.
func TestTrimmedBlocksAudioFollowsStayInner(t *testing.T) {
	own := map[int]int64{4: 100, 5: endTrimDur, 6: endTrimDur}
	file := muxEndTrim(t, endTrimTrack(), false, false, own, 96)
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	want := []container.PacketTrim{
		{Pos: 5*endTrimDur - 100, Samples: 100}, {Pos: 5 * endTrimDur, Samples: endTrimDur}, {Pos: 6 * endTrimDur, Samples: endTrimDur},
	}
	if tr.Padding != 96 || tr.MidPadding != 100+2*endTrimDur || !slices.Equal(tr.MidTrims, want) {
		t.Errorf("the walk found Padding %d, MidPadding %d, MidTrims %v; want 96, %d, %v",
			tr.Padding, tr.MidPadding, tr.MidTrims, 100+2*endTrimDur, want)
	}
}

// endTrimPins are the bytes the muxer wrote for each shape before it spread
// a trim: sha256, first eight bytes. A trim within its last block, a trim
// nothing foretold, and every WebM file keep them.
var endTrimPins = map[string]string{
	"webm=false/none/479/seekable=false":    "58a995a02eb38d49",
	"webm=false/none/479/seekable=true":     "1bd1a044e485b8ba",
	"webm=false/none/480/seekable=false":    "a9147a1213ae8a75",
	"webm=false/none/480/seekable=true":     "a57e86d5488d2592",
	"webm=false/none/1000/seekable=false":   "40a3a43244107eb5",
	"webm=false/none/1000/seekable=true":    "3661d30e9106f8c4",
	"webm=false/padding/479/seekable=false": "58a995a02eb38d49",
	"webm=false/padding/479/seekable=true":  "1bd1a044e485b8ba",
	"webm=false/padding/480/seekable=false": "a9147a1213ae8a75",
	"webm=false/padding/480/seekable=true":  "a57e86d5488d2592",
	"webm=false/length/479/seekable=false":  "fadab03d7943db32",
	"webm=false/length/479/seekable=true":   "1bd1a044e485b8ba",
	"webm=false/length/480/seekable=false":  "d0b81a0eb6a8494f",
	"webm=false/length/480/seekable=true":   "a57e86d5488d2592",
	"webm=true/none/479/seekable=false":     "cfa1124e2bdaed08",
	"webm=true/none/479/seekable=true":      "1afcfe634dd72b26",
	"webm=true/none/480/seekable=false":     "91c1d8ed7c9df1b6",
	"webm=true/none/480/seekable=true":      "c8b6135f1e0c2bd2",
	"webm=true/none/1000/seekable=false":    "8e4a5929148ac97a",
	"webm=true/none/1000/seekable=true":     "7e1c3d57a30829ef",
	"webm=true/padding/479/seekable=false":  "cfa1124e2bdaed08",
	"webm=true/padding/479/seekable=true":   "1afcfe634dd72b26",
	"webm=true/padding/480/seekable=false":  "91c1d8ed7c9df1b6",
	"webm=true/padding/480/seekable=true":   "c8b6135f1e0c2bd2",
	"webm=true/padding/1000/seekable=false": "8e4a5929148ac97a",
	"webm=true/padding/1000/seekable=true":  "7e1c3d57a30829ef",
	"webm=true/length/479/seekable=false":   "5bde8f09a97b16ca",
	"webm=true/length/479/seekable=true":    "1afcfe634dd72b26",
	"webm=true/length/480/seekable=false":   "a8f7e638e5b5240e",
	"webm=true/length/480/seekable=true":    "c8b6135f1e0c2bd2",
	"webm=true/length/1000/seekable=false":  "ac8f760c1f91fb8d",
	"webm=true/length/1000/seekable=true":   "7e1c3d57a30829ef",
}

// TestEndTrimWithinItsBlockWritesTheSameBytes: a trim of exactly one block or
// one sample under is one DiscardPadding on the last block, as it was, and so
// is a longer one where the muxer holds one block: nothing foretold it, or
// the file is WebM.
func TestEndTrimWithinItsBlockWritesTheSameBytes(t *testing.T) {
	for _, webm := range []bool{false, true} {
		for _, how := range []string{"none", "padding", "length"} {
			for _, trim := range []int64{endTrimDur - 1, endTrimDur, 1000} {
				if trim > endTrimDur && !webm && how != "none" {
					continue // spread: see TestEndTrimSpreadsOverTheFinalBlocks
				}
				for _, seekable := range []bool{false, true} {
					name := fmt.Sprintf("webm=%v/%s/%d/seekable=%v", webm, how, trim, seekable)
					file := muxEndTrim(t, foretold(how, trim), webm, seekable, nil, trim)
					sum := sha256.Sum256(file)
					if got := fmt.Sprintf("%x", sum[:8]); got != endTrimPins[name] {
						t.Errorf("%q: %q,", name, got)
					}
				}
			}
		}
	}
}

// TestWebMRefusesAnEndTrimNoPacketHolds: WebM states an end trim on its last
// block alone, so TrackFits and Begin refuse one longer than any packet of
// the codec, and Matroska takes it.
func TestWebMRefusesAnEndTrimNoPacketHolds(t *testing.T) {
	const longest = 48000 * 120 / 1000 // 120 ms of Opus
	track := endTrimTrack()
	track.Padding = longest
	if err := TrackFits(track, true); err != nil {
		t.Errorf("a trim one packet can hold: %v", err)
	}
	track.Padding = longest + 1
	if err := TrackFits(track, false); err != nil {
		t.Errorf("Matroska: %v", err)
	}
	err := TrackFits(track, true)
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
		t.Fatalf("TrackFits = %v, want an unsupported-format refusal", err)
	}
	begin := NewMuxer(io.Discard, &MuxerOptions{WebM: true}).Begin([]container.Track{track})
	if begin == nil || begin.Error() != err.Error() {
		t.Errorf("Begin = %v, want TrackFits' refusal %v", begin, err)
	}
}
