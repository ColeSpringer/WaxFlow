package mka

// Structural coverage of Info > Duration, the SeekHead and the Cues, asserted
// over the written bytes with the in-package parser: a layout mistake a
// decode-and-compare would not see fails here.

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// readElemAt parses the element header at off. An unknown or overrunning size
// runs to the end of buf.
func readElemAt(t *testing.T, buf []byte, off int) (id uint32, start, end int) {
	t.Helper()
	if off < 0 || off >= len(buf) {
		t.Fatalf("element offset %d outside a %d-byte buffer", off, len(buf))
	}
	id64, idLen, _, ok := parseVint(buf[off:], true)
	if !ok {
		t.Fatalf("bad element ID at %d", off)
	}
	size, sizeLen, unknown, ok := parseVint(buf[off+idLen:], false)
	if !ok {
		t.Fatalf("bad element size at %d", off)
	}
	start = off + idLen + sizeLen
	end = len(buf)
	if !unknown && size <= uint64(len(buf)-start) {
		end = start + int(size)
	}
	return uint32(id64), start, end
}

// segment returns the Segment's body, the absolute offset that every
// SeekPosition and CueClusterPosition is relative to, and whether its size was
// declared rather than left unknown.
func segment(t *testing.T, file []byte) (body []byte, dataOff int, definite bool) {
	t.Helper()
	id, _, end := readElemAt(t, file, 0)
	if id != idEBML {
		t.Fatalf("file does not open with an EBML header (id %#x)", id)
	}
	id, start, segEnd := readElemAt(t, file, end)
	if id != idSegment {
		t.Fatalf("no Segment after the EBML header (id %#x)", id)
	}
	// The size vint sits between the 4-byte Segment ID and its data.
	_, _, unknown, _ := parseVint(file[end+4:], false)
	return file[start:segEnd], start, !unknown
}

// segChild finds the first Segment-level child with the given ID, returning its
// body and its position relative to the segment's data start.
func segChild(t *testing.T, file []byte, want uint32) (body []byte, pos int, found bool) {
	t.Helper()
	seg, _, _ := segment(t, file)
	for off := 0; off < len(seg); {
		id, start, end := readElemAt(t, seg, off)
		if id == want {
			return seg[start:end], off, true
		}
		if end <= off {
			t.Fatalf("element %#x at %d made no progress", id, off)
		}
		off = end
	}
	return nil, 0, false
}

// infoDuration returns the Duration the file's Info element declares, in
// TimestampScale ticks.
func infoDuration(t *testing.T, file []byte) (float64, bool) {
	t.Helper()
	info, _, ok := segChild(t, file, idInfo)
	if !ok {
		t.Fatal("no Info element")
	}
	var ticks float64
	found := false
	if err := walkElements(info, func(id uint32, data []byte) error {
		if id == idDuration {
			v, ok := beFloat(data)
			if !ok {
				t.Fatalf("Duration body is %d bytes, not a 4- or 8-byte float", len(data))
			}
			ticks, found = v, true
		}
		return nil
	}); err != nil {
		t.Fatalf("walking Info: %v", err)
	}
	return ticks, found
}

// seekPoint is one parsed SeekHead entry.
type seekPoint struct {
	target uint32
	pos    int64
}

// seekHeadEntries returns the entries and the count of Void elements standing
// in for entries that were reserved and never filled.
func seekHeadEntries(t *testing.T, file []byte) (pts []seekPoint, voids int) {
	t.Helper()
	sh, _, ok := segChild(t, file, idSeekHead)
	if !ok {
		t.Fatal("no SeekHead element")
	}
	if err := walkElements(sh, func(id uint32, data []byte) error {
		switch id {
		case idVoid:
			voids++
			// It must be exactly as wide as the entry it replaced.
			if got := len(data) + 2; got != seekEntryLen {
				t.Errorf("Void in the SeekHead spans %d bytes, want %d", got, seekEntryLen)
			}
		case idSeek:
			var p seekPoint
			_ = walkElements(data, func(cid uint32, cdata []byte) error {
				switch cid {
				case idSeekID:
					p.target = uint32(beUint(cdata))
				case idSeekPosition:
					if len(cdata) != 8 {
						t.Errorf("SeekPosition is %d bytes, want the fixed 8 a patch needs", len(cdata))
					}
					p.pos = int64(beUint(cdata))
				}
				return nil
			})
			pts = append(pts, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the SeekHead: %v", err)
	}
	return pts, voids
}

// pcmCase builds a PCM track and its packets.
func pcmCase(t *testing.T, packets int, samples int64) (container.Track, []codec.Packet) {
	t.Helper()
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	track := container.Track{Codec: codec.PCM, Fmt: f, Samples: samples, Default: true}
	var pkts []codec.Packet
	for i := 0; i < packets; i++ {
		data := make([]byte, 480*4)
		for j := range data {
			data[j] = byte(i*13 + j*5)
		}
		pkts = append(pkts, codec.Packet{Data: data, PTS: int64(i * 480), Dur: 480})
	}
	return track, pkts
}

// TestMuxLayoutConstants pins the fixed widths the back-patches depend on: a
// patch at the wrong offset is invisible until a reader chokes.
func TestMuxLayoutConstants(t *testing.T) {
	entry := appendSeekEntry(nil, idCues, 0x0102030405)
	if len(entry) != seekEntryLen {
		t.Fatalf("a Seek entry is %d bytes, seekEntryLen says %d", len(entry), seekEntryLen)
	}
	if got := beUint(entry[seekPosValueOff : seekPosValueOff+8]); got != 0x0102030405 {
		t.Errorf("SeekPosition value at +%d reads %#x, want %#x", seekPosValueOff, got, 0x0102030405)
	}
	if got := len(appendFloat(nil, idDuration, 1)); got != durationLen {
		t.Errorf("a Duration element is %d bytes, durationLen says %d", got, durationLen)
	}
	if got := len(appendVoid(nil, durationLen)); got != durationLen {
		t.Errorf("the Duration Void spans %d bytes, want %d", got, durationLen)
	}
	if got := len(unknownSizeVint); got != 8 {
		t.Errorf("the unknown Segment size is %d bytes; the definite patch writes 8", got)
	}
}

// TestMuxDuration covers the Duration slot's states: from the projection,
// absent, and repatched to the trailer.
func TestMuxDuration(t *testing.T) {
	const rate = 48000

	t.Run("projection on a plain writer", func(t *testing.T) {
		track, pkts := pcmCase(t, 6, 6*480)
		file := muxToBytes(t, track, nil, pkts, codec.Trailer{Samples: 6 * 480})
		ticks, ok := infoDuration(t, file)
		if !ok {
			t.Fatal("no Duration written from a known projection")
		}
		if want := durationTicks(6*480, rate); ticks != want {
			t.Errorf("Duration = %v ticks, want %v", ticks, want)
		}
	})

	t.Run("absent when unknown", func(t *testing.T) {
		track, pkts := pcmCase(t, 6, -1)
		file := muxToBytes(t, track, nil, pkts, codec.Trailer{Samples: 6 * 480})
		if _, ok := infoDuration(t, file); ok {
			t.Error("a Duration appeared with no projection and no way to patch one")
		}
	})

	t.Run("patched to the trailer", func(t *testing.T) {
		// A deliberately wrong projection: End must overwrite it.
		const exact = 4801
		track, pkts := pcmCase(t, 6, 9999)
		file := muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: exact})
		ticks, ok := infoDuration(t, file)
		if !ok {
			t.Fatal("no Duration on a seekable writer")
		}
		if want := durationTicks(exact, rate); ticks != want {
			t.Errorf("Duration = %v ticks, want the trailer's %v", ticks, want)
		}
	})

	t.Run("reserved then patched", func(t *testing.T) {
		// No projection, so Begin reserves a Void and End fills it.
		const exact = 2880
		track, pkts := pcmCase(t, 6, -1)
		file := muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: exact})
		ticks, ok := infoDuration(t, file)
		if !ok {
			t.Fatal("the reserved Duration slot was never filled")
		}
		if want := durationTicks(exact, rate); ticks != want {
			t.Errorf("Duration = %v ticks, want %v", ticks, want)
		}
	})
}

// TestMuxSeekHeadResolves follows every entry: asserting the numbers instead
// would pass on an off-by-one in the position arithmetic.
func TestMuxSeekHeadResolves(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seekable bool
		wantN    int
	}{
		{"streaming", false, 2},
		{"seekable", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track, pkts := pcmCase(t, 6, 6*480)
			mux := muxToBytes
			if tc.seekable {
				mux = muxToSeekable
			}
			file := mux(t, track, nil, pkts, codec.Trailer{Samples: 6 * 480})
			pts, voids := seekHeadEntries(t, file)
			if len(pts) != tc.wantN || voids != 0 {
				t.Fatalf("SeekHead has %d entries and %d voids, want %d and 0", len(pts), voids, tc.wantN)
			}
			seg, dataOff, _ := segment(t, file)
			for _, p := range pts {
				if p.pos < 0 || p.pos >= int64(len(seg)) {
					t.Errorf("%#x points at %d, outside the %d-byte segment", p.target, p.pos, len(seg))
					continue
				}
				id, _, _ := readElemAt(t, seg, int(p.pos))
				if id != p.target {
					t.Errorf("SeekHead points %#x at segment offset %d (file %d), which holds %#x",
						p.target, p.pos, int64(dataOff)+p.pos, id)
				}
			}
		})
	}
}

// TestMuxCues: one CuePoint per cluster, times matching the cluster
// timestamps, every position landing on a Cluster ID.
func TestMuxCues(t *testing.T) {
	// 12 s, past maxClusterMs, so there are several clusters to index.
	track, pkts := pcmCase(t, 1200, 1200*480)
	file := muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: 1200 * 480})
	seg, _, _ := segment(t, file)

	// The clusters in file order, with their timestamps.
	var clusterPos []int64
	var clusterTimes []int64
	for off := 0; off < len(seg); {
		id, start, end := readElemAt(t, seg, off)
		if id == idCluster {
			clusterPos = append(clusterPos, int64(off))
			ts := int64(-1)
			_ = walkElements(seg[start:end], func(cid uint32, data []byte) error {
				if cid == idTimestamp && ts < 0 {
					ts = int64(beUint(data))
				}
				return nil
			})
			clusterTimes = append(clusterTimes, ts)
		}
		off = end
	}
	if len(clusterPos) < 3 {
		t.Fatalf("the fixture produced %d clusters; too few to index meaningfully", len(clusterPos))
	}

	cues, _, ok := segChild(t, file, idCues)
	if !ok {
		t.Fatal("no Cues element on a seekable writer")
	}
	var gotPos, gotTimes []int64
	if err := walkElements(cues, func(id uint32, data []byte) error {
		if id != idCuePoint {
			t.Errorf("unexpected %#x among the CuePoints", id)
			return nil
		}
		time, pos, track := int64(-1), int64(-1), uint64(0)
		_ = walkElements(data, func(cid uint32, cdata []byte) error {
			switch cid {
			case idCueTime:
				time = int64(beUint(cdata))
			case idCueTrackPositions:
				_ = walkElements(cdata, func(gid uint32, gdata []byte) error {
					switch gid {
					case idCueTrack:
						track = beUint(gdata)
					case idCueClusterPosition:
						pos = int64(beUint(gdata))
					}
					return nil
				})
			}
			return nil
		})
		if track != muxTrackNumber {
			t.Errorf("CueTrack = %d, want %d", track, muxTrackNumber)
		}
		gotTimes = append(gotTimes, time)
		gotPos = append(gotPos, pos)
		return nil
	}); err != nil {
		t.Fatalf("walking Cues: %v", err)
	}

	if len(gotPos) != len(clusterPos) {
		t.Fatalf("%d cue points for %d clusters", len(gotPos), len(clusterPos))
	}
	for i := range gotPos {
		if gotPos[i] != clusterPos[i] {
			t.Errorf("cue %d points at %d, cluster is at %d", i, gotPos[i], clusterPos[i])
			continue
		}
		if id, _, _ := readElemAt(t, seg, int(gotPos[i])); id != idCluster {
			t.Errorf("cue %d points at %#x, not a Cluster", i, id)
		}
		if gotTimes[i] != clusterTimes[i] {
			t.Errorf("cue %d says time %d, the cluster says %d", i, gotTimes[i], clusterTimes[i])
		}
	}
}

// TestMuxSeekableLayout pins what only the seekable path produces: a definite
// Segment size and a Cues SeekPosition that resolves.
func TestMuxSeekableLayout(t *testing.T) {
	track, pkts := pcmCase(t, 1200, 1200*480)
	file := muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: 1200 * 480})

	seg, dataOff, definite := segment(t, file)
	if !definite {
		t.Error("the Segment kept its unknown size on a seekable writer")
	}
	if want := len(file) - dataOff; len(seg) != want {
		t.Errorf("the Segment declares %d bytes, %d follow it", len(seg), want)
	}

	_, cuesPos, ok := segChild(t, file, idCues)
	if !ok {
		t.Fatal("no Cues element")
	}
	pts, _ := seekHeadEntries(t, file)
	found := false
	for _, p := range pts {
		if p.target != idCues {
			continue
		}
		found = true
		if p.pos != int64(cuesPos) {
			t.Errorf("the Cues SeekPosition is %d, the Cues element is at %d", p.pos, cuesPos)
		}
	}
	if !found {
		t.Error("no SeekHead entry points at the Cues")
	}
}

// TestMuxStreamingHasNoCues: a plain io.Writer gets everything that needs no
// rewind and nothing that does.
func TestMuxStreamingHasNoCues(t *testing.T) {
	track, pkts := pcmCase(t, 1200, 1200*480)
	file := muxToBytes(t, track, nil, pkts, codec.Trailer{Samples: 1200 * 480})
	if _, _, ok := segChild(t, file, idCues); ok {
		t.Error("a plain writer got a Cues element nothing can point at")
	}
	if _, _, definite := segment(t, file); definite {
		t.Error("a plain writer got a definite Segment size")
	}
	if _, ok := infoDuration(t, file); !ok {
		t.Error("a plain writer lost the projected Duration, which needs no rewind")
	}
}

// TestMuxEmptyStreamVoidsTheCuesEntry: no packets means no Cues, so the
// reserved Seek entry becomes a Void of the same width. webm runs too, since it
// is a subset profile and a Void in a SeekHead is worth a second look there.
func TestMuxEmptyStreamVoidsTheCuesEntry(t *testing.T) {
	pcmFmt := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	opusFmt := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint16(head[10:], 312)
	binary.LittleEndian.PutUint32(head[12:], 48000)

	for _, tc := range []struct {
		name  string
		track container.Track
		opts  *MuxerOptions
	}{
		{"matroska", container.Track{Codec: codec.PCM, Fmt: pcmFmt, Default: true}, nil},
		{"webm", container.Track{Codec: codec.Opus, CodecConfig: head, Fmt: opusFmt, Delay: 312, Default: true},
			&MuxerOptions{WebM: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := muxToSeekable(t, tc.track, tc.opts, nil, codec.Trailer{Samples: -1})
			if _, _, ok := segChild(t, file, idCues); ok {
				t.Error("an empty stream produced a Cues element")
			}
			pts, voids := seekHeadEntries(t, file)
			if voids != 1 {
				t.Errorf("SeekHead has %d Void entries, want the one abandoned Cues slot", voids)
			}
			for _, p := range pts {
				if p.target == idCues {
					t.Error("a SeekHead entry still points at Cues that were never written")
				}
			}
			// Strict: a Void must read as reserved space, not as damage.
			d, err := NewDemuxer(container.BytesSource(file), &DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatalf("strict open of an empty stream: %v", err)
			}
			if w := d.Warnings(); len(w) != 0 {
				t.Errorf("warnings on an empty stream: %v", w)
			}
		})
	}
}

// TestMuxSeekableRoundTrip covers what only a real parse sees: the definite
// Segment size bounds the walk, the trailing Cues is skipped, and the patched
// Duration comes back exactly.
func TestMuxSeekableRoundTrip(t *testing.T) {
	const frames = 4801
	track, pkts := pcmCase(t, 12, -1)
	// An odd tail, so the total is not a whole millisecond.
	pkts = pkts[:10]
	pkts = append(pkts, codec.Packet{Data: make([]byte, 1*4), PTS: 4800, Dur: 1})
	file := muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: frames})

	// Trailing bytes: an unknown size would swallow them as segment content.
	const junk = 64
	src := append(append([]byte(nil), file...), make([]byte, junk)...)

	d, err := NewDemuxer(container.BytesSource(src), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if d.segmentEnd != int64(len(file)) {
		t.Errorf("segmentEnd = %d, want the declared %d (source is %d)", d.segmentEnd, len(file), len(src))
	}
	if tr := d.Tracks()[0]; tr.Samples != frames {
		t.Errorf("Samples = %d, want the patched %d", tr.Samples, frames)
	}
	if got := len(readAll(t, d)); got != len(pkts) {
		t.Errorf("read %d packets past the trailing Cues, wrote %d", got, len(pkts))
	}
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("warnings on our own output: %v", w)
	}
}

// TestDurationTicksRoundTrip: a written tick value must recover the exact
// sample count through the read path's ticks -> ns -> nsToSamples chain.
func TestDurationTicksRoundTrip(t *testing.T) {
	for _, rate := range []int{8000, 44100, 48000, 96000} {
		for _, n := range []int64{1, 2, 999, 4801, 9111, 44099, 1 << 20, 1<<32 + 7} {
			ticks := durationTicks(n, rate)
			ns := int64(ticks*float64(defaultTimestampScale) + 0.5)
			if got := nsToSamples(ns, rate); got != n {
				t.Errorf("rate %d, %d samples: round-tripped to %d (ticks %v)", rate, n, got, ticks)
			}
			if math.IsNaN(ticks) || math.IsInf(ticks, 0) {
				t.Errorf("rate %d, %d samples: ticks = %v", rate, n, ticks)
			}
		}
	}
}

// TestMuxCueCapHalves drives recordCue past its cap directly, since the cap
// covers days of audio. The kept entries must stay a uniform sample: a stride
// that loses its phase leaves the index dense at one end and sparse at the
// other, which is worse than a coarse index everywhere.
func TestMuxCueCapHalves(t *testing.T) {
	m := &Muxer{ws: &memWS{}, cueStride: 1}
	const clusters = maxCuePoints*3 + 17
	for i := 0; i < clusters; i++ {
		m.off = int64(i) * 1000
		m.clusterMs = int64(i) * 4000
		m.recordCue()
	}
	if len(m.cues) > maxCuePoints {
		t.Fatalf("the index grew to %d entries, past the %d cap", len(m.cues), maxCuePoints)
	}
	if len(m.cues) <= maxCuePoints/2 {
		t.Errorf("the index holds %d entries; halving should leave it filling back toward %d",
			len(m.cues), maxCuePoints)
	}
	if len(m.cues) < 2 {
		t.Fatal("too few entries to check the stride")
	}
	step := m.cues[1].pos - m.cues[0].pos
	for i := 1; i < len(m.cues); i++ {
		if m.cues[i].timeMs <= m.cues[i-1].timeMs {
			t.Fatalf("entry %d is at time %d, not after its predecessor's %d",
				i, m.cues[i].timeMs, m.cues[i-1].timeMs)
		}
		if got := m.cues[i].pos - m.cues[i-1].pos; got != step {
			t.Fatalf("entry %d sits %d after its predecessor; earlier entries are %d apart",
				i, got, step)
		}
	}
	// And it must still reach the end of the file.
	if last, end := m.cues[len(m.cues)-1].pos, int64(clusters-1)*1000; last < end-step {
		t.Errorf("the last entry is at %d, the last cluster at %d (stride %d)", last, end, step)
	}
}

// pipeWS satisfies io.WriteSeeker the way *os.File does for a pipe: the method
// is there whether or not the object can seek.
type pipeWS struct{ b []byte }

func (p *pipeWS) Write(b []byte) (int, error) { p.b = append(p.b, b...); return len(b), nil }

func (p *pipeWS) Seek(int64, int) (int64, error) { return 0, errors.New("illegal seek") }

// TestMuxPipeLandsInTheStreamingColumn: seekability is probed, not inferred.
// Trusting the type assertion writes the whole Cues element before the first
// patch fails, leaving the reader a stream with an index nothing points at.
func TestMuxPipeLandsInTheStreamingColumn(t *testing.T) {
	track, pkts := pcmCase(t, 1200, 1200*480)
	w := &pipeWS{}
	// runMuxer fails on an End error, which is the other half: the muxer must
	// not need the seek it cannot have.
	runMuxer(t, w, track, nil, pkts, codec.Trailer{Samples: 1200 * 480})

	if _, _, ok := segChild(t, w.b, idCues); ok {
		t.Error("a writer whose Seek fails got a Cues element")
	}
	if _, _, definite := segment(t, w.b); definite {
		t.Error("a writer whose Seek fails got a definite Segment size")
	}
	if _, ok := infoDuration(t, w.b); !ok {
		t.Error("the projected Duration was dropped; it needs no seek")
	}
}

// TestMuxSeekableAtNonZeroStart writes through a writer already positioned past
// the beginning: patch offsets are absolute, so taking header-buffer indices for
// file offsets would patch whatever sits in front.
func TestMuxSeekableAtNonZeroStart(t *testing.T) {
	const base = 97
	w := &memWS{}
	w.SeekTo(base)
	for i := range w.Buf {
		w.Buf[i] = 0xAA // recognizable, so a stray patch shows up
	}
	const frames = 4801
	track, pkts := pcmCase(t, 12, -1)
	runMuxer(t, w, track, nil, pkts, codec.Trailer{Samples: frames})

	for i, v := range w.Buf[:base] {
		if v != 0xAA {
			t.Fatalf("byte %d ahead of the stream was overwritten (%#x)", i, v)
		}
	}
	file := w.Buf[base:]
	ticks, ok := infoDuration(t, file)
	if !ok {
		t.Fatal("no Duration")
	}
	if want := durationTicks(frames, 48000); ticks != want {
		t.Errorf("Duration = %v ticks, want %v", ticks, want)
	}
	seg, dataOff, definite := segment(t, file)
	if !definite {
		t.Error("the Segment kept its unknown size")
	}
	if want := len(file) - dataOff; len(seg) != want {
		t.Errorf("the Segment declares %d bytes, %d follow it", len(seg), want)
	}
	pts, voids := seekHeadEntries(t, file)
	if voids != 0 {
		t.Errorf("%d Void entries in the SeekHead", voids)
	}
	for _, p := range pts {
		if id, _, _ := readElemAt(t, seg, int(p.pos)); id != p.target {
			t.Errorf("SeekHead points %#x at segment offset %d, which holds %#x", p.target, p.pos, id)
		}
	}
	d, err := NewDemuxer(container.BytesSource(file), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if got := d.Tracks()[0].Samples; got != frames {
		t.Errorf("Samples = %d, want %d", got, frames)
	}
}

// TestMuxDurationFromPacketsWhenTrailerIsUnknown covers the -1 sentinel. No
// encoder here emits it today, but codec.Trailer documents it as how a producer
// that does not know the length stays silent, so a muxer writing a Duration
// needs an answer: its own packet durations, less the trims.
func TestMuxDurationFromPacketsWhenTrailerIsUnknown(t *testing.T) {
	const packets, dur = 6, 480
	const delay, padding = 312, 100
	track, pkts := pcmCase(t, packets, -1)
	file := muxToSeekable(t, track, nil, pkts,
		codec.Trailer{Samples: -1, Delay: delay, Padding: padding})

	ticks, ok := infoDuration(t, file)
	if !ok {
		t.Fatal("no Duration written for a trailer that declares no length")
	}
	if want := durationTicks(packets*dur-delay-padding, 48000); ticks != want {
		t.Errorf("Duration = %v ticks, want %v (the packets, less the trims)", ticks, want)
	}
}
