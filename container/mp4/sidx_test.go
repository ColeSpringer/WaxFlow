package mp4

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// sidxRef is one reference in a segment index: its byte size, the presentation
// time it covers, and whether it points at a child index rather than at media.
type sidxRef struct {
	size, dur   uint32
	hierarchial bool
}

// sidxBox builds a segment index box, version 0.
func sidxBox(refID, timescale, ept, firstOffset uint32, refs []sidxRef) []byte {
	body := [][]byte{u32(refID), u32(timescale), u32(ept), u32(firstOffset), u16(0), u16(uint16(len(refs)))}
	for _, r := range refs {
		size := r.size
		if r.hierarchial {
			size |= 1 << 31
		}
		body = append(body, u32(size), u32(r.dur), u32(1<<31|1<<28)) // starts_with_SAP, SAP type 1
	}
	return makeFullBox("sidx", 0, 0, body...)
}

// fragFileWithSidx assembles a self-contained fragmented Opus file whose
// segment index sits between the init header and the fragments: init, sidx,
// media. mk is handed the media's byte length and its total duration in the
// track's own 48 kHz ticks, and returns the box to insert.
//
// The track declares no length, so the init carries no edit-list segment
// duration and the sidx tier is the one under test. That is also the shape
// ffmpeg's default fragmented output has: no edit list, zeroed header
// durations, and a length stated only where a writer chose to state it.
func fragFileWithSidx(t *testing.T, npkt int, preSkip int64, mk func(mediaLen, dur int64) []byte) []byte {
	t.Helper()
	track, pkts := opusTrackFor(preSkip, -1, npkt)
	init, media := segmentize(t, track, pkts, 2*960)
	raw := append([]byte(nil), init...)
	if mk != nil {
		raw = append(raw, mk(int64(len(media)), int64(npkt)*960)...)
	}
	return append(raw, media...)
}

// openFrag opens a crafted fragmented file and returns its demuxer.
func openFrag(t *testing.T, raw []byte, strict bool) (*Demuxer, error) {
	t.Helper()
	return NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: strict})
}

// TestFragmentedSidxStatesTheLength is the sidx tier, one cell per structural
// field of the box and one per shape a real writer produces.
//
// The count is declared, not exact: nothing has read the fragments, so both
// flags stay false and the number sits in the same tier as a progressive
// movie's sample table.
func TestFragmentedSidxStatesTheLength(t *testing.T) {
	const npkt = 8
	const dur = npkt * 960

	t.Run("a complete index is the headers' own count", func(t *testing.T) {
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Samples != dur {
			t.Fatalf("Samples = %d, want the index's %d", tr.Samples, dur)
		}
		if tr.SamplesExact || tr.SamplesAdvisory {
			t.Errorf("exact=%v advisory=%v; a declared count carries neither flag until a walk",
				tr.SamplesExact, tr.SamplesAdvisory)
		}
		if len(d.Warnings()) != 0 {
			t.Errorf("a complete index warned: %v", d.Warnings())
		}
	})

	t.Run("a delay beside it stays in Delay", func(t *testing.T) {
		// The Opus pre-skip shape: an edit with a media time and no segment
		// duration. A sidx is timed on the presentation timeline, so its sum is
		// already the played length and the delay is never subtracted from it.
		raw := fragFileWithSidx(t, npkt, 312, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d - 312)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Delay != 312 {
			t.Fatalf("Delay = %d, want the edit's 312", tr.Delay)
		}
		if tr.Samples != dur-312 {
			t.Fatalf("Samples = %d, want the index's %d (the delay is not subtracted again)", tr.Samples, dur-312)
		}
	})

	t.Run("a timescale that is not the rate rescales", func(t *testing.T) {
		// Twice the rate with an odd sum: the rescale loses half a tick, so the
		// count is advisory rather than declared.
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 96000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(2*d - 1)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Samples != dur-1 {
			t.Fatalf("Samples = %d, want %d", tr.Samples, dur-1)
		}
		if !tr.SamplesAdvisory {
			t.Error("an inexact rescale must be advisory: the number is rounded")
		}
	})

	t.Run("an exact rescale stays declared", func(t *testing.T) {
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 96000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(2 * d)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		tr := d.Tracks()[0]
		if tr.Samples != dur || tr.SamplesAdvisory {
			t.Fatalf("Samples = %d advisory=%v, want %d and not advisory", tr.Samples, tr.SamplesAdvisory, dur)
		}
	})

	t.Run("an index of a prefix is ignored", func(t *testing.T) {
		// ffmpeg's +dash shape: one sidx per fragment, so coverage stops at a
		// moof and the sum is a fraction of the length.
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen / 4), dur: uint32(d / 4)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Tracks()[0].Samples; got != -1 {
			t.Fatalf("Samples = %d, want -1: the index covers a prefix", got)
		}
	})

	t.Run("an index for another track is ignored", func(t *testing.T) {
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(7, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Tracks()[0].Samples; got != -1 {
			t.Fatalf("Samples = %d, want -1: the index names reference_ID 7", got)
		}
	})

	t.Run("a trailing box at the coverage end is complete", func(t *testing.T) {
		// ffmpeg's +global_sidx writes an mfra after the last fragment, so
		// coverage ends one box short of the file and is still complete.
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
		})
		raw = append(raw, makeBox("mfra", u32(0))...)
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Tracks()[0].Samples; got != dur {
			t.Fatalf("Samples = %d, want %d: an mfra past the coverage is not more segments", got, dur)
		}
	})

	t.Run("a hierarchical index sums the same way", func(t *testing.T) {
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return sidxBox(1, 48000, 0, 0, []sidxRef{
				{size: uint32(mediaLen / 2), dur: uint32(d / 2), hierarchial: true},
				{size: uint32(mediaLen - mediaLen/2), dur: uint32(d - d/2)},
			})
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Tracks()[0].Samples; got != dur {
			t.Fatalf("Samples = %d, want %d", got, dur)
		}
	})

	t.Run("a first_offset shifts the coverage", func(t *testing.T) {
		// A free box between the index and the fragments: first_offset is the
		// distance from the box's end to the first referenced item, so coverage
		// still reaches the file's end.
		free := makeBox("free", make([]byte, 24))
		raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
			return append(sidxBox(1, 48000, 0, uint32(len(free)),
				[]sidxRef{{size: uint32(mediaLen), dur: uint32(d)}}), free...)
		})
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Tracks()[0].Samples; got != dur {
			t.Fatalf("Samples = %d, want %d: first_offset skips the free box", got, dur)
		}
	})
}

// TestFragmentedSidxNamesATruncation: an index describing more bytes than the
// file holds is a truncated download rather than a bad index. The count is kept
// (it is what the writer meant to deliver) and the shortfall is named, so a
// strict open refuses and a tolerant one says how much is missing.
func TestFragmentedSidxNamesATruncation(t *testing.T) {
	const npkt = 40
	raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
		return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
	})
	cut := raw[:len(raw)-100] // mid-fragment, so the index outruns the bytes

	d, err := openFrag(t, cut, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Tracks()[0].Samples; got != npkt*960 {
		t.Fatalf("Samples = %d, want the declared %d", got, npkt*960)
	}
	var found bool
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage && strings.Contains(w.Msg, "past the end of the source") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a truncated file produced no Damage warning: %v", d.Warnings())
	}
	if _, err := openFrag(t, cut, true); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Fatalf("a strict open of a truncated file = %v, want malformed", err)
	}
}

// TestFragmentedSidxOnABareSegment: the HLS client's live probe opens a
// zero-length media source against a fetched init. The scan finds nothing and
// says so rather than failing the open.
func TestFragmentedSidxOnABareSegment(t *testing.T) {
	track, pkts := opusTrackFor(312, -1, 4)
	init, _ := segmentize(t, track, pkts, 2*960)
	d, err := NewFragmentedDemuxer(init, container.BytesSource(nil))
	if err != nil {
		t.Fatalf("opening a zero-length segment source: %v", err)
	}
	if got := d.Tracks()[0].Samples; got != -1 {
		t.Fatalf("Samples = %d, want -1", got)
	}
}
