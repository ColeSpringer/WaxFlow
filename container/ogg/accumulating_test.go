package ogg

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// TestResolvePositions covers the granule-anchor back-fill in isolation
// (ffmpeg-free): every packet's start is derived from its duration and the
// nearest anchors, forward from a preceding anchor and backward into the
// prefix before the first anchor.
func TestResolvePositions(t *testing.T) {
	for _, tc := range []struct {
		name string
		durs []int64
		ends []int64
		want []int64 // expected starts
	}{
		{"single anchor at end", []int64{10, 20, 30}, []int64{-1, -1, 60}, []int64{0, 10, 30}},
		{"anchor at start", []int64{10, 20}, []int64{10, -1}, []int64{0, 10}},
		{"multiple anchors", []int64{10, 20, 30, 40}, []int64{-1, 30, -1, 100}, []int64{0, 10, 30, 60}},
		{"all anchored", []int64{5, 5, 5}, []int64{5, 10, 15}, []int64{0, 5, 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePositions(tc.durs, tc.ends)
			if len(got) != len(tc.want) {
				t.Fatalf("len %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("start[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// buildSyntheticOpus assembles an Ogg-Opus stream by hand (no ffmpeg): an
// OpusHead BOS, an OpusTags header, then audio pages of TOC-config-31 packets
// (CELT FB 20 ms = 960 samples each) with cumulative granule positions. padTo
// sizes each audio packet's dummy body so the stream can be made larger than
// the bisection window when a seek needs to actually narrow.
func buildSyntheticOpus(preSkip, packetsPerPage, pages, padTo int) (stream []byte, samplesPerPacket int64, total int64) {
	serial := uint32(4321)
	head := append([]byte("OpusHead"), 1, 2) // version 1, 2 channels
	head = binary.LittleEndian.AppendUint16(head, uint16(preSkip))
	head = binary.LittleEndian.AppendUint32(head, 48000) // input rate
	head = binary.LittleEndian.AppendUint16(head, 0)     // output gain
	head = append(head, 0)                               // mapping family 0
	tags := append([]byte("OpusTags"), 0, 0, 0, 0, 0, 0, 0, 0)

	stream = append(stream, buildPage(flagBOS, 0, serial, 0, head)...)
	stream = append(stream, buildPage(0, 0, serial, 1, tags)...)

	audioPkt := make([]byte, max(padTo, 3))
	audioPkt[0] = byte(31<<3 | 0x4) // config 31 (CELT FB 20 ms), stereo, code 0
	samplesPerPacket = 960
	seq := uint32(2)
	var granule int64
	for p := 0; p < pages; p++ {
		pkts := make([][]byte, packetsPerPage)
		for i := range pkts {
			pkts[i] = audioPkt
			granule += samplesPerPacket
		}
		flags := byte(0)
		if p == pages-1 {
			flags = flagEOS
		}
		stream = append(stream, buildPage(flags, granule, serial, seq, pkts...)...)
		seq++
	}
	return stream, samplesPerPacket, granule
}

func TestSyntheticOpusDemux(t *testing.T) {
	preSkip := 312
	stream, dur, total := buildSyntheticOpus(preSkip, 3, 8, 0)

	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.Opus || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
		t.Fatalf("track = %+v", tr)
	}
	if tr.Delay != int64(preSkip) {
		t.Fatalf("delay = %d, want %d", tr.Delay, preSkip)
	}
	if want := total - int64(preSkip); tr.Samples != want {
		t.Fatalf("samples = %d, want %d (granule %d - preskip %d)", tr.Samples, want, total, preSkip)
	}

	// Linear walk: every packet is 960 samples, PTS accumulates, all sync.
	var pkt container.Packet
	next := int64(0)
	count := 0
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.Dur != dur || !pkt.Sync || pkt.PTS != next {
			t.Fatalf("packet %d: pts=%d dur=%d sync=%v, want pts=%d dur=%d", count, pkt.PTS, pkt.Dur, pkt.Sync, next, dur)
		}
		next += pkt.Dur
		count++
	}
	if count != 24 {
		t.Fatalf("walked %d packets, want 24", count)
	}
}

func TestSyntheticOpusSeek(t *testing.T) {
	// Pad packets so the stream comfortably exceeds a few bisection windows,
	// exercising the actual granule bisection (not just the walk).
	stream, _, total := buildSyntheticOpus(312, 4, 400, 512)
	if int64(len(stream)) < 3*seekWindow {
		t.Fatalf("stream %d bytes; want > %d to exercise bisection", len(stream), 3*seekWindow)
	}
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	lateTarget := total - 5000
	for _, target := range []int64{960, total / 2, lateTarget} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek %d: %v", target, err)
		}
		// Lands at or before the target, on a packet boundary. (The exact
		// distance is bounded by the bisection byte resolution, not the sample
		// pre-roll; sample-exactness after pre-roll is covered end-to-end by
		// the ffmpeg-gated Vorbis seek test through format.Media.)
		if landed > target {
			t.Errorf("seek %d landed after target at %d", target, landed)
		}
		if landed%960 != 0 {
			t.Errorf("seek %d landed %d, not on a packet boundary", target, landed)
		}
		// A deep target must land past the stream start, proving bisection
		// narrowed rather than falling back to the first page.
		if target == lateTarget && landed == 0 {
			t.Errorf("late seek %d landed at 0: bisection did not narrow", target)
		}
		// The next packet's PTS re-anchors to the landing (regression guard for
		// the stale-d.running-after-accumulating-seek finding).
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek %d: %v", target, err)
		}
		if pkt.PTS != landed {
			t.Errorf("seek %d: first packet PTS %d != landed %d", target, pkt.PTS, landed)
		}
	}
}
