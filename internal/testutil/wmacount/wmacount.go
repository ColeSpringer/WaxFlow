// Package wmacount is the oracle the WMA v1/v2 landing tests compare seeks
// with: every media object of a stream in order, and where a decode resumed
// at it begins.
package wmacount

import (
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
)

// Object is one media object as a linear read delivers it.
type Object struct {
	PTS  int64 // the stated time, in samples
	Size int
	// Before is where a decode resumed at the object begins, in frames.
	Before int64
}

// Objects reads every object of raw under Strict and returns them with the
// stream's frame length.
//
// Each object is placed by decoding: a decode resumed at it delivers
// everything from its position to the end, so the position is what a linear
// decode delivers less that. The frame counts the demuxer under test reads
// (PacketFrames and the carry check) are checked against every such
// position and fail the test where they disagree, so a mistake in them
// cannot move the oracle with the demuxer. An object whose resumed decode
// needs state from before it cannot be measured that way and takes the
// count's position, which the measured objects either side of it bound.
func Objects(t testing.TB, raw []byte) (objs []Object, frameLen int64) {
	t.Helper()
	d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := wma.ParseConfig(d.Tracks()[0].CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	var pkts [][]byte
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, append([]byte(nil), pkt.Data...))
		objs = append(objs, Object{PTS: pkt.PTS, Size: len(pkt.Data)})
	}
	frameLen = int64(cfg.FrameLen())
	counted := countFrames(t, cfg, pkts)
	total, _ := decoded(t, cfg, pkts, 0, false)
	unmeasured := 0
	for j := range objs {
		objs[j].Before = counted[j]
		rest, ok := decoded(t, cfg, pkts, j, true)
		if !ok {
			unmeasured++
			continue
		}
		at := total - rest
		if at%frameLen != 0 {
			t.Fatalf("a decode resumed at object %d begins %d samples in, off the %d-sample frame grid", j, at, frameLen)
		}
		if at/frameLen != counted[j] {
			t.Fatalf("the frame counts put object %d at frame %d, a decode resumed there begins at frame %d",
				j, counted[j], at/frameLen)
		}
	}
	if len(objs) > 0 && unmeasured*4 > len(objs) {
		t.Fatalf("%d of %d objects need state from before them; the oracle measured too few", unmeasured, len(objs))
	}
	for j := 1; j < len(objs); j++ {
		if objs[j].Before < objs[j-1].Before {
			t.Fatalf("object %d is placed at frame %d, before object %d at %d", j, objs[j].Before, j-1, objs[j-1].Before)
		}
	}
	return objs, frameLen
}

// countFrames is each packet's position as the frame counts give it.
func countFrames(t testing.TB, cfg wma.Config, pkts [][]byte) []int64 {
	t.Helper()
	check := cfg.CarryCheck()
	at := make([]int64, len(pkts))
	var before int64
	for i, p := range pkts {
		f, ok := cfg.PacketFrames(p)
		if !ok {
			t.Fatalf("object %d has no frame count", i)
		}
		if i > 0 {
			un, err := check(pkts[i-1], p)
			if err != nil {
				t.Fatalf("object %d: %v", i-1, err)
			}
			if un {
				before--
			}
		}
		at[i] = before
		before += int64(f)
	}
	return at
}

// decoded is how many samples a decode of pkts from packet from delivers, a
// fresh decoder's or one Reset as a seek resets it. ok is false when the
// resumed decode needs state from before its landing.
func decoded(t testing.TB, cfg wma.Config, pkts [][]byte, from int, resumed bool) (int64, bool) {
	t.Helper()
	dec, err := wma.NewDecoder(cfg, cfg.Format())
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	if resumed {
		dec.Reset()
	}
	var n int64
	emit := func(b *audio.Buffer) error {
		n += int64(b.N)
		return nil
	}
	for i := from; i < len(pkts); i++ {
		if err := dec.Decode(pkts[i], emit); err != nil {
			if resumed && errors.Is(err, codec.ErrNeedsHistory) {
				return 0, false
			}
			t.Fatalf("a decode from object %d failed at object %d: %v", from, i, err)
		}
	}
	if err := dec.Drain(emit); err != nil {
		t.Fatal(err)
	}
	return n, true
}
