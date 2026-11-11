package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestTruncatedSampleTableCountsWhatItStillDeclares: a movie whose sample
// data is cut short keeps a table timing every packet it was written with.
// The ones the file no longer holds are counted at their own durations, so
// what is kept and what is counted add up to the whole file's run.
func TestTruncatedSampleTableCountsWhatItStillDeclares(t *testing.T) {
	full := fixture(t, "alac-stereo.m4a")
	// ALAC has no priming and no padding, so the played length is the run.
	run := func(tr container.Track) int64 { return tr.Samples }
	whole, err := NewDemuxer(container.BytesSource(full), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := container.TruncatedOf(whole, whole.Tracks()[0].ID); got != 0 {
		t.Fatalf("%d samples truncated from the whole file", got)
	}
	want := run(whole.Tracks()[0])
	cuts := 0
	for n := len(full) - 1; n > 0; n -= 997 {
		d, err := NewDemuxer(container.BytesSource(full[:n]), nil)
		if err != nil {
			continue // the cut reached the movie's own boxes
		}
		short := false
		for _, w := range d.Warnings() {
			short = short || strings.Contains(w.Msg, "runs past end of file")
		}
		if !short {
			continue
		}
		cuts++
		tr := d.Tracks()[0]
		if got := container.TruncatedOf(d, tr.ID); got <= 0 || run(tr)+got != want {
			t.Fatalf("cut to %d bytes: %d samples kept and %d truncated, want them to make the whole file's %d",
				n, run(tr), got, want)
		}
	}
	if cuts == 0 {
		t.Fatal("no cut left a table running past the end of the file; the fixture's movie box is not ahead of its data")
	}
}

// TestTimeShort: the samples a table declares past the ones kept are timed
// by the stts runs they fall in, wherever the cut lands among the runs, and
// by the last cadence where the stts covers fewer samples than the table.
func TestTimeShort(t *testing.T) {
	for _, c := range []struct {
		name         string
		stts         []sttsEntry
		total, short int64
		want         int64
	}{
		{"one run", []sttsEntry{{1000, 1024}}, 500, 500, 512000},
		{"a short last run", []sttsEntry{{999, 1024}, {1, 300}}, 500, 500, 499*1024 + 300},
		{"the cut inside the second run", []sttsEntry{{100, 1024}, {900, 1024}}, 500, 500, 512000},
		{"a run wholly kept", []sttsEntry{{400, 1024}, {600, 512}}, 900, 100, 51200},
		{"the cut inside the first run", []sttsEntry{{600, 1024}, {400, 512}}, 500, 500, 100*1024 + 400*512},
		{"an stts past the table", []sttsEntry{{2000, 1024}}, 500, 500, 512000},
		{"an stts short of the table", []sttsEntry{{700, 1024}}, 500, 500, 512000},
		{"an stts short of what is kept", []sttsEntry{{300, 1024}}, 500, 500, 512000},
		{"nothing short", []sttsEntry{{1000, 1024}}, 1000, 0, 0},
		{"a delta of zero still advances", []sttsEntry{{1000, 0}}, 500, 500, 500},
		{"a count no length can hold", []sttsEntry{{1 << 40, 1 << 40}}, 1, 1<<40 - 1, math.MaxInt64},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &sampleTable{total: c.total, short: c.short}
			if got := st.timeShort(c.stts, false, 48000, 48000); got != c.want {
				t.Fatalf("timed %d samples, want %d", got, c.want)
			}
		})
	}
	// A media timescale that is not the rate is rescaled, run by run.
	st := &sampleTable{total: 500, short: 500}
	if got := st.timeShort([]sttsEntry{{1000, 2048}}, true, 48000, 96000); got != 512000 {
		t.Fatalf("timed %d samples across a rescale, want 512000", got)
	}
}

// TestTruncationIsCountedInPlayedSamples: what a file cut short lost is
// counted on the timeline its length is on, the played one. A cut that only
// took end padding lost no audio, and one that reached the audio lost what is
// between the length the tags state and the length the run can now hold.
func TestTruncationIsCountedInPlayedSamples(t *testing.T) {
	// The whole table times 10000 raw samples: 1000 of priming, 8000 played,
	// 1000 of padding.
	const whole, delay, played = 10000, 1000, 8000
	for _, c := range []struct {
		name       string
		smpb       bool
		held, want int64
	}{
		{"cut inside the padding", true, 9500, 0},
		{"cut at the end of the audio", true, 9000, 0},
		{"cut inside the audio", true, 7000, 2000},
		{"cut inside the priming", true, 600, 8000},
		{"no tags, so the run is the length", false, 7000, 3000},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &Demuxer{smpbOK: c.smpb, smpbDelay: delay, smpbTotal: played}
			tr := &track{fmt: audio.Format{Rate: 44100}}
			tr.st.totalDur, tr.st.shortDur = c.held, whole-c.held
			_, _, samples, _ := d.gapless(tr)
			if tr.st.truncated != c.want {
				t.Fatalf("%d samples truncated beside %d played, want %d", tr.st.truncated, samples, c.want)
			}
			if c.smpb && samples+tr.st.truncated != played {
				t.Fatalf("%d played and %d truncated do not make the %d the tag states", samples, tr.st.truncated, played)
			}
		})
	}
}

// TestFragmentedReadToItsEndHoldsTheDeclaredCount: a fragmented movie cut at
// a fragment boundary reads to a clean end, short of what its head declares.
// The read that reaches that end reports it, as a walk does, so the damage is
// found where it lies and counted; the length stays the walk's to settle.
func TestFragmentedReadToItsEndHoldsTheDeclaredCount(t *testing.T) {
	const npkt, dur = 40, 40 * 960
	full := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
		return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
	})
	cut := full[:bytes.LastIndex(full, []byte("moof"))-4] // the last fragment, whole, gone
	const lost = 2 * 960                                  // segmentize puts two packets in a fragment
	shortfall := func(d *Demuxer) (damage int) {
		for _, w := range d.Warnings() {
			if w.Kind == container.Damage && strings.Contains(w.Msg, "the fragments hold") {
				damage++
			}
		}
		return damage
	}
	readAll := func(d *Demuxer) (int64, error) {
		var pkt container.Packet
		var end int64
		for {
			if err := d.ReadPacket(&pkt); err != nil {
				return end, err
			}
			end = pkt.PTS + pkt.Dur
		}
	}

	d, err := openFrag(t, cut, false)
	if err != nil {
		t.Fatal(err)
	}
	if end, err := readAll(d); err != io.EOF || end != dur-lost {
		t.Fatalf("the read ended at %d with %v, want a clean end at %d", end, err, dur-lost)
	}
	if n := shortfall(d); n != 1 {
		t.Errorf("%d damage warnings naming the shortfall after a read to the end, want 1: %v", n, d.Warnings())
	}
	if got := container.TruncatedOf(d, d.Tracks()[0].ID); got != lost {
		t.Errorf("%d samples truncated after a read to the end, want %d", got, lost)
	}
	if d.Walked() {
		t.Error("a read to the end settled the length; that is Walk's to do")
	}
	// The walk that follows says the same thing, once.
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, got := shortfall(d), container.TruncatedOf(d, d.Tracks()[0].ID); n != 1 || got != lost {
		t.Errorf("after the walk: %d warnings and %d samples truncated, want 1 and %d", n, got, lost)
	}

	// A read that began at a seek knows the whole run from the index it built.
	d, err = openFrag(t, cut, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(context.Background(), d.Tracks()[0].ID, dur/2); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(d); err != io.EOF {
		t.Fatalf("the read after a seek ended with %v", err)
	}
	if n, got := shortfall(d), container.TruncatedOf(d, d.Tracks()[0].ID); n != 1 || got != lost {
		t.Errorf("after a seek and a read to the end: %d warnings and %d samples truncated, want 1 and %d", n, got, lost)
	}

	// A whole file reads to its end with nothing to report.
	d, err = openFrag(t, full, false)
	if err != nil {
		t.Fatal(err)
	}
	if end, err := readAll(d); err != io.EOF || end != dur {
		t.Fatalf("the whole file read to %d with %v", end, err)
	}
	if n, got := len(d.Warnings()), container.TruncatedOf(d, d.Tracks()[0].ID); n != 0 || got != 0 {
		t.Errorf("the whole file: %d warnings and %d samples truncated: %v", n, got, d.Warnings())
	}

	// Strict refuses at the end it reaches, as a strict walk does. The length
	// here is an edit list's, which a strict open has no bytes to check
	// against (a sidx that outruns the file is refused before any read).
	track, pkts := opusTrackFor(312, npkt*960-312, npkt)
	head, media := segmentize(t, track, pkts, 2*960)
	edited := append(append([]byte(nil), head...), media...)
	edited = edited[:bytes.LastIndex(edited, []byte("moof"))-4]
	d, err = openFrag(t, edited, true)
	if err != nil {
		t.Fatalf("a strict open of a movie cut at a fragment boundary: %v", err)
	}
	if _, err := readAll(d); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Fatalf("a strict read to the end of a truncated file = %v, want malformed", err)
	}
	if _, err := readAll(d); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Fatalf("the read after it = %v, want the same refusal", err)
	}
	d, err = openFrag(t, edited, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(d); err != io.EOF {
		t.Fatalf("a tolerant read ended with %v", err)
	}
	if got := container.TruncatedOf(d, d.Tracks()[0].ID); got != lost {
		t.Errorf("%d samples truncated under an edit list's length, want %d", got, lost)
	}
}

// TestHybridMovieCountsItsShortfallOnce: a movie with both a sample table and
// fragments has two places a shortfall is found, the table's own run past the
// file's data and the head's declared length against everything held. The
// second is measured over a table already shrunk by the first, so it contains
// it, and the count is the larger of the two rather than their sum.
func TestHybridMovieCountsItsShortfallOnce(t *testing.T) {
	// The table declared 1500 samples and holds 1000; two fragments of 1920
	// follow it, placed where the held table ends.
	const tableHeld, tableShort, perFrag = 1000, 500, 1920
	durs, sizes := []uint32{960, 960}, []uint32{4, 4}
	d := craftFragmentsFrom(false, fragmentFile([]uint64{tableHeld, tableHeld + perFrag}, durs, sizes, 8))
	d.sel.st.totalDur, d.sel.st.tickDur = tableHeld, tableHeld
	d.sel.st.shortDur, d.sel.st.truncated = tableShort, tableShort
	d.track = container.Track{Samples: tableHeld + tableShort + 2*perFrag}
	if got := container.TruncatedOf(d, 0); got != tableShort {
		t.Fatalf("%d samples truncated before any walk, want the table's %d", got, tableShort)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := container.TruncatedOf(d, 0); got != tableShort {
		t.Fatalf("%d samples truncated after the walk, want %d: the head's shortfall is the table's, counted once", got, tableShort)
	}
}

// TestFragmentedShortfallIsOnThePlayedTimeline: fragments that end inside the
// priming hold none of the declared audio, and the priming itself is not
// audio the file lost.
func TestFragmentedShortfallIsOnThePlayedTimeline(t *testing.T) {
	d := craftFragmentsFrom(false, fragmentFile([]uint64{0}, []uint32{200}, []uint32{4}, 8))
	d.track = container.Track{Samples: 5000, Delay: 312}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := container.TruncatedOf(d, 0); got != 5000 {
		t.Fatalf("%d samples truncated, want the declared 5000: the run ends inside the 312 of priming", got)
	}
}
