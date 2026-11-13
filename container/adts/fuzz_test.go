package adts

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux fuzzes the ADTS framing parser. Invariants: no panic, accepted
// tracks are well-formed, packet duration agrees with the stamped codec,
// packet production is bounded, and seeks never overshoot.
//
// Corpus entry cd1b2108ef2b5857 is a fuzzer find from the SBR-detection
// landing: a probe-upgraded stream whose 2048-sample durations tripped
// this harness's own stale 1024-only invariant (a test bug, not a code
// bug); it stays as a seed that exercises the DetectSBR path.
func FuzzDemux(f *testing.F) {
	for _, name := range []string{"stereo.aac", "mono.aac"} {
		full := fixture(f, name)
		f.Add(full)
		f.Add(full[:len(full)/2])
		f.Add(full[:20])
	}
	f.Add([]byte("\xff\xf1\x50\x80\x21\x1f\xfc")) // a lone header
	f.Add([]byte("ID3\x04\x00\x00\x00\x00\x00\x0a\xff\xf1\x50\x80\x21\x1f\xfc"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// The core frame is 1024 samples; a stream the first-frame probe
		// upgraded to HE-AAC delivers 2048 at the doubled rate.
		var want int64
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return NewDemuxer(src, &DemuxerOptions{Strict: strict})
			},
			MaxPackets: func(raw []byte) int64 { return int64(len(raw)/7 + 4) },
			Targets:    []int64{0, 1024, 1 << 20, 1 << 40},
			// This target stops reading at the cap and does not fail there.
			CapNotFatal: true,
			// A frame may be its header and nothing more, and its packet is empty.
			EmptyData: true,
			Opened: func(_ testing.TB, d container.Demuxer, _ bool) {
				want = 1024
				if d.Tracks()[0].Codec == codec.HEAAC {
					want = 2048
				}
			},
			Packet: func(t testing.TB, pkt *container.Packet) {
				if pkt.Dur != want {
					t.Fatalf("frame duration %d, want %d", pkt.Dur, want)
				}
			},
			After: func(t testing.TB, raw []byte, _ container.Demuxer, strict bool) {
				if !strict {
					walkCountsWhatAReadDelivers(t, raw)
				}
			},
		}.Run(t, data)
	})
}

// walkCountsWhatAReadDelivers holds a tolerant walk of data to a read of it:
// the walk publishes exactly the frames a read delivers, and every one of
// them is whole. The index is what makes a strict probe's count trustworthy,
// so a frame it holds must fit in the source.
func walkCountsWhatAReadDelivers(t testing.TB, data []byte) {
	w, err := NewDemuxer(container.BytesSource(data), nil)
	if err != nil {
		return
	}
	if err := w.Walk(context.Background()); err != nil || !w.Walked() {
		t.Fatalf("a tolerant walk of accepted bytes failed: %v (walked %v)", err, w.Walked())
	}
	// Every entry's whole frame lies inside the data. Asserted on the
	// index rather than on the packets a read delivers, because a read
	// stops at the first entry it cannot fill: the bad entry is exactly
	// the one no packet reaches.
	for i, off := range w.idx {
		h, ok := parseHeader(w.w.BytesAt(off, 9))
		if !ok {
			t.Fatalf("index entry %d at %d has no header", i, off)
		}
		if end := off + int64(h.frameLen); end > w.w.DataEnd() {
			t.Fatalf("index entry %d runs to %d, past the %d bytes of data", i, end, w.w.DataEnd())
		}
	}
	r, err := NewDemuxer(container.BytesSource(data), nil)
	if err != nil {
		t.Fatalf("the same bytes were refused on a second open: %v", err)
	}
	var pkt container.Packet
	var delivered int64
	for {
		err := r.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		delivered++
	}
	if got := w.Tracks()[0].Samples / w.spf; got != delivered {
		t.Fatalf("the walk counted %d frames, a read delivered %d", got, delivered)
	}
}
