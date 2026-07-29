package mka

// Read-side coverage of the Cues-anchored bounded walk, including that the
// bound is taken: correctness alone passes when the rung stops firing.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// countingSource wraps a Source and totals the bytes read through it.
type countingSource struct {
	src  container.Source
	read int64
}

func (c *countingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.src.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

func (c *countingSource) Size() int64 { return c.src.Size() }

// cuesFixture is a 40 s seekable PCM file: ten clusters, ten cue points.
func cuesFixture(t *testing.T) ([]byte, int64) {
	t.Helper()
	const packets = 4000 // 480 samples each
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	track := container.Track{Codec: codec.PCM, Fmt: f, Samples: packets * 480, Default: true}
	var pkts []codec.Packet
	for i := 0; i < packets; i++ {
		data := make([]byte, 480*4)
		for j := range data {
			data[j] = byte(i*17 + j*3)
		}
		pkts = append(pkts, codec.Packet{Data: data, PTS: int64(i * 480), Dur: 480})
	}
	return muxToSeekable(t, track, nil, pkts, codec.Trailer{Samples: packets * 480}), packets * 480
}

// cueEntriesOf returns the file's Cues in the muxer's terms.
func cueEntriesOf(t *testing.T, file []byte) []cuePoint {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	d.resolveCues()
	if len(d.cues) == 0 {
		t.Fatal("the fixture's own Cues did not parse")
	}
	var out []cuePoint
	for _, c := range d.cues {
		out = append(out, cuePoint{timeMs: c.time, pos: c.off - d.segmentDataOff})
	}
	return out
}

// replaceCues rewrites the file's Cues. Cues is the last element the muxer
// writes, so it may change length; the Segment size is re-patched to match.
func replaceCues(t *testing.T, file []byte, pts []cuePoint) []byte {
	t.Helper()
	_, dataOff, definite := segment(t, file)
	if !definite {
		t.Fatal("the fixture's Segment has no definite size to re-patch")
	}
	_, pos, ok := segChild(t, file, idCues)
	if !ok {
		t.Fatal("the fixture has no Cues element")
	}
	m := &Muxer{cues: pts}
	out := append(append([]byte(nil), file[:dataOff+pos]...), m.cuesElement()...)

	// The Segment's 8-byte size vint sits right after its 4-byte ID, which
	// follows the EBML header.
	_, _, segIDOff := readElemAt(t, out, 0)
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(len(out)-dataOff))
	v[0] = 0x01
	copy(out[segIDOff+4:], v[:])
	return out
}

// stripCues blanks the SeekHead's Cues entry, leaving an unbounded walk.
func stripCues(t *testing.T, file []byte) []byte {
	t.Helper()
	out := append([]byte(nil), file...)
	sh, pos, ok := segChild(t, out, idSeekHead)
	if !ok {
		t.Fatal("no SeekHead")
	}
	_, dataOff, _ := segment(t, out)
	_, bodyStart, _ := readElemAt(t, out, dataOff+pos)
	if i := bytes.Index(sh, appendID(nil, idCues)); i >= 0 {
		copy(out[bodyStart+i:], appendID(nil, idVoid))
		return out
	}
	t.Fatal("the SeekHead has no Cues entry to strip")
	return nil
}

// landAll seeks to each target on one fresh demuxer, returning the landings,
// the bytes served, and the demuxer.
func landAll(t *testing.T, file []byte, targets []int64) ([]int64, int64, *Demuxer) {
	t.Helper()
	c := &countingSource{src: container.BytesSource(file)}
	d, err := NewDemuxer(c, nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	var landed []int64
	for _, want := range targets {
		got, err := d.SeekSample(0, want)
		if err != nil {
			t.Fatalf("seek to %d: %v", want, err)
		}
		landed = append(landed, got)
	}
	return landed, c.read, d
}

// TestCuesBoundTheWalk is the rung-was-taken proof: a shallow seek must land
// where the unbounded walk lands and read far fewer bytes doing it.
func TestCuesBoundTheWalk(t *testing.T) {
	file, total := cuesFixture(t)
	target := []int64{total / 10}

	withCues, cuesRead, d := landAll(t, file, target)
	noCues, fullRead, _ := landAll(t, stripCues(t, file), target)

	if len(d.cues) == 0 {
		t.Fatal("the fixture's own Cues did not parse; nothing below tests the bound")
	}
	if d.walked {
		t.Error("the full walk ran anyway; the bound was computed and then ignored")
	}
	if withCues[0] != noCues[0] {
		t.Errorf("bounded seek landed at %d, unbounded at %d", withCues[0], noCues[0])
	}
	if cuesRead*2 >= fullRead {
		t.Errorf("bounded seek read %d bytes, unbounded read %d; the Cues bound is not being taken",
			cuesRead, fullRead)
	}
	t.Logf("bounded read %d bytes, unbounded %d (%d-byte file)", cuesRead, fullRead, len(file))
}

// TestCuesWalkExtends: a scrub must not re-walk what it already counted.
func TestCuesWalkExtends(t *testing.T) {
	file, total := cuesFixture(t)
	scrub := []int64{total / 10, total / 5, total * 3 / 10}

	stepped, steppedRead, _ := landAll(t, file, scrub)
	deepest, deepestRead, _ := landAll(t, file, scrub[len(scrub)-1:])

	if stepped[len(stepped)-1] != deepest[0] {
		t.Errorf("scrubbed to %d, direct seek landed at %d", stepped[len(stepped)-1], deepest[0])
	}
	// A restarting walk costs 0.1n + 0.2n + 0.3n against one walk's 0.3n.
	if steppedRead > deepestRead*3/2 {
		t.Errorf("three deepening seeks read %d bytes, one seek to the deepest read %d; the walk restarts",
			steppedRead, deepestRead)
	}
	t.Logf("scrub read %d bytes, single deep seek %d", steppedRead, deepestRead)
}

// TestCuesCorruptedStayAtOrBefore is the invariant test: a wrong index can only
// make a landing earlier or slower, never wrong.
func TestCuesCorruptedStayAtOrBefore(t *testing.T) {
	file, total := cuesFixture(t)
	targets := []int64{0, total / 10, total / 3, total / 2, total * 4 / 5, total - 1}
	sound := cueEntriesOf(t, file)
	if len(sound) < 4 {
		t.Fatalf("the fixture has %d cue points; too few to shuffle meaningfully", len(sound))
	}

	// The reference: every landing with a sound index.
	want, _, _ := landAll(t, file, targets)

	cases := []struct {
		name string
		fn   func(pts []cuePoint)
		// bound is whether a usable bound still comes out. Garbage fails
		// cueLimit's read-back; the shuffled cases name real clusters, so the
		// bound is taken and wrong, which is what the invariant is about.
		bound bool
	}{
		// Garbage: past the segment, or naming no cluster.
		{"out of range", func(pts []cuePoint) {
			for i := range pts {
				pts[i].pos += 1 << 40
			}
		}, false},
		{"all zero", func(pts []cuePoint) {
			for i := range pts {
				pts[i].pos = 0
			}
		}, false},
		{"shifted into a block", func(pts []cuePoint) {
			for i := range pts {
				pts[i].pos += 37
			}
		}, false},
		// Plausible: real clusters attached to the wrong times.
		{"rotated", func(pts []cuePoint) {
			for i := range pts {
				pts[i].pos = sound[(i+1)%len(sound)].pos
			}
		}, true},
		{"reversed", func(pts []cuePoint) {
			for i := range pts {
				pts[i].pos = sound[len(sound)-1-i].pos
			}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pts := append([]cuePoint(nil), sound...)
			tc.fn(pts)
			bad := replaceCues(t, file, pts)
			got, _, _ := landAll(t, bad, targets)
			// Own demuxer, one mid-file target: landAll's last target sits past
			// the final cue, which puts every run on the full walk.
			if taken := boundTaken(t, bad, total/3); taken != tc.bound {
				t.Errorf("bound taken = %v, want %v", taken, tc.bound)
			}
			for i, target := range targets {
				if got[i] > target {
					t.Errorf("target %d: landed at %d, past it", target, got[i])
				}
				if got[i] > want[i] {
					t.Errorf("target %d: corrupted index landed at %d, sound index at %d",
						target, got[i], want[i])
				}
			}
			// And the audio from a corrupted-index landing is still the audio.
			assertTailExact(t, file, bad, targets[len(targets)/2])
		})
	}
}

// assertTailExact requires identical payloads from the later of two landings.
func assertTailExact(t *testing.T, good, bad []byte, target int64) {
	t.Helper()
	gotAt, gotPkts := seekAndRead(t, bad, target)
	wantAt, wantPkts := seekAndRead(t, good, target)
	// Either may be the earlier one, so advance whichever is behind. Both sit
	// on packet boundaries by construction.
	advance := func(at int64, pkts [][]byte, to int64) (int64, [][]byte) {
		for at < to && len(pkts) > 0 {
			at += int64(len(pkts[0]) / 4) // stereo int16: four bytes a frame
			pkts = pkts[1:]
		}
		return at, pkts
	}
	gotAt, gotPkts = advance(gotAt, gotPkts, wantAt)
	wantAt, wantPkts = advance(wantAt, wantPkts, gotAt)
	if gotAt != wantAt {
		t.Fatalf("landings %d and %d do not converge on a packet boundary", gotAt, wantAt)
	}
	if len(gotPkts) != len(wantPkts) {
		t.Fatalf("read %d packets from the corrupted index, %d from the sound one", len(gotPkts), len(wantPkts))
	}
	for i := range gotPkts {
		if !bytes.Equal(gotPkts[i], wantPkts[i]) {
			t.Fatalf("packet %d after the seek differs", i)
		}
	}
}

// seekAndRead seeks to target and drains the rest of the stream.
func seekAndRead(t *testing.T, file []byte, target int64) (int64, [][]byte) {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatalf("seek to %d: %v", target, err)
	}
	var out [][]byte
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return landed, out
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, append([]byte(nil), pkt.Data...))
	}
}

// TestCuesFromFFmpegBoundTheWalk runs the rung against a third-party index, so
// the parser is read against the format rather than against its own output.
func TestCuesFromFFmpegBoundTheWalk(t *testing.T) {
	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "cues.mka")
	// Long, with short clusters, so a shallow seek has most of it to skip.
	testutil.FFmpegGenerateDuration(t, path, 30.0, 48000, 2, "flac", "-cluster_time_limit", "500")
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	c := &countingSource{src: container.BytesSource(file)}
	d, err := NewDemuxer(c, nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	total := d.Tracks()[0].Samples
	if total <= 0 {
		t.Fatalf("ffmpeg's fixture declares %d samples", total)
	}
	got, err := d.SeekSample(0, total/10)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if len(d.cues) == 0 {
		t.Fatal("ffmpeg's Cues did not parse")
	}
	if d.walked {
		t.Error("the full walk ran anyway; ffmpeg's Cues bounded nothing")
	}

	// The landing must match the unbounded walk's, and cost materially less.
	ref := &countingSource{src: container.BytesSource(file)}
	rd, err := NewDemuxer(ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.ensureWalk(); err != nil {
		t.Fatal(err)
	}
	want, err := rd.SeekSample(0, total/10)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("bounded seek landed at %d, unbounded at %d", got, want)
	}
	if c.read*2 >= ref.read {
		t.Errorf("bounded seek read %d bytes, unbounded read %d", c.read, ref.read)
	}
	t.Logf("%d cues, bounded read %d bytes, unbounded %d (%d-byte file)", len(d.cues), c.read, ref.read, len(file))
}

// TestCuesVorbisRefusesTheBound pins the guard against a codec whose frame
// duration depends on the previous block, which a resume would mis-time.
// needsGaplessWalk also covers Vorbis today; this is what survives editing it.
func TestCuesVorbisRefusesTheBound(t *testing.T) {
	d := &Demuxer{}
	d.setup.id = codec.Vorbis
	if d.boundedWalkSafe() {
		t.Error("Vorbis accepted a bounded walk; a resume would mis-time its frames")
	}
	for _, id := range []codec.ID{codec.PCM, codec.FLAC, codec.Opus, codec.AACLC} {
		d.setup.id = id
		if !d.boundedWalkSafe() {
			t.Errorf("%s refused a bounded walk; it carries no inter-frame duration state", id)
		}
	}
}

// flakySource fails reads inside a byte window while armed. A window rather
// than a threshold, so cueLimit's read-back of the deep limit cluster can still
// succeed while the walk dies before reaching it.
type flakySource struct {
	src      container.Source
	from, to int64
	armed    bool
}

func (f *flakySource) ReadAt(p []byte, off int64) (int, error) {
	if f.armed && off >= f.from && off < f.to {
		return 0, errors.New("flaky source")
	}
	return f.src.ReadAt(p, off)
}

func (f *flakySource) Size() int64 { return f.src.Size() }

// TestCuesWalkRetryAfterFailure: a bounded walk that dies partway has advanced
// the counters past its resume point, so it must drop that point or the retry
// counts the stretch twice. Strict mode is what makes the failure propagate
// rather than reading as trailing damage.
func TestCuesWalkRetryAfterFailure(t *testing.T) {
	file, total := cuesFixture(t)
	shallow, deep := total/10, total/2
	want, _, _ := landAll(t, file, []int64{shallow, deep})

	f := &flakySource{src: container.BytesSource(file)}
	d, err := NewDemuxer(f, &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	// Resolve while reads work, so the seeks below take the bounded path. The
	// full walk is marked spent on its first attempt and would never retry.
	d.resolveCues()
	if len(d.cues) == 0 {
		t.Fatal("no Cues; the seeks below would not be bounded walks")
	}

	// A shallow seek first, so there is a resume point to be wrong about: a
	// first walk starts from zero and leaves nothing stale behind.
	if got, err := d.SeekSample(0, shallow); err != nil {
		t.Fatalf("shallow seek: %v", err)
	} else if got != want[0] {
		t.Fatalf("shallow seek landed at %d, want %d", got, want[0])
	}
	if d.walkedTo <= 0 {
		t.Fatal("the shallow seek left no resume point; there is nothing for the retry to trip over")
	}

	// Fail inside the stretch the deeper seek walks, not at its bound cluster.
	f.from, f.to, f.armed = int64(len(file))*3/10, int64(len(file))*45/100, true
	if _, err := d.SeekSample(0, deep); err == nil {
		t.Fatal("the deep seek succeeded through a failing source")
	}
	if d.walked {
		t.Fatal("the failed walk was the full one; this test needs the bounded path")
	}

	f.armed = false
	got, err := d.SeekSample(0, deep)
	if err != nil {
		t.Fatalf("retry after the source recovered: %v", err)
	}
	if got != want[1] {
		t.Errorf("retry landed at %d, a clean run lands at %d: the walk resumed onto a double count",
			got, want[1])
	}
}

// TestCuesSeekToStartCostsNothing: a target of zero lands on the first cluster
// by definition, so the walk must read no audio at all.
func TestCuesSeekToStartCostsNothing(t *testing.T) {
	file, total := cuesFixture(t)

	c := &countingSource{src: container.BytesSource(file)}
	d, err := NewDemuxer(c, nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	d.resolveCues()
	opened := c.read

	landed, err := d.SeekSample(0, 0)
	if err != nil {
		t.Fatalf("SeekSample(0): %v", err)
	}
	if landed != 0 {
		t.Errorf("seek to 0 landed at %d", landed)
	}
	// A cluster here is ~770 KB, so reading one is unmistakable.
	if walked := c.read - opened; walked > 4096 {
		t.Errorf("seeking to 0 read %d bytes; the bound should stop before the first cluster", walked)
	}

	// The cheap path must leave the walk resumable, not wedged.
	deep := total / 2
	got, err := d.SeekSample(0, deep)
	if err != nil {
		t.Fatalf("deep seek after seeking to 0: %v", err)
	}
	want, _, _ := landAll(t, file, []int64{deep})
	if got != want[0] {
		t.Errorf("deep seek after a seek to 0 landed at %d, a fresh one lands at %d", got, want[0])
	}
}

// boundTaken reports whether a seek took the bound rather than the full walk.
func boundTaken(t *testing.T, file []byte, target int64) bool {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if _, err := d.SeekSample(0, target); err != nil {
		t.Fatalf("seek to %d: %v", target, err)
	}
	return !d.walked
}

// TestCuesReadErrorDoesNotFailTheSeek: nothing about a Cues index may fail a
// seek. Raising would fail seeks that succeeded before this parser existed, and
// would make the outcome depend on call order.
func TestCuesReadErrorDoesNotFailTheSeek(t *testing.T) {
	file, total := cuesFixture(t)
	want, _, _ := landAll(t, file, []int64{total / 3})

	// Make the Cues element unreadable.
	_, dataOff, _ := segment(t, file)
	body, pos, ok := segChild(t, file, idCues)
	if !ok {
		t.Fatal("the fixture has no Cues element")
	}
	f := &flakySource{
		src:   container.BytesSource(file),
		from:  int64(dataOff + pos),
		to:    int64(dataOff + pos + len(body) + 16),
		armed: true,
	}
	d, err := NewDemuxer(f, nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	got, err := d.SeekSample(0, total/3)
	if err != nil {
		t.Fatalf("an unreadable Cues element failed the seek: %v", err)
	}
	if got != want[0] {
		t.Errorf("landed at %d, a readable index lands at %d", got, want[0])
	}
	if !d.walked {
		t.Error("the seek claimed a bound from an index it could not read")
	}
	// The second seek must agree: the outcome cannot depend on which call
	// triggered the read.
	if again, err := d.SeekSample(0, total/3); err != nil || again != got {
		t.Errorf("the second seek gave (%d, %v), the first gave (%d, nil)", again, err, got)
	}
}

// TestCuesThinRatherThanTruncate: an over-cap index must be thinned across the
// whole file, not cut off in file order. A head-only index leaves every seek
// past the retained part with no bound at all.
func TestCuesThinRatherThanTruncate(t *testing.T) {
	file, _ := cuesFixture(t)
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	const n = maxCuePoints*2 + 5
	span := d.segmentEnd - d.segmentDataOff
	pts := make([]cuePoint, n)
	for i := range pts {
		pts[i] = cuePoint{timeMs: int64(i), pos: int64(i) * (span - 1) / n}
	}
	elem := (&Muxer{cues: pts}).cuesElement()
	_, bodyStart, bodyEnd := readElemAt(t, elem, 0)
	if err := d.parseCues(elem[bodyStart:bodyEnd]); err != nil {
		t.Fatalf("parseCues: %v", err)
	}

	if len(d.cues) > maxCuePoints {
		t.Fatalf("the index holds %d entries, past the %d cap", len(d.cues), maxCuePoints)
	}
	if len(d.cues) <= maxCuePoints/2 {
		t.Errorf("the index holds %d entries; thinning should leave it filling back toward %d",
			len(d.cues), maxCuePoints)
	}
	// The retained index still reaches the end of the file; truncating would
	// stop it halfway. time carries each point's original index.
	if last, want := d.cues[len(d.cues)-1].time, int64(n-1); last < want-4 {
		t.Errorf("the last retained point is #%d of %d: the index is head-only", last, want)
	}
	// Evenly spaced, so the index samples the file uniformly.
	step := d.cues[1].time - d.cues[0].time
	for i := 1; i < len(d.cues); i++ {
		if got := d.cues[i].time - d.cues[i-1].time; got != step {
			t.Fatalf("point %d is %d after its predecessor; earlier ones are %d apart", i, got, step)
		}
	}
}
