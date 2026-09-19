package mp4

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

// fragmentFile builds a run of moof+mdat pairs with the given base times and
// per-fragment sample sizes: the shapes no muxer writes, for the structural
// cells that need one.
func fragmentFile(baseTimes []uint64, durs, sizes []uint32, mdat int) []byte {
	var file []byte
	for i, base := range baseTimes {
		mfhd := makeFullBox("mfhd", 0, 0, u32(uint32(i+1)))
		file = append(file, makeBox("moof", mfhd, trafBox(1, base, durs, sizes))...)
		file = append(file, makeBox("mdat", make([]byte, mdat))...)
	}
	return file
}

// craftFragmentsFrom wires a Demuxer to a crafted fragment run, bypassing the
// movie header the shapes below have no use for.
func craftFragmentsFrom(strict bool, file []byte) *Demuxer {
	src := container.BytesSource(file)
	d := &Demuxer{
		src:        src,
		size:       int64(len(file)),
		opts:       DemuxerOptions{Strict: strict},
		w:          srcwin.New(src, int64(len(file)), "mp4: test"),
		fragmented: true,
		trex:       trexDefaults{have: true},
		sel: &track{
			id:        1,
			timescale: 48000,
			fmt:       audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		},
	}
	d.track = container.Track{Samples: -1}
	return d
}

func craftFragments(strict bool, baseTimes []uint64, durs, sizes []uint32, mdat int) *Demuxer {
	return craftFragmentsFrom(strict, fragmentFile(baseTimes, durs, sizes, mdat))
}

// TestFragmentedWalkLatchesOnlyWhenItSettles: the walk's "already walked" flag
// belongs after the settlement, not before it. Set first, a Strict compare
// failure left Walked true over a track nothing had settled, and every later
// Walk took the idempotent early return and reported success.
func TestFragmentedWalkLatchesOnlyWhenItSettles(t *testing.T) {
	const npkt, dur = 8, 8 * 960
	// A complete index (coverage reaches the end, so no truncation warning)
	// that nonetheless claims more samples than the fragments hold.
	raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
		return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d + 4096)}})
	})

	t.Run("tolerant", func(t *testing.T) {
		d, err := openFrag(t, raw, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Walk(); err != nil {
			t.Fatal(err)
		}
		if !d.Walked() {
			t.Fatal("a walk that settled must latch")
		}
		if got := d.Tracks()[0].Samples; got != dur {
			t.Fatalf("Samples = %d, want the %d the fragments hold", got, dur)
		}
	})

	t.Run("strict", func(t *testing.T) {
		d, err := openFrag(t, raw, true)
		if err != nil {
			t.Fatal(err)
		}
		first := d.Walk()
		if !errors.Is(first, waxerr.ErrMalformedInput) {
			t.Fatalf("first Walk = %v, want malformed", first)
		}
		if d.Walked() {
			t.Fatal("Walked is true after a walk that refused to settle")
		}
		if second := d.Walk(); !errors.Is(second, waxerr.ErrMalformedInput) {
			t.Fatalf("second Walk = %v, want the same refusal; the latch made it a silent success", second)
		}
		if d.Tracks()[0].SamplesExact {
			t.Error("a refused walk left the track marked exact")
		}
	})
}

// TestFragmentedWalkNamesTheDamageItStopsAt: a box chain that stops making
// sense is the end of the payload, and it is Damage. Settling the shortened
// count as exact with nothing said would let a truncated file claim to have
// been measured, and Strict would accept it.
func TestFragmentedWalkNamesTheDamageItStopsAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		file []byte
		says string
		// want is the length a tolerant walk settles on: only the fragments
		// ahead of the damage.
		want int64
	}{
		{
			name: "a fragment whose samples run past the source",
			file: fragmentFile([]uint64{0, 960}, []uint32{960}, []uint32{1 << 20}, 32),
			says: "runs past the end of the source",
			want: 0,
		},
		{
			name: "a box size past the end",
			file: corruptSecondMoofSize(fragmentFile([]uint64{0, 960}, []uint32{960}, []uint32{8}, 32)),
			says: "fragment chain ends at",
			want: 960,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := craftFragmentsFrom(false, tc.file)
			if err := d.Walk(); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, w := range d.Warnings() {
				if w.Kind == container.Damage && strings.Contains(w.Msg, tc.says) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no Damage warning saying %q: %v", tc.says, d.Warnings())
			}
			if got := d.Tracks()[0].Samples; got != tc.want {
				t.Errorf("Samples = %d, want %d: only what lies ahead of the damage", got, tc.want)
			}

			if err := craftFragmentsFrom(true, tc.file).Walk(); !errors.Is(err, waxerr.ErrMalformedInput) {
				t.Fatalf("strict walk = %v, want malformed", err)
			}
		})
	}
}

// corruptSecondMoofSize grows the second moof's declared size past the bytes
// that follow it, which is what a truncated download looks like from the box
// chain: the header parses and the box it names is not there.
func corruptSecondMoofSize(file []byte) []byte {
	out := append([]byte(nil), file...)
	first := int(be32(out[0:]))
	mdat := int(be32(out[first:]))
	at := first + mdat
	binary.BigEndian.PutUint32(out[at:], uint32(len(out)-at+64))
	return out
}

// TestFragmentedIndexStaysSorted: fragmentAt binary-searches the index, which
// needs the base times non-decreasing. A tfdt is a writer's claim, so a crafted
// file can point one backwards; such a fragment is not a landing place, and the
// search must not be handed an unsorted slice.
func TestFragmentedIndexStaysSorted(t *testing.T) {
	d := craftFragments(false, []uint64{0, 4800, 960, 9600}, []uint32{960}, []uint32{8}, 32)
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if len(d.fragIndex) == 0 {
		t.Fatal("the walk indexed nothing")
	}
	for i := 1; i < len(d.fragIndex); i++ {
		if d.fragIndex[i].base < d.fragIndex[i-1].base {
			t.Fatalf("index is not sorted: entry %d base %d follows %d",
				i, d.fragIndex[i].base, d.fragIndex[i-1].base)
		}
	}
	// The backwards fragment still counts toward the length: the reader
	// produces its samples whatever its tfdt claims.
	if got := d.Tracks()[0].Samples; got != 10560 {
		t.Errorf("Samples = %d, want 10560 (the furthest end any fragment reaches)", got)
	}
	// And every landing is still at or before its target.
	for _, target := range []int64{0, 1000, 5000, 20000} {
		landed, _, err := d.seekFragmented(target)
		if err != nil {
			t.Fatal(err)
		}
		if landed > target {
			t.Errorf("seek to %d landed at %d", target, landed)
		}
	}
}

// TestFragmentedSeekDoesNotSettle: a seek builds the index and leaves the
// length alone. Settling there flipped Walked() true behind format.media's
// back, and a run about to commit a count into headers it cannot patch reads
// that flag: it would have committed the unverified number.
func TestFragmentedSeekDoesNotSettle(t *testing.T) {
	const npkt = 8
	raw := fragFileWithSidx(t, npkt, 0, func(mediaLen, d int64) []byte {
		return sidxBox(1, 48000, 0, 0, []sidxRef{{size: uint32(mediaLen), dur: uint32(d)}})
	})
	d, err := openFrag(t, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	before := d.Tracks()[0]
	if _, err := d.SeekSample(0, 3*960); err != nil {
		t.Fatal(err)
	}
	if d.Walked() {
		t.Error("a seek reported the payload as measured; only Walk measures it")
	}
	after := d.Tracks()[0]
	if after.Samples != before.Samples || after.SamplesExact != before.SamplesExact {
		t.Errorf("a seek changed the length (%d/%v -> %d/%v)",
			before.Samples, before.SamplesExact, after.Samples, after.SamplesExact)
	}
	// And the index it built is not rebuilt by the walk that follows.
	if !d.fragIndexed {
		t.Error("the seek did not leave an index behind")
	}
	if err := d.Walk(); err != nil {
		t.Fatal(err)
	}
	if !d.Walked() || !d.Tracks()[0].SamplesExact {
		t.Error("the walk after a seek did not settle")
	}
}

// TestHeaderDurationIgnoresUnknown: the spec spells "duration unknown" as all
// ones, which a fragmented writer often means literally. mvhd and mehd mapped
// it to absent from the start; mdhd, the tier the resolver reads first, did
// not, so it arrived as 4294967295 ticks of advisory length.
func TestHeaderDurationIgnoresUnknown(t *testing.T) {
	const npkt = 8
	base := fragFileWithSidx(t, npkt, 0, nil)
	for _, tc := range []struct {
		name string
		dur  uint32
		want int64
	}{
		{"a real duration is advisory", 12345, 12345},
		{"all ones is no duration at all", 0xFFFFFFFF, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := openFrag(t, withMediaDuration(base, int64(tc.dur)), false)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Tracks()[0].Samples; got != tc.want {
				t.Fatalf("Samples = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMovieHeaderKeepsItsTimescaleOnAShortBox: the timescale and the duration
// are separate fields, and the duration is a last-resort fallback. Requiring
// both would drop the movie timescale an edit list is rescaled against for the
// sake of a number this may never read.
func TestMovieHeaderKeepsItsTimescaleOnAShortBox(t *testing.T) {
	// version 0, creation + modification + timescale, and nothing after it.
	short := makeFullBox("mvhd", 0, 0, u32(0), u32(0), u32(44100))
	_, _, rest, ok := fullBox(short[8:])
	if !ok {
		t.Fatal("the crafted box does not parse as a full box")
	}
	ts, dur := mvhdTime(short[8:])
	if ts != 44100 {
		t.Errorf("timescale = %d over a %d-byte payload, want 44100", ts, len(rest))
	}
	if dur != 0 {
		t.Errorf("duration = %d, want 0: the box does not carry one", dur)
	}
}

// TestMvexPicksTheSelectedTracksDefaults: one trex per track, and a traf may
// omit a duration, size or flags and defer to its own. Keeping only the last
// box applied the video track's defaults to the audio track's samples.
func TestMvexPicksTheSelectedTracksDefaults(t *testing.T) {
	trex := func(id, dur, size uint32) []byte {
		return makeFullBox("trex", 0, 0, u32(id), u32(1), u32(dur), u32(size), u32(0))
	}
	for _, tc := range []struct {
		name  string
		boxes [][]byte
		sel   int
		want  trexDefaults
	}{
		{"the selected track's, not the last", [][]byte{trex(1, 1024, 100), trex(2, 3000, 900)}, 1,
			trexDefaults{trackID: 1, defaultDur: 1024, defaultSize: 100, have: true}},
		{"the later one when it is the selected track's", [][]byte{trex(1, 1024, 100), trex(2, 3000, 900)}, 2,
			trexDefaults{trackID: 2, defaultDur: 3000, defaultSize: 900, have: true}},
		{"a lone trex whatever id it names", [][]byte{trex(7, 1024, 100)}, 1,
			trexDefaults{trackID: 7, defaultDur: 1024, defaultSize: 100, have: true}},
		{"none when several name other tracks", [][]byte{trex(2, 1, 1), trex(3, 2, 2)}, 1, trexDefaults{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Demuxer{}
			d.parseMvex(makeBox("mvex", tc.boxes...)[8:])
			if got := trexFor(d.trexes, tc.sel); got != tc.want {
				t.Fatalf("trexFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestFragmentedMovieOpensWithoutAnStsz: a fragmented movie's table is used
// only if it is complete. The completeness check below it is fatal for an audio
// track, and before the hybrid change no mvex-bearing movie reached it at all,
// so a moov carrying stts and chunks but no stsz has to keep opening.
func TestFragmentedMovieOpensWithoutAnStsz(t *testing.T) {
	const npkt = 4
	track, pkts := opusTrackFor(0, -1, npkt)
	init, media := segmentize(t, track, pkts, 2*960)
	// The init's stbl is the empty fragmented one; give it stts and chunk
	// entries and no stsz, which is an incomplete table however it got there.
	patched := replaceBox(init, "stts", makeFullBox("stts", 0, 0, u32(1), u32(4), u32(960)))
	patched = replaceBox(patched, "stco", makeFullBox("stco", 0, 0, u32(1), u32(0)))
	patched = replaceBox(patched, "stsz", nil)

	d, err := NewDemuxer(container.BytesSource(append(patched, media...)), nil)
	if err != nil {
		t.Fatalf("a fragmented movie with an incomplete table failed to open: %v", err)
	}
	if got := d.sel.st.total; got != 0 {
		t.Errorf("the incomplete table yielded %d samples; it must be treated as no table", got)
	}
	if _, pts := readFrag(t, d); len(pts) != npkt {
		t.Errorf("delivered %d samples, want the %d in the fragments", len(pts), npkt)
	}
}

// replaceBox swaps the first box of a type inside a built file, resizing every
// container box that holds it. A nil replacement removes it.
func replaceBox(raw []byte, typ string, with []byte) []byte {
	at := indexBox(raw, typ)
	if at < 0 {
		return raw
	}
	size := int(be32(raw[at:]))
	out := append([]byte(nil), raw[:at]...)
	out = append(out, with...)
	out = append(out, raw[at+size:]...)
	delta := len(with) - size
	// Every ancestor's size moves with it. They are the boxes whose span
	// contains at, which for a built movie is ftyp-less: moov/trak/mdia/minf/stbl.
	for _, anc := range []string{"moov", "trak", "mdia", "minf", "stbl"} {
		a := indexBox(out, anc)
		if a < 0 || a > at {
			continue
		}
		binary.BigEndian.PutUint32(out[a:], uint32(int(be32(out[a:]))+delta))
	}
	return out
}
