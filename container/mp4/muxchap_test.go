package mp4

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
)

// chapterSpacing is the gap between synthetic chapters. It is a whole number
// of chapterTimescale ticks, so a round trip that loses nothing reads the
// starts back exactly.
const chapterSpacing = 100 * time.Millisecond

// chaptersFor builds n synthetic chapters at chapterSpacing intervals.
func chaptersFor(n int) []container.Chapter {
	out := make([]container.Chapter, n)
	for i := range out {
		out[i] = container.Chapter{
			Start: time.Duration(i) * chapterSpacing,
			Title: fmt.Sprintf("Chapter %d", i+1),
		}
	}
	return out
}

// muxChapterFile writes a progressive movie carrying chapters over npkt Opus
// packets (20 ms each at 48 kHz), closed with trailer. The trailer is the
// caller's because it decides the presentation the chapter track is measured
// against: a delay or padding trims the movie below the raw sample total.
func muxChapterFile(t *testing.T, chapters []container.Chapter, npkt int, trailer codec.Trailer) []byte {
	t.Helper()
	track, pkts := opusTrackFor(trailer.Delay, trailer.Samples, npkt)
	return muxChapterTrack(t, chapters, track, pkts, trailer)
}

// muxChapterTrack writes a progressive movie carrying chapters over the given
// track and packets. It is muxChapterFile's lower half, for the fixtures that
// need a rate Opus cannot have.
func muxChapterTrack(t *testing.T, chapters []container.Chapter, track container.Track, pkts []codec.Packet, trailer codec.Trailer) []byte {
	t.Helper()
	sb := &seekBuf{}
	m := NewProgressiveMuxer(sb, &MuxerOptions{Chapters: chapters})
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, p := range pkts {
		if err := m.WritePacket(container.Packet{Track: 0, Packet: p}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(trailer); err != nil {
		t.Fatalf("End: %v", err)
	}
	return sb.b
}

// flacTrackAt builds a synthetic FLAC track at rate plus npkt packets. The
// chapter fixtures here are otherwise Opus, which is 48 kHz by definition, and
// 48 kHz cannot show the rescale this covers: a millisecond is a whole 48 ticks
// there, so the anchor lands on the grid however it is rounded.
func flacTrackAt(t *testing.T, rate, npkt int) (container.Track, []codec.Packet) {
	t.Helper()
	si := flac.StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: rate, Channels: 2, Bits: 16,
		Samples: int64(npkt) * 4096}
	cfg, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{Codec: codec.FLAC, CodecConfig: cfg, Fmt: si.PCMFormat(),
		Samples: int64(npkt) * 4096}
	var pkts []codec.Packet
	for i := 0; i < npkt; i++ {
		pkts = append(pkts, codec.Packet{Data: bytes.Repeat([]byte{byte(i + 1)}, 64), Dur: 4096, Sync: true})
	}
	return track, pkts
}

// muxChaptered writes a progressive movie carrying n chapters, with enough
// Opus audio (20 ms per packet) to outlast the last one.
func muxChaptered(t *testing.T, n int) ([]byte, []container.Chapter) {
	t.Helper()
	chapters := chaptersFor(n)
	npkt := int(time.Duration(n)*chapterSpacing/(20*time.Millisecond)) + 10
	return muxChapterFile(t, chapters, npkt, codec.Trailer{Samples: int64(npkt) * 960}), chapters
}

// dropChpl renames the Nero chapter list to a box nothing reads, leaving a
// movie whose only chapter form is the text track. The rename is in place, so
// every enclosing box size stays correct without any size arithmetic.
func dropChpl(t *testing.T, file []byte) []byte {
	t.Helper()
	if n := bytes.Count(file, []byte("chpl")); n != 1 {
		t.Fatalf("found %d chpl boxes, want exactly 1 to rename", n)
	}
	return bytes.Replace(file, []byte("chpl"), []byte("free"), 1)
}

// wantChapters asserts a demuxer read back exactly the chapters written.
func wantChapters(t *testing.T, got, want []container.Chapter) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d chapters, wrote %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Title != want[i].Title {
			t.Errorf("chapter %d title = %q, want %q", i, got[i].Title, want[i].Title)
		}
		if got[i].Start != want[i].Start {
			t.Errorf("chapter %d start = %v, want %v", i, got[i].Start, want[i].Start)
		}
	}
}

// TestProgressiveChapterTrackRoundTrip pins the text chapter track the
// progressive muxer synthesizes against our own reader: titles and start times
// survive a write/read cycle, and they survive it past the 255 the Nero chpl
// list beside them can spell.
func TestProgressiveChapterTrackRoundTrip(t *testing.T) {
	t.Run("titles and starts", func(t *testing.T) {
		file, want := muxChaptered(t, 3)
		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		wantChapters(t, d.Chapters(), want)
	})

	// End is what the text track has and chpl structurally cannot, so it is
	// the reason this source outranks chpl. Reading it back is what makes
	// that reason true of the code and not just of the format: a chapter's
	// end is its sample's duration, and discarding it would leave the
	// precedence comment above describing a container rather than a reader.
	t.Run("ends", func(t *testing.T) {
		file, want := muxChaptered(t, 4)
		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		got := d.Chapters()
		if len(got) != len(want) {
			t.Fatalf("read %d chapters, want %d", len(got), len(want))
		}
		for i, ch := range got {
			if ch.End == 0 {
				t.Errorf("chapter %d (%q) has no end; the text track times every chapter to one",
					i, ch.Title)
				continue
			}
			if ch.End <= ch.Start {
				t.Errorf("chapter %d ends at %v, at or before its %v start", i, ch.End, ch.Start)
			}
			// Each chapter runs to the next one's start, which is what the
			// sample durations chain into.
			if i+1 < len(got) && ch.End != got[i+1].Start {
				t.Errorf("chapter %d ends at %v but chapter %d starts at %v; the ends do not chain",
					i, ch.End, i+1, got[i+1].Start)
			}
		}
	})

	// The case chpl cannot represent, and so the case that proves the text
	// track is real rather than a decoration on a list the reader still
	// prefers: 400 chapters in, 400 chapters out, from a file whose chpl
	// truncated to 255 on the way past.
	t.Run("beyond the chpl cap", func(t *testing.T) {
		file, want := muxChaptered(t, 400)
		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		if len(d.chplChapters) != 255 {
			t.Fatalf("chpl carries %d chapters, want the 255 cap (is the fixture exercising the cap?)", len(d.chplChapters))
		}
		wantChapters(t, d.Chapters(), want)
	})

	// A movie whose only chapter form is the text track, which is what every
	// non-Nero writer produces and what the read precedence must not need a
	// chpl to fall back on.
	t.Run("text track only", func(t *testing.T) {
		file, want := muxChaptered(t, 5)
		d, err := NewDemuxer(container.BytesSource(dropChpl(t, file)), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		if len(d.chplChapters) != 0 {
			t.Fatalf("chpl survived the rename: %d chapters", len(d.chplChapters))
		}
		wantChapters(t, d.Chapters(), want)
	})

	// No chapters means no text track and no tref, not an empty one.
	t.Run("no chapters", func(t *testing.T) {
		track, pkts := opusTrackFor(0, 5*960, 5)
		file := muxProgressive(t, track, pkts, codec.Trailer{Samples: 5 * 960})
		if bytes.Contains(file, []byte("tref")) || bytes.Contains(file, []byte("SubtitleHandler")) {
			t.Error("a chapterless movie carries a chapter track")
		}
		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		if d.Chapters() != nil {
			t.Errorf("Chapters() = %v, want nil", d.Chapters())
		}
	})
}

// TestProgressiveChapterTrackShape pins the box-level contract the text track
// owes any other reader: its own track_ID, a text handler (not a sound one), a
// 'chap' reference from the audio track, and an mvhd next_track_ID clear of
// both tracks.
func TestProgressiveChapterTrackShape(t *testing.T) {
	file, _ := muxChaptered(t, 3)
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	moov := findBoxForTest(t, file, "moov")
	tracks, err := d.parseMoov(moov)
	if err != nil {
		t.Fatalf("parseMoov: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("movie has %d tracks, want audio plus chapters", len(tracks))
	}
	audio, text := tracks[0], tracks[1]
	if audio.handler != "soun" || audio.id != trackID {
		t.Errorf("audio track = id %d handler %q", audio.id, audio.handler)
	}
	if text.handler != "text" || text.id != chapterTrackID {
		t.Errorf("chapter track = id %d handler %q, want id %d handler \"text\"", text.id, text.handler, chapterTrackID)
	}
	if text.timescale != chapterTimescale {
		t.Errorf("chapter timescale = %d, want %d", text.timescale, chapterTimescale)
	}
	// The tref is what binds the two; without it a reader falls back to
	// guessing at any text track it finds.
	if len(audio.chapRefs) != 1 || audio.chapRefs[0] != chapterTrackID {
		t.Errorf("audio chapRefs = %v, want [%d]", audio.chapRefs, chapterTrackID)
	}
	mvhd := findBoxForTest(t, moov, "mvhd")
	// mvhd (version 0): ver/flags(4) creation(4) modification(4) timescale(4)
	// duration(4) rate(4) volume(2) reserved(10) matrix(36) predefined(24),
	// then next_track_ID.
	next := be32(mvhd[len(mvhd)-4:])
	if next != chapterTrackID+1 {
		t.Errorf("mvhd next_track_ID = %d, want %d (it must clear every track in the movie)", next, chapterTrackID+1)
	}
}

// traksForTest returns every trak payload in a moov, in movie order: the audio
// track, then the chapter track when there is one. findBoxForTest cannot reach
// the second one, and the second one is the chapter track.
func traksForTest(t *testing.T, moov []byte) [][]byte {
	t.Helper()
	var out [][]byte
	_ = walkBoxes(moov, func(typ string, payload []byte) error {
		if typ == "trak" {
			out = append(out, payload)
		}
		return nil
	})
	if len(out) == 0 {
		t.Fatal("no trak box")
	}
	return out
}

// mvhdDuration reads the movie duration out of an mvhd payload, at either
// version's field offsets.
func mvhdDuration(t *testing.T, payload []byte) int64 {
	t.Helper()
	version, _, rest, ok := fullBox(payload)
	if !ok {
		t.Fatal("mvhd is not a full box")
	}
	// version 0: creation(4) modification(4) timescale(4) duration(4)
	// version 1: creation(8) modification(8) timescale(4) duration(8)
	if version == 1 {
		if len(rest) < 28 {
			t.Fatal("mvhd truncated")
		}
		return int64(be64(rest[20:]))
	}
	if len(rest) < 16 {
		t.Fatal("mvhd truncated")
	}
	return int64(be32(rest[12:]))
}

// tkhdDuration reads the track duration out of a tkhd payload, at either
// version's field offsets. It is stated on the movie timeline, so it is
// directly comparable with mvhdDuration.
func tkhdDuration(t *testing.T, payload []byte) int64 {
	t.Helper()
	version, _, rest, ok := fullBox(payload)
	if !ok {
		t.Fatal("tkhd is not a full box")
	}
	// version 0: creation(4) modification(4) track_ID(4) reserved(4) duration(4)
	// version 1: creation(8) modification(8) track_ID(4) reserved(4) duration(8)
	if version == 1 {
		if len(rest) < 32 {
			t.Fatal("tkhd truncated")
		}
		return int64(be64(rest[24:]))
	}
	if len(rest) < 20 {
		t.Fatal("tkhd truncated")
	}
	return int64(be32(rest[16:]))
}

// TestProgressiveChapterFirstStart covers the anchor half of the chapter
// track's timing: a chapter list whose first chapter starts partway into the
// movie.
//
// stts spells deltas, accumulated from zero, so the samples hold every start
// but the first, and the first is what the rest are measured from. A track that
// writes only the deltas reads back with every chapter pulled forward onto the
// movie's start, and nothing in the track says otherwise. The empty edit is
// where that start goes.
//
// Every other fixture here starts its list at zero, where the anchor is zero
// and losing it costs nothing. This is the case that tells the two apart.
func TestProgressiveChapterFirstStart(t *testing.T) {
	want := []container.Chapter{
		{Start: 5 * time.Second, Title: "Prologue"},
		{Start: 65 * time.Second, Title: "Chapter One"},
		{Start: 125 * time.Second, Title: "Chapter Two"},
	}
	const npkt = 150 * 50 // 150 seconds of 20 ms packets, past the last chapter
	file := muxChapterFile(t, want, npkt, codec.Trailer{Samples: npkt * 960})

	t.Run("as written", func(t *testing.T) {
		d, err := NewDemuxer(container.BytesSource(file), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		wantChapters(t, d.Chapters(), want)
	})

	// chpl stores absolute starts, so it is right about this by construction
	// and would cover for a text track that is wrong. resolveChapters prefers
	// the text track, so the subtest above reads the track under test; renaming
	// chpl away proves it rather than trusting that precedence to stay put.
	t.Run("text track only", func(t *testing.T) {
		d, err := NewDemuxer(container.BytesSource(dropChpl(t, file)), nil)
		if err != nil {
			t.Fatalf("NewDemuxer: %v", err)
		}
		wantChapters(t, d.Chapters(), want)
	})

	// The empty edit is the delay, so it must be exactly the first start on the
	// movie timeline, and the samples must follow it rather than the movie's
	// start.
	t.Run("the empty edit", func(t *testing.T) {
		moov := findBoxForTest(t, file, "moov")
		traks := traksForTest(t, moov)
		if len(traks) != 2 {
			t.Fatalf("movie has %d tracks, want audio plus chapters", len(traks))
		}
		edts := findBoxForTest(t, traks[1], "edts")
		chapTrak := &track{editMedia: -1}
		parseElst(chapTrak, findBoxForTest(t, edts, "elst"))
		if want := int64(5 * 48000); chapTrak.emptyEdit != want {
			t.Errorf("empty edit = %d movie ticks, want %d (5 s at 48 kHz)", chapTrak.emptyEdit, want)
		}
		if chapTrak.editMedia != 0 {
			t.Errorf("the edit after it starts %d media ticks in, want 0: the samples are not offset, the edit is",
				chapTrak.editMedia)
		}
	})
}

// TestChapterAnchorRescaleRounds pins the rounding on the anchor's way back in.
//
// The empty edit crosses two grids that do not divide each other: the muxer
// spells it in movie ticks, 44.1 of which make a millisecond at 44.1 kHz, and
// the reader wants back the millisecond the chapter was authored on. Truncating
// there drops the anchor to the millisecond below and shifts the whole track by
// it, which is the same class of error as losing the anchor outright, only
// smaller and harder to see.
func TestChapterAnchorRescaleRounds(t *testing.T) {
	for _, rate := range []int64{44100, 48000, 22050, 96000} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			for ms := int64(0); ms < 20000; ms += 7 {
				empty := mulDivSat(ms, rate, chapterTimescale) // what editDurs writes
				if got := mulDivRound(empty, chapterTimescale, rate); got != ms {
					t.Fatalf("a chapter at %d ms writes an empty edit of %d movie ticks and reads back at %d ms",
						ms, empty, got)
				}
			}
		})
	}
	// And the truncating rescale beside it does not round trip, which is what
	// makes the one above a choice rather than a spelling.
	if empty := mulDivSat(1, 44100, chapterTimescale); mulDivSat(empty, chapterTimescale, 44100) == 1 {
		t.Fatal("the premise moved: truncating now round trips a millisecond at 44.1 kHz")
	}
}

// TestProgressiveChapterAnchorAt44k is the rounding pin above wired through the
// muxer and the reader, at the one rate in this file that can show it. The Opus
// fixtures are 48 kHz, where a millisecond is a whole 48 movie ticks and the
// anchor round trips whichever way the rescale goes; at 44.1 kHz a millisecond
// is 44.1 ticks and an anchor between them is what a truncating read loses.
func TestProgressiveChapterAnchorAt44k(t *testing.T) {
	const rate, npkt = 44100, 40 // ~3.7 s, past the last chapter
	// 1001 ms is on the millisecond grid the chapter track spells and off the
	// movie-tick grid the anchor is stored on: 44144.1 ticks at 44.1 kHz.
	want := []container.Chapter{
		{Start: 1001 * time.Millisecond, Title: "One"},
		{Start: 2500 * time.Millisecond, Title: "Two"},
	}
	track, pkts := flacTrackAt(t, rate, npkt)
	file := muxChapterTrack(t, want, track, pkts, codec.Trailer{Samples: int64(npkt) * 4096})
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	// chpl stores absolute starts and would cover for the anchor being wrong.
	wantChapters(t, d.Chapters(), want)
	d2, err := NewDemuxer(container.BytesSource(dropChpl(t, file)), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	wantChapters(t, d2.Chapters(), want)
}

// TestProgressiveChapterTailWithinPresentation covers the other end of the
// chapter track's anchor. The audio track's edit list trims the presentation to
// the encoder's real length, but the chapter track carries no such trim, so its
// media time is presentation time: measured against the raw sample total, the
// last chapter runs past the end of the movie by exactly the delay plus the
// padding, and the chapter trak claims to outlast the mvhd it sits in.
func TestProgressiveChapterTailWithinPresentation(t *testing.T) {
	const (
		rate    = 48000
		npkt    = 500             // 10 s raw at 20 ms per packet
		delay   = int64(rate)     // 1 s of encoder priming
		padding = int64(rate / 2) // 0.5 s of trailing padding
		played  = npkt*960 - delay - padding
	)
	trailer := codec.Trailer{Samples: played, Delay: delay, Padding: padding}
	// Chapters over the raw timeline; the last one asks to run to the end.
	want := []container.Chapter{
		{Start: 0, Title: "One"},
		{Start: 4 * time.Second, Title: "Two"},
	}
	file := muxChapterFile(t, want, npkt, trailer)
	presentation := time.Duration(played) * time.Second / rate // 8.5 s

	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if got := d.Tracks()[0].Samples; got != played {
		t.Fatalf("the movie presents %d samples, want %d: the fixture is not trimmed", got, played)
	}
	got := d.Chapters()
	if len(got) != len(want) {
		t.Fatalf("read %d chapters, wrote %d", len(got), len(want))
	}
	if last := got[len(got)-1]; last.End > presentation {
		t.Errorf("the last chapter ends at %v, past the %v presentation (the movie's raw length is %v)",
			last.End, presentation, time.Duration(npkt*960)*time.Second/rate)
	}

	// And the same overrun stated in the headers: a trak cannot outlast the
	// movie it is in.
	moov := findBoxForTest(t, file, "moov")
	movieDur := mvhdDuration(t, findBoxForTest(t, moov, "mvhd"))
	if movieDur != played {
		t.Fatalf("mvhd duration = %d, want %d", movieDur, played)
	}
	traks := traksForTest(t, moov)
	if len(traks) != 2 {
		t.Fatalf("movie has %d tracks, want audio plus chapters", len(traks))
	}
	if chapDur := tkhdDuration(t, findBoxForTest(t, traks[1], "tkhd")); chapDur > movieDur {
		t.Errorf("the chapter trak runs %d movie ticks, past the mvhd's %d", chapDur, movieDur)
	}
}

// TestProgressiveChapterZeroStartNoEdit pins the common case against the empty
// edit the nonzero one needs. A first chapter at zero is already anchored where
// the deltas put it, so it takes no edit list at all: an unconditional edts
// would move the bytes of every chaptered file ever written for an offset of
// nothing.
func TestProgressiveChapterZeroStartNoEdit(t *testing.T) {
	file, want := muxChaptered(t, 3)
	if want[0].Start != 0 {
		t.Fatal("the fixture's first chapter is not at zero; this test covers nothing")
	}
	moov := findBoxForTest(t, file, "moov")
	traks := traksForTest(t, moov)
	if len(traks) != 2 {
		t.Fatalf("movie has %d tracks, want audio plus chapters", len(traks))
	}
	chapTrak := traks[1]
	if bytes.Contains(chapTrak, []byte("edts")) || bytes.Contains(chapTrak, []byte("elst")) {
		t.Error("a chapter track anchored at zero carries an edit list")
	}
	tkhd := findBoxForTest(t, chapTrak, "tkhd")
	if v, _, _, ok := fullBox(tkhd); !ok || v != 0 {
		t.Errorf("chapter tkhd version = %d, want 0", v)
	}
	// The duration a muxer with no notion of an empty edit writes: the whole
	// media rescaled onto the movie timeline, with nothing added ahead of it and
	// no clamp biting. An untrimmed movie must not move off it.
	want4 := mulDivSat(chapterTrackMediaDur(t, file), 48000, chapterTimescale)
	if got := tkhdDuration(t, tkhd); got != want4 {
		t.Errorf("chapter tkhd duration = %d movie ticks, want %d", got, want4)
	}
}

// chapterTrackMediaDur reads the chapter track's own media duration (in
// chapterTimescale ticks) back out of a muxed movie's mdhd.
func chapterTrackMediaDur(t *testing.T, file []byte) int64 {
	t.Helper()
	traks := traksForTest(t, findBoxForTest(t, file, "moov"))
	if len(traks) != 2 {
		t.Fatalf("movie has %d tracks, want audio plus chapters", len(traks))
	}
	mdia := findBoxForTest(t, traks[1], "mdia")
	ts, dur := mdhdTime(findBoxForTest(t, mdia, "mdhd"))
	if ts != chapterTimescale {
		t.Fatalf("chapter mdhd timescale = %d, want %d", ts, chapterTimescale)
	}
	return dur
}

// TestFragmentedChaptersSurvive covers the chapter source a fragmented movie
// actually carries. The fragmented muxer writes chpl into its moov's udta
// exactly as the progressive one does, and the demuxer parses it out of any
// moov; the resolve was gated on the movie not being fragmented, so those
// chapters were read off the wire and then dropped on the floor.
//
// The text track is the part a fragmented moov genuinely cannot hold: its
// samples would live in the fragments and its moov sample table is empty by
// design, so there is nothing for chapterTrack to match. That, and only that,
// is what a fragmented file goes without.
func TestFragmentedChaptersSurvive(t *testing.T) {
	src := metaTone(alac.FrameSize * 3)
	defer audio.Put(src)
	want := chaptersFor(2)
	var out bytes.Buffer
	muxALACMeta(t, &out, src, &MuxerOptions{Chapters: want})

	d, err := NewDemuxer(container.BytesSource(out.Bytes()), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if !d.fragmented {
		t.Fatal("the fixture is not fragmented; this test is not covering the gate")
	}
	if len(d.chplChapters) != len(want) {
		t.Fatalf("the moov parsed %d chpl chapters, want %d (the fixture carries no chapters to drop)",
			len(d.chplChapters), len(want))
	}
	wantChapters(t, d.Chapters(), want)
}

// findBoxForTest returns the payload of the first box of the given type among
// body's immediate children. walkBoxes does not recurse, so neither does this:
// reaching a nested box means calling it once per level (a trak's elst is
// findBoxForTest(t, findBoxForTest(t, trak, "edts"), "elst")).
func findBoxForTest(t *testing.T, body []byte, want string) []byte {
	t.Helper()
	var found []byte
	_ = walkBoxes(body, func(typ string, payload []byte) error {
		if found != nil {
			return nil
		}
		if typ == want {
			found = payload
		}
		return nil
	})
	if found == nil {
		t.Fatalf("no %s box", want)
	}
	return found
}

// TestProgStblChunkOffsetWidth pins the co64 escape. The audio chunk sits near
// the file head and always fits a 32-bit stco, but the chapter chunk trails the
// whole audio payload, which a long audiobook pushes past 4 GiB; a 32-bit stco
// would truncate that offset and send the reader somewhere else entirely.
func TestProgStblChunkOffsetWidth(t *testing.T) {
	cases := []struct {
		name string
		off  int64
		want string
	}{
		{"near the file head", 0x1000, "stco"},
		{"the last offset stco spells", math.MaxUint32, "stco"},
		{"one byte past 4 GiB", math.MaxUint32 + 1, "co64"},
		{"a long audiobook", 12 << 30, "co64"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			stbl := progStblBox(textSampleEntry(), []uint32{100}, []uint32{7}, tt.off)
			payload := findBoxForTest(t, stbl[8:], tt.want) // past stbl's own header
			offs, err := parseStco(payload, tt.want == "co64")
			if err != nil {
				t.Fatalf("parseStco: %v", err)
			}
			if len(offs) != 1 || offs[0] != tt.off {
				t.Errorf("%s read back %v, want [%d]", tt.want, offs, tt.off)
			}
			// Exactly one chunk-offset box: emitting both would leave the
			// reader's box order deciding which offset wins.
			other := map[string]string{"stco": "co64", "co64": "stco"}[tt.want]
			if bytes.Contains(stbl, []byte(other)) {
				t.Errorf("stbl carries both %s and %s", tt.want, other)
			}
		})
	}
}

// TestProgressiveChapterSampleFormat pins the sample wire format byte for
// byte: a 16-bit big-endian title length, then that many UTF-8 bytes.
//
// The round trip above cannot pin this by itself. readTextChapters clamps a
// length that overruns the sample against the stsz size, so it recovers a
// little-endian prefix correctly by accident and a byte-swapped writer reads
// back clean through our own demuxer. A third-party reader trusts the prefix
// and gets nothing (the ffprobe cell in oracletest catches exactly that, but
// it self-skips wherever ffmpeg is not installed, so the format is pinned
// here too).
func TestProgressiveChapterSampleFormat(t *testing.T) {
	file, want := muxChaptered(t, 3)
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	tracks, err := d.parseMoov(findBoxForTest(t, file, "moov"))
	if err != nil {
		t.Fatalf("parseMoov: %v", err)
	}
	text := tracks[1]
	if text.st.total != int64(len(want)) {
		t.Fatalf("text track has %d samples, want %d", text.st.total, len(want))
	}
	for i, ch := range want {
		got := file[text.st.offsets[i] : text.st.offsets[i]+int64(text.st.sizes[i])]
		wantBytes := append(u16(uint16(len(ch.Title))), ch.Title...)
		if !bytes.Equal(got, wantBytes) {
			t.Errorf("sample %d = %x, want %x", i, got, wantBytes)
		}
	}
	// The samples sit at the tail of the audio mdat, which End's size patch
	// must have grown to cover them: the moov begins exactly where they stop.
	// A patch that missed them would leave the moov overlapping the mdat.
	moov := findBoxForTest(t, file, "moov")
	mdatEnd := int64(len(file)) - int64(len(moov)) - 8
	last := text.st.offsets[len(want)-1] + int64(text.st.sizes[len(want)-1])
	if last != mdatEnd {
		t.Errorf("chapter samples end at %d, the mdat at %d: the size patch missed them", last, mdatEnd)
	}
}
