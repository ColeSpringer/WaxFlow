package adts

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
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
		for _, strict := range []bool{false, true} {
			d, err := NewDemuxer(container.BytesSource(data), &DemuxerOptions{Strict: strict})
			if err != nil {
				continue
			}
			if err := d.Tracks()[0].Fmt.Valid(); err != nil {
				t.Fatalf("accepted track with invalid format: %v", err)
			}
			// The core frame is 1024 samples; a stream the first-frame probe
			// upgraded to HE-AAC delivers 2048 at the doubled rate.
			want := int64(1024)
			if d.Tracks()[0].Codec == codec.HEAAC {
				want = 2048
			}
			maxPackets := len(data)/7 + 4
			var pkt container.Packet
			for i := 0; i < maxPackets; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) || err != nil {
					break
				}
				if pkt.Dur != want {
					t.Fatalf("frame duration %d, want %d", pkt.Dur, want)
				}
			}
			for _, target := range []int64{0, 1024, 1 << 20, 1 << 40} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					continue
				}
				if landed > target {
					t.Fatalf("seek to %d overshot to %d", target, landed)
				}
			}
		}

		// The walk publishes exactly the frames a read delivers, and every
		// one of them is whole: the index is what makes a strict probe's
		// count trustworthy, so a frame it holds must fit in the source.
		w, err := NewDemuxer(container.BytesSource(data), nil)
		if err != nil {
			return
		}
		if err := w.Walk(); err != nil || !w.Walked() {
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
	})
}
