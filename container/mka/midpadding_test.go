package mka

// The mid-stream DiscardPadding fixture and the demuxer half of what it pins.
// Reading it back through format.Open needs a package that may import format,
// so the fixture is exported for the external test package and the two halves
// live either side of that line.

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
	// is the trailer's, on the last block. Raw is Blocks*BlockDur. Either trim
	// may exceed BlockDur, which is the shape a block cannot honour whole.
	Mid    int
	MidPad int64
	EndPad int64
	Raw    int64
	// Seekable writes the fixture through an io.WriteSeeker, so it carries a
	// Cues index and a seek of it takes the bounded walk.
	Seekable bool
}

// MidClamped and EndClamped are the trims as the blocks carrying them can
// honour them: a DiscardPadding rides on its block's last frame and cannot
// reach back past it.
func (p MidPadding) MidClamped() int64 { return min(p.MidPad, p.BlockDur) }
func (p MidPadding) EndClamped() int64 { return min(p.EndPad, p.BlockDur) }

// MidTrimStart is the first raw sample the mid-stream trim drops.
func (p MidPadding) MidTrimStart() int64 {
	return int64(p.Mid+1)*p.BlockDur - p.MidClamped()
}

// RawTotal is the run a finished walk counts: the packet timeline, which
// excludes the mid-stream trim, with the final block's own frames still in it.
func (p MidPadding) RawTotal() int64 { return p.Raw - p.MidClamped() }

// Samples is what a finished walk settles the length to. A final trim larger
// than its own frame still comes off in full, through the raw-end cap.
func (p MidPadding) Samples() int64 { return p.RawTotal() - min(p.EndPad, p.RawTotal()) }

// Delivered is what a linear read hands out before any walk: both trims, each
// bounded by the frame it rides on.
func (p MidPadding) Delivered() int64 { return p.Raw - p.MidClamped() - p.EndClamped() }

// PTS is the position the demuxer stamps packet i with: the timeline excludes
// every trim before it.
func (p MidPadding) PTS(i int) int64 {
	pos := int64(i) * p.BlockDur
	if i > p.Mid {
		pos -= p.MidClamped()
	}
	return pos
}

// BuildMidPadding writes a PCM stream whose middle block carries a 1 ms
// DiscardPadding and whose last carries 2 ms.
//
// The block count is what puts a cluster boundary after the padded block: the
// muxer starts a fresh cluster past 4000 ms, so 500 ten-millisecond blocks
// make two, and a seek can land on one that never decodes the trim.
func BuildMidPadding(t *testing.T) MidPadding {
	t.Helper()
	return buildMidPadding(t, MidPadding{
		Blocks:   500,
		BlockDur: 480,
		Mid:      100,
		MidPad:   48, // 1 ms at 48 kHz
		EndPad:   96, // 2 ms
	})
}

// BuildMidPaddingShape is BuildMidPadding with the two trims chosen, for the
// cells that need one larger than the block it rides on.
func BuildMidPaddingShape(t *testing.T, midPad, endPad int64) MidPadding {
	t.Helper()
	return buildMidPadding(t, MidPadding{
		Blocks: 500, BlockDur: 480, Mid: 100, MidPad: midPad, EndPad: endPad,
	})
}

// buildMidPadding muxes one fixture shape. The trims go through WritePacket,
// which is the muxer's only seam for one: the held packet carries its own
// DiscardPadding out and the final one takes the trailer's instead.
func buildMidPadding(t *testing.T, p MidPadding) MidPadding {
	t.Helper()
	p.Fmt = audio.Format{Rate: 48000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 32}
	p.Raw = int64(p.Blocks) * p.BlockDur

	track := container.Track{Codec: codec.PCM, Fmt: p.Fmt, Samples: p.Samples(), Default: true}
	buf := &bytes.Buffer{}
	ws := &memWS{}
	var w io.Writer = buf
	if p.Seekable {
		w = ws
	}
	m := NewMuxer(w, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := range p.Blocks {
		data := make([]byte, p.BlockDur*4)
		for j := range int(p.BlockDur) {
			binary.LittleEndian.PutUint32(data[j*4:], uint32(int64(i)*p.BlockDur+int64(j)))
		}
		pkt := container.Packet{Packet: codec.Packet{
			Data: data, PTS: int64(i) * p.BlockDur, Dur: p.BlockDur,
		}}
		if i == p.Mid {
			pkt.Padding = p.MidPad
		}
		if err := m.WritePacket(pkt); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(codec.Trailer{Samples: p.Samples(), Padding: p.EndPad}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if p.Seekable {
		p.File = ws.Buf
	} else {
		p.File = buf.Bytes()
	}
	return p
}

// countWarnings splits a demuxer's list by kind.
func countWarnings(ws []container.Warning) (damage, notes int) {
	for _, w := range ws {
		if w.Kind == container.Damage {
			damage++
		} else {
			notes++
		}
	}
	return damage, notes
}

// TestMidStreamDiscardPadding is the demuxer's half: each trim is reported on
// the block that carries it and nowhere else, the packet timeline excludes it,
// and a walk splits the final block's trim (the track's own Padding) from the
// ones before it (MidPadding).
//
// The three shapes are the three a block can be in. A trim that fits its last
// frame is honoured whole; one that does not trims the frame and warns, since
// nothing here can reach back into the frames already delivered before it; and
// a final trim that does not fit is still fully applied, because the raw-end
// cap takes the rest off the settled length rather than off a frame.
func TestMidStreamDiscardPadding(t *testing.T) {
	for _, tc := range []struct {
		name           string
		midPad, endPad int64
		wantDamage     int
	}{
		{"trims that fit their frames", 48, 96, 0},
		{"a mid trim larger than its block", 600, 96, 1},
		{"a final trim larger than its block", 48, 600, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := BuildMidPaddingShape(t, tc.midPad, tc.endPad)
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
				if pkt.PTS != p.PTS(i) {
					t.Fatalf("packet %d: PTS = %d, want %d (the timeline excludes the trims before it)",
						i, pkt.PTS, p.PTS(i))
				}
			}

			if err := d.Walk(); err != nil {
				t.Fatalf("Walk: %v", err)
			}
			tr := d.Tracks()[0]
			if tr.Samples != p.Samples() || !tr.SamplesExact {
				t.Errorf("after Walk: %d samples (exact %v), want %d", tr.Samples, tr.SamplesExact, p.Samples())
			}
			if want := min(p.EndPad, p.RawTotal()); tr.Padding != want {
				t.Errorf("after Walk: Padding = %d, want the final block's trim (%d)", tr.Padding, want)
			}
			if want := p.MidClamped(); tr.MidPadding != want {
				t.Errorf("after Walk: MidPadding = %d, want the inner trim (%d)", tr.MidPadding, want)
			}
			// One Damage for an oversized trim, whichever block carried it and
			// however many times the read and the walk both crossed it; one
			// Note for the inner trim, which every shape here has.
			damage, notes := countWarnings(d.Warnings())
			if damage != tc.wantDamage {
				t.Errorf("%d Damage warnings, want %d: %v", damage, tc.wantDamage, d.Warnings())
			}
			if notes != 1 {
				t.Errorf("%d Notes, want the one naming the inner trim: %v", notes, d.Warnings())
			}
		})
	}
}

// TestMidPaddingWalkRetryAfterFailure: a bounded walk that dies partway has
// advanced the trim counters as well as the sample count, so the restart must
// reset all of them. A fresh demuxer over the same bytes is the control.
func TestMidPaddingWalkRetryAfterFailure(t *testing.T) {
	p := buildMidPadding(t, MidPadding{
		Blocks: 500, BlockDur: 480, Mid: 100, MidPad: 48, EndPad: 96, Seekable: true,
	})

	clean, err := NewDemuxer(container.BytesSource(p.File), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if err := clean.Walk(); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := clean.Tracks()[0]

	f := &flakySource{src: container.BytesSource(p.File)}
	d, err := NewDemuxer(f, &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	// Resolve while reads work, so the seek below takes the bounded path: the
	// full walk is marked spent on its first attempt and would never retry.
	d.resolveCues()
	if len(d.cues) == 0 {
		t.Fatal("no Cues; the seek below would not be a bounded walk")
	}
	// Fail across the padded block, so the dead walk has counted a trim.
	f.from, f.to, f.armed = int64(len(p.File))/8, int64(len(p.File))/2, true
	if _, err := d.SeekSample(0, p.Raw/2); err == nil {
		t.Fatal("the seek succeeded through a failing source")
	}
	if d.walked {
		t.Fatal("the failed walk was the full one; this test needs the bounded path")
	}

	f.armed = false
	if err := d.Walk(); err != nil {
		t.Fatalf("retry after the source recovered: %v", err)
	}
	got := d.Tracks()[0]
	if got.Samples != want.Samples || got.Padding != want.Padding || got.MidPadding != want.MidPadding {
		t.Errorf("after the retry: %d samples, Padding %d, MidPadding %d; a clean walk settles %d/%d/%d",
			got.Samples, got.Padding, got.MidPadding, want.Samples, want.Padding, want.MidPadding)
	}
}
