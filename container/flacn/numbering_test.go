package flacn_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// withBounds returns raw, a native FLAC, with STREAMINFO's block size bounds
// set as given, whatever a parser would make of them.
func withBounds(raw []byte, minBlock, maxBlock int) []byte {
	const siAt = 8 // "fLaC" and the block header
	out := append([]byte(nil), raw...)
	binary.BigEndian.PutUint16(out[siAt:], uint16(minBlock))
	binary.BigEndian.PutUint16(out[siAt+2:], uint16(maxBlock))
	return out
}

// oldFormat returns raw, a fixed-blocksize stream, as the pre-1.0 format
// wrote one: sample numbers under the fixed blocking strategy bit, and
// unequal block size bounds.
func oldFormat(t *testing.T, raw []byte) []byte {
	t.Helper()
	frames := testutil.FLACFrames(t, raw)
	out := withBounds(raw[:frames[0].Off], 1152, 4096)
	for _, f := range frames {
		out = append(out, testutil.SetFLACFrameNumber(t, raw[f.Off:f.End], false, uint64(f.Pos))...)
	}
	return out
}

// reads opens raw, reads it to the end tolerant, and returns the track as it
// opened and the packets.
func reads(t *testing.T, raw []byte) (container.Track, []frame, *flacn.Demuxer) {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	frames, err := readFrames(t, d)
	if err != nil {
		t.Fatal(err)
	}
	return tr, frames, d
}

// TestHeadDamageLeavesTheShortLastFrameWhereItBelongs: with every frame but
// the short last one gone, that frame sits where STREAMINFO's block size
// puts its number, the hole in front of it is the samples those frames
// held, and the length is the stream's.
func TestHeadDamageLeavesTheShortLastFrameWhereItBelongs(t *testing.T) {
	whole := unseekableTwin(t, 2)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	last := frames[len(frames)-1]
	if last.Dur >= 4096 || last.Pos != int64(len(frames)-1)*4096 {
		t.Fatalf("the closing frame holds %d samples at %d, want a short one", last.Dur, last.Pos)
	}
	raw := append([]byte(nil), whole...)
	clear(raw[frames[0].Off:last.Off])

	tr, got, d := reads(t, raw)
	if tr.Samples != total || !tr.SamplesExact {
		t.Errorf("the track says %d samples, exact %v, want the stream's %d exact", tr.Samples, tr.SamplesExact, total)
	}
	if len(got) != 1 || got[0].pts != last.Pos || got[0].dur != last.Dur || got[0].lost != last.Pos || !got[0].discont {
		t.Fatalf("read %d packets, the first %d samples at %d owing %d; want the closing frame at %d owing the %d samples in front of it",
			len(got), got[0].dur, got[0].pts, got[0].lost, last.Pos, last.Pos)
	}
	if n := container.TruncatedOf(d, 0); n != 0 {
		t.Errorf("%d samples counted truncated, want none: the hole took them", n)
	}
	for _, target := range []int64{0, last.Pos / 2, last.Pos, total - 1} {
		landed, err := d.SeekSample(context.Background(), 0, target)
		want := int64(0)
		if target >= last.Pos {
			want = last.Pos
		}
		if err != nil || landed != want {
			t.Errorf("a seek to %d landed at %d (%v), want %d", target, landed, err, want)
		}
	}

	// A short frame with bytes behind it that are no frame is not known to
	// be the last, so it is placed in its own size, as it was.
	raw = append(raw, bytes.Repeat([]byte{0x11}, 64)...)
	if _, got, _ = reads(t, raw); len(got) != 1 || got[0].pts != int64(len(frames)-1)*last.Dur || got[0].lost != got[0].pts {
		t.Errorf("with junk behind it the closing frame is at %d owing %d, want %d as it was", got[0].pts, got[0].lost, int64(len(frames)-1)*last.Dur)
	}

	// Later frames of the stated size read as they did.
	raw = append([]byte(nil), whole...)
	clear(raw[frames[0].Off:frames[5].Off])
	tr, got, _ = reads(t, raw)
	if tr.Samples != total || got[0].pts != 5*4096 || got[0].lost != 5*4096 || len(got) != len(frames)-5 {
		t.Errorf("with five frames gone: %d samples, %d packets, the first at %d owing %d", tr.Samples, len(got), got[0].pts, got[0].lost)
	}
}

// TestDamagedBlockBoundsDoNotRenumberTheFrames: block size bounds no stream
// may state, and unequal ones over frames that count up by one, leave a
// fixed-blocksize stream the frames it is.
func TestDamagedBlockBoundsDoNotRenumberTheFrames(t *testing.T) {
	whole := unseekableTwin(t, 2)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	for _, b := range [][2]int{{4608, 256}, {8, 4096}, {2048, 4096}, {4096, 8192}, {16, 65535}} {
		tr, got, d := reads(t, withBounds(whole, b[0], b[1]))
		if tr.Samples != total || !tr.SamplesExact || len(got) != len(frames) || len(d.Warnings()) != 0 {
			t.Errorf("bounds %d..%d: %d samples, exact %v, %d packets, warnings %v; want %d exact in %d packets and none",
				b[0], b[1], tr.Samples, tr.SamplesExact, len(got), d.Warnings(), total, len(frames))
			continue
		}
		for i, f := range got {
			if f.pts != frames[i].Pos || f.dur != frames[i].Dur || f.lost != 0 || f.discont {
				t.Errorf("bounds %d..%d: packet %d is %d samples at %d, lost %d, discont %v; want %d samples at %d",
					b[0], b[1], i, f.dur, f.pts, f.lost, f.discont, frames[i].Dur, frames[i].Pos)
				break
			}
		}
		if landed, err := d.SeekSample(context.Background(), 0, 3*4096+9); err != nil || landed != 3*4096 {
			t.Errorf("bounds %d..%d: a seek landed at %d (%v), want %d", b[0], b[1], landed, err, 3*4096)
		}
	}
}

// TestAFirstFrameThatDoesNotCheckLeavesTheEvidenceToTheFramesBehindIt: with
// the first frame damaged, the first whole frame behind it and the header
// behind that say how the stream counts, under bounds no stream may state.
func TestAFirstFrameThatDoesNotCheckLeavesTheEvidenceToTheFramesBehindIt(t *testing.T) {
	whole := unseekableTwin(t, 2)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	for name, raw := range map[string][]byte{
		"frame numbers":  withBounds(damageFrames(t, whole, frameSpans(t, whole), 0), 4608, 256),
		"sample numbers": damageFrames(t, oldFormat(t, whole), frameSpans(t, oldFormat(t, whole)), 0),
	} {
		tr, got, d := reads(t, raw)
		if tr.Samples != total || !tr.SamplesExact || len(got) != len(frames)-1 || got[0].lost != frames[1].Pos {
			t.Errorf("%s: %d samples, exact %v, %d packets, findings %v; want %d exact and every frame but the first, owing it",
				name, tr.Samples, tr.SamplesExact, len(got), damageOf(d), total)
			continue
		}
		for i, f := range got {
			if want := frames[i+1]; f.pts != want.Pos || f.dur != want.Dur {
				t.Errorf("%s: packet %d is %d samples at %d, want %d at %d", name, i, f.dur, f.pts, want.Dur, want.Pos)
				break
			}
		}
		if landed, err := d.SeekSample(context.Background(), 0, 3*4096+9); err != nil || landed != 3*4096 {
			t.Errorf("%s: a seek landed at %d (%v), want %d", name, landed, err, 3*4096)
		}
	}
}

// TestTheEvidenceBehindADamagedFirstFrameIsAFrameOfTheStream: the frame read
// for evidence is of the stream's blocking strategy, and one of a single
// sample, whose number one on means the same counted either way, says
// nothing.
func TestTheEvidenceBehindADamagedFirstFrameIsAFrameOfTheStream(t *testing.T) {
	whole := unseekableTwin(t, 2)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)

	// Frame 1 under the variable strategy: passed over for frames 2 and 3.
	raw := withBounds(damageFrames(t, whole, frameSpans(t, whole), 0), 4608, 256)
	raw = slices.Concat(raw[:frames[1].Off], testutil.SetFLACFrameNumber(t, whole[frames[1].Off:frames[1].End], true, uint64(frames[1].Pos)), raw[frames[2].Off:])
	tr, got, _ := reads(t, raw)
	if tr.Samples != total || len(got) != len(frames)-2 || got[0].pts != frames[2].Pos || got[0].lost != frames[2].Pos {
		t.Errorf("a variable frame behind the first: %d samples, %d packets, the first at %d owing %d; want %d, frame 2 owing the two in front",
			tr.Samples, len(got), got[0].pts, got[0].lost, total)
	}

	// An old-format stream with a frame of one sample behind the first.
	old := oldFormat(t, whole)
	oldFrames := testutil.FLACFrames(t, old)
	raw = damageFrames(t, old, frameSpans(t, old), 0)[:oldFrames[1].Off]
	raw = append(raw, testutil.SetFLACFrameNumber(t, oneSampleFrame(t), false, uint64(frames[1].Pos))...)
	for _, f := range oldFrames[1:] {
		raw = append(raw, testutil.SetFLACFrameNumber(t, old[f.Off:f.End], false, uint64(f.Pos+1))...)
	}
	tr, got, d := reads(t, raw)
	if tr.Samples != total+1 || len(got) != len(frames) || got[0].pts != frames[1].Pos || got[0].dur != 1 || got[1].pts != frames[1].Pos+1 || container.DamageOf(d) != 1 {
		t.Errorf("a frame of one sample behind the first: %d samples, %d packets, findings %v; want %d and the frames at their sample numbers",
			tr.Samples, len(got), damageOf(d), total+1)
	}
}

// TestTheEvidenceLookTriesEightHeadersAtMost: behind a damaged first frame
// the look for a whole frame parses at most eight headers that do not check;
// past them it gives up, as a head dense with them would otherwise cost a
// window's worth of frame parses at open.
func TestTheEvidenceLookTriesEightHeadersAtMost(t *testing.T) {
	whole := unseekableTwin(t, 2)
	frames := testutil.FLACFrames(t, whole)
	f1 := whole[frames[1].Off:frames[1].End]
	// Frame 1's header, which parses, and bytes that make no frame of it.
	decoy := append(append([]byte(nil), f1[:8]...), bytes.Repeat([]byte{0x11}, 24)...)
	for _, tc := range []struct {
		decoys int
		read   bool
	}{{flacn.MaxHeldForTest - 1, true}, {flacn.MaxHeldForTest, false}} {
		raw := withBounds(damageFrames(t, whole, frameSpans(t, whole), 0), 4608, 256)
		raw = slices.Concat(raw[:frames[1].Off], bytes.Repeat(decoy, tc.decoys), raw[frames[1].Off:])
		_, got, _ := reads(t, raw)
		if read := len(got) == len(frames)-1; read != tc.read {
			t.Errorf("%d headers that do not check in front of frame 1: %d packets, the frames read as frames %v, want %v",
				tc.decoys, len(got), read, tc.read)
		}
	}
}

// oneSampleFrame is a whole frame of one sample of 16-bit stereo, its rate
// and depth from STREAMINFO, numbered 0.
func oneSampleFrame(t *testing.T) []byte {
	t.Helper()
	h := []byte{0xFF, 0xF8, 0x60, 0x10, 0x00, 0x00} // an 8-bit block size of 1
	for crc := range 256 {
		if fi, err := flac.ParseFrameHeader(append(h, byte(crc))); err == nil && fi.BlockSize == 1 {
			f := append(h, byte(crc), 0x02, 0x12, 0x34, 0x02, 0x56, 0x78) // two verbatim subframes
			c := flac.UpdateCRC16(0, f)
			return append(f, byte(c>>8), byte(c))
		}
	}
	t.Fatal("no CRC-8 checks the header")
	return nil
}

// TestABlockSizeTheFramesContradictIsNotTaken: STREAMINFO stating a block
// size its frames do not have is not believed over a first frame that is
// larger, or that has a frame behind it.
func TestABlockSizeTheFramesContradictIsNotTaken(t *testing.T) {
	whole := unseekableTwin(t, 2)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	for _, b := range []int{256, 8192} {
		tr, got, _ := reads(t, withBounds(whole, b, b))
		if tr.Samples != total || !tr.SamplesExact || len(got) != len(frames) || got[2].pts != 2*4096 {
			t.Errorf("a stated block size of %d: %d samples, exact %v, %d packets, the third at %d; want the frames' own 4096",
				b, tr.Samples, tr.SamplesExact, len(got), got[2].pts)
		}
		// With the second frame's header gone nothing says what follows
		// the first, and its own size stands.
		tr, got, _ = reads(t, headless(withBounds(whole, b, b), frames[1].Off))
		if tr.Samples != total || len(got) != len(frames)-1 || got[1].pts != 2*4096 || got[1].lost != 4096 {
			t.Errorf("a stated block size of %d and no second header: %d samples, %d packets, the second at %d owing %d; want the frames' own 4096",
				b, tr.Samples, len(got), got[1].pts, got[1].lost)
		}
	}
}

// TestSampleNumberedStreamsReadAsTheyDid: a variable-blocksize stream and an
// old-format one, whose fixed-strategy frames carry sample numbers, are
// placed by those numbers.
func TestSampleNumberedStreamsReadAsTheyDid(t *testing.T) {
	whole := unseekableTwin(t, 2)
	total := streamSamples(t, whole)
	frames := testutil.FLACFrames(t, whole)
	for name, raw := range map[string][]byte{
		"variable":   testutil.VariableFLACFrames(t, whole, 0),
		"old format": oldFormat(t, whole),
		// As a copy writes one whose source stated such bounds.
		"sample numbers under bounds no stream may state": withBounds(oldFormat(t, whole), 4608, 256),
		// Its second frame gone: nothing says how the first is numbered.
		"old format with a hole behind the first frame": damageFrames(t, oldFormat(t, whole), frameSpans(t, oldFormat(t, whole)), 1),
	} {
		tr, got, d := reads(t, raw)
		holes := 0
		if len(got) != len(frames) {
			holes = 1
		}
		if tr.Samples != total || !tr.SamplesExact || len(got) != len(frames)-holes || container.DamageOf(d) != holes {
			t.Errorf("%s: %d samples, exact %v, %d packets, findings %v", name, tr.Samples, tr.SamplesExact, len(got), damageOf(d))
			continue
		}
		for i, f := range got {
			if want := frames[i+min(i, holes)]; f.pts != want.Pos || f.dur != want.Dur {
				t.Errorf("%s: packet %d is %d samples at %d, want %d at %d", name, i, f.dur, f.pts, want.Dur, want.Pos)
				break
			}
		}
		if landed, err := d.SeekSample(context.Background(), 0, 3*4096+9); err != nil || landed != 3*4096 {
			t.Errorf("%s: a seek landed at %d (%v), want %d", name, landed, err, 3*4096)
		}
	}
}

// TestTheNumberingLookCostsOneWindowAtMost pins what settling the numbering
// by the second frame costs an open: nothing on a stream under a window,
// whose tail scan reads those bytes, and one read of a window on a longer
// one. A stream whose bounds are equal and whose first frame has their size
// never pays it.
func TestTheNumberingLookCostsOneWindowAtMost(t *testing.T) {
	cost := func(raw []byte) (int, int64, [][2]int64) {
		src := &testutil.CountingSource{Src: container.BytesSource(raw), Log: true}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tr := d.Tracks()[0]; !tr.SamplesExact || len(d.Warnings()) != 0 {
			t.Fatalf("opened at %d samples, exact %v, warnings %v", tr.Samples, tr.SamplesExact, d.Warnings())
		}
		return src.Reads, src.Bytes, src.Spans
	}
	small := unseekableTwin(t, 2)
	if len(small) > srcwin.Chunk {
		t.Fatalf("%d bytes, want a stream under a window", len(small))
	}
	reads, bytes, _ := cost(small)
	if r, b, spans := cost(oldFormat(t, small)); r != reads || b < bytes || b > bytes+64 {
		t.Errorf("an old-format stream under a window opened in %d reads of %d bytes, the plain one in %d of %d: %v", r, b, reads, bytes, spans)
	}
	big := noiseStream(t, 1<<18)
	first := int64(testutil.FLACFrames(t, big)[0].Off)
	reads, bytes, _ = cost(big)
	r, b, spans := cost(oldFormat(t, big))
	if r != reads+1 || b < bytes+srcwin.Chunk || b > bytes+srcwin.Chunk+1024 || !lookAt(spans, first) {
		t.Errorf("an old-format stream over a window opened in %d reads of %d bytes, the plain one in %d of %d; want one read more, a window at the first frame %d: %v",
			r, b, reads, bytes, first, spans)
	}
}

// lookAt reports a read of one window at the first frame, at, or behind the
// header bytes of it the open already holds.
func lookAt(spans [][2]int64, at int64) bool {
	return slices.ContainsFunc(spans, func(s [2]int64) bool {
		return s[1]-s[0] == srcwin.Chunk && s[0] >= at && s[0] <= at+flac.MaxFrameHeaderLen
	})
}

// TestAReadThatFailsInTheNumberingLookIsNotTheOpensError: the look at the
// second frame reads where an open read nothing before, so on a stream over
// a window a read that fails there is no answer, and the stream opens and
// seeks as it did.
func TestAReadThatFailsInTheNumberingLookIsNotTheOpensError(t *testing.T) {
	raw := oldFormat(t, noiseStream(t, 1<<18))
	first := int64(testutil.FLACFrames(t, noiseStream(t, 1<<18))[0].Off)
	src := &testutil.FailingSource{Src: container.BytesSource(raw), From: first + 4096, To: first + 4096 + 40}
	d, err := flacn.NewDemuxer(src, nil)
	if err != nil {
		t.Fatalf("the open returned %v after %d failed reads", err, src.Failed)
	}
	if src.Failed != 1 {
		t.Fatalf("%d reads failed at open, want the look's alone: this holds nothing", src.Failed)
	}
	if tr := d.Tracks()[0]; tr.Samples != 1<<18 || !tr.SamplesExact || len(d.Warnings()) != 0 {
		t.Errorf("opened at %d samples, exact %v, warnings %v", tr.Samples, tr.SamplesExact, d.Warnings())
	}
	if landed, err := d.SeekSample(context.Background(), 0, 40*4096); err != nil || landed != 40*4096 {
		t.Fatalf("a seek past the unreadable bytes landed at %d (%v)", landed, err)
	}
	if n := readCount(t, d); n != 24*4096 {
		t.Errorf("read %d samples from the seek, want %d", n, 24*4096)
	}
	// A read from the start meets the unreadable bytes itself.
	d, err = flacn.NewDemuxer(&testutil.FailingSource{Src: container.BytesSource(raw), From: first + 4096, To: first + 4096 + 40}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); waxerr.CodeOf(err) != waxerr.CodeSourceUnreadable {
		t.Errorf("a read from the start returned %v, want the failed read", err)
	}
}

// TestTrackNamesTheDemuxerRevision: the track carries DemuxerVersion, which
// is what puts the revision in every cache key over a native FLAC source,
// and keeps it through a read that settles the length.
func TestTrackNamesTheDemuxerRevision(t *testing.T) {
	d, err := flacn.NewDemuxer(container.BytesSource(farBehindAHole(t, unseekableTwin(t, 2), 5, 5000)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Tracks()[0].DemuxerVersion; got != "flacn-demux-2" {
		t.Fatalf("the track names demuxer revision %q, want %q", got, flacn.DemuxerVersion)
	}
	readCount(t, d)
	if tr := d.Tracks()[0]; tr.DemuxerVersion != flacn.DemuxerVersion || tr.SamplesExact {
		t.Errorf("after a read that settled the length the track names %q, exact %v", tr.DemuxerVersion, tr.SamplesExact)
	}
}

// TestALengthBehindAHeadHoleIsHeldToTheBytesTheHoleTook: the samples a hole
// at the head took are in the length, so what the bytes can hold counts the
// bytes that hole took, and the length the closing frame states is exact.
func TestALengthBehindAHeadHoleIsHeldToTheBytesTheHoleTook(t *testing.T) {
	whole := testutil.RenumberFLACFrames(t, unseekableTwin(t, 2), 3000)
	frames := testutil.FLACFrames(t, whole)
	last := frames[len(frames)-1]
	raw := append([]byte(nil), whole...)
	clear(raw[frames[0].Off:last.Off])
	at := int64(3000+len(frames)-1) * 4096
	tr, got, d := reads(t, raw)
	if tr.Samples != at+last.Dur || !tr.SamplesExact || container.DamageOf(d) != 1 {
		t.Errorf("the track says %d samples, exact %v, findings %v; want %d exact and the hole alone", tr.Samples, tr.SamplesExact, damageOf(d), at+last.Dur)
	}
	if len(got) != 1 || got[0].pts != at || got[0].lost != at {
		t.Errorf("read %d packets, the first at %d owing %d; want the closing frame at %d owing as much", len(got), got[0].pts, got[0].lost, at)
	}
}

// TestAFirstFrameNumberedPastTheLengthIsNoHoleFromZero: a first frame read
// behind damage whose number puts it at or past where the closing frame ends
// is not owed a hole from sample 0, whose silence a decode would be cut in.
// The stream starts at it, as one whose head bytes could not hold the hole.
func TestAFirstFrameNumberedPastTheLengthIsNoHoleFromZero(t *testing.T) {
	whole := unseekableTwin(t, 4)
	frames := testutil.FLACFrames(t, whole)
	raw := headless(strayBehindAHole(t, whole, 0), frames[0].Off)
	tr, got, d := reads(t, raw)
	if tr.SamplesExact {
		t.Errorf("the track says %d samples exact, want no exact length over frames numbered both sides of it", tr.Samples)
	}
	if len(got) != 2 || got[0].pts != 0 || got[0].lost != 0 || got[1].pts != 4096 {
		t.Fatalf("read %d packets, the first at %d owing %d; want the two frames behind the damage from 0, nothing owed", len(got), got[0].pts, got[0].lost)
	}
	if msgs := strings.Join(damageOf(d), "; "); !strings.Contains(msgs, "unparsable bytes before the first frame") || strings.Contains(msgs, "samples lost") {
		t.Errorf("findings %v, want the bytes in front of the first frame and no hole", msgs)
	}
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if n := decodeRest(t, med); n != 2*4096 || med.Info().Lost != 0 {
		t.Errorf("a decode delivered %d samples, %d of them silence; want the two frames' %d and none", n, med.Info().Lost, 2*4096)
	}
}
