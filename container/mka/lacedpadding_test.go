package mka

// A laced block carrying a DiscardPadding: the shape no producer in this tree
// writes (the muxer never laces), built by rewriting a muxer-written file.
// The spec attaches the trim to the end of the block, so one larger than the
// last lace reaches back into the laces before it; ffmpeg hands it to every
// lace instead, so it is no oracle here and the expectations are the spec's.

import (
	"bytes"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/container"
)

// rawBlock is one block of a cluster as the rewriter sees it: the frame
// bytes after the block header, the header's timestamp, and the trim the
// enclosing BlockGroup stated, if any.
type rawBlock struct {
	relTime   int16
	frame     []byte
	discardNS int64
}

// splitBlockBody parses a single-frame block body written by this package's
// muxer (track 1, no lacing) into its timestamp and frame.
func splitBlockBody(t *testing.T, body []byte) rawBlock {
	t.Helper()
	_, tlen, _, ok := parseVint(body, false)
	if !ok || len(body) < tlen+3 {
		t.Fatalf("block body of %d bytes has no header", len(body))
	}
	if lacing := (body[tlen+2] >> 1) & 3; lacing != laceNone {
		t.Fatalf("the rewriter takes unlaced blocks; this one uses lacing %d", lacing)
	}
	return rawBlock{
		relTime: int16(uint16(body[tlen])<<8 | uint16(body[tlen+1])),
		frame:   body[tlen+3:],
	}
}

// clusterBlocks parses one cluster's children into its timestamp element
// and its blocks, in order.
func clusterBlocks(t *testing.T, body []byte) (timestamp []byte, blocks []rawBlock) {
	t.Helper()
	err := walkElements(body, func(id uint32, data []byte) error {
		switch id {
		case idTimestamp:
			timestamp = appendElement(nil, idTimestamp, data)
		case idSimpleBlock:
			blocks = append(blocks, splitBlockBody(t, data))
		case idBlockGroup:
			var b rawBlock
			_ = walkElements(data, func(cid uint32, cdata []byte) error {
				switch cid {
				case idBlock:
					b = splitBlockBody(t, cdata)
				case idDiscardPadding:
					b.discardNS = beInt(cdata)
				}
				return nil
			})
			blocks = append(blocks, b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cluster body: %v", err)
	}
	return timestamp, blocks
}

// xiphLaced assembles one block body lacing frames together (Xiph lacing,
// track 1), stamped with the first frame's timestamp.
func xiphLaced(rel int16, frames [][]byte, keyframe bool) []byte {
	flags := byte(laceXiph << 1)
	if keyframe {
		flags |= 0x80
	}
	b := appendVint(nil, muxTrackNumber)
	b = append(b, byte(uint16(rel)>>8), byte(uint16(rel)), flags, byte(len(frames)-1))
	for _, f := range frames[:len(frames)-1] {
		n := len(f)
		for ; n >= 255; n -= 255 {
			b = append(b, 255)
		}
		b = append(b, byte(n))
	}
	for _, f := range frames {
		b = append(b, f...)
	}
	return b
}

// laceBlocks rewrites a file this package's muxer wrote to a plain writer
// (an unknown-size Segment, no Cues, so nothing points at a byte offset) so
// that the count blocks from block first onward become one Xiph-laced block.
// With discardNS set it is a BlockGroup carrying that DiscardPadding, else a
// laced SimpleBlock. Every other block is re-emitted as it was.
func laceBlocks(t *testing.T, file []byte, first, count int, discardNS int64) []byte {
	t.Helper()
	if count < 2 {
		t.Fatalf("lacing %d blocks is not lacing", count)
	}
	body, dataOff, definite := segment(t, file)
	if definite {
		t.Fatal("the rewriter needs an unknown-size Segment; write the fixture to a plain io.Writer")
	}
	out := append([]byte(nil), file[:dataOff]...)
	index := 0
	laced := false
	for off := 0; off < len(body); {
		id, start, end := readElemAt(t, body, off)
		if id != idCluster {
			out = append(out, body[off:end]...)
			off = end
			continue
		}
		timestamp, blocks := clusterBlocks(t, body[start:end])
		cluster := append([]byte(nil), timestamp...)
		for i := 0; i < len(blocks); i++ {
			b := blocks[i]
			if index == first {
				if i+count > len(blocks) {
					t.Fatalf("blocks %d..%d cross a cluster boundary at block %d", first, first+count-1, index+len(blocks)-i)
				}
				var frames [][]byte
				for _, lb := range blocks[i : i+count] {
					frames = append(frames, lb.frame)
				}
				if discardNS != 0 {
					var group []byte
					group = appendElement(group, idBlock, xiphLaced(b.relTime, frames, false))
					group = appendElement(group, idDiscardPadding, beIntBytes(discardNS))
					cluster = appendElement(cluster, idBlockGroup, group)
				} else {
					cluster = appendElement(cluster, idSimpleBlock, xiphLaced(b.relTime, frames, true))
				}
				i += count - 1
				index += count
				laced = true
				continue
			}
			index++
			if b.discardNS != 0 {
				var group []byte
				group = appendElement(group, idBlock, blockBody(b.relTime, b.frame, 0x00))
				group = appendElement(group, idDiscardPadding, beIntBytes(b.discardNS))
				cluster = appendElement(cluster, idBlockGroup, group)
				continue
			}
			cluster = appendElement(cluster, idSimpleBlock, blockBody(b.relTime, b.frame, 0x80))
		}
		out = appendElement(out, idCluster, cluster)
		off = end
	}
	if !laced {
		t.Fatalf("block %d not found among %d blocks", first, index)
	}
	return out
}

// LacedPadding is the laced-block fixture: BuildMidPadding's PCM stream
// (mono 32-bit, every sample its own raw index) with Count blocks from First
// laced into one block carrying Trim samples of DiscardPadding, and the
// muxer's own EndPad on the last block unless the laced block is the last.
type LacedPadding struct {
	File     []byte
	Fmt      audio.Format
	Blocks   int
	BlockDur int64
	First    int
	Count    int
	Trim     int64
	EndPad   int64
}

// BuildLacedPadding builds the fixture. A laced block that ends the stream
// (first+count == blocks) carries trim as its final trim, in place of the
// muxer's end trim.
func BuildLacedPadding(t *testing.T, first, count int, trim int64) LacedPadding {
	t.Helper()
	p := buildMidPadding(t, MidPadding{Blocks: 500, BlockDur: 480, Mid: 0, MidPad: 0, EndPad: 96})
	l := LacedPadding{
		Fmt: p.Fmt, Blocks: p.Blocks, BlockDur: p.BlockDur,
		First: first, Count: count, Trim: trim, EndPad: p.EndPad,
	}
	if first+count == p.Blocks {
		l.EndPad = trim
	}
	l.File = laceBlocks(t, p.File, first, count, samplesToNs(trim, p.Fmt.Rate))
	return l
}

// Raw is the samples the frames hold.
func (l LacedPadding) Raw() int64 { return int64(l.Blocks) * l.BlockDur }

// Pads is each lace's trim as the block's DiscardPadding spreads over them,
// from the last lace backwards; a trim past the whole block stays on the
// last lace, unhonoured, which is what its Packet.Padding still reports.
func (l LacedPadding) Pads() []int64 {
	pads := make([]int64, l.Count)
	left := l.Trim
	for i := l.Count - 1; i >= 0 && left > 0; i-- {
		pads[i] = min(left, l.BlockDur)
		left -= pads[i]
	}
	pads[l.Count-1] += left
	return pads
}

// Honoured is the trim the block's laces can hold.
func (l LacedPadding) Honoured() int64 { return min(l.Trim, int64(l.Count)*l.BlockDur) }

// TrimStart is the first raw sample the block's trim drops.
func (l LacedPadding) TrimStart() int64 { return int64(l.First+l.Count)*l.BlockDur - l.Honoured() }

// Final reports whether the laced block ends the stream.
func (l LacedPadding) Final() bool { return l.First+l.Count == l.Blocks }

// Padding is the walk's end trim: the last packet's own, which for a final
// laced block is its last lace's share plus any excess past the block.
func (l LacedPadding) Padding() int64 {
	if l.Final() {
		return l.Pads()[l.Count-1]
	}
	return l.EndPad
}

// MidPadding is the walk's inner trim total: every lace's share but the
// stream's final packet's.
func (l LacedPadding) MidPadding() int64 {
	if l.Final() {
		return l.Honoured() - min(l.Pads()[l.Count-1], l.BlockDur)
	}
	return l.Honoured()
}

// MidTrims is the walk's record of those, on the raw timeline, each clamped
// to the lace it rides on: an excess past the block is not a place in the
// stream.
func (l LacedPadding) MidTrims() []container.PacketTrim {
	var out []container.PacketTrim
	for i, pad := range l.Pads() {
		if l.Final() && i == l.Count-1 {
			break
		}
		if pad = min(pad, l.BlockDur); pad > 0 {
			end := int64(l.First+i+1) * l.BlockDur
			out = append(out, container.PacketTrim{Pos: end - pad, Samples: pad})
		}
	}
	return out
}

// Samples is what a finished walk settles: every trim off in full, the
// excess of a final one through the raw-end cap.
func (l LacedPadding) Samples() int64 {
	if l.Final() {
		return l.Raw() - min(l.Trim, l.Raw())
	}
	return l.Raw() - l.Honoured() - l.EndPad
}

// Delivered is what a linear read hands out before any walk: each packet
// trimmed by what it holds.
func (l LacedPadding) Delivered() int64 {
	if l.Final() {
		return l.Raw() - l.Honoured()
	}
	return l.Raw() - l.Honoured() - l.EndPad
}

// PTS is the position packet i is stamped with: the timeline excludes the
// trims before it.
func (l LacedPadding) PTS(i int) int64 {
	pos := int64(i) * l.BlockDur
	for j, pad := range l.Pads() {
		if l.First+j < i {
			pos -= min(pad, l.BlockDur)
		}
	}
	return pos
}

// Pad is the Packet.Padding packet i reports.
func (l LacedPadding) Pad(i int) int64 {
	if i >= l.First && i < l.First+l.Count {
		return l.Pads()[i-l.First]
	}
	if i == l.Blocks-1 {
		return l.EndPad
	}
	return 0
}

// TestLacedBlockSpreadsItsTrim is the demuxer's half: a DiscardPadding on a
// laced block trims the block's end, so the last lace gives what it holds and
// the rest comes off the laces before it. Each lace reports its own share,
// the timeline excludes each as it passes, and a walk records every share
// that is not the stream's final packet's, with its position.
func TestLacedBlockSpreadsItsTrim(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, count int
		trim         int64
		wantDamage   int
	}{
		{"fits the last lace", 100, 3, 240, 0},
		{"reaches the lace before", 100, 3, 600, 0},
		{"takes every lace whole", 100, 3, 1440, 0},
		{"larger than the block", 100, 3, 1500, 1},
		{"ends the stream", 497, 3, 1000, 0},
		{"ends the stream past its block", 497, 3, 1500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := BuildLacedPadding(t, tc.first, tc.count, tc.trim)
			d, err := NewDemuxer(container.BytesSource(l.File), nil)
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			var pkt container.Packet
			for i := 0; ; i++ {
				err := d.ReadPacket(&pkt)
				if err == io.EOF {
					if i != l.Blocks {
						t.Fatalf("stream ended after %d packets, wrote %d", i, l.Blocks)
					}
					break
				}
				if err != nil {
					t.Fatalf("ReadPacket %d: %v", i, err)
				}
				if pkt.Dur != l.BlockDur {
					t.Fatalf("packet %d: Dur = %d, want %d", i, pkt.Dur, l.BlockDur)
				}
				if pkt.Padding != l.Pad(i) {
					t.Errorf("packet %d: Padding = %d, want %d", i, pkt.Padding, l.Pad(i))
				}
				if pkt.PTS != l.PTS(i) {
					t.Fatalf("packet %d: PTS = %d, want %d", i, pkt.PTS, l.PTS(i))
				}
			}

			if err := d.Walk(); err != nil {
				t.Fatalf("Walk: %v", err)
			}
			tr := d.Tracks()[0]
			if tr.Samples != l.Samples() || !tr.SamplesExact {
				t.Errorf("after Walk: %d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, l.Samples())
			}
			if tr.Padding != l.Padding() {
				t.Errorf("after Walk: Padding = %d, want the final packet's %d", tr.Padding, l.Padding())
			}
			if tr.MidPadding != l.MidPadding() {
				t.Errorf("after Walk: MidPadding = %d, want %d", tr.MidPadding, l.MidPadding())
			}
			if want := l.MidTrims(); !slices.Equal(tr.MidTrims, want) {
				t.Errorf("after Walk: MidTrims = %v, want %v", tr.MidTrims, want)
			}
			if !tr.MidTrimsComplete() {
				t.Error("after Walk: the trim list does not add up to MidPadding")
			}
			damage, notes := countWarnings(d.Warnings())
			if damage != tc.wantDamage {
				t.Errorf("%d Damage warnings, want %d: %v", damage, tc.wantDamage, d.Warnings())
			}
			wantNotes := 0
			if l.MidPadding() > 0 {
				wantNotes = 1
			}
			if notes != wantNotes {
				t.Errorf("%d Notes, want %d: %v", notes, wantNotes, d.Warnings())
			}
		})
	}
}

// TestStrictRefusalDeliversNothing: a strict demuxer refuses a block whose
// trim is larger than the block, and the refusal is where the read stops.
// The block must not be installed behind the refusal, or a caller that reads
// on gets its frames after an error and then the same refusal again, forever.
func TestStrictRefusalDeliversNothing(t *testing.T) {
	l := BuildLacedPadding(t, 100, 3, 1500)
	d, err := NewDemuxer(container.BytesSource(l.File), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	var pkt container.Packet
	n := 0
	for {
		err := d.ReadPacket(&pkt)
		if err == nil {
			n++
			continue
		}
		if err == io.EOF {
			t.Fatal("a strict read reached the end past a trim larger than its block")
		}
		break
	}
	if n != l.First {
		t.Errorf("the refusal came after %d packets, want the %d before the block", n, l.First)
	}
	for i := 0; i < 3; i++ {
		if err := d.ReadPacket(&pkt); err == nil || err == io.EOF {
			t.Fatalf("read %d after the refusal returned %v; the refused block's frames must not come out", i, err)
		}
	}
}

// TestLacedBlockWithoutATrimReadsAsUnlaced: lacing alone changes nothing a
// packet reports, which is what makes the cells above about the trim.
func TestLacedBlockWithoutATrimReadsAsUnlaced(t *testing.T) {
	p := buildMidPadding(t, MidPadding{Blocks: 500, BlockDur: 480, Mid: 0, MidPad: 0, EndPad: 96})
	laced := laceBlocks(t, p.File, 100, 4, 0)
	comparePackets(t, laced, p.File)
}

// comparePackets reads both files and asserts the packets agree field for
// field, then that their finished walks settle the same track.
func comparePackets(t *testing.T, got, want []byte) {
	t.Helper()
	dg, err := NewDemuxer(container.BytesSource(got), nil)
	if err != nil {
		t.Fatalf("NewDemuxer(got): %v", err)
	}
	dw, err := NewDemuxer(container.BytesSource(want), nil)
	if err != nil {
		t.Fatalf("NewDemuxer(want): %v", err)
	}
	var pg, pw container.Packet
	for i := 0; ; i++ {
		eg, ew := dg.ReadPacket(&pg), dw.ReadPacket(&pw)
		if eg != ew {
			t.Fatalf("packet %d: got %v, want %v", i, eg, ew)
		}
		if eg == io.EOF {
			break
		}
		if eg != nil {
			t.Fatalf("packet %d: %v", i, eg)
		}
		if pg.Dur != pw.Dur || pg.PTS != pw.PTS || pg.Padding != pw.Padding || !bytes.Equal(pg.Data, pw.Data) {
			t.Fatalf("packet %d differs: {Dur %d PTS %d Padding %d %d bytes} vs {Dur %d PTS %d Padding %d %d bytes}",
				i, pg.Dur, pg.PTS, pg.Padding, len(pg.Data), pw.Dur, pw.PTS, pw.Padding, len(pw.Data))
		}
	}
	if err := dg.Walk(); err != nil {
		t.Fatal(err)
	}
	if err := dw.Walk(); err != nil {
		t.Fatal(err)
	}
	tg, tw := dg.Tracks()[0], dw.Tracks()[0]
	if tg.Samples != tw.Samples || tg.Padding != tw.Padding || tg.MidPadding != tw.MidPadding || !slices.Equal(tg.MidTrims, tw.MidTrims) {
		t.Errorf("walks settle %d/%d/%d %v and %d/%d/%d %v",
			tg.Samples, tg.Padding, tg.MidPadding, tg.MidTrims, tw.Samples, tw.Padding, tw.MidPadding, tw.MidTrims)
	}
}

// vorbisMKA encodes a tone with the real Vorbis encoder and muxes it through
// this package's muxer to a plain writer, with pad applied as Packet.Padding
// on the packet it names (none when empty).
func vorbisMKA(t *testing.T, pads map[int]int64) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	enc, err := vorbis.NewEncoder(f, &vorbis.EncoderOptions{Quality: 3})
	if err != nil {
		t.Fatalf("vorbis.NewEncoder: %v", err)
	}
	var packets [][]byte
	emit := func(p codec.Packet) error {
		packets = append(packets, append([]byte(nil), p.Data...))
		return nil
	}
	const n = 44100
	for off := 0; off < n; off += 2048 {
		end := min(off+2048, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off
		ch := buf.ChanF(0)
		for i := range ch {
			x := float64(off+i) / 44100
			ch[i] = float32(0.5*math.Sin(2*math.Pi*440*x) + 0.25*math.Sin(2*math.Pi*3000*x))
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatalf("encode: %v", err)
		}
		audio.Put(buf)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	track := container.Track{Codec: codec.Vorbis, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: tr.Samples, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i, p := range packets {
		if err := m.WritePacket(container.Packet{Padding: pads[i], Packet: codec.Packet{Data: p, Sync: true}}); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(tr); err != nil {
		t.Fatalf("End: %v", err)
	}
	return out.Bytes()
}

// vorbisDurations reads every packet's duration off the demuxer.
func vorbisDurations(t *testing.T, file []byte) []int64 {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	var durs []int64
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return durs
		}
		if err != nil {
			t.Fatal(err)
		}
		durs = append(durs, pkt.Dur)
	}
}

// TestLacedVorbisBlockKeepsItsTiming: a Vorbis frame's duration depends on
// the block before it, so timing a laced block's frames when the block loads
// must advance that state exactly once per frame, in order. Lacing three
// blocks changes nothing about the packets, and a trim spread over two of the
// laces reads as the same stream muxed with each lace's share stated on its
// own block.
func TestLacedVorbisBlockKeepsItsTiming(t *testing.T) {
	plain := vorbisMKA(t, nil)
	durs := vorbisDurations(t, plain)
	const first, count = 6, 3
	if len(durs) < first+count+2 {
		t.Fatalf("the Vorbis fixture has %d packets; too few to lace", len(durs))
	}
	for i := first; i < first+count; i++ {
		if durs[i] <= 0 {
			t.Fatalf("packet %d has duration %d; the lace needs timed frames", i, durs[i])
		}
	}
	comparePackets(t, laceBlocks(t, plain, first, count, 0), plain)

	// A trim that takes the last lace whole and half of the one before it.
	last := first + count - 1
	trim := durs[last] + durs[last-1]/2
	rate := 44100
	laced := laceBlocks(t, plain, first, count, samplesToNs(trim, rate))
	spread := vorbisMKA(t, map[int]int64{last - 1: trim - durs[last], last: durs[last]})
	comparePackets(t, laced, spread)
}
