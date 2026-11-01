package asf_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/internal/testutil/wmacount"
)

// v1v2Fixtures are the committed WMA v1/v2 files: the six here, the one the
// engine tests share, and the one Windows wrote.
var v1v2Fixtures = []string{
	"sine-wmav1.wma", "sine-wmav2.wma", "frag.wma", "tagged.wma", "mono-8k.wma", "chapters.wma",
	"../../../testdata/sine-s16.wma", windowsFixture,
}

// landingFor is the object a seek to target must land on: one before the
// object that holds the target, so the decode has that object as run-up. The
// object holding a frame is the last one whose frames before it are at most
// that frame; an object that completes no frame shares its position with the
// next, so the later of the two is the one.
func landingFor(objs []wmacount.Object, frameLen, target int64) int {
	hold := holding(objs, target/frameLen)
	if objs[hold].Before == 0 {
		return hold
	}
	return holding(objs, objs[hold].Before-1)
}

// holding is the object that holds frame n.
func holding(objs []wmacount.Object, n int64) int {
	k := 0
	for j, o := range objs {
		if o.Before <= n {
			k = j
		}
	}
	return k
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// reservoirStream is a WMA v2 stream with the bit reservoir, a packet per
// entry of counts holding one object per count, each object stating that
// frame count. The container reads nothing of an object past the nibble, so
// the rest is the object's index in the stream and filler.
func reservoirStream(t *testing.T, counts [][]byte) []byte {
	t.Helper()
	b := reservoirBuilder()
	b.playHNS = b.prerollMS*10_000 + uint64(len(counts))*100*10_000
	pres, obj := uint32(b.prerollMS), byte(0)
	for i, packet := range counts {
		var subs [][]byte
		for _, f := range packet {
			subs = append(subs, []byte{f, obj, 0, 0})
			obj++
		}
		b.compressedPacket(uint32(i*100), pres+uint32(i*100), 20, subs)
	}
	return b.build()
}

// reservoirBuilder is a builder whose stream is WMA v2 with the bit reservoir.
func reservoirBuilder() *builder {
	b := newBuilder()
	b.blockAlign = 4
	wfx := make([]byte, 18+6)
	le.PutUint16(wfx, 0x0161)
	le.PutUint16(wfx[2:], uint16(b.channels))
	le.PutUint32(wfx[4:], uint32(b.rate))
	le.PutUint32(wfx[8:], 16000)
	le.PutUint16(wfx[12:], uint16(b.blockAlign))
	le.PutUint16(wfx[14:], 16)
	le.PutUint16(wfx[16:], 6)
	le.PutUint16(wfx[18+4:], 0x0003) // VLC exponents and the bit reservoir
	b.wfx = wfx
	return b
}

// TestASeekReportsNoDamageItsWindowNeverReads. A counted seek walks the
// packets ahead of its landing, which no read the caller asked for will
// reach. What that walk finds is the walk's to report: a seek past damage
// lands, under Strict too, and Walk reports the damage, refusing under
// Strict, where the count is then not walked.
func TestASeekReportsNoDamageItsWindowNeverReads(t *testing.T) {
	b := reservoirBuilder()
	b.playHNS = b.prerollMS*10_000 + 10_000_000
	pres := uint32(b.prerollMS)
	for i := range 8 {
		if i == 2 {
			// A continuation of an object nothing started.
			b.packet(uint32(i*20), byte(i+1), 8, 12, pres+uint32(i*20), []byte{9, 9, 9, 9})
			continue
		}
		b.packet(uint32(i*20), byte(i+1), 0, 4, pres+uint32(i*20), []byte{1, byte(i), 0, 0})
	}
	raw := b.build()

	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(0, 5*2048); err != nil {
		t.Fatal(err)
	}
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("a seek past the damage reported %v", w)
	}
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings()) == 0 {
		t.Error("Walk did not report the damage it walked over")
	}

	s, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeekSample(0, 5*2048); err != nil {
		t.Errorf("a strict seek past the damage: %v", err)
	}
	var pkt container.Packet
	if err := s.ReadPacket(&pkt); err != nil || pkt.Data[1] < 3 {
		t.Errorf("read after the seek: %v, object %v, want one behind the damage", err, pkt.Data)
	}
	first := s.Walk()
	if first == nil {
		t.Fatal("a strict walk accepted the damage")
	}
	if s.Walked() {
		t.Error("a strict walk that refused reports the count walked")
	}
	if again := s.Walk(); again == nil || again.Error() != first.Error() {
		t.Errorf("a second strict walk returned %v, want the first refusal %v", again, first)
	}
}

// TestALandingStepsOverAnObjectThatCompletesNoFrame. An object whose count is
// zero shares its position with the next one, and the later of the two is
// the landing; the run-up an object back passes it over too, since landing
// on it gives the decode nothing the next one would not.
func TestALandingStepsOverAnObjectThatCompletesNoFrame(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts [][]byte
		seeks  [][3]int64 // target frame, landed frame, object read next
	}{
		{
			// Positions in frames: 0, 3, 3, 5, 9, 10, a packet each.
			"a packet each", [][]byte{{3}, {0}, {2}, {4}, {1}, {5}},
			[][3]int64{{3, 0, 0}, {5, 3, 2}, {9, 5, 3}, {4, 0, 0}, {10, 9, 4}},
		},
		{
			// The same counts, three objects to a packet, so the landing is
			// found by stepping through a packet's own objects.
			"three to a packet", [][]byte{{3, 0, 2}, {4, 1, 5}},
			[][3]int64{{3, 0, 0}, {5, 3, 2}, {9, 5, 3}, {4, 0, 0}, {10, 9, 4}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := asf.NewDemuxer(container.BytesSource(reservoirStream(t, tc.counts)), &asf.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range tc.seeks {
				landed, err := d.SeekSample(0, c[0]*2048)
				if err != nil {
					t.Fatal(err)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatal(err)
				}
				if landed != c[1]*2048 || int64(pkt.Data[1]) != c[2] {
					t.Errorf("seek to frame %d landed at %d on object %d, want object %d at %d",
						c[0], landed, pkt.Data[1], c[2], c[1]*2048)
				}
			}
		})
	}
}

// TestACountTakesBackAFrameNothingCarries. A writer that counts a frame
// ending on its packet's last bit where it ends, not where it began, leaves
// the next packet's header reading as if the frame were still carried, and
// the sum of the counts runs a frame high from there on. The count asks the
// codec at each object, so landings, inside a packet's own objects too, the
// length a walk settles and a restored count all follow the corrected
// positions.
func TestACountTakesBackAFrameNothingCarries(t *testing.T) {
	var counts [][]byte
	for i := range 20 {
		counts = append(counts, []byte{byte(2 + i%3), byte(1 + i%2), 3})
	}
	// Objects a decode enters with nothing carried: the first in packet 3,
	// the second in packet 4, the third in packet 7 and the second in the
	// last, each in a step the restore probes check.
	un := map[byte]bool{9: true, 13: true, 23: true, 58: true}
	raw := reservoirStream(t, counts)
	const fl = 2048
	var objs []wmacount.Object
	var total int64
	for _, packet := range counts {
		for _, f := range packet {
			if un[byte(len(objs))] {
				total--
			}
			objs = append(objs, wmacount.Object{Before: total})
			total += int64(f)
		}
	}
	openHooked := func() *asf.Demuxer {
		d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
		if err != nil {
			t.Fatal(err)
		}
		asf.UncarriedForTest(d, func(_, pkt []byte) (bool, error) { return un[pkt[1]], nil })
		return d
	}
	seeks := func(what string, d *asf.Demuxer) {
		t.Helper()
		for _, o := range objs {
			for _, target := range []int64{o.Before * fl, o.Before*fl + fl/2} {
				want := landingFor(objs, fl, target)
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("%s: seek to %d: %v", what, target, err)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatalf("%s: reading after a seek to %d: %v", what, target, err)
				}
				if landed != objs[want].Before*fl || int(pkt.Data[1]) != want {
					t.Fatalf("%s: seek to %d landed at %d on object %d, want object %d at %d",
						what, target, landed, pkt.Data[1], want, objs[want].Before*fl)
				}
			}
		}
	}

	d := openHooked()
	seeks("counted", d)
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; !tr.SamplesExact || tr.Samples != (total-1)*fl {
		t.Errorf("the walk settled %d samples (exact %v), the corrected count gives %d", tr.Samples, tr.SamplesExact, (total-1)*fl)
	}
	blob := d.IndexSnapshot()
	if blob == nil {
		t.Fatal("no snapshot of a walked count")
	}
	r := openHooked()
	if !r.RestoreIndex(blob) {
		t.Fatal("the probes refused a count the walk took")
	}
	seeks("restored", r)

	// A read feeds the count the same corrections.
	fed := openHooked()
	if err := readAll(t, fed); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if got := fed.IndexSnapshot(); !bytes.Equal(got, blob) {
		t.Error("the count a read took is not the one the walk took")
	}
}

// readAll reads d to its end and returns what ended the read.
func readAll(t *testing.T, d *asf.Demuxer) error {
	t.Helper()
	var pkt container.Packet
	for range 1 << 20 {
		if err := d.ReadPacket(&pkt); err != nil {
			return err
		}
	}
	t.Fatal("a read that never ends")
	return nil
}

// TestALinearReadTakesTheCount. A read from the start passes every object a
// walk would count, so the read counts them: after a play to the end the
// length is settled and the count is the one a walk takes, so a measure reads
// nothing more, a seek reads only from its landing, and the count is there to
// save.
func TestALinearReadTakesTheCount(t *testing.T) {
	raw := readFixture(t, windowsFixture)
	objs, fl := wmacount.Objects(t, raw)
	start, plen, _ := packetGeometry(t, raw)
	walked := open(t, raw)
	if err := walked.Walk(); err != nil {
		t.Fatal(err)
	}
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := asf.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := readAll(t, d); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !d.Walked() {
		t.Fatal("a read to the end left the count unwalked")
	}
	if got, want := d.Tracks()[0], walked.Tracks()[0]; !got.SamplesExact || got.Samples != want.Samples {
		t.Errorf("a read to the end settled %d (exact %v), a walk %d", got.Samples, got.SamplesExact, want.Samples)
	}
	if got := d.IndexSnapshot(); got == nil || !bytes.Equal(got, walked.IndexSnapshot()) {
		t.Error("the read's count is not the one a walk takes")
	}
	src.Reset()
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if src.Reads != 0 {
		t.Errorf("Walk read %d times after a read to the end", src.Reads)
	}
	j := len(objs) - 3
	landed, err := d.SeekSample(0, objs[j].Before*fl+fl/2)
	if err != nil {
		t.Fatal(err)
	}
	if landed != objs[j-1].Before*fl {
		t.Errorf("the seek landed at %d, the count puts object %d at %d", landed, j-1, objs[j-1].Before*fl)
	}
	if at := start + int64(j-1)*plen; src.Reads > 0 && src.Low < at {
		t.Errorf("the seek read from %d, before its landing packet at %d", src.Low, at)
	}
}

// TestAReadTakesOverFromAWalk. A seek counts a little past its target, and
// the read after it catches that count up and carries it on. A seek further
// on walks again from what the read counted, a read to the end completes the
// count, and every landing after it is the one a walk to the end gives.
func TestAReadTakesOverFromAWalk(t *testing.T) {
	raw := readFixture(t, windowsFixture)
	objs, fl := wmacount.Objects(t, raw)
	walked := open(t, raw)
	if err := walked.Walk(); err != nil {
		t.Fatal(err)
	}
	d := open(t, raw)
	if _, err := d.SeekSample(0, objs[len(objs)/2].Before*fl); err != nil {
		t.Fatal(err)
	}
	// The walk stands an object or two past the target, which six objects
	// read from the landing pass.
	var pkt container.Packet
	for range 6 {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
	}
	j := len(objs) - 2
	landed, err := d.SeekSample(0, objs[j].Before*fl)
	if err != nil {
		t.Fatal(err)
	}
	if landed != objs[j-1].Before*fl {
		t.Fatalf("a seek after the read took the count over landed at %d, want %d", landed, objs[j-1].Before*fl)
	}
	if err := readAll(t, d); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !d.Walked() {
		t.Fatal("a read to the end after a seek left the count unwalked")
	}
	if got := d.IndexSnapshot(); got == nil || !bytes.Equal(got, walked.IndexSnapshot()) {
		t.Error("the count the walk began and the read finished is not the one a walk takes")
	}
	for j, o := range objs {
		target := o.Before*fl + fl/2
		want := landingFor(objs, fl, target)
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		if landed != objs[want].Before*fl {
			t.Fatalf("object %d: seek to %d landed at %d, want %d", j, target, landed, objs[want].Before*fl)
		}
	}
}

// TestAReadJoinsTheCountInsideAPacket: a landing stepped into a packet's own
// objects tells the read which object it delivers, so a read from there
// joins the count at the right one, wherever in a packet the count stands.
func TestAReadJoinsTheCountInsideAPacket(t *testing.T) {
	var counts [][]byte
	for i := range 20 {
		counts = append(counts, []byte{byte(1 + i%3), 2, byte(3 - i%2)})
	}
	raw := reservoirStream(t, counts)
	walked := open(t, raw)
	if err := walked.Walk(); err != nil {
		t.Fatal(err)
	}
	want := walked.Tracks()[0].Samples
	for _, target := range []int64{9 * 2048, 21 * 2048, 40 * 2048, 58 * 2048} {
		d := open(t, raw)
		if _, err := d.SeekSample(0, target); err != nil {
			t.Fatal(err)
		}
		if err := readAll(t, d); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if got := d.Tracks()[0]; !d.Walked() || !got.SamplesExact || got.Samples != want {
			t.Errorf("a read after a seek to %d settled %d (exact %v, walked %v), a walk %d",
				target, got.Samples, got.SamplesExact, d.Walked(), want)
		}
		if got := d.IndexSnapshot(); got == nil || !bytes.Equal(got, walked.IndexSnapshot()) {
			t.Errorf("after a seek to %d the read's count is not the one a walk takes", target)
		}
	}
}

// TestASeekAfterAReadCountsOnFromIt. A read from the start counts what it
// passes, so a seek past it walks on from the last packet the read counted,
// not from the top.
func TestASeekAfterAReadCountsOnFromIt(t *testing.T) {
	raw := readFixture(t, windowsFixture)
	objs, fl := wmacount.Objects(t, raw)
	start, plen, packets := packetGeometry(t, raw)
	if int64(len(objs)) != packets {
		t.Fatalf("%d objects in %d packets; the test reads object j as packet j", len(objs), packets)
	}
	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := asf.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	const read = 8
	var pkt container.Packet
	for range read {
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
	}
	src.Reset()
	j := len(objs) - 3
	landed, err := d.SeekSample(0, objs[j].Before*fl+fl/2)
	if err != nil {
		t.Fatal(err)
	}
	if landed != objs[j-1].Before*fl {
		t.Errorf("the seek landed at %d, the count puts object %d at %d", landed, j-1, objs[j-1].Before*fl)
	}
	if at := start + (read-1)*plen; src.Reads == 0 || src.Low < at {
		t.Errorf("the seek read from %d; the read had counted to the packet at %d", src.Low, at)
	}
}

// TestADamagedReadSettlesNothing. Damage on the read's way costs the count
// what it costs a walk: the decoder restarts at the hole, so neither a
// settled length nor a sidecar comes of it.
func TestADamagedReadSettlesNothing(t *testing.T) {
	var counts [][]byte
	for range 20 {
		counts = append(counts, []byte{2, 1})
	}
	raw := reservoirStream(t, counts)
	start, plen, _ := packetGeometry(t, raw)
	for i := range 4 {
		raw[start+9*plen+int64(i)] = 0xff
	}
	d := open(t, raw)
	if err := readAll(t, d); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(d.Warnings()) == 0 {
		t.Fatal("the damage went unreported; the test proves nothing")
	}
	if tr := d.Tracks()[0]; tr.SamplesExact {
		t.Errorf("a read past damage settled %d samples", tr.Samples)
	}
	if d.IndexSnapshot() != nil {
		t.Error("a read past damage left a count to save")
	}
}

// foldedStream is reservoirStream's twenty packets of two objects with
// packet 9 stamped before packet 8, a fold that costs no object.
func foldedStream(t *testing.T) []byte {
	t.Helper()
	b := reservoirBuilder()
	b.playHNS = b.prerollMS*10_000 + 20*100*10_000
	pres, obj := uint32(b.prerollMS), byte(0)
	for i := range 20 {
		at := pres + uint32(i*100)
		if i == 9 {
			at -= 150
		}
		b.compressedPacket(uint32(i*100), at, 20, [][]byte{{2, obj, 0, 0}, {1, obj + 1, 0, 0}})
		obj += 2
	}
	return b.build()
}

// TestACountWithAFoldedStampKeepsItsSidecar: stamps that run backwards cost
// no object and leave the count exact, so the count is worth a sidecar like
// any other. The warning travels in it, whether a walk or a read found it,
// and a restore reports it where a walk would, refusing under Strict as a
// walk does.
func TestACountWithAFoldedStampKeepsItsSidecar(t *testing.T) {
	raw := foldedStream(t)
	const fold = "presentation times run backwards"
	has := func(ws []container.Warning) bool {
		return slices.ContainsFunc(ws, func(w container.Warning) bool { return w.Msg == fold })
	}
	walked := open(t, raw)
	if err := walked.Walk(); err != nil {
		t.Fatal(err)
	}
	if !has(walked.Warnings()) {
		t.Fatal("the walk found no fold; the test proves nothing")
	}
	blob := walked.IndexSnapshot()
	if blob == nil {
		t.Fatal("a count whose only finding is a fold left no sidecar")
	}
	read := open(t, raw)
	if err := readAll(t, read); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if got := read.IndexSnapshot(); !bytes.Equal(got, blob) {
		t.Error("the count a read took does not carry what the walk's does")
	}

	r := open(t, raw)
	if !r.RestoreIndex(blob) {
		t.Fatal("the sidecar was refused")
	}
	if err := r.Walk(); err != nil {
		t.Fatal(err)
	}
	if !has(r.Warnings()) {
		t.Errorf("the restored count reported %v, want the fold a walk reports", r.Warnings())
	}
	strict, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strict.RestoreIndex(blob) {
		t.Fatal("the sidecar was refused under Strict")
	}
	if strict.Walk() == nil || strict.Walked() {
		t.Error("under Strict the restored count took the fold a walk refuses")
	}
}

// TestAHoleBreaksTheCarryPair: the carry check reads an object against the
// one before it, and across a hole the object before it is gone, so neither
// a walk nor a read asks.
func TestAHoleBreaksTheCarryPair(t *testing.T) {
	b := reservoirBuilder()
	pres := uint32(b.prerollMS)
	for j := range 20 {
		size := uint32(4)
		if j == 7 {
			size = 0 // dropped as damage
		}
		b.packet(uint32(j*20), byte(j), 0, size, pres+uint32(j*20), []byte{2, byte(j), 0, 0})
	}
	raw := b.build()
	for _, how := range []string{"walk", "read"} {
		d := open(t, raw)
		var asked []byte
		asf.UncarriedForTest(d, func(_, pkt []byte) (bool, error) {
			asked = append(asked, pkt[1])
			return false, nil
		})
		if how == "walk" {
			if err := d.Walk(); err != nil {
				t.Fatal(err)
			}
		} else if err := readAll(t, d); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if !slices.Contains(asked, 9) {
			t.Fatalf("%s: the count asked about %v; the test proves nothing", how, asked)
		}
		if slices.Contains(asked, 8) {
			t.Errorf("%s: the count asked about object 8 across the hole object 7 left", how)
		}
	}
}

// TestSeekLandsOnTheFrameIndex is the WMA v1/v2 landing. ASF states
// presentation times in milliseconds, and the codec resumes on a frame grid
// the stamps miss: by up to 39 samples on ffmpeg's files and by exactly two
// frames on every object past the first on Windows'. A landing reported off
// the stamps puts every caller's pre-roll short by the difference, so the
// audio after a seek is shifted under a position that claims to be exact.
// Counted from the superframe headers it is exact for both writers. It is
// one object early, as run-up for the object holding the target.
func TestSeekLandsOnTheFrameIndex(t *testing.T) {
	for _, name := range v1v2Fixtures {
		t.Run(filepath.Base(name), func(t *testing.T) {
			raw := readFixture(t, name)
			objs, frameLen := wmacount.Objects(t, raw)
			d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			pastFirst, landings := 0, 0
			for j, o := range objs {
				for _, target := range []int64{o.Before * frameLen, o.Before*frameLen + frameLen/2} {
					want := landingFor(objs, frameLen, target)
					landed, err := d.SeekSample(0, target)
					if err != nil {
						t.Fatalf("seek to %d: %v", target, err)
					}
					if wantAt := objs[want].Before * frameLen; landed != wantAt {
						t.Fatalf("object %d: seek to %d landed at %d, the frame counts put object %d at %d",
							j, target, landed, want, wantAt)
					}
					var pkt container.Packet
					if err := d.ReadPacket(&pkt); err != nil {
						t.Fatalf("object %d: reading after a seek to %d: %v", j, target, err)
					}
					if pkt.PTS != objs[want].PTS || len(pkt.Data) != objs[want].Size {
						t.Fatalf("object %d: seek to %d read the object at %d (%d bytes), want object %d at %d (%d bytes)",
							j, target, pkt.PTS, len(pkt.Data), want, objs[want].PTS, objs[want].Size)
					}
					landings++
					if want > 0 {
						pastFirst++
					}
				}
			}
			if name == windowsFixture && 2*pastFirst < landings {
				t.Errorf("%d of %d landings were past the first object; the gate proves little", pastFirst, landings)
			}
		})
	}
}

// TestWalkSettlesTheLength is the measurement a walk of the frame counts
// stands behind: exact, and what a decode of the same bytes delivers. ASF's
// own play duration is a rounded total on either side of that.
func TestWalkSettlesTheLength(t *testing.T) {
	for _, name := range v1v2Fixtures {
		t.Run(filepath.Base(name), func(t *testing.T) {
			raw := readFixture(t, name)
			d, err := asf.NewDemuxer(container.BytesSource(raw), &asf.DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.SamplesExact || !tr.SamplesAdvisory || d.Walked() {
				t.Fatalf("before the walk: exact %v advisory %v walked %v, want an advisory length not yet walked",
					tr.SamplesExact, tr.SamplesAdvisory, d.Walked())
			}
			if err := d.Walk(); err != nil {
				t.Fatal(err)
			}
			tr := d.Tracks()[0]
			if !tr.SamplesExact || tr.SamplesAdvisory || !d.Walked() {
				t.Fatalf("after the walk: exact %v advisory %v walked %v", tr.SamplesExact, tr.SamplesAdvisory, d.Walked())
			}
			if got := decodedLength(t, raw); tr.Samples != got {
				t.Errorf("the walk settled %d samples, a decode delivers %d", tr.Samples, got)
			}
			// The walk read through its own window: the reader is still at
			// the first object.
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != 0 {
				t.Errorf("after the walk the first read is at %d (%v), want the first object", pkt.PTS, err)
			}
		})
	}
}

// decodedLength opens raw through the format layer and counts what a read to
// the end delivers.
func decodedLength(t *testing.T, raw []byte) int64 {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "wma", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n += int64(buf.N)
	}
}

// TestIndexSnapshotRoundTrip is the sidecar: a count taken once serves the
// next open, whose seek into the tail reads nothing before the packet it
// lands on and whose Walk settles the length without reading at all.
func TestIndexSnapshotRoundTrip(t *testing.T) {
	raw := readFixture(t, windowsFixture)
	objs, fl := wmacount.Objects(t, raw)
	start, plen, packets := packetGeometry(t, raw)
	if int64(len(objs)) != packets {
		t.Fatalf("%d objects in %d packets; the test reads object j as packet j", len(objs), packets)
	}
	walked := open(t, raw)
	if walked.IndexSnapshot() != nil {
		t.Error("a count not yet taken gave a snapshot")
	}
	if err := walked.Walk(); err != nil {
		t.Fatal(err)
	}
	blob := walked.IndexSnapshot()
	if blob == nil {
		t.Fatalf("a walked count of %d packets gave no snapshot", packets)
	}

	src := &testutil.CountingSource{Src: container.BytesSource(raw)}
	d, err := asf.NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.RestoreIndex(blob) {
		t.Fatal("a fresh open refused its own file's snapshot")
	}
	if !d.Walked() {
		t.Error("a restored complete count does not read as walked")
	}
	if d.IndexSnapshot() != nil {
		t.Error("a restored count that has not grown asked to be saved again")
	}
	j := len(objs) - 3
	target := objs[j].Before*fl + fl/2
	src.Reset()
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatal(err)
	}
	if landed != objs[j-1].Before*fl {
		t.Errorf("seek to %d landed at %d, the walked count puts object %d at %d", target, landed, j-1, objs[j-1].Before*fl)
	}
	if at := start + int64(j-1)*plen; src.Reads > 0 && src.Low < at {
		t.Errorf("the seek read from %d, before the landing packet at %d", src.Low, at)
	}
	reads := src.Reads
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if src.Reads != reads {
		t.Errorf("Walk read %d times after a complete restore", src.Reads-reads)
	}
	if got, want := d.Tracks()[0], walked.Tracks()[0]; !got.SamplesExact || got.Samples != want.Samples {
		t.Errorf("Walk after the restore settled %d (exact %v), the walk that took the count %d",
			got.Samples, got.SamplesExact, want.Samples)
	}
}

// packetGeometry reads where the data packets start, how long each is and how
// many there are, from the File Properties and Data objects.
func packetGeometry(t *testing.T, raw []byte) (start, packetLen, packets int64) {
	t.Helper()
	fp := headerObject(t, raw, guidFileProperties)
	packetLen = int64(le.Uint32(fp[24+68:]))
	data := int64(le.Uint64(raw[16:]))
	if !bytes.Equal(raw[data:data+16], guidData) {
		t.Fatal("no Data Object behind the header")
	}
	return data + 50, packetLen, int64(le.Uint64(raw[data+40:]))
}

// TestACountFinishedAnyWaySettlesTheTrack: a walk reached the end, so the
// track reports its length, whatever reached it. A seek past the end counts
// to the end and a sidecar can restore a complete count, and either used to
// leave Walked true beside the rounded declaration, which a caller trusting
// Walked then kept.
func TestACountFinishedAnyWaySettlesTheTrack(t *testing.T) {
	raw := readFixture(t, windowsFixture)
	want := decodedLength(t, raw)
	check := func(how string, d *asf.Demuxer) {
		t.Helper()
		if !d.Walked() {
			t.Fatalf("%s: not walked", how)
		}
		if tr := d.Tracks()[0]; !tr.SamplesExact || tr.SamplesAdvisory || tr.Samples != want {
			t.Errorf("%s: the track reports %d (exact %v advisory %v), a decode delivers %d",
				how, tr.Samples, tr.SamplesExact, tr.SamplesAdvisory, want)
		}
	}
	d := open(t, raw)
	if _, err := d.SeekSample(0, 1<<40); err != nil {
		t.Fatal(err)
	}
	check("a seek past the end", d)

	walked := open(t, raw)
	if err := walked.Walk(); err != nil {
		t.Fatal(err)
	}
	r := open(t, raw)
	if !r.RestoreIndex(walked.IndexSnapshot()) {
		t.Fatal("the snapshot was refused")
	}
	check("a complete restore", r)

	// And through the format layer, whose Media carries its own copy, taken
	// by Walk, which a finished count makes free.
	med, err := format.Open(container.BytesSource(raw), "wma", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if _, err := med.SeekSample(1 << 40); err != nil {
		t.Fatal(err)
	}
	if !format.MediaWalked(med) {
		t.Fatal("the media does not report the walk a seek finished")
	}
	if err := format.WalkMedia(med); err != nil {
		t.Fatal(err)
	}
	if tr := med.Info().Default(); !tr.SamplesExact || tr.Samples != want {
		t.Errorf("the media's Walk settled %d (exact %v), a decode delivers %d", tr.Samples, tr.SamplesExact, want)
	}
}
