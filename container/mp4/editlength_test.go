package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// editTrack is a track of raw samples at rate with one edit list.
func editTrack(rate int, raw, mediaTime, segDur int64, edits int) *track {
	t := &track{
		fmt:        audio.Format{Rate: rate},
		timescale:  int64(rate),
		hasEdit:    true,
		editMedia:  mediaTime,
		editSegDur: segDur,
		edits:      edits,
	}
	t.st.totalDur = raw
	return t
}

// TestEditListStatesTheEndTrim: the progressive reader takes an edit list's
// duration as the played length, with a delay or without, and reads a
// coarse movie timescale as the rounding it is, so an edit that only
// restates the table's length trims nothing.
func TestEditListStatesTheEndTrim(t *testing.T) {
	for _, tc := range []struct {
		name               string
		rate               int
		movieTimescale     int64
		raw, media, segDur int64
		edits              int
		delay, samples     int64
	}{
		// This engine's progressive muxer: one edit from 0, in samples.
		{"no delay, an end trim in samples", 48000, 48000, 50176, 0, 50000, 1, 0, 50000},
		{"no delay, an end trim in milliseconds", 44100, 1000, 45939, 0, 1000, 1, 0, 44144},
		// ffmpeg's ALAC, FLAC, PCM and stream-copied AAC: the length in
		// milliseconds, rounded up, or down by a writer that rounds so.
		{"no delay, the length rounded up", 44100, 1000, 45939, 0, 1042, 1, 0, 45939},
		{"no delay, the length rounded down", 44100, 1000, 45939, 0, 1041, 1, 0, 45939},
		{"no delay, the length in samples", 44100, 44100, 45939, 0, 45939, 1, 0, 45939},
		{"no delay, an edit past the table", 48000, 48000, 50176, 0, 60000, 1, 0, 50176},
		{"no delay, a cut list", 48000, 48000, 48000, 0, 20000, 2, 0, 48000},
		// ffmpeg's AAC and MP3: the length less the delay, each rounded.
		{"a delay, the length rounded down", 44100, 1000, 46963, 1024, 1041, 1, 1024, 45939},
		{"a delay, the length rounded up", 48000, 1000, 51026, 1024, 1042, 1, 1024, 50002},
		{"a delay, an end trim in samples", 48000, 48000, 51200, 1024, 50002, 1, 1024, 50002},
		{"a delay, an end trim in milliseconds", 44100, 1000, 46963, 1024, 1000, 1, 1024, 44144},
		{"a delay, a cut list", 48000, 48000, 51200, 1024, 20000, 2, 1024, 50176},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Demuxer{movieTimescale: tc.movieTimescale}
			tr := editTrack(tc.rate, tc.raw, tc.media, tc.segDur, tc.edits)
			delay, padding, samples, advisory := d.gapless(tr)
			if delay != tc.delay || samples != tc.samples || padding != tc.raw-tc.delay-tc.samples || advisory {
				t.Errorf("gapless = delay %d, padding %d, samples %d, advisory %v; want %d, %d, %d, false",
					delay, padding, samples, advisory, tc.delay, tc.raw-tc.delay-tc.samples, tc.samples)
			}
		})
	}
}

// TestLongestIn: a duration converts to the longest length it can be the
// rounding of, which is the duration itself where a tick is a whole sample.
func TestLongestIn(t *testing.T) {
	for _, tc := range []struct{ ticks, rate, timescale, want int64 }{
		{50000, 48000, 48000, 50000},
		{1, 48000, 48000, 1},
		{10, 48000, 96000, 5},
		{11, 48000, 96000, 5},
		{1041, 44100, 1000, 45952}, // under 1042 ms, which is 45952.2 samples
		{1042, 44100, 1000, 45996},
		{100, 8000, 1000, 807}, // under 101 ms, which is 808 samples
		{600, 44100, 600, 44173},
	} {
		if got := longestIn(tc.ticks, tc.rate, tc.timescale); got != tc.want {
			t.Errorf("longestIn(%d, %d, %d) = %d, want %d", tc.ticks, tc.rate, tc.timescale, got, tc.want)
		}
	}
}

// TestFragmentedEditInSamplesIsExact: the fragmented reader's length from an
// edit list in samples is the edit's own, with a delay or without.
func TestFragmentedEditInSamplesIsExact(t *testing.T) {
	for _, media := range []int64{0, 1024} {
		d := &Demuxer{movieTimescale: 48000}
		delay, samples, exact, advisory, err := d.fragmentedLength(editTrack(48000, 0, media, 50000, 1))
		if err != nil || delay != media || samples != 50000 || !exact || advisory {
			t.Errorf("media_time %d: delay %d, samples %d, exact %v, advisory %v (%v); want %d, 50000, true, false",
				media, delay, samples, exact, advisory, err, media)
		}
	}
}

// coarseFragmented writes packets of 1024 samples as fragmented MP4, then
// restates its edit list as ticks of a movie timescale of 1000.
func coarseFragmented(t *testing.T, delay int64, packets int, ticks uint64) []byte {
	t.Helper()
	track := aacTrackAt(t, 44100)
	track.Delay, track.Samples = delay, int64(packets)*1024-delay
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	for i := range packets {
		pkt := codec.Packet{Data: []byte{byte(i), 1, 2, 3}, PTS: int64(i) * 1024, Dur: 1024, Sync: true}
		if err := m.WritePacket(container.Packet{Packet: pkt}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.End(codec.Trailer{Samples: track.Samples, Delay: delay}); err != nil {
		t.Fatal(err)
	}
	file := out.Bytes()
	mvhd, elst := bytes.Index(file, []byte("mvhd")), bytes.Index(file, []byte("elst"))
	if mvhd < 0 || elst < 0 || file[mvhd+4] != 0 || file[elst+4] != 1 {
		t.Fatal("no version 0 mvhd and version 1 elst to patch")
	}
	binary.BigEndian.PutUint32(file[mvhd+16:], 1000)
	binary.BigEndian.PutUint64(file[elst+12:], ticks)
	return file
}

// TestFragmentedCoarseEditIsABound: a fragmented edit in a timescale coarser
// than the samples states the longest length its ticks can mean, declared
// and not exact. Fragments that hold any length those ticks can be the
// rounding of are whole, and a walk caps the run at the bound.
func TestFragmentedCoarseEditIsABound(t *testing.T) {
	const packets = 69
	for _, delay := range []int64{0, 1024} {
		held := int64(packets)*1024 - delay
		for _, tc := range []struct {
			name      string
			ticks     uint64
			declared  int64 // Samples at open: the bound
			samples   int64 // Samples once walked
			truncated int64
		}{
			{"restates, rounded down", uint64(held * 1000 / 44100), 0, held, 0},
			{"restates, rounded up", uint64(held*1000/44100) + 1, 0, held, 0},
			{"trims", 1500, 66194, 66194, 0},
			{"states more than the fragments hold", 1700, 75014, held, 74926 - held},
		} {
			name := tc.name
			if delay > 0 {
				name += ", with a delay"
			}
			t.Run(name, func(t *testing.T) {
				file := coarseFragmented(t, delay, packets, tc.ticks)
				want := tc.declared
				if want == 0 {
					want = longestIn(int64(tc.ticks), 44100, 1000)
				}
				strict, err := NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := strict.Walk(context.Background()); (err != nil) != (tc.truncated > 0) {
					t.Fatalf("a strict walk = %v, want a refusal only of a short file", err)
				}
				d := open(t, file)
				if tr := d.Tracks()[0]; tr.Delay != delay || tr.Samples != want || tr.SamplesExact || tr.SamplesAdvisory || !tr.TrimsUnread {
					t.Fatalf("opens with Delay %d, Samples %d, exact %v, advisory %v, TrimsUnread %v; want %d, the bound %d, declared, its end unread",
						tr.Delay, tr.Samples, tr.SamplesExact, tr.SamplesAdvisory, tr.TrimsUnread, delay, want)
				}
				var pkt container.Packet
				var raw int64
				for {
					err := d.ReadPacket(&pkt)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					raw += pkt.Dur
				}
				if raw != held+delay {
					t.Fatalf("read %d samples of packets, want %d", raw, held+delay)
				}
				if got := d.TruncatedSamples(0); got != tc.truncated || (d.DamageCount() > 0) != (tc.truncated > 0) {
					t.Errorf("a read to the end counts %d truncated and %d damage; want %d", got, d.DamageCount(), tc.truncated)
				}
				if err := d.Walk(context.Background()); err != nil {
					t.Fatal(err)
				}
				if tr := d.Tracks()[0]; tr.Samples != tc.samples || tr.Padding != held-tc.samples || !tr.SamplesExact || tr.TrimsUnread {
					t.Errorf("walks to Samples %d, Padding %d, exact %v, TrimsUnread %v; want %d, %d, true, false",
						tr.Samples, tr.Padding, tr.SamplesExact, tr.TrimsUnread, tc.samples, held-tc.samples)
				}
			})
		}
	}
}

// TestParseElstCountsTheEditsThatPlay: the count that tells one run from a
// cut list comes from the box. An empty edit plays no media, it only starts
// the track late, so it is not counted. An edit of no duration is: a writer
// stating one played run writes one edit, and a list of two is left to the
// sample table whatever the second holds.
func TestParseElstCountsTheEditsThatPlay(t *testing.T) {
	type edit struct{ dur, media int64 }
	box := func(version byte, edits ...edit) []byte {
		b := []byte{version, 0, 0, 0}
		b = binary.BigEndian.AppendUint32(b, uint32(len(edits)))
		for _, e := range edits {
			if version == 1 {
				b = binary.BigEndian.AppendUint64(b, uint64(e.dur))
				b = binary.BigEndian.AppendUint64(b, uint64(e.media))
			} else {
				b = binary.BigEndian.AppendUint32(b, uint32(e.dur))
				b = binary.BigEndian.AppendUint32(b, uint32(int32(e.media)))
			}
			b = append(b, 0, 1, 0, 0) // media rate 1.0
		}
		return b
	}
	for _, tc := range []struct {
		name    string
		list    []edit
		edits   int
		segDur  int64
		empty   int64
		samples int64 // of a 48000-sample table, in a 48000 timescale
	}{
		{"one edit from 0", []edit{{40000, 0}}, 1, 40000, 0, 40000},
		{"two edits from 0", []edit{{10000, 0}, {10000, 30000}}, 2, 20000, 0, 48000},
		{"an empty edit, then one from 0", []edit{{500, -1}, {40000, 0}}, 1, 40000, 500, 40000},
		{"one edit, then one of no duration", []edit{{40000, 0}, {0, 45000}}, 2, 40000, 0, 48000},
		{"two edits from a delay", []edit{{10000, 1024}, {10000, 30000}}, 2, 20000, 0, 48000 - 1024},
	} {
		for _, version := range []byte{0, 1} {
			tr := editTrack(48000, 48000, -1, 0, 0)
			tr.hasEdit = false
			parseElst(tr, box(version, tc.list...))
			lead := int64(parseChapterEdits(box(version, tc.list...)).lead)
			if !tr.hasEdit || tr.edits != tc.edits || tr.editSegDur != tc.segDur || lead != tc.empty {
				t.Errorf("%s (version %d): edits %d, duration %d, empty %d; want %d, %d, %d",
					tc.name, version, tr.edits, tr.editSegDur, lead, tc.edits, tc.segDur, tc.empty)
			}
			d := &Demuxer{movieTimescale: 48000}
			if _, _, samples, _ := d.gapless(tr); samples != tc.samples {
				t.Errorf("%s (version %d): reads %d samples, want %d", tc.name, version, samples, tc.samples)
			}
		}
	}
}
