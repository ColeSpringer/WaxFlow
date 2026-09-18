package mka_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/internal/testutil"
)

// The hand-built Opus stream these cells walk. Nothing decodes it: the walk
// reads each frame's TOC byte for its duration, so a payload of the right
// length behind a TOC 0x00 (SILK NB, one 10 ms frame) is a stream as far as
// the demuxer is concerned.
const (
	costPreSkip = 312
	// costPadding makes the trimmed count a whole number of milliseconds at
	// 48 kHz, so the Duration the muxer writes round-trips exactly and a cell
	// can compare the advisory total against the walked one without slack.
	costPadding    = 72
	costPacketDur  = 480
	costPacketSize = 200
)

// buildOpusFile writes an Opus-in-Matroska file of n packets onto a plain
// io.Writer, which is what decides whether a Duration is written at all: the
// muxer projects one from Track.Samples, and leaves the element out entirely
// when there is nothing to project and nothing to back-patch.
func buildOpusFile(t *testing.T, n int, duration bool) []byte {
	t.Helper()
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint16(head[10:], costPreSkip)
	binary.LittleEndian.PutUint32(head[12:], 48000)

	raw := int64(n) * costPacketDur
	trimmed := raw - costPreSkip - costPadding
	track := container.Track{
		Codec:       codec.Opus,
		CodecConfig: head,
		Fmt:         audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Delay:       costPreSkip,
		Default:     true,
	}
	if duration {
		track.Samples = trimmed
	}

	var buf bytes.Buffer
	m := mka.NewMuxer(&buf, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	body := make([]byte, costPacketSize)
	for i := range n {
		body[0], body[1] = 0x00, byte(i) // TOC 0x00: SILK NB, one 10 ms frame
		if err := m.WritePacket(container.Packet{Track: 0, Packet: codec.Packet{
			Data: body, Dur: costPacketDur, PTS: int64(i) * costPacketDur,
		}}); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(codec.Trailer{Samples: trimmed, Delay: costPreSkip, Padding: costPadding}); err != nil {
		t.Fatalf("End: %v", err)
	}
	return buf.Bytes()
}

func openCounted(t *testing.T, raw []byte, o *mka.DemuxerOptions) (*mka.Demuxer, *testutil.CountingSource) {
	t.Helper()
	cs := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := mka.NewDemuxer(cs, o)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	return d, cs
}

// TestDeferWalkOpensOnTheHead is what a tolerant probe of a WebM Opus file
// costs. Opening one walks every cluster for the exact gapless total, so a
// probe that only wanted headers read the whole file; DeferWalk leaves the
// walk to Walk or the first seek, and the open is a head read whatever the
// stream's length.
//
// Two lengths, because a bound that holds only for the shorter one is not a
// bound: the cost must not scale with the stream.
func TestDeferWalkOpensOnTheHead(t *testing.T) {
	var costs [2][2]int64
	for i, n := range []int{1500, 3000} {
		raw := buildOpusFile(t, n, true)
		if int64(len(raw)) < 2*srcwin.Chunk {
			t.Fatalf("%d packets wrote %d bytes, want over two windows", n, len(raw))
		}
		d, cs := openCounted(t, raw, &mka.DemuxerOptions{DeferWalk: true})
		t.Logf("%d packets (%d bytes): %d bytes in %d reads", n, len(raw), cs.Bytes, cs.Reads)
		if cs.Bytes > srcwin.Chunk {
			t.Errorf("a deferred open of %d bytes read %d, want a head read", len(raw), cs.Bytes)
		}
		if d.Walked() {
			t.Error("Walked is true after a deferred open")
		}
		tr := d.Tracks()[0]
		if !tr.SamplesAdvisory || tr.SamplesExact {
			t.Errorf("track flags advisory=%v exact=%v, want the advisory Duration", tr.SamplesAdvisory, tr.SamplesExact)
		}
		if want := int64(n)*costPacketDur - costPreSkip - costPadding; tr.Samples != want {
			t.Errorf("deferred length = %d, want the Duration's %d", tr.Samples, want)
		}
		costs[i] = [2]int64{int64(cs.Reads), cs.Bytes}

		// The default open is what this measures against: it walks, so it
		// reads past one window, and its count is what the walk settles on.
		full, fcs := openCounted(t, raw, nil)
		if fcs.Bytes <= srcwin.Chunk {
			t.Errorf("a default open read %d bytes; this cell cannot see the walk it is measuring", fcs.Bytes)
		}
		exact := full.Tracks()[0]
		if !exact.SamplesExact {
			t.Fatal("a default open of an Opus track must measure it")
		}
		if err := d.Walk(); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if got := d.Tracks()[0]; got.Samples != exact.Samples || !got.SamplesExact || got.SamplesAdvisory {
			t.Errorf("after Walk: %d (exact %v advisory %v), want the default open's %d exact",
				got.Samples, got.SamplesExact, got.SamplesAdvisory, exact.Samples)
		}
	}
	if costs[0] != costs[1] {
		t.Errorf("the shorter stream opened at %v (reads, bytes) and the longer at %v; the open must not scale", costs[0], costs[1])
	}
}

// TestDeferWalkWithNoDuration is the shape the advisory fallback cannot
// serve: a file with no Info Duration has nothing to report until the walk
// runs, so the deferred track says -1 rather than guessing. Promoting it is
// the bug the same walk used to leave behind, since adoptWalkedLength only
// looked at the advisory flag.
func TestDeferWalkWithNoDuration(t *testing.T) {
	raw := buildOpusFile(t, 200, false)
	d, _ := openCounted(t, raw, &mka.DemuxerOptions{DeferWalk: true})
	if tr := d.Tracks()[0]; tr.Samples != -1 || tr.SamplesExact || tr.SamplesAdvisory {
		t.Fatalf("deferred track = %d (exact %v advisory %v), want -1 with neither flag",
			tr.Samples, tr.SamplesExact, tr.SamplesAdvisory)
	}
	full, _ := openCounted(t, raw, nil)
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	got, want := d.Tracks()[0], full.Tracks()[0]
	if got.Samples != want.Samples || !got.SamplesExact {
		t.Errorf("after Walk: %d (exact %v), want the default open's %d exact", got.Samples, got.SamplesExact, want.Samples)
	}
}

// TestDeferWalkOnVorbis covers the other codec that walks at open, on a real
// encode: Vorbis carries no absolute sample count in its bitstream, so its
// exact length is the walk's rawTotal minus the DiscardPadding tail, and a
// probe of one paid for that walk exactly as an Opus probe did.
func TestDeferWalkOnVorbis(t *testing.T) {
	dir := t.TempDir()
	for _, s := range specs {
		if s.codec != codec.Vorbis {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			raw, err := os.ReadFile(gen(t, dir, s))
			if err != nil {
				t.Fatal(err)
			}
			d, _ := openCounted(t, raw, &mka.DemuxerOptions{DeferWalk: true})
			if d.Walked() {
				t.Error("Walked is true after a deferred open")
			}
			if tr := d.Tracks()[0]; tr.SamplesExact {
				t.Errorf("deferred track reports %d exact; the walk has not run", tr.Samples)
			}
			full, _ := openCounted(t, raw, nil)
			if err := d.Walk(); err != nil {
				t.Fatal(err)
			}
			got, want := d.Tracks()[0], full.Tracks()[0]
			if !want.SamplesExact {
				t.Fatal("a default open of a Vorbis track must measure it")
			}
			if got.Samples != want.Samples || !got.SamplesExact {
				t.Errorf("after Walk: %d (exact %v), want the default open's %d exact",
					got.Samples, got.SamplesExact, want.Samples)
			}
		})
	}
}
