package mka

// The fixture the cut-short and junk cells damage, and what they assert with:
// a PCM stream whose every sample is its own index, so a delivered packet
// names the source samples it carries.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

const (
	indexRate   = 8000
	indexBlock  = 800 // samples a block, 100 ms
	indexBlocks = 200 // five clusters: the muxer starts one past 4000 ms
)

// IndexPCM is a mono 32-bit PCM stream at 8 kHz, indexBlocks blocks of
// indexBlock samples, sample i holding i. Seekable writes it through an
// io.WriteSeeker, which gives the Segment a size and the file Cues; otherwise
// the Segment states no size and there are no Cues.
func IndexPCM(t testing.TB, seekable bool) []byte {
	t.Helper()
	f := audio.Format{Rate: indexRate, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 32}
	track := container.Track{Codec: codec.PCM, Fmt: f, Samples: indexBlocks * indexBlock, Default: true}
	var w io.Writer
	buf, ws := &bytes.Buffer{}, &memWS{}
	w = buf
	if seekable {
		w = ws
	}
	m := NewMuxer(w, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := range indexBlocks {
		data := make([]byte, indexBlock*4)
		for j := range indexBlock {
			binary.LittleEndian.PutUint32(data[j*4:], uint32(i*indexBlock+j))
		}
		if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: data, PTS: int64(i * indexBlock), Dur: indexBlock}}); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(codec.Trailer{Samples: indexBlocks * indexBlock}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if seekable {
		return ws.Buf
	}
	return buf.Bytes()
}

// span is one element's extent in a file: its first byte and one past its
// data.
type span struct{ start, end int }

// segmentChildren lists the Segment's children in a clean file, and the
// SimpleBlocks inside its clusters.
func segmentChildren(t testing.TB, file []byte) (children map[uint32][]span, blocks []span) {
	t.Helper()
	children = map[uint32][]span{}
	read := func(off, end int) (uint32, int, int) {
		id, idLen, _, ok := parseVint(file[off:end], true)
		if !ok {
			t.Fatalf("no element ID at %d", off)
		}
		size, sizeLen, unknown, ok := parseVint(file[off+idLen:end], false)
		if !ok {
			t.Fatalf("no element size at %d", off)
		}
		data := off + idLen + sizeLen
		if unknown {
			return uint32(id), data, end
		}
		return uint32(id), data, data + int(size)
	}
	_, _, headEnd := read(0, len(file))
	_, segData, segEnd := read(headEnd, len(file))
	for off := segData; off < segEnd; {
		id, data, end := read(off, segEnd)
		children[id] = append(children[id], span{off, end})
		if id == idCluster {
			for c := data; c < end; {
				cid, _, cend := read(c, end)
				if cid == idSimpleBlock {
					blocks = append(blocks, span{c, cend})
				}
				c = cend
			}
		}
		off = end
	}
	if len(children[idCluster]) < 5 || len(blocks) != indexBlocks {
		t.Fatalf("fixture has %d clusters and %d blocks, want 5 and %d", len(children[idCluster]), len(blocks), indexBlocks)
	}
	return children, blocks
}

// readRun drains d, requiring every packet to be the next whole block of
// the index stream from block first, and returns how many there were and the
// error the read ended with (nil for io.EOF).
func readRun(t *testing.T, d *Demuxer, first int) (int, error) {
	t.Helper()
	n := 0
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		want := int64(first+n) * indexBlock
		if pkt.PTS != want || pkt.Dur != indexBlock || len(pkt.Data) != indexBlock*4 {
			t.Fatalf("packet %d: pts %d dur %d, %d bytes; want block %d at %d", n, pkt.PTS, pkt.Dur, len(pkt.Data), first+n, want)
		}
		if v := binary.LittleEndian.Uint32(pkt.Data); int64(v) != want {
			t.Fatalf("packet %d carries sample %d, want %d", n, v, want)
		}
		if v := binary.LittleEndian.Uint32(pkt.Data[len(pkt.Data)-4:]); int64(v) != want+indexBlock-1 {
			t.Fatalf("packet %d ends on sample %d, want %d", n, v, want+indexBlock-1)
		}
		n++
	}
}

// damaged is what a cell expects of a damaged file: the whole blocks every
// path delivers, and how a strict open answers.
type damaged struct {
	file   []byte
	blocks int // blocks a tolerant read delivers, from the first
	// strictOpen says a strict open refuses; otherwise a strict read
	// delivers strictBlocks and then refuses, as does a strict walk.
	strictOpen   bool
	strictBlocks int
}

// checkDamaged holds a damaged file to one answer on every path: a tolerant
// read delivers blocks [0, want.blocks) and lists one Damage; a walk settles
// that length exactly; a seek to any sample lands at or before it on a block
// and reads on with exactly the blocks the read delivered from there; a strict
// open or read refuses as malformed.
func checkDamaged(t *testing.T, want damaged) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(want.file), nil)
	if err != nil {
		t.Fatalf("a tolerant open refused: %v", err)
	}
	n, err := readRun(t, d, 0)
	if err != nil {
		t.Fatalf("the tolerant read failed after %d blocks: %v", n, err)
	}
	if n != want.blocks {
		t.Fatalf("the tolerant read delivered %d blocks, want %d", n, want.blocks)
	}
	if dmg, _ := countWarnings(d.Warnings()); dmg != 1 || container.DamageOf(d) != 1 {
		t.Errorf("listed %d Damage warnings and counted %d, want 1: %v", dmg, container.DamageOf(d), d.Warnings())
	}

	w, err := NewDemuxer(container.BytesSource(want.file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Walk(context.Background()); err != nil || !w.Walked() {
		t.Fatalf("Walk = %v, Walked %v", err, w.Walked())
	}
	if tr := w.Tracks()[0]; tr.Samples != int64(want.blocks)*indexBlock || !tr.SamplesExact {
		t.Errorf("the walk settled %d samples (exact %v), want the %d the read delivered", tr.Samples, tr.SamplesExact, want.blocks*indexBlock)
	}

	total := int64(want.blocks) * indexBlock
	for _, target := range []int64{0, total / 3, total / 2, total - 1, total, total + 5*indexBlock} {
		target = max(target, 0)
		s, err := NewDemuxer(container.BytesSource(want.file), nil)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := s.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target || landed%indexBlock != 0 || landed > total {
			t.Fatalf("seek to %d landed at %d", target, landed)
		}
		first := int(landed / indexBlock)
		n, err := readRun(t, s, first)
		if err != nil || first+n != want.blocks {
			t.Errorf("after a seek to %d (landed %d) the read delivered blocks %d to %d (%v), want to %d",
				target, landed, first, first+n, err, want.blocks)
		}
	}

	strict, err := NewDemuxer(container.BytesSource(want.file), &container.DemuxerOptions{Strict: true})
	if want.strictOpen {
		if !errors.Is(err, waxerr.ErrMalformedInput) {
			t.Errorf("a strict open = %v, want malformed", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("a strict open refused: %v", err)
	}
	if n, err := readRun(t, strict, 0); !errors.Is(err, waxerr.ErrMalformedInput) || n != want.strictBlocks {
		t.Errorf("a strict read delivered %d blocks and ended %v, want %d and malformed", n, err, want.strictBlocks)
	}
	sw, err := NewDemuxer(container.BytesSource(want.file), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Walk(context.Background()); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a strict walk = %v, want malformed", err)
	}
}

// wholeBefore counts the blocks whose element ends at or before cut.
func wholeBefore(blocks []span, cut int) int {
	n := 0
	for _, b := range blocks {
		if b.end <= cut {
			n++
		}
	}
	return n
}

// unsizeClusters returns file with every Cluster's size rewritten as the
// unknown size of the same width, so each runs to the next Segment-level
// element, as a live stream writes them. It returns nil for a file it cannot
// walk.
func unsizeClusters(file []byte) []byte {
	out := append([]byte(nil), file...)
	read := func(off, end int) (id uint32, sizeOff, sizeLen, data, next int, ok bool) {
		v, idLen, _, ok1 := parseVint(out[off:end], true)
		size, sl, unknown, ok2 := parseVint(out[off+idLen:end], false)
		if !ok1 || !ok2 {
			return 0, 0, 0, 0, 0, false
		}
		data = off + idLen + sl
		next = end
		if !unknown && size <= uint64(end-data) {
			next = data + int(size)
		}
		return uint32(v), off + idLen, sl, data, next, true
	}
	_, _, _, _, headEnd, ok := read(0, len(out))
	if !ok {
		return nil
	}
	_, _, _, segData, segEnd, ok := read(headEnd, len(out))
	if !ok {
		return nil
	}
	for off := segData; off < segEnd; {
		id, sizeOff, sizeLen, _, next, ok := read(off, segEnd)
		if !ok {
			return nil
		}
		if id == idCluster {
			out[sizeOff] = 0xFF >> (sizeLen - 1)
			for i := 1; i < sizeLen; i++ {
				out[sizeOff+i] = 0xFF
			}
		}
		off = next
	}
	return out
}

// checkBlocks holds a damaged file to one answer on every path, where the
// blocks that survive need not run unbroken: a tolerant read delivers exactly
// the blocks want names, in order; a walk settles their length; a seek lands
// on a delivered block and reads on with the blocks the read delivered from
// there; a strict read refuses as malformed.
func checkBlocks(t *testing.T, file []byte, want []int) {
	t.Helper()
	values := func(d *Demuxer) []int {
		var out []int
		for {
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				if err != io.EOF {
					t.Fatalf("ReadPacket: %v", err)
				}
				return out
			}
			out = append(out, int(binary.LittleEndian.Uint32(pkt.Data))/indexBlock)
		}
	}
	open := func(strict bool) *Demuxer {
		d, err := NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: strict})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := open(false)
	if got := values(d); !slices.Equal(got, want) {
		t.Fatalf("read blocks %v, want %v", got, want)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != int64(len(want))*indexBlock || !tr.SamplesExact {
		t.Errorf("the walk settled %d samples, the read delivered %d", tr.Samples, len(want)*indexBlock)
	}
	for _, target := range []int64{0, int64(len(want)) * indexBlock / 2, int64(len(want)-1) * indexBlock} {
		s := open(false)
		landed, err := s.SeekSample(context.Background(), 0, target)
		if err != nil || landed > target || landed%indexBlock != 0 {
			t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
		}
		from := int(landed / indexBlock)
		if got := values(s); !slices.Equal(got, want[from:]) {
			t.Errorf("after a seek to %d (delivered block %d): blocks %v, want %v", target, from, got, want[from:])
		}
	}
	if _, err := readRun(t, open(true), 0); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a strict read = %v, want malformed", err)
	}
}
