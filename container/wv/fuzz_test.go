package wv_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production, and
// seeks that never come back past their target.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "seek-mono.wv")
	f.Add(full)
	f.Add(full[:200])
	f.Add(full[:32])
	f.Add(fixture(f, "sine-s16.wv"))
	f.Add(fixture(f, "tagged.wv"))
	f.Add([]byte("wvpk\x18\x00\x00\x00\x10\x04\x00\x00"))
	// Two streams whose read outruns the length their tail gives, with no
	// step back on the way: the block that steps back is one the read never
	// reaches.
	f.Add(swappedOverrun(f))
	f.Add(loweredTail(f))
	// A first index the block behind it does not bear out: only the head
	// check keeps the open from calling one sample exact.
	f.Add(headRaised(f))
	// One whose read outruns it by a step back the read does reach.
	f.Add(append(append([]byte(nil), full...), full...))

	f.Fuzz(func(t *testing.T, data []byte) {
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return wv.NewDemuxer(src, &container.DemuxerOptions{Strict: strict})
			},
			// A block is at least its 32-byte header, so packet production is
			// bounded by the input.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/32 + 4 },
			Targets:    []int64{0, 1000, 1 << 40},
			SeekPTS:    true,
			// Landing past the target is only legitimate when the stream
			// itself starts after it.
			LateLanding: true,
			ExactLength: true,
			// An open checks the index at the head and the tail, so a read
			// holds more than it called exact only past a block between that
			// steps back (counted, less the block behind the first where the
			// head check read it), or where a total and tail that agree made
			// it exact alone and the read never delivered that tail block.
			OpenLength: true,
			LongerWhen: func(d container.Demuxer) bool {
				w := d.(*wv.Demuxer)
				return w.BacksForTest() > 0 || w.TailUnreadForTest()
			},
			After: measureLeavesTheRead,
		}.Run(t, data)
	})
}

// measureLeavesTheRead holds a measure to its contract: it fails on nothing
// a source that reads gives it, answers an exact length as the open stated
// it, and leaves the read what it is without one, packet for packet.
func measureLeavesTheRead(t testing.TB, raw []byte, _ container.Demuxer, strict bool) {
	if strict {
		return
	}
	read := func(measure bool) (pkts []container.Packet, end error) {
		d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatalf("a second open failed: %v", err)
		}
		if measure {
			n, err := d.MeasureSamples(context.Background(), 0)
			if tr := d.Tracks()[0]; err != nil || n < -1 || tr.SamplesExact && n != tr.Samples {
				t.Fatalf("a measure answered %d (%v) over a track of %d samples, exact %v", n, err, tr.Samples, tr.SamplesExact)
			}
		}
		for range len(raw)/32 + 4 {
			var pkt container.Packet
			if end = d.ReadPacket(&pkt); end != nil {
				break
			}
			pkt.Data = nil
			pkts = append(pkts, pkt)
		}
		return pkts, end
	}
	plain, plainEnd := read(false)
	measured, measuredEnd := read(true)
	if len(plain) != len(measured) || fmt.Sprint(plainEnd) != fmt.Sprint(measuredEnd) {
		t.Fatalf("a read behind a measure gave %d packets ending %v, one without %d ending %v", len(measured), measuredEnd, len(plain), plainEnd)
	}
	for i := range plain {
		if a, b := plain[i], measured[i]; a.PTS != b.PTS || a.Dur != b.Dur || a.Lost != b.Lost {
			t.Fatalf("packet %d behind a measure is at %d (%d samples, %d lost), without one at %d (%d, %d)", i, b.PTS, b.Dur, b.Lost, a.PTS, a.Dur, a.Lost)
		}
	}
}

// swappedOverrun is sine-s16 with its two audio blocks swapped and the one
// now first stating eight bytes of the next one's header as its own.
func swappedOverrun(t testing.TB) []byte {
	raw := fixture(t, "sine-s16.wv")
	s := blockStarts(t, raw)
	first := append([]byte(nil), raw[s[1]:s[2]]...)
	binary.LittleEndian.PutUint32(first[4:], binary.LittleEndian.Uint32(first[4:])+8)
	wavpack.UpdateBlockChecksum(first)
	return append(append(first, raw[:s[1]]...), raw[s[2]:]...)
}

// loweredTail is seek-mono with its last block's index lowered by 10000 and
// the block before it stating eight bytes of the last one's header.
func loweredTail(t testing.TB) []byte {
	raw := append([]byte(nil), fixture(t, "seek-mono.wv")...)
	s := blockStarts(t, raw)
	binary.LittleEndian.PutUint32(raw[s[19]+16:], 28000)
	wavpack.UpdateBlockChecksum(raw[s[19]:s[20]])
	binary.LittleEndian.PutUint32(raw[s[18]+4:], binary.LittleEndian.Uint32(raw[s[18]+4:])+8)
	return raw
}

// headRaised is seek-mono with its first block's index raised to 39999: the
// tail then gives a length of one sample, which only the head check refuses.
func headRaised(t testing.TB) []byte {
	raw := append([]byte(nil), fixture(t, "seek-mono.wv")...)
	s := blockStarts(t, raw)
	binary.LittleEndian.PutUint32(raw[s[0]+16:], 39999)
	wavpack.UpdateBlockChecksum(raw[s[0]:s[1]])
	return raw
}
