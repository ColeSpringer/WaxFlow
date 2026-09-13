package mpegframes

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
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
		f[i] = byte(i)
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
	fail   error // returned by Warn, the Strict policy an owner applies
	tailOK bool  // the Trailer hook's answer, when one is wired
	trail  bool  // wire a Trailer hook at all
}

func (o *walkOpts) options() Options {
	opts := Options{Prefix: "wav: ", Warn: func(off int64, msg string) error {
		o.msgs = append(o.msgs, msg)
		o.offs = append(o.offs, off)
		return o.fail
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
