package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// hybridMovie builds a movie carrying both: a populated sample table in the
// moov and a run of fragments behind it. That is what `ffmpeg -movflags
// frag_keyframe` writes without `empty_moov`, and skipping the table (which
// every mvex-bearing movie used to do) silently dropped its samples.
//
// It grows the moov of a progressive movie by an mvex box and shifts the chunk
// offsets by the same amount, which is the only thing in the table that knows
// where the mdat sits. tfdt says whether the fragments state their own base
// decode time; without one the reader has to continue from the table's end.
func hybridMovie(m movie, nfrag, perFrag int, tfdt bool) []byte {
	raw := m.build()
	ftypLen := int(binary.BigEndian.Uint32(raw[0:4]))
	moovLen := int(binary.BigEndian.Uint32(raw[ftypLen : ftypLen+4]))
	mvex := makeBox("mvex", makeFullBox("trex", 0, 0,
		u32(trackID), u32(1), u32(0), u32(0), u32(0)))

	out := append([]byte(nil), raw[:ftypLen+moovLen]...)
	out = append(out, mvex...)
	binary.BigEndian.PutUint32(out[ftypLen:], uint32(moovLen+len(mvex)))

	chunkFrames := m.chunkFrames
	if chunkFrames <= 0 {
		chunkFrames = m.frames
	}
	chunks := 0
	if m.frames > 0 {
		chunks = (m.frames + chunkFrames - 1) / chunkFrames
	}
	at := bytes.LastIndex(out[:ftypLen+moovLen], []byte("stco")) + 12
	for c := range chunks {
		p := out[at+4*c:]
		binary.BigEndian.PutUint32(p, binary.BigEndian.Uint32(p)+uint32(len(mvex)))
	}
	out = append(out, raw[ftypLen+moovLen:]...) // the mdat the table indexes

	delta := m.sttsDelta
	if delta <= 0 {
		delta = 1
	}
	base := int64(m.frames * delta)
	for f := range nfrag {
		durs := make([]uint32, perFrag)
		sizes := make([]uint32, perFrag)
		for i := range durs {
			durs[i], sizes[i] = uint32(delta), uint32(m.unitBytes)
		}
		frag := fragmentBoxes(uint32(f+1), base+int64(f*perFrag*delta), durs, sizes,
			make([]byte, perFrag*m.unitBytes))
		if !tfdt {
			frag = stripTfdt(frag)
		}
		out = append(out, frag...)
	}
	return out
}

// stripTfdt removes the tfdt from a fragment's traf, shrinking every box that
// contained it. A movie whose fragments carry none is the shape the running
// decode time exists for: Smooth Streaming times its fragments with a uuid
// tfxd box instead, and nothing else states where a fragment begins.
func stripTfdt(frag []byte) []byte {
	i := bytes.Index(frag, []byte("tfdt"))
	if i < 4 {
		return frag
	}
	start := i - 4
	size := int(binary.BigEndian.Uint32(frag[start:]))
	out := append(append([]byte(nil), frag[:start]...), frag[start+size:]...)
	// moof, traf and the trun's data_offset all shrink by the box removed.
	moofSize := int(binary.BigEndian.Uint32(out[0:4]))
	binary.BigEndian.PutUint32(out[0:], uint32(moofSize-size))
	j := bytes.Index(out, []byte("traf"))
	trafSize := int(binary.BigEndian.Uint32(out[j-4:]))
	binary.BigEndian.PutUint32(out[j-4:], uint32(trafSize-size))
	k := bytes.Index(out, []byte("trun"))
	binary.BigEndian.PutUint32(out[k+4+4+4:], uint32(moofSize-size+8))
	return out
}

// hybridEntry is a sample entry for a codec whose sample is many frames, so
// the fragmented reader accepts the track: a byte-linear one is skipped in a
// fragmented movie by design (its trun would carry an entry per frame).
func hybridEntry(t *testing.T) []byte {
	t.Helper()
	track, _ := opusTrackFor(0, -1, 1)
	entry, err := sampleEntryFor(track)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// TestHybridMovieDeliversBothHalves: a movie whose moov carries samples and
// whose moofs carry more must deliver all of them, in order, on one continuous
// timeline. Before the table was built for such a movie, the moov's samples
// were dropped without a warning: the read path served fragments only.
func TestHybridMovieDeliversBothHalves(t *testing.T) {
	const tableFrames, nfrag, perFrag, delta, unit = 6, 3, 4, 960, 40
	m := movie{entry: hybridEntry(t), unitBytes: unit, frames: tableFrames,
		chunkFrames: 3, timescale: 48000, sttsDelta: delta}

	for _, tfdt := range []bool{true, false} {
		name := "with tfdt"
		if !tfdt {
			name = "without tfdt"
		}
		t.Run(name, func(t *testing.T) {
			raw := hybridMovie(m, nfrag, perFrag, tfdt)
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !d.fragmented {
				t.Fatal("the movie carries an mvex; it must read as fragmented")
			}
			if got := d.sel.st.total; got != tableFrames {
				t.Fatalf("the moov's table holds %d samples, want %d; it was skipped", got, tableFrames)
			}

			_, pts := readFrag(t, d)
			want := tableFrames + nfrag*perFrag
			if len(pts) != want {
				t.Fatalf("delivered %d samples, want %d (%d from the table, %d from the fragments)",
					len(pts), want, tableFrames, nfrag*perFrag)
			}
			for i, p := range pts {
				if p != int64(i*delta) {
					t.Fatalf("sample %d has PTS %d, want %d: the timeline must be continuous across the seam",
						i, p, i*delta)
				}
			}
		})
	}
}

// TestHybridMovieSeeksIntoBothHalves: a target below the table's end lands in
// the table and the fragments behind it are rewound with it; one at or past it
// lands in the fragments.
func TestHybridMovieSeeksIntoBothHalves(t *testing.T) {
	const tableFrames, nfrag, perFrag, delta, unit = 6, 3, 4, 960, 40
	raw := hybridMovie(movie{entry: hybridEntry(t), unitBytes: unit, frames: tableFrames,
		chunkFrames: 3, timescale: 48000, sttsDelta: delta}, nfrag, perFrag, true)
	total := tableFrames + nfrag*perFrag

	for _, target := range []int64{0, 2 * delta, 5 * delta, 6 * delta, 9 * delta, 17 * delta} {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		landed, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek to %d: %v", target, err)
		}
		if landed > target {
			t.Fatalf("seek to %d landed at %d, past the target", target, landed)
		}
		_, pts := readFrag(t, d)
		if len(pts) == 0 {
			t.Fatalf("seek to %d delivered nothing", target)
		}
		if pts[0] != landed {
			t.Fatalf("seek to %d reported %d and delivered from %d", target, landed, pts[0])
		}
		if want := int64(total)*delta - landed; int64(len(pts))*delta != want {
			t.Fatalf("seek to %d delivered %d samples, want %d", target, len(pts), want/delta)
		}
		for i, p := range pts {
			if p != landed+int64(i)*delta {
				t.Fatalf("after a seek to %d, sample %d has PTS %d, want %d",
					target, i, p, landed+int64(i)*delta)
			}
		}
	}
}
