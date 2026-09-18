package mka_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
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

// TestOpenReadsTheHead is what opening a WebM Opus file costs. Opening one
// used to walk every cluster for the exact gapless total, so anything that
// only wanted headers read the whole file; the tail trim now rides on the
// block that carries it, so the open is a head read whatever the stream's
// length and only the number waits for Walk.
//
// Two lengths, because a bound that holds only for the shorter one is not a
// bound: the cost must not scale with the stream.
func TestOpenReadsTheHead(t *testing.T) {
	var costs [2][2]int64
	for i, n := range []int{1500, 3000} {
		raw := buildOpusFile(t, n, true)
		if int64(len(raw)) < 2*srcwin.Chunk {
			t.Fatalf("%d packets wrote %d bytes, want over two windows", n, len(raw))
		}
		d, cs := openCounted(t, raw, nil)
		t.Logf("%d packets (%d bytes): %d bytes in %d reads", n, len(raw), cs.Bytes, cs.Reads)
		if cs.Bytes > srcwin.Chunk {
			t.Errorf("an open of %d bytes read %d, want a head read", len(raw), cs.Bytes)
		}
		if d.Walked() {
			t.Error("Walked is true after an open")
		}
		tr := d.Tracks()[0]
		if !tr.SamplesAdvisory || tr.SamplesExact {
			t.Errorf("track flags advisory=%v exact=%v, want the advisory Duration", tr.SamplesAdvisory, tr.SamplesExact)
		}
		want := int64(n)*costPacketDur - costPreSkip - costPadding
		if tr.Samples != want {
			t.Errorf("opened length = %d, want the Duration's %d", tr.Samples, want)
		}
		costs[i] = [2]int64{int64(cs.Reads), cs.Bytes}

		// Anti-vacuous: the walk this open skipped is a whole-file read, so
		// the same source reads past one window once Walk runs. Without this
		// the cell above would pass on a demuxer that read nothing at all.
		if err := d.Walk(); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if cs.Bytes <= srcwin.Chunk {
			t.Errorf("open plus Walk read %d bytes; this cell cannot see the walk it is measuring", cs.Bytes)
		}
		if got := d.Tracks()[0]; got.Samples != want || !got.SamplesExact || got.SamplesAdvisory {
			t.Errorf("after Walk: %d (exact %v advisory %v), want %d exact",
				got.Samples, got.SamplesExact, got.SamplesAdvisory, want)
		}
		if got := d.Tracks()[0].Padding; got != costPadding {
			t.Errorf("after Walk: Padding = %d, want the trailer's %d", got, costPadding)
		}
	}
	if costs[0] != costs[1] {
		t.Errorf("the shorter stream opened at %v (reads, bytes) and the longer at %v; the open must not scale", costs[0], costs[1])
	}
}

// TestOpenWithNoDuration is the shape the advisory fallback cannot serve: a
// file with no Info Duration has nothing to report until the walk runs, so the
// opened track says -1 rather than guessing. Promoting it is the bug the same
// walk used to leave behind, since adoptWalkedLength only looked at the
// advisory flag.
func TestOpenWithNoDuration(t *testing.T) {
	const n = 200
	raw := buildOpusFile(t, n, false)
	d, _ := openCounted(t, raw, nil)
	if tr := d.Tracks()[0]; tr.Samples != -1 || tr.SamplesExact || tr.SamplesAdvisory {
		t.Fatalf("opened track = %d (exact %v advisory %v), want -1 with neither flag",
			tr.Samples, tr.SamplesExact, tr.SamplesAdvisory)
	}
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	got := d.Tracks()[0]
	want := int64(n)*costPacketDur - costPreSkip - costPadding
	if got.Samples != want || !got.SamplesExact {
		t.Errorf("after Walk: %d (exact %v), want %d exact", got.Samples, got.SamplesExact, want)
	}
}

// TestOpenOnVorbis covers the other codec that used to walk at open, on a real
// encode: Vorbis carries no absolute sample count in its bitstream, so its
// exact length is the walk's raw total minus the DiscardPadding tail, and an
// open of one paid for that walk exactly as an Opus open did.
func TestOpenOnVorbis(t *testing.T) {
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
			d, _ := openCounted(t, raw, nil)
			if d.Walked() {
				t.Error("Walked is true after an open")
			}
			if tr := d.Tracks()[0]; tr.SamplesExact {
				t.Errorf("opened track reports %d exact; the walk has not run", tr.Samples)
			}
			if err := d.Walk(); err != nil {
				t.Fatal(err)
			}
			got := d.Tracks()[0]
			if !got.SamplesExact || got.Samples <= 0 {
				t.Errorf("after Walk: %d (exact %v), want a measured length",
					got.Samples, got.SamplesExact)
			}
		})
	}
}

// TestReadDeliversTheGaplessRun is the contract the lazy open rests on: a read
// with no walk delivers the gapless stream, not the raw one. The tail trim is a
// DiscardPadding on the block that carries it, so it is dropped as that block
// decodes, and the total the walk would have measured never enters it.
//
// Before the per-packet trim this delivered the encoder's tail as audio,
// because the advisory total vetoes the raw-end cap.
func TestReadDeliversTheGaplessRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "seed-opus.webm"))
	if err != nil {
		t.Fatal(err)
	}
	strict, err := format.Probe(container.BytesSource(raw), "", &format.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	want := strict.Default()
	if !want.SamplesExact || want.Padding == 0 {
		t.Fatalf("strict probe = %+v, want an exact length with a tail trim", want)
	}

	med, err := format.Open(container.BytesSource(raw), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	opened := med.Info().Default()
	if !opened.SamplesAdvisory || opened.Samples <= want.Samples {
		t.Errorf("opened track = %d (advisory %v), want the Info Duration's estimate over %d",
			opened.Samples, opened.SamplesAdvisory, want.Samples)
	}
	w, ok := med.(format.Walker)
	if !ok {
		t.Fatal("a Matroska Media must implement format.Walker")
	}
	if w.Walked() {
		t.Error("Walked is true after an open")
	}

	var count int64
	buf := audio.Get(opened.Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		count += int64(buf.N)
	}
	if count != want.Samples {
		t.Errorf("read delivered %d frames with no walk, want the gapless %d", count, want.Samples)
	}

	if err := w.Walk(); err != nil {
		t.Fatal(err)
	}
	got := med.Info().Default()
	if got.Samples != want.Samples || !got.SamplesExact || got.Padding != want.Padding {
		t.Errorf("after Walk: %d samples (exact %v, padding %d), want %d (padding %d)",
			got.Samples, got.SamplesExact, got.Padding, want.Samples, want.Padding)
	}
}
