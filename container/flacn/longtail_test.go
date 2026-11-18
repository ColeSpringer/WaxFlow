package flacn_test

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// tailRead is what an open and a read to the end make of a stream.
type tailRead struct {
	samples  int64
	exact    bool
	packets  []string // position, duration and checksum of each
	end      error
	warnings []container.Warning
}

func readTail(t *testing.T, raw []byte) tailRead {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	out := tailRead{samples: tr.Samples, exact: tr.SamplesExact}
	var pkt container.Packet
	for {
		if out.end = d.ReadPacket(&pkt); out.end != nil {
			break
		}
		out.packets = append(out.packets, fmt.Sprintf("%d %d %08x", pkt.PTS, pkt.Dur, crc32.ChecksumIEEE(pkt.Data)))
	}
	out.warnings = d.Warnings()
	return out
}

// TestALongJunkTailIsAShortOne: more junk behind the frames than a frame can
// be long is what less is: every frame is delivered, the junk is reported
// as trailing bytes where the frames end, and Strict refuses it at open.
func TestALongJunkTailIsAShortOne(t *testing.T) {
	whole := fixture(t, "sine-s24.flac")
	frames := testutil.FLACFrames(t, whole)
	const short, long = 8 << 20, flacn.MaxFrameLenForTest + 1<<20
	for _, tc := range []struct {
		name string
		junk func(n int) []byte
	}{
		{"junk with no sync byte", func(n int) []byte { return bytes.Repeat([]byte{0x11}, n) }},
		{"junk dense with sync bytes", func(n int) []byte { return testutil.SyncJunk(n, 0xFF) }},
	} {
		with := func(n int) []byte { return append(append([]byte(nil), whole...), tc.junk(n)...) }
		want, got := readTail(t, with(short)), readTail(t, with(long))
		// The 8 MiB tail reads as it did.
		if want.samples != 16800 || want.exact || len(want.packets) != len(frames) || want.end != io.EOF || len(want.warnings) != 1 ||
			want.warnings[0] != (container.Warning{Offset: int64(len(whole)), Msg: fmt.Sprintf("%d trailing bytes are not a valid frame, dropped", short)}) {
			t.Fatalf("%s, %d bytes of it: %d samples, exact %v, %d packets, ending %v, warnings %v", tc.name, short,
				want.samples, want.exact, len(want.packets), want.end, want.warnings)
		}
		if got.samples != want.samples || got.exact != want.exact || got.end != io.EOF || strings.Join(got.packets, ",") != strings.Join(want.packets, ",") {
			t.Errorf("%s, %d bytes of it: %d samples, exact %v, packets %v, ending %v; the shorter tail gives %d, %v, %v",
				tc.name, long, got.samples, got.exact, got.packets, got.end, want.samples, want.exact, want.packets)
		}
		if len(got.warnings) != 1 || got.warnings[0] != (container.Warning{Offset: int64(len(whole)), Msg: fmt.Sprintf("%d trailing bytes are not a valid frame, dropped", long)}) {
			t.Errorf("%s, %d bytes of it: warnings %v, want the junk alone, where the frames end at %d", tc.name, long, got.warnings, len(whole))
		}
		for _, n := range []int{short, long} {
			_, err := flacn.NewDemuxer(container.BytesSource(with(n)), &container.DemuxerOptions{Strict: true})
			if refusal := fmt.Sprintf("flac: %d trailing bytes are not a valid frame, dropped (at offset %d)", n, len(whole)); err == nil || err.Error() != refusal {
				t.Errorf("%s, %d bytes of it: a Strict open returned %v, want %q", tc.name, n, err, refusal)
			}
		}
	}
}

// TestAJunkTailCostsWhatItDid pins the reads, the bytes and what the window
// holds for a junk tail under a frame's length and one over it. The scan
// for a successor is what a tail costs, and it stops at a frame's length.
func TestAJunkTailCostsWhatItDid(t *testing.T) {
	whole := fixture(t, "sine-s24.flac")
	for _, tc := range []struct {
		junk, reads   int
		bytes, window int64
	}{
		{8 << 20, 185, 16880324, 8407381},
		{flacn.MaxFrameLenForTest + 1<<20, 338, 34814407, 16908288},
	} {
		src := &testutil.CountingSource{Src: container.BytesSource(append(append([]byte(nil), whole...), bytes.Repeat([]byte{0x11}, tc.junk)...))}
		d, err := flacn.NewDemuxer(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		held := int64(d.ResidentForTest())
		var pkt container.Packet
		for d.ReadPacket(&pkt) == nil {
			held = max(held, int64(d.ResidentForTest()))
		}
		if src.Reads != tc.reads || src.Bytes != tc.bytes || held != tc.window {
			t.Errorf("%d bytes of junk: an open and a read took %d reads of %d bytes and held %d, want %d of %d and %d",
				tc.junk, src.Reads, src.Bytes, held, tc.reads, tc.bytes, tc.window)
		}
	}
}

// TestMoreThanAFrameOfJunkMidStreamEndsTheRead: a frame followed by more
// bytes than a frame can be, none of them a successor, is delivered and
// ends a read as a tail does, short of the length the closing frame
// confirmed. A seek still reaches the frames behind the junk.
func TestMoreThanAFrameOfJunkMidStreamEndsTheRead(t *testing.T) {
	whole := fixture(t, "sine-s24.flac")
	frames := testutil.FLACFrames(t, whole)
	const junk = flacn.MaxFrameLenForTest + 1<<20
	at := frames[2].Off
	raw := testutil.Splice(whole, at, bytes.Repeat([]byte{0x11}, junk))
	d := opened(t, raw, 16800)
	if n := readCount(t, d); n != frames[2].Pos {
		t.Fatalf("read %d samples, want the %d in front of the junk", n, frames[2].Pos)
	}
	settled(t, d, frames[2].Pos, 16800-frames[2].Pos, "STREAMINFO declares 16800 samples but the frames end at 9216")
	if got := damageOf(d); len(got) != 2 || got[0] != fmt.Sprintf("%d bytes behind the frame hold no frame within reach, dropped short of the closing frame", len(raw)-at) {
		t.Errorf("findings %v, want the bytes behind the second frame and the shortfall", got)
	}
	if landed, err := d.SeekSample(context.Background(), 0, frames[3].Pos); err != nil || landed != frames[3].Pos {
		t.Fatalf("a seek behind the junk landed at %d (%v), want %d", landed, err, frames[3].Pos)
	}
	if n := readCount(t, d); n != frames[3].Dur {
		t.Errorf("read %d samples from the seek, want the closing frame's %d", n, frames[3].Dur)
	}
	// A decode ends where the read does, and Strict refuses the junk.
	med, err := format.Open(container.BytesSource(raw), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if n := decodeRest(t, med); n != frames[2].Pos {
		t.Errorf("a decode delivered %d samples, want %d", n, frames[2].Pos)
	}
	ds, err := flacn.NewDemuxer(container.BytesSource(raw), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readFrames(t, ds); err == nil || !strings.Contains(err.Error(), "hold no frame within reach") {
		t.Errorf("a Strict read ended %v, want the junk refused", err)
	}
}
