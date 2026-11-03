package flacn_test

// A FLAC stream whose first frame is not numbered 0, which is what an
// `ffmpeg -ss X -c copy` cut leaves. RFC 9639 section 9.1.5 calls it
// non-conforming; libFLAC verifies it and decodes it whole. It reads, measures
// and seeks as its twin numbered from 0, with positions running from 0.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// encodedTwin encodes testdata/sine-s16.wav, looped to seconds, through this
// tree's encoder into a native FLAC whose STREAMINFO is fully settled.
func encodedTwin(t *testing.T, seconds int) []byte {
	t.Helper()
	src := loopedPCM(t, seconds)
	defer audio.Put(src)
	ws := &testutil.MemWriteSeeker{}
	encodeStream(t, src, 5, ws, int64(src.N))
	return ws.Buf
}

// loopedPCM is testdata/sine-s16.wav looped to at least seconds long.
func loopedPCM(t *testing.T, seconds int) *audio.Buffer {
	t.Helper()
	raw := fixture(t, "sine-s16.wav")
	if ch, bits := binary.LittleEndian.Uint16(raw[22:]), binary.LittleEndian.Uint16(raw[34:]); ch != 2 || bits != 16 {
		t.Fatalf("the fixture is %d-channel %d-bit, want 16-bit stereo", ch, bits)
	}
	data := testutil.WAVData(t, raw)
	f := muxFmt(int(binary.LittleEndian.Uint32(raw[24:])), 2, 16)
	frames := len(data) / 4
	src := audio.Get(f, seconds*f.Rate+frames)
	for src.N < seconds*f.Rate {
		at := src.N
		src.N += frames // a channel is src.N long, so grow it first
		for i := range frames {
			for c := range 2 {
				src.ChanI(c)[at+i] = int32(int16(binary.LittleEndian.Uint16(data[4*i+2*c:])))
			}
		}
	}
	return src
}

// TestFirstFrameNotZero: frames numbered from 1000 read as the same frames
// numbered from 0.
func TestFirstFrameNotZero(t *testing.T) {
	twin := encodedTwin(t, 30)
	if len(twin) < 3*128<<10 {
		t.Fatalf("the stream is %d bytes, too short for a seek to bisect", len(twin))
	}
	shifted := testutil.RenumberFLACFrames(t, twin, 1000)

	ref, err := flacn.NewDemuxer(container.BytesSource(twin), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := flacn.NewDemuxer(container.BytesSource(shifted), &flacn.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict open refused the renumbered stream: %v", err)
	}
	want, tr := ref.Tracks()[0], got.Tracks()[0]
	if tr.Samples != want.Samples || !tr.SamplesExact {
		t.Errorf("renumbered: %d samples (exact %v); its twin %d", tr.Samples, tr.SamplesExact, want.Samples)
	}
	// A note, not damage: every frame checks and the audio is whole, but a
	// reader that seeks by frame number lands in the wrong place.
	if ws := got.Warnings(); len(ws) != 1 || ws[0].Kind != container.Note ||
		!strings.Contains(ws[0].Msg, "4096000") {
		t.Errorf("warnings %v, want one note naming the first frame's sample 4096000", ws)
	}

	var pa, pb container.Packet
	for i := 0; ; i++ {
		ea, eb := ref.ReadPacket(&pa), got.ReadPacket(&pb)
		if ea == io.EOF && eb == io.EOF {
			break
		}
		if ea != nil || eb != nil {
			t.Fatalf("packet %d: twin %v, renumbered %v", i, ea, eb)
		}
		if pa.PTS != pb.PTS || pa.Dur != pb.Dur {
			t.Fatalf("packet %d: [%d, +%d) renumbered, [%d, +%d) twin", i, pb.PTS, pb.Dur, pa.PTS, pa.Dur)
		}
	}

	end := want.Samples
	targets := []int64{0, 1, 4095, 4096, end - 1, end, end + 5000}
	for i := int64(1); i < 40; i++ {
		targets = append(targets, end*i/40+i)
	}
	for _, target := range targets {
		la, err := ref.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatalf("twin seek to %d: %v", target, err)
		}
		lb, err := got.SeekSample(context.Background(), 0, target)
		if err != nil {
			t.Fatalf("renumbered seek to %d: %v", target, err)
		}
		if la != lb {
			t.Fatalf("seek to %d lands at %d, its twin's at %d", target, lb, la)
		}
		if err := ref.ReadPacket(&pa); err != nil {
			t.Fatal(err)
		}
		if err := got.ReadPacket(&pb); err != nil {
			t.Fatal(err)
		}
		if pa.PTS != pb.PTS || pa.Dur != pb.Dur {
			t.Fatalf("seek to %d: the next frame is [%d, +%d), its twin's [%d, +%d)", target, pb.PTS, pb.Dur, pa.PTS, pa.Dur)
		}
	}
}

// TestFramesBeforeTheFirstPlaceNothing: positions run from the first frame,
// so a frame numbered before it is not this stream's even where a seek's
// probe finds one. Here the first 100 frames are numbered from 5000 and the
// rest keep their own numbers: a read stops at the head, where the numbers stop
// chaining, the tail's closing frame cannot settle the length, and no seek,
// whether it bisects or follows a SEEKTABLE point into the tail, lands before 0.
func TestFramesBeforeTheFirstPlaceNothing(t *testing.T) {
	const head = 100
	// Unseekable, so the encode carries no SEEKTABLE of its own.
	src := loopedPCM(t, 30)
	var enc bytes.Buffer
	encodeStream(t, src, 5, &enc, int64(src.N))
	audio.Put(src)
	twin := enc.Bytes()
	frames := func(raw []byte) [][]byte {
		d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		var out [][]byte
		var pkt container.Packet
		for d.ReadPacket(&pkt) == nil {
			out = append(out, append([]byte(nil), pkt.Data...))
		}
		return out
	}
	renumbered := testutil.RenumberFLACFrames(t, twin, 5000)
	ren, orig := frames(renumbered), frames(twin)
	raw := append([]byte(nil), renumbered[:bytes.Index(renumbered, ren[0])]...)
	for _, f := range ren[:head] {
		raw = append(raw, f...)
	}
	tail := len(raw) // where the frames still numbered from head begin
	for _, f := range orig[head:] {
		raw = append(raw, f...)
	}
	meta := int(bytes.Index(raw, ren[0]))
	// One SEEKTABLE point, for a sample the head holds, pointing into the tail.
	pointed := withSeekTable(t, raw, [][3]uint64{{4096 * 50, uint64(tail - meta), 4096}})

	for name, stream := range map[string][]byte{"bisection": raw, "seek table": pointed} {
		t.Run(name, func(t *testing.T) {
			d, err := flacn.NewDemuxer(container.BytesSource(stream), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.SamplesExact {
				t.Errorf("%d samples settled exact from a tail numbered before the first frame", tr.Samples)
			}
			for _, target := range []int64{0, 4096*50 + 5, 4096*(head-2) + 1, 4096 * (head + 50), 1 << 40} {
				landed, err := d.SeekSample(context.Background(), 0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				if landed < 0 || landed > target {
					t.Fatalf("seek to %d landed at %d", target, landed)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err == nil && pkt.PTS != landed {
					t.Fatalf("seek to %d landed at %d, then read a frame at %d", target, landed, pkt.PTS)
				}
			}
		})
	}
}

// TestSourceLengthTotalIsANote: a stream-copy cut keeps its source's
// STREAMINFO, whose total is where the frames end counted from sample 0, the
// first frame's number plus the span. That is the source's length rather than a
// claim about this stream, so it is a note, beside the first frame's, and the
// track reads the span exactly. A total that is neither that nor the span is
// still a shortfall, which warns and which a strict open refuses.
func TestSourceLengthTotalIsANote(t *testing.T) {
	twin := encodedTwin(t, 30)
	ref, err := flacn.NewDemuxer(container.BytesSource(twin), nil)
	if err != nil {
		t.Fatal(err)
	}
	span := ref.Tracks()[0].Samples
	const origin = 1000 * 4096
	renumbered := testutil.RenumberFLACFrames(t, twin, 1000)
	for _, c := range []struct {
		name   string
		total  int64
		damage bool
	}{
		{"the source's length", origin + span, false},
		{"past it", origin + span + 4096, true},
		{"short of it", origin + span - 4096, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := patchFLACTotal(t, renumbered, c.total)
			if _, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true}); (err != nil) != c.damage {
				t.Errorf("a strict open returned %v; damage expected: %v", err, c.damage)
			}
			d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.Samples != span || !tr.SamplesExact {
				t.Errorf("%d samples (exact %v), want the span's %d exact", tr.Samples, tr.SamplesExact, span)
			}
			ws := d.Warnings()
			if len(ws) != 2 || !strings.Contains(ws[0].Msg, "first frame") || !strings.Contains(ws[1].Msg, "STREAMINFO") ||
				(ws[1].Kind == container.Damage) != c.damage {
				t.Errorf("warnings %v, want the first frame's note and one about STREAMINFO, damage: %v", ws, c.damage)
			}
			if _, walked := walk(t, d); walked != span {
				t.Errorf("walked %d samples, want the span's %d", walked, span)
			}
		})
	}
}
