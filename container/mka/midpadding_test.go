package mka

// The mid-stream DiscardPadding fixture and the demuxer half of what it pins.
// Building it needs emitBlock (the muxer only ever writes a trim on the final
// block, which is the only one an output container can express), and reading it
// back through format.Open needs a package that may import format, so the
// fixture is exported for the external test package and the two halves live
// either side of that line.

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// MidPadding is the fixture and the numbers a cell asserts against.
type MidPadding struct {
	File []byte
	// Fmt, Blocks and BlockDur describe the raw stream: mono 32-bit PCM whose
	// every sample equals its own raw index, so a delivered value names the
	// source sample it came from.
	Fmt      audio.Format
	Blocks   int
	BlockDur int64
	// Mid is the block carrying a mid-stream trim, MidPad its samples; EndPad
	// is the trailer's, on the last block. Raw is Blocks*BlockDur.
	Mid    int
	MidPad int64
	EndPad int64
	Raw    int64
}

// MidTrimStart is the first raw sample the mid-stream trim drops.
func (p MidPadding) MidTrimStart() int64 { return int64(p.Mid+1)*p.BlockDur - p.MidPad }

// Samples is what a finished walk settles the length to.
func (p MidPadding) Samples() int64 { return p.Raw - p.MidPad - p.EndPad }

// BuildMidPadding writes a PCM stream whose middle block carries a 1 ms
// DiscardPadding and whose last carries 2 ms.
//
// The block count is what puts a cluster boundary after the padded block: the
// muxer starts a fresh cluster past 4000 ms, so 500 ten-millisecond blocks
// make two, and a seek can land on one that never decodes the trim.
func BuildMidPadding(t *testing.T) MidPadding {
	t.Helper()
	p := MidPadding{
		Fmt:      audio.Format{Rate: 48000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 32},
		Blocks:   500,
		BlockDur: 480,
		Mid:      100,
		MidPad:   48, // 1 ms at 48 kHz
		EndPad:   96, // 2 ms
	}
	p.Raw = int64(p.Blocks) * p.BlockDur

	track := container.Track{Codec: codec.PCM, Fmt: p.Fmt, Samples: p.Samples(), Default: true}
	var buf bytes.Buffer
	m := NewMuxer(&buf, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := range p.Blocks {
		data := make([]byte, p.BlockDur*4)
		for j := range int(p.BlockDur) {
			binary.LittleEndian.PutUint32(data[j*4:], uint32(int64(i)*p.BlockDur+int64(j)))
		}
		pkt := codec.Packet{Data: data, PTS: int64(i) * p.BlockDur, Dur: p.BlockDur}
		if i != p.Mid {
			if err := m.WritePacket(container.Packet{Packet: pkt}); err != nil {
				t.Fatalf("WritePacket %d: %v", i, err)
			}
			continue
		}
		// The held packet goes out first so the injected one is its own block,
		// then the block carrying the trim, by hand: WritePacket has no seam
		// for a mid-stream DiscardPadding and should not grow one.
		if m.havePending {
			if err := m.emitBlock(m.pending, 0); err != nil {
				t.Fatalf("emitBlock %d: %v", i-1, err)
			}
			m.havePending = false
		}
		if err := m.emitBlock(pkt, samplesToNs(p.MidPad, m.rate)); err != nil {
			t.Fatalf("emitBlock %d: %v", i, err)
		}
		m.rawSamples += pkt.Dur
	}
	if err := m.End(codec.Trailer{Samples: p.Samples(), Padding: p.EndPad}); err != nil {
		t.Fatalf("End: %v", err)
	}
	p.File = buf.Bytes()
	return p
}

// TestMidStreamDiscardPadding is the demuxer's half: the trim is reported on
// the block that carries it and nowhere else, and a walk sums both into the
// track's own Padding before settling the length.
func TestMidStreamDiscardPadding(t *testing.T) {
	p := BuildMidPadding(t)
	d, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	var pkt container.Packet
	for i := 0; ; i++ {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			if i != p.Blocks {
				t.Fatalf("stream ended after %d packets, wrote %d", i, p.Blocks)
			}
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket %d: %v", i, err)
		}
		want := int64(0)
		switch i {
		case p.Mid:
			want = p.MidPad
		case p.Blocks - 1:
			want = p.EndPad
		}
		if pkt.Padding != want {
			t.Errorf("packet %d: Padding = %d, want %d", i, pkt.Padding, want)
		}
	}

	if err := d.Walk(); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	tr := d.Tracks()[0]
	if tr.Samples != p.Samples() || !tr.SamplesExact {
		t.Errorf("after Walk: %d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, p.Samples())
	}
	if want := p.MidPad + p.EndPad; tr.Padding != want {
		t.Errorf("after Walk: Padding = %d, want both trims summed (%d)", tr.Padding, want)
	}
}
