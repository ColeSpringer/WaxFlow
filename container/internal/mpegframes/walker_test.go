package mpegframes

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// The synthetic stream every test here walks: MPEG-1 Layer III, 128 kbit/s,
// 44.1 kHz, stereo, no CRC, which sizes a frame at 417 bytes. Nothing
// decodes it, and nothing here needs to: the walk reads headers and hops.
var frameHeader = []byte{0xFF, 0xFB, 0x90, 0x00}

const (
	frameLen = 417
	frameSPF = 1152
	sideInfo = 32 // MPEG-1 stereo
)

// oneFrame is a whole frame: the header, then payload bytes that are not
// 0xFF, so a resync scan cannot mistake the middle of one for a sync word.
func oneFrame() []byte {
	f := make([]byte, frameLen)
	copy(f, frameHeader)
	for i := len(frameHeader); i < len(f); i++ {
		if f[i] = byte(i); f[i] == 0xFF {
			f[i] = 0
		}
	}
	return f
}

func frames(n int) []byte { return bytes.Repeat(oneFrame(), n) }

// xingFrame is a metadata frame carrying a frame count, and optionally the
// LAME gapless extension behind it.
func xingFrame(count uint32, lame bool, delay, padding uint16) []byte {
	f := make([]byte, frameLen)
	copy(f, frameHeader)
	off := mp3.HeaderLen + sideInfo
	copy(f[off:], "Xing")
	binary.BigEndian.PutUint32(f[off+4:], 1) // frames flag
	binary.BigEndian.PutUint32(f[off+8:], count)
	if lame {
		p := off + 12
		copy(f[p:], "LAME3.100")
		b := f[p+9+12:]
		b[0] = byte(delay >> 4)
		b[1] = byte(delay<<4) | byte(padding>>8)
		b[2] = byte(padding)
	}
	return f
}

// walkOpts collects what a walk reported, so a test can assert on the
// findings the way the owning demuxer would.
type walkOpts struct {
	msgs   []string
	offs   []int64
	notes  []string
	noffs  []int64
	fail   error // returned by Warn, the Strict policy an owner applies
	tailOK bool  // the Trailer hook's answer, when one is wired
	trail  bool  // wire a Trailer hook at all
}

func (o *walkOpts) options() Options {
	opts := Options{Prefix: "wav: ", Warn: func(off int64, msg string) error {
		o.msgs = append(o.msgs, msg)
		o.offs = append(o.offs, off)
		return o.fail
	}, Note: func(off int64, msg string) {
		o.notes = append(o.notes, msg)
		o.noffs = append(o.noffs, off)
	}}
	if o.trail {
		opts.Trailer = func(int64) bool { return o.tailOK }
	}
	return opts
}

func begin(t *testing.T, data []byte, o *walkOpts) (*Walker, VBRInfo, bool) {
	t.Helper()
	w := New(container.BytesSource(data), int64(len(data)), o.options())
	tag, has, err := w.Begin(0)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return w, tag, has
}

// collect walks every frame the run holds and returns their offsets, which
// is the whole of what the index is.
func collect(t *testing.T, w *Walker) []int64 {
	t.Helper()
	var got []int64
	for n := int64(0); ; n++ {
		f, err := w.Frame(n)
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Frame(%d): %v", n, err)
		}
		if len(f.Data) != frameLen {
			t.Fatalf("Frame(%d) is %d bytes, want %d", n, len(f.Data), frameLen)
		}
		got = append(got, n)
	}
}

// TestWalkPlainRun is the baseline: a clean run of frames walks end to end,
// states its own geometry, and stops at the last whole frame.
func TestWalkPlainRun(t *testing.T) {
	var o walkOpts
	w, _, has := begin(t, frames(10), &o)
	if has {
		t.Error("a run with no metadata frame reported a VBR tag")
	}
	if w.SamplesPerFrame() != frameSPF {
		t.Errorf("samples per frame = %d, want %d", w.SamplesPerFrame(), frameSPF)
	}
	if w.FirstFrame() != 0 {
		t.Errorf("first frame at %d, want 0", w.FirstFrame())
	}
	if got := w.Header(); got.Size() != frameLen {
		t.Errorf("reference header sizes a frame at %d, want %d", got.Size(), frameLen)
	}
	if got := collect(t, w); len(got) != 10 {
		t.Errorf("walked %d frames, want 10", len(got))
	}
	if len(o.msgs) != 0 {
		t.Errorf("findings on a clean run: %v", o.msgs)
	}
}

// TestRunIsBoundedByItsRange is the property a wrapped run rests on: the
// walk stops at the end it was given, whatever the source holds after it.
// A WAV data chunk is followed by more chunks, and those bytes are not
// frames however much they look like them.
func TestRunIsBoundedByItsRange(t *testing.T) {
	payload := frames(4)
	src := append(append([]byte("RIFFxxxxWAVEdata...."), payload...), frames(3)...)
	start := int64(20)
	end := start + int64(len(payload))
	var o walkOpts
	w := New(container.BytesSource(src), end, o.options())
	if _, _, err := w.Begin(start); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := collect(t, w); len(got) != 4 {
		t.Errorf("walked %d frames, want the 4 inside the range", len(got))
	}
	if len(o.msgs) != 0 {
		t.Errorf("findings on a run that ends exactly at its bound: %v", o.msgs)
	}
}

// TestLeadingJunkIsReported pins the wording and the offset: the finding is
// reported at the start of the junk, not at the frame it ended before.
func TestLeadingJunkIsReported(t *testing.T) {
	var o walkOpts
	w, _, _ := begin(t, append([]byte("not audio, sorry"), frames(3)...), &o)
	if w.FirstFrame() != 16 {
		t.Errorf("first frame at %d, want 16", w.FirstFrame())
	}
	if len(o.msgs) != 1 || o.msgs[0] != "16 unparsable bytes before the first frame" {
		t.Fatalf("findings = %v", o.msgs)
	}
	if o.offs[0] != 0 {
		t.Errorf("finding reported at offset %d, want 0", o.offs[0])
	}
	if got := collect(t, w); len(got) != 3 {
		t.Errorf("walked %d frames, want 3", len(got))
	}
}

// TestWarnAbortsTheWalk is the Strict half of the hook: an owner that turns
// damage into an error stops the walk with it rather than being ignored.
func TestWarnAbortsTheWalk(t *testing.T) {
	boom := errors.New("strict")
	o := walkOpts{fail: boom}
	w := New(container.BytesSource(append([]byte("junk"), frames(2)...)), 4+2*frameLen, o.options())
	if _, _, err := w.Begin(0); !errors.Is(err, boom) {
		t.Fatalf("Begin error = %v, want the hook's", err)
	}
}

// TestMidRunDamageResyncs walks past a broken frame header and says so.
func TestMidRunDamageResyncs(t *testing.T) {
	// The third of five, so the two ahead of it still confirm each other:
	// a candidate is only accepted when the header its size points at is
	// kin, which is what keeps a sync word inside payload bytes from
	// starting a walk.
	data := frames(5)
	copy(data[2*frameLen:], []byte{0, 0, 0, 0})
	var o walkOpts
	w, _, _ := begin(t, data, &o)
	if got := collect(t, w); len(got) != 4 {
		t.Errorf("walked %d frames, want the 4 that still parse", len(got))
	}
	if len(o.msgs) != 1 || o.msgs[0] != "417 unparsable bytes skipped" {
		t.Fatalf("findings = %v", o.msgs)
	}
}

// TestTrailerHookDecidesTheTail is the difference between a bare stream and
// a wrapped one. Bytes past the last frame are tag baggage where the owner
// says so and damage where it has no say, which is every container that
// bounds the run itself.
func TestTrailerHookDecidesTheTail(t *testing.T) {
	data := append(frames(3), []byte("TAG this is an ID3v1 trailer, near enough")...)
	for _, tc := range []struct {
		name  string
		opts  walkOpts
		msgs  int
		first string
	}{
		{name: "no hook", msgs: 1, first: "41 trailing bytes are not frames, dropped"},
		{name: "recognized", opts: walkOpts{trail: true, tailOK: true}},
		{name: "unrecognized", opts: walkOpts{trail: true}, msgs: 1,
			first: "41 trailing bytes are not frames, dropped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.opts
			w, _, _ := begin(t, data, &o)
			if got := collect(t, w); len(got) != 3 {
				t.Errorf("walked %d frames, want 3", len(got))
			}
			if len(o.msgs) != tc.msgs {
				t.Fatalf("findings = %v, want %d", o.msgs, tc.msgs)
			}
			if tc.msgs > 0 && o.msgs[0] != tc.first {
				t.Errorf("finding = %q, want %q", o.msgs[0], tc.first)
			}
		})
	}
}

// TestTruncatedFinalFrameIsDropped keeps a half frame out of the index,
// which is what lets Frame trust every entry in it.
func TestTruncatedFinalFrameIsDropped(t *testing.T) {
	var o walkOpts
	w, _, _ := begin(t, frames(3)[:2*frameLen+100], &o)
	if got := collect(t, w); len(got) != 2 {
		t.Errorf("walked %d frames, want 2 whole ones", len(got))
	}
	if len(o.msgs) != 1 || o.msgs[0] != "truncated final frame dropped" {
		t.Fatalf("findings = %v", o.msgs)
	}
	// The premise Frame relies on, stated directly: every entry's whole
	// frame lies inside the run.
	for i, off := range w.idx {
		h, ok := w.headerAt(off)
		if !ok || off+int64(h.Size()) > w.DataEnd() {
			t.Errorf("entry %d at %d does not hold a whole frame inside %d bytes", i, off, w.DataEnd())
		}
	}
}

// TestVBRTagIsConsumed pins that a metadata frame is read and then skipped:
// it is not audio, and delivering it would put a frame of silence at the
// head of every tagged stream.
func TestVBRTagIsConsumed(t *testing.T) {
	var o walkOpts
	w, tag, has := begin(t, append(xingFrame(7, true, 576, 1000), frames(7)...), &o)
	if !has {
		t.Fatal("the Xing frame was not recognized")
	}
	if tag.Frames != 7 || tag.Delay != 576 || tag.Padding != 1000 {
		t.Errorf("tag = %+v, want frames 7 delay 576 padding 1000", tag)
	}
	if w.FirstFrame() != frameLen {
		t.Errorf("first audio frame at %d, want %d", w.FirstFrame(), frameLen)
	}
	if got := collect(t, w); len(got) != 7 {
		t.Errorf("walked %d frames, want the 7 behind the tag", len(got))
	}
}

// TestVBRTagWithoutLAMEHasNoTrims: a bare Xing states a count and nothing
// about gapless, which the -1 sentinels are how a caller tells apart from a
// tag that states zero trims.
func TestVBRTagWithoutLAMEHasNoTrims(t *testing.T) {
	_, tag, has := begin(t, append(xingFrame(2, false, 0, 0), frames(2)...), &walkOpts{})
	if !has || tag.Frames != 2 {
		t.Fatalf("tag = %+v, present = %v", tag, has)
	}
	if tag.Delay != -1 || tag.Padding != -1 {
		t.Errorf("trims = %d/%d, want -1/-1 for a tag with no LAME extension", tag.Delay, tag.Padding)
	}
}

// canonicalXingFrame is a metadata frame in the layout LAME itself writes:
// all four optional fields ahead of the extension, which puts the encoder
// string at magic+120. The fixtures above put it at magic+12, so without
// this one nothing here reads a tag laid out the way most files in the
// world are.
func canonicalXingFrame(count uint32, delay, padding uint16) []byte {
	f := make([]byte, frameLen)
	copy(f, frameHeader)
	off := mp3.HeaderLen + sideInfo
	copy(f[off:], "Xing")
	binary.BigEndian.PutUint32(f[off+4:], 1|2|4|8) // frames, bytes, TOC, quality
	binary.BigEndian.PutUint32(f[off+8:], count)
	binary.BigEndian.PutUint32(f[off+12:], frameLen*uint32(count))
	for i := range 100 {
		f[off+16+i] = byte(min(i*256/100, 255))
	}
	p := off + 120
	copy(f[p:], "LAME3.100")
	b := f[p+9+12:]
	b[0] = byte(delay >> 4)
	b[1] = byte(delay<<4) | byte(padding>>8)
	b[2] = byte(padding)
	return f
}

// TestVBRTagAtItsCanonicalOffset reads the extension out of a frame that
// carries every optional field ahead of it, which is where a reader that
// walks the flags has to find it and where a fixed-offset reader looks.
func TestVBRTagAtItsCanonicalOffset(t *testing.T) {
	data := append(canonicalXingFrame(4, 576, 1000), frames(4)...)
	w, tag, has := begin(t, data, &walkOpts{})
	if !has {
		t.Fatal("the canonical Xing frame was not recognized")
	}
	if tag.Frames != 4 || tag.Delay != 576 || tag.Padding != 1000 {
		t.Errorf("tag = %+v, want frames 4 delay 576 padding 1000", tag)
	}
	if w.FirstFrame() != frameLen {
		t.Errorf("first audio frame at %d, want %d", w.FirstFrame(), frameLen)
	}
}

// TestRefusalsCarryTheOwnersPrefix is why Prefix exists: these messages
// reach users, and they must name the format the request asked for rather
// than this package.
func TestRefusalsCarryTheOwnersPrefix(t *testing.T) {
	free := append([]byte{0xFF, 0xFB, 0x00, 0x00}, make([]byte, 400)...)
	for _, tc := range []struct {
		name string
		data []byte
		want string
		is   error
	}{
		{"free format", free, "wav: free-format stream", waxerr.ErrUnsupportedFormat},
		{"no frames", bytes.Repeat([]byte{0x11}, 4096), "wav: no Layer III frames found", waxerr.ErrMalformedInput},
		{"tag with nothing behind it", xingFrame(3, false, 0, 0), "wav: no audio frames after the VBR tag", waxerr.ErrMalformedInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := walkOpts{}
			w := New(container.BytesSource(tc.data), int64(len(tc.data)), o.options())
			_, _, err := w.Begin(0)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("error = %v, want %v", err, tc.is)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestLandingBacksOffForTheReservoir is the seek contract, and it is
// structural on purpose: a landing that equals the target passes every
// decode comparison on a high-bitrate fixture and corrupts a low-bitrate
// one, so what is pinned is that the frames skipped over carry enough main
// data to satisfy any reservoir reference at the target.
func TestLandingBacksOffForTheReservoir(t *testing.T) {
	w, _, _ := begin(t, frames(200), &walkOpts{})
	const target = 150
	land, err := w.Landing(target)
	if err != nil {
		t.Fatal(err)
	}
	if land >= target-stateFrames {
		t.Fatalf("landed at frame %d for target %d; the filterbank alone wants %d frames",
			land, target, stateFrames)
	}
	overhead := int64(mp3.HeaderLen + sideInfo)
	cover := int64(0)
	for n := land; n < target-stateFrames; n++ {
		cover += frameLen - overhead
	}
	if cover < reservoirCover {
		t.Errorf("the backoff covers %d bytes of main data, want at least %d", cover, reservoirCover)
	}
	// Targets at or before the head clamp rather than going negative, and a
	// target past the end lands on the last frame's backoff.
	for _, target := range []int64{0, 1, 1 << 40} {
		land, err := w.Landing(target)
		if err != nil {
			t.Fatal(err)
		}
		if land < 0 || land > max(target, 0) {
			t.Errorf("landing for target %d = %d", target, land)
		}
	}
}

// TestIndexRoundTrips takes the snapshot through a fresh walker and back,
// and pins the floor under it: an index small enough to rebuild faster than
// a disk round trip is not worth keeping.
func TestIndexRoundTrips(t *testing.T) {
	data := frames(IdxMinFrames + 50)
	w, _, _ := begin(t, data, &walkOpts{})
	if blob := w.Snapshot(); blob != nil {
		t.Fatal("snapshot before any walk should be nil")
	}
	want := collect(t, w)
	blob := w.Snapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}

	w2, _, _ := begin(t, data, &walkOpts{})
	if !w2.Restore(blob) {
		t.Fatal("snapshot rejected by an identical source")
	}
	if w2.Snapshot() != nil {
		t.Error("a restored index re-snapshots without growth")
	}
	if got := collect(t, w2); len(got) != len(want) {
		t.Errorf("the restored index walks %d frames, the built one %d", len(got), len(want))
	}

	// Rejections: a shorter source, a torn blob, a blob whose first entry
	// does not match the run head.
	short, _, _ := begin(t, data[:len(data)/2], &walkOpts{})
	if short.Restore(blob) {
		t.Error("blob accepted by a truncated source")
	}
	if w3, _, _ := begin(t, data, &walkOpts{}); w3.Restore(blob[:len(blob)/3]) {
		t.Error("torn blob accepted")
	}
	shifted, _, _ := begin(t, append([]byte("junk"), data...), &walkOpts{})
	if shifted.Restore(blob) {
		t.Error("blob accepted by a source whose frames moved")
	}
}

// TestSmallIndexIsNotWorthKeeping is the other side of the floor.
func TestSmallIndexIsNotWorthKeeping(t *testing.T) {
	w, _, _ := begin(t, frames(64), &walkOpts{})
	collect(t, w)
	if blob := w.Snapshot(); blob != nil {
		t.Errorf("snapshotted %d bytes for a 64-frame run", len(blob))
	}
}

// countedWalk begins a walk over data through a counting source, so a test
// can pin what the walk asked the source for. Reads are pinned beside bytes
// throughout: a scan that re-requests the same window after every false
// candidate is invisible to a byte count.
func countedWalk(t *testing.T, data []byte) (*Walker, *testutil.CountingSource) {
	t.Helper()
	src := &testutil.CountingSource{Src: container.BytesSource(data)}
	w := New(src, src.Size(), (&walkOpts{}).options())
	if _, _, err := w.Begin(0); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return w, src
}

// TestBeginReadsTheHeadOnly pins the open cost of a clean run: the header,
// one frame and the confirming header behind it, whatever the run's length.
// A caller that opens and never reads a packet (a probe, the daemon's track
// memo) pays for what it uses, which is under 2 KiB, not a window.
func TestBeginReadsTheHeadOnly(t *testing.T) {
	var reads []int
	for _, n := range []int{2000, 4000} {
		_, src := countedWalk(t, frames(n))
		if src.Bytes > 2048 || src.Reads > 3 {
			t.Errorf("Begin on %d frames read %d bytes in %d reads, want under 2048 in at most 3", n, src.Bytes, src.Reads)
		}
		reads = append(reads, src.Reads)
	}
	if reads[0] != reads[1] {
		t.Errorf("Begin cost %d reads on 2000 frames and %d on 4000; the head costs the same at any length", reads[0], reads[1])
	}
}

// TestJunkScanIsBoundedBySpans pins the scan a run led by junk pays: about
// twice the junk plus one frame, in a number of reads that grows with the
// junk's length and not with its content. Junk with a sync byte every hundred
// bytes, which is what a JPEG looks like, costs the same reads as junk with
// none; a scan that asked for a window after every false candidate would pay
// a read per sync byte.
func TestJunkScanIsBoundedBySpans(t *testing.T) {
	junk := func(n int, syncs bool) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = 0x11
			if syncs && i%100 == 0 {
				b[i] = 0xFF // followed by 0x11, which is not a sync
			}
		}
		return b
	}
	cost := func(j []byte) (reads int, bytes int64) {
		w, src := countedWalk(t, append(j, frames(3)...))
		if w.FirstFrame() != int64(len(j)) {
			t.Fatalf("first frame at %d, want %d", w.FirstFrame(), len(j))
		}
		return src.Reads, src.Bytes
	}
	const big = 100 << 10
	plainReads, plainBytes := cost(junk(big, false))
	syncReads, _ := cost(junk(big, true))
	if plainBytes > 2*big+frameLen+64 {
		t.Errorf("scanning %d bytes of junk read %d bytes, want no more than about twice the junk plus a frame", big, plainBytes)
	}
	if syncReads != plainReads {
		t.Errorf("junk with sync bytes cost %d reads, junk without cost %d; the reads must not depend on the content", syncReads, plainReads)
	}
	smallReads, _ := cost(junk(4<<10, true))
	if smallReads >= plainReads {
		t.Errorf("4 KiB of junk cost %d reads and 100 KiB cost %d; the reads grow with the junk's length", smallReads, plainReads)
	}
}

// TestWalkCostsOneReadPerWindow pins the packet path's cost after the exact
// open: the first frames cost about one window, a walk of several windows
// stays at one read per window, and the window behind it stays bounded.
func TestWalkCostsOneReadPerWindow(t *testing.T) {
	const n = 2000 // 834 KB, over six windows
	w, src := countedWalk(t, frames(n))
	src.Reset()
	for i := int64(0); i < 8; i++ {
		if _, err := w.Frame(i); err != nil {
			t.Fatal(err)
		}
	}
	if src.Reads > 1 || src.Bytes > srcwin.Chunk {
		t.Errorf("the first frames read %d bytes in %d reads, want about one window in one read", src.Bytes, src.Reads)
	}
	src.Reset()
	collect(t, w)
	windows := (n*frameLen + srcwin.Chunk - 1) / srcwin.Chunk
	if src.Reads > windows+2 {
		t.Errorf("walking %d windows took %d reads, want about one per window", windows, src.Reads)
	}
	if resident, capacity := w.w.Resident(); resident > 2*srcwin.Chunk+frameLen || capacity > 3*srcwin.Chunk {
		t.Errorf("the window holds %d bytes (capacity %d) after the walk, want it bounded near a window", resident, capacity)
	}
}

// TestLandingLeavesTheWindowBounded is the measure pass: a seek to the end
// walks the whole index, and the window must follow the walk rather than
// accrete the run behind it.
func TestLandingLeavesTheWindowBounded(t *testing.T) {
	const n = 2000
	w, src := countedWalk(t, frames(n))
	src.Reset()
	if _, err := w.Landing(n - 10); err != nil {
		t.Fatal(err)
	}
	if resident, capacity := w.w.Resident(); resident > 2*srcwin.Chunk+frameLen || capacity > 3*srcwin.Chunk {
		t.Errorf("the window holds %d bytes (capacity %d) after a landing near the end, want at most two windows", resident, capacity)
	}
	windows := (n*frameLen + srcwin.Chunk - 1) / srcwin.Chunk
	if src.Reads > windows+2 {
		t.Errorf("the landing walk took %d reads over %d windows", src.Reads, windows)
	}
}

// TestRestoreProbesHeadersNotWindows pins the sidecar restore's cost: nine
// spread header probes and the one behind the last entry, each an exact read
// rather than a window.
func TestRestoreProbesHeadersNotWindows(t *testing.T) {
	data := frames(IdxMinFrames + 50)
	w, _ := countedWalk(t, data)
	collect(t, w)
	blob := w.Snapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}
	w2, src := countedWalk(t, data)
	src.Reset()
	if !w2.Restore(blob) {
		t.Fatal("snapshot rejected by an identical source")
	}
	if src.Reads > idxProbes+2 || src.Bytes > 2048 {
		t.Errorf("Restore read %d bytes in %d reads, want a header per probe", src.Bytes, src.Reads)
	}
}

// TestCompleteFinishesTheWalk pins the strict probe's half of the walker:
// Complete extends the index to the end of the run and reports each finding
// through the hook exactly once, a hook that returns an error stops it at the
// first finding, and Done says whether the index reaches the end: not after
// Begin, yes after Complete, and yes after restoring a complete snapshot.
func TestCompleteFinishesTheWalk(t *testing.T) {
	data := frames(6)
	copy(data[3*frameLen:], []byte{0, 0, 0, 0}) // the fourth frame's header

	var o walkOpts
	w, _, _ := begin(t, data, &o)
	if w.Done() {
		t.Fatal("Done before any walk")
	}
	if err := w.Complete(); err != nil {
		t.Fatal(err)
	}
	if !w.Done() {
		t.Error("not Done after Complete")
	}
	if len(o.msgs) != 1 || o.msgs[0] != "417 unparsable bytes skipped" {
		t.Fatalf("findings = %v, want the skipped frame once", o.msgs)
	}
	if err := w.Complete(); err != nil || len(o.msgs) != 1 {
		t.Errorf("a second Complete returned %v and left %d findings; a finished walk reports nothing again", err, len(o.msgs))
	}
	if got := collect(t, w); len(got) != 5 {
		t.Errorf("walked %d frames after Complete, want the 5 that parse", len(got))
	}

	boom := errors.New("strict")
	strict := walkOpts{fail: boom}
	ws, _, _ := begin(t, data, &strict)
	if err := ws.Complete(); !errors.Is(err, boom) {
		t.Errorf("Complete under a refusing hook returned %v, want the hook's error", err)
	}
	if ws.Done() {
		t.Error("a walk stopped by its hook claims to be Done")
	}

	// A restored complete snapshot is a finished walk.
	long := frames(IdxMinFrames + 50)
	full, _, _ := begin(t, long, &walkOpts{})
	if err := full.Complete(); err != nil {
		t.Fatal(err)
	}
	restored, _, _ := begin(t, long, &walkOpts{})
	if !restored.Restore(full.Snapshot()) {
		t.Fatal("snapshot rejected by an identical source")
	}
	if !restored.Done() {
		t.Error("a restored complete index is not Done")
	}
}

// TestDeclaredCountIsCheckedAgainstTheRun covers the comparison the walk
// makes when the index latches: a metadata frame's count is a claim, and the
// run is the measurement.
//
// Which side it lands on is the whole of it. A run that comes up short
// promised audio its bytes do not hold, so the length shrinks and that is
// damage. A run that overruns holds everything the tag named and more, and a
// run one frame short of a count on an otherwise clean end is an encoder
// counting its own metadata frame; both are Notes, and both settle the same
// way.
func TestDeclaredCountIsCheckedAgainstTheRun(t *testing.T) {
	const short = "the metadata frame declares 8 frames but the run holds 5"
	for _, tc := range []struct {
		name string
		data []byte
		warn string
		note string
	}{
		{"the run is whole", append(xingFrame(5, false, 0, 0), frames(5)...), "", ""},
		{"the run comes up short", append(xingFrame(8, false, 0, 0), frames(5)...), short, ""},
		{
			"one frame short on a clean end",
			append(xingFrame(6, false, 0, 0), frames(5)...),
			"", "the metadata frame declares 6 frames but the run holds 5",
		},
		{
			"the run overruns the count",
			append(xingFrame(5, false, 0, 0), frames(8)...),
			"", "the run holds 3 frames past the 5 the metadata frame declares",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o walkOpts
			w, _, _ := begin(t, tc.data, &o)
			if err := w.Complete(); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got := joined(o.msgs); got != tc.warn {
				t.Errorf("warnings = %q, want %q", got, tc.warn)
			}
			if got := joined(o.notes); got != tc.note {
				t.Errorf("notes = %q, want %q", got, tc.note)
			}
			// The finding names where the frames the tag promised would have
			// continued from, which is the end of the run.
			if tc.warn != "" && o.offs[0] != int64(len(tc.data)) {
				t.Errorf("reported at %d, want the end of the run (%d)", o.offs[0], len(tc.data))
			}
		})
	}
}

// TestAShortfallIsFoundOnTheReadPathToo is why the comparison lives in the
// walker rather than in each owner's Walk: a file cut exactly on a frame
// boundary leaves nothing for a read to notice, so a read and a walk of it
// would otherwise disagree about whether it is damaged.
func TestAShortfallIsFoundOnTheReadPathToo(t *testing.T) {
	data := append(xingFrame(8, false, 0, 0), frames(5)...)
	var o walkOpts
	w, _, _ := begin(t, data, &o)
	if got := collect(t, w); len(got) != 5 {
		t.Fatalf("read %d frames, want 5", len(got))
	}
	if got := joined(o.msgs); got == "" {
		t.Error("a read to the end of a short run said nothing")
	}

	// And a strict owner refuses on the read, where it refuses on the walk.
	strict := walkOpts{fail: errors.New("strict")}
	w2, _, _ := begin(t, data, &strict)
	var err error
	for n := int64(0); err == nil; n++ {
		_, err = w2.Frame(n)
	}
	if !errors.Is(err, strict.fail) {
		t.Errorf("a strict read to the end = %v, want the refusal", err)
	}
}

// TestImpossibleDeclaredCountIsIgnored bounds the tag the way riff bounds its
// fact chunk: the smallest compliant frame puts a ceiling on the run at O(1),
// and a count above it is one the payload cannot be describing. Clearing it
// takes the trims with it, since a length and its trims are adopted together
// or not at all.
func TestImpossibleDeclaredCountIsIgnored(t *testing.T) {
	var o walkOpts
	data := append(xingFrame(1<<20, true, 576, 1000), frames(3)...)
	w, tag, has := begin(t, data, &o)
	if !has {
		t.Fatal("the Xing frame was not recognized")
	}
	if tag.Frames != 0 {
		t.Errorf("tag frames = %d, want it cleared", tag.Frames)
	}
	if samples, delay, padding := tag.Gapless(frameSPF); samples != -1 || delay != 0 || padding != 0 {
		t.Errorf("gapless = %d/%d/%d, want the trims to go with the count", samples, delay, padding)
	}
	want := fmt.Sprintf("the metadata frame declares %d frames, more than %d bytes can hold; ignored",
		1<<20, int64(len(data))-frameLen)
	if got := joined(o.msgs); got != want {
		t.Errorf("findings = %q, want %q", got, want)
	}
	// An ignored count is not compared against anything.
	if err := w.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(o.msgs) != 1 || len(o.notes) != 0 {
		t.Errorf("after the walk: warnings %v, notes %v", o.msgs, o.notes)
	}
}

// TestRestoredIndexSettlesTheCount closes the one path that finishes an index
// without walking to its end. The sidecar says where every frame is, so the
// run's count is known the moment the blob is adopted and the comparison runs
// there; a strict owner that refuses it declines the blob instead, and the
// rebuilt walk reports at its own end, where it can also say what the run
// holds.
func TestRestoredIndexSettlesTheCount(t *testing.T) {
	const n = IdxMinFrames + 50
	data := append(xingFrame(n+9, false, 0, 0), frames(n)...)
	build, _, _ := begin(t, data, &walkOpts{})
	if err := build.Complete(); err != nil {
		t.Fatal(err)
	}
	blob := build.Snapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}

	var o walkOpts
	w, _, _ := begin(t, data, &o)
	if !w.Restore(blob) {
		t.Fatal("a tolerant owner declined its own blob")
	}
	want := fmt.Sprintf("the metadata frame declares %d frames but the run holds %d", n+9, n)
	if got := joined(o.msgs); got != want {
		t.Errorf("findings after Restore = %q, want %q", got, want)
	}

	strict := walkOpts{fail: errors.New("strict")}
	sw, _, _ := begin(t, data, &strict)
	if sw.Restore(blob) {
		t.Fatal("a strict owner adopted a blob whose count it refuses")
	}
	if sw.Done() || sw.Frames() > 1 {
		t.Errorf("the declined blob left the walker at done=%v frames=%d", sw.Done(), sw.Frames())
	}
	strict.msgs = nil
	if err := sw.Complete(); !errors.Is(err, strict.fail) {
		t.Errorf("the rebuilt walk = %v, want the refusal at its own end", err)
	}
	if got := joined(strict.msgs); got != want {
		t.Errorf("the rebuilt walk reported %q, want %q", got, want)
	}
}

// TestADamagedRunIsNeverSnapshottedComplete is what keeps a sidecar from
// changing a verdict. The blob carries the index and the completeness flag
// and nothing about how the run ended, so a restore that adopted a complete
// index over a damaged run would compare the declared count with the damage
// forgotten: a one-frame shortfall would come back a Note where the cold walk
// called it damage, and strict would pass on the warm open and fail on the
// cold one.
func TestADamagedRunIsNeverSnapshottedComplete(t *testing.T) {
	const n = IdxMinFrames + 50
	// n frames behind a tag that counts one more, with junk after them: a
	// one-frame shortfall whose end is damaged, which is the pair the
	// tolerance turns on. Trailing junk rather than a truncated final frame,
	// because Restore's own trust-but-verify already catches that one (a half
	// frame still parses a kin header, so the blob's completeness is refused
	// on the spot); junk does not parse, so the flag would stand.
	data := append(xingFrame(n+1, false, 0, 0), frames(n)...)
	data = append(data, bytes.Repeat([]byte{0x11}, 64)...)

	var cold walkOpts
	w, _, _ := begin(t, data, &cold)
	if err := w.Complete(); err != nil {
		t.Fatal(err)
	}
	if !w.Done() {
		t.Fatal("the walk did not reach the end")
	}
	if len(cold.notes) != 0 || len(cold.msgs) != 2 {
		t.Fatalf("cold walk: warnings %v, notes %v; want the trailing bytes and the shortfall as damage",
			cold.msgs, cold.notes)
	}
	blob := w.Snapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}

	var warm walkOpts
	w2, _, _ := begin(t, data, &warm)
	if !w2.Restore(blob) {
		t.Fatal("the blob was rejected by its own source")
	}
	if w2.Done() {
		t.Error("a run that ended on damage was restored as complete")
	}
	if err := w2.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(warm.notes) != len(cold.notes) || len(warm.msgs) != len(cold.msgs) {
		t.Errorf("warm walk: warnings %v, notes %v; want the cold walk's %v / %v",
			warm.msgs, warm.notes, cold.msgs, cold.notes)
	}
}

// joined renders a finding list for comparison; every case here expects at
// most one, and an unexpected second must not read as a pass.
func joined(msgs []string) string { return strings.Join(msgs, " | ") }

// TestMidRunDamageIsNotSnapshotted: a sidecar must not change a verdict. The
// blob has no room for a finding, and unlike the head's findings and a run's
// end, a skip between two indexed frames is nothing a restore re-walks; a
// run with one keeps no snapshot and re-walks on every open instead.
func TestMidRunDamageIsNotSnapshotted(t *testing.T) {
	data := frames(IdxMinFrames + 50)
	copy(data[(IdxMinFrames/2)*frameLen:], []byte{0, 0, 0, 0})
	var o walkOpts
	w, _, _ := begin(t, data, &o)
	if got := collect(t, w); len(got) != IdxMinFrames+49 {
		t.Errorf("walked %d frames, want the %d that still parse", len(got), IdxMinFrames+49)
	}
	if len(o.msgs) != 1 || o.msgs[0] != "417 unparsable bytes skipped" {
		t.Fatalf("findings = %v", o.msgs)
	}
	if blob := w.Snapshot(); blob != nil {
		t.Errorf("a run with damage in it snapshotted %d bytes; a restore would forget the finding", len(blob))
	}
}
