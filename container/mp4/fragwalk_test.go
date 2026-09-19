package mp4

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestFragmentedWalkSettlesTheDeclaredCount: a walk turns whatever the head
// declared into a measurement, and says nothing about an intact file whichever
// timeline its writer stated the count on.
func TestFragmentedWalkSettlesTheDeclaredCount(t *testing.T) {
	const npkt, dur = 8, 8 * 960

	t.Run("a sidx count becomes exact", func(t *testing.T) {
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if d.Walked() {
			t.Fatal("a fragmented movie defers a walk; Walked must be false before it runs")
		}
		if err := d.Walk(); err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Samples != dur || !tr.SamplesExact {
			t.Fatalf("after Walk: Samples = %d exact=%v, want %d and exact", tr.Samples, tr.SamplesExact, dur)
		}
		if !d.Walked() {
			t.Error("Walked must be true after a walk that reached the end")
		}
		if len(d.Warnings()) != 0 {
			t.Errorf("an intact file warned: %v", d.Warnings())
		}
	})

	t.Run("a count on the presentation timeline is intact too", func(t *testing.T) {
		// ffmpeg's +delay_moov+global_sidx shape: the index sums to the truns
		// less the priming, so the raw run is Delay + Samples and the compare
		// must accept it as agreement rather than call it a shortfall.
		raw := fragFileWithSidx(t, npkt, 312, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d - 312)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Walk(); err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Delay != 312 || tr.Samples != dur-312 || !tr.SamplesExact {
			t.Fatalf("after Walk: Delay=%d Samples=%d exact=%v, want 312 / %d / true",
				tr.Delay, tr.Samples, tr.SamplesExact, dur-312)
		}
		if len(d.Warnings()) != 0 {
			t.Errorf("a presentation-timeline count warned: %v", d.Warnings())
		}
	})

	t.Run("a header duration becomes exact", func(t *testing.T) {
		raw := fragFileWithSidx(t, npkt, 0, nil)
		raw = withMediaDuration(raw, dur+7) // advisory and wrong, as headers are
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; !tr.SamplesAdvisory || tr.Samples != dur+7 {
			t.Fatalf("Samples = %d advisory=%v, want %d and advisory", tr.Samples, tr.SamplesAdvisory, dur+7)
		}
		if err := d.Walk(); err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Samples != dur || !tr.SamplesExact || tr.SamplesAdvisory {
			t.Fatalf("after Walk: Samples = %d exact=%v advisory=%v, want %d exact",
				tr.Samples, tr.SamplesExact, tr.SamplesAdvisory, dur)
		}
	})

	t.Run("a hybrid movie counts both halves", func(t *testing.T) {
		const tableFrames, nfrag, perFrag, delta = 6, 3, 4, 960
		raw := hybridMovie(movie{entry: hybridEntry(t), unitBytes: 40, frames: tableFrames,
			chunkFrames: 3, timescale: 48000, sttsDelta: delta}, nfrag, perFrag, true)
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Walk(); err != nil {
			t.Fatal(err)
		}
		want := int64((tableFrames + nfrag*perFrag) * delta)
		if tr := d.Tracks()[0]; tr.Samples != want || !tr.SamplesExact {
			t.Fatalf("after Walk: Samples = %d exact=%v, want %d exact", tr.Samples, tr.SamplesExact, want)
		}
	})

	t.Run("a progressive movie walks nothing", func(t *testing.T) {
		raw := movie{entry: v1SoundEntry("twos", 1, 16, movieTimescale, 2, waveBox(endaBox(1))),
			unitBytes: 2, frames: 64, chunkFrames: 16}.build()
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Walked() {
			t.Fatal("a progressive movie defers nothing; Walked must be true at open")
		}
		before := d.Tracks()[0]
		if err := d.Walk(); err != nil {
			t.Fatal(err)
		}
		after := d.Tracks()[0]
		if after.Samples != before.Samples || after.SamplesExact != before.SamplesExact ||
			after.Padding != before.Padding || after.SamplesAdvisory != before.SamplesAdvisory {
			t.Fatalf("Walk changed a progressive track's length fields (%d/%v -> %d/%v); settling its "+
				"sample table would flip SamplesExact on a total nothing verified",
				before.Samples, before.SamplesExact, after.Samples, after.SamplesExact)
		}
	})
}

// TestFragmentedWalkShrinksATruncatedFile: a declared count the fragments
// cannot supply is damage, and the walk shortens the length to what is there.
func TestFragmentedWalkShrinksATruncatedFile(t *testing.T) {
	const npkt, dur = 40, 40 * 960
	full := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
		return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
	})
	cut := full[:len(full)-100]

	d, err := openFrag(t, cut, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Samples >= dur {
		t.Fatalf("after Walk: Samples = %d, want less than the declared %d", tr.Samples, dur)
	}
	if !tr.SamplesExact {
		t.Error("a finished walk measures the payload, so its count is exact")
	}
	if !d.Walked() {
		t.Error("the walk reached the end of what is there; under tolerance that counts as walked")
	}
	var damaged bool
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage && strings.Contains(w.Msg, "the fragments hold") {
			damaged = true
		}
	}
	if !damaged {
		t.Errorf("a short file produced no Damage warning naming the shortfall: %v", d.Warnings())
	}

	strict, err := openFrag(t, cut, true)
	if err == nil {
		err = strict.Walk()
	}
	if !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Fatalf("a strict walk of a truncated file = %v, want malformed", err)
	}
}

// TestFragmentedSeekLandsThroughTheIndex: the walk's index replaces the
// per-seek payload sweep, so the second seek reads no moof payloads at all.
func TestFragmentedSeekLandsThroughTheIndex(t *testing.T) {
	const npkt, perFrag = 40, 2
	track, pkts := opusTrackFor(312, int64(npkt)*960-312, npkt)
	init, media := segmentize(t, track, pkts, perFrag*960)
	file := append(append([]byte(nil), init...), media...)

	src := &testutil.CountingSource{Src: container.BytesSource(file)}
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 5 * 960, 21 * 960, 39 * 960} {
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		want := target / (perFrag * 960) * (perFrag * 960)
		if landed != want {
			t.Fatalf("seek to %d landed at %d, want the fragment base %d", target, landed, want)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if pkt.PTS != landed {
			t.Fatalf("seek to %d reported %d, first packet is at %d", target, landed, pkt.PTS)
		}
	}
	// The walk ran once, inside the first seek. Every seek after it reads only
	// the fragment it lands on.
	src.Reset()
	if _, err := d.SeekSample(0, 13*960); err != nil {
		t.Fatal(err)
	}
	if src.Reads != 0 {
		t.Errorf("a seek after the walk read %d times (%d bytes); the index is the landing",
			src.Reads, src.Bytes)
	}
}

// TestFragmentedSeekWithoutTfdt: a movie whose fragments carry no tfdt states
// where each one begins nowhere, so the reader has to keep the running sum
// itself. The scan this replaced read every base as 0, which made no fragment
// ever "start after the target": every seek landed on the last fragment and
// reported position 0, and each fragment restarted the timeline at 0 as it was
// read, so PTS went backwards.
//
// The timescale is not the sample rate, which is where the sum has to be kept
// in ticks: summing the per-sample rescaled durations floors once per sample
// and drifts from where a tfdt would have put the fragment.
func TestFragmentedSeekWithoutTfdt(t *testing.T) {
	const tableFrames, nfrag, perFrag = 0, 4, 3
	// A 90 kHz media timescale against a 48 kHz rate: 1875 ticks is 1000
	// samples exactly, so the expectations stay readable while the rescale is
	// genuinely exercised.
	const delta = 1875
	m := movie{entry: hybridEntry(t), unitBytes: 40, frames: tableFrames,
		chunkFrames: 1, timescale: 90000, sttsDelta: delta}
	raw := hybridMovie(m, nfrag, perFrag, false)

	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, pts := readFrag(t, d)
	if len(pts) != nfrag*perFrag {
		t.Fatalf("delivered %d samples, want %d", len(pts), nfrag*perFrag)
	}
	for i, p := range pts {
		if want := int64(i) * 1000; p != want {
			t.Fatalf("sample %d has PTS %d, want %d: a fragment with no tfdt continues the timeline",
				i, p, want)
		}
	}

	// A twin whose fragments do carry a tfdt must give the identical timeline:
	// the absent tfdt is reconstructed, not approximated.
	twin, err := NewDemuxer(container.BytesSource(hybridMovie(m, nfrag, perFrag, true)), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, twinPTS := readFrag(t, twin)
	if len(twinPTS) != len(pts) {
		t.Fatalf("the tfdt-bearing twin delivered %d samples, not %d", len(twinPTS), len(pts))
	}
	for i := range pts {
		if pts[i] != twinPTS[i] {
			t.Fatalf("sample %d: %d without a tfdt, %d with one", i, pts[i], twinPTS[i])
		}
	}

	for _, tc := range []struct{ target, want int64 }{
		{0, 0},
		{3500, 3000},
		{9000, 9000},
		{11500, 9000},
	} {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := d.SeekSample(0, tc.target)
		if err != nil {
			t.Fatalf("seek to %d: %v", tc.target, err)
		}
		if landed != tc.want {
			t.Fatalf("seek to %d landed at %d, want %d", tc.target, landed, tc.want)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek to %d: %v", tc.target, err)
		}
		if pkt.PTS != landed {
			t.Fatalf("seek to %d reported %d, first packet is at %d", tc.target, landed, pkt.PTS)
		}
	}
}

// withMediaDuration rewrites the mdhd duration of a version-0 media header, so
// a crafted file can state a length in its head and nowhere else. That is the
// Smooth Streaming shape: mvhd and mdhd durations over an empty sample table
// with no segment index.
func withMediaDuration(raw []byte, samples int64) []byte {
	out := append([]byte(nil), raw...)
	i := indexBox(out, "mdhd")
	if i < 0 {
		return out
	}
	// version(1) flags(3) creation(4) modification(4) timescale(4) duration(4)
	binary.BigEndian.PutUint32(out[i+8+4+4+4+4:], uint32(samples))
	return out
}

// indexBox finds a box's header offset by its type, for a test that patches one
// field of a built file.
func indexBox(raw []byte, typ string) int {
	i := strings.Index(string(raw), typ)
	if i < 4 {
		return -1
	}
	return i - 4
}
