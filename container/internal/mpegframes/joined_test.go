package mpegframes

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/trailer"
)

// stream is a metadata frame and the n frames it counts: its frame count, the
// byte count of the whole stream, and the LAME trims.
func stream(n int, delay, padding uint16) []byte {
	return streamCounting(n, uint32(n), delay, padding)
}

// streamCounting is stream with the frame count its metadata frame states
// given apart from the n frames it holds.
func streamCounting(n int, count uint32, delay, padding uint16) []byte {
	head := xingFrame(count, false, 0, 0)
	off := 4 + sideInfo
	binary.BigEndian.PutUint32(head[off+4:], 3) // frames and bytes
	binary.BigEndian.PutUint32(head[off+12:], uint32((n+1)*frameLen))
	p := off + 16
	copy(head[p:], "LAME3.100")
	b := head[p+9+12:]
	b[0], b[1], b[2] = byte(delay>>4), byte(delay<<4)|byte(padding>>8), byte(padding)
	return append(head, frames(n)...)
}

// id3v2 is a leading tag of n body bytes; id3v1 a trailing one.
func id3v2(n int) []byte {
	return append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, byte(n >> 7), byte(n & 0x7f)}, make([]byte, n)...)
}

func id3v1() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAGtitle")
	return tag
}

// bare is the options a bare stream's owner walks with.
func (o *walkOpts) bare() Options {
	opts := o.options()
	opts.Trailers = trailer.APEv2 | trailer.ID3v1 | trailer.ID3v2 | trailer.Lyrics3
	opts.Joined = true
	return opts
}

func beginBare(t *testing.T, data []byte, o *walkOpts) (*Walker, VBRInfo) {
	t.Helper()
	w := New(container.BytesSource(data), int64(len(data)), o.bare())
	tag, _, err := w.Begin(0)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return w, tag
}

// TestTrailersArePeeledAtBegin: the tags behind a bare stream come off the
// data at Begin, so the walk ends at the last frame with nothing asked of the
// hook, and a wrapped run, which names none, keeps every byte as its own.
func TestTrailersArePeeledAtBegin(t *testing.T) {
	data := slices.Concat(frames(3), id3v1())
	var o walkOpts
	w, _ := beginBare(t, data, &o)
	if w.DataEnd() != 3*frameLen {
		t.Errorf("the data ends at %d, want the %d the frames hold", w.DataEnd(), 3*frameLen)
	}
	if got := collect(t, w); len(got) != 3 || len(o.msgs) != 0 {
		t.Errorf("walked %d frames with %v, want 3 and nothing", len(got), o.msgs)
	}
	var wrapped walkOpts
	w, _, _ = begin(t, data, &wrapped)
	if got := collect(t, w); w.DataEnd() != int64(len(data)) || len(got) != 3 || len(wrapped.msgs) != 1 {
		t.Errorf("a wrapped run ends at %d, %d frames, %v; want every byte its own and the tail reported", w.DataEnd(), len(got), wrapped.msgs)
	}
}

// TestTruncatedFinalFrameEndsAtTheTags: a last frame cut short is dropped
// with tags behind it as it is without. Bounded by the end of the file the
// frame fit again, the tags' bytes its payload.
func TestTruncatedFinalFrameEndsAtTheTags(t *testing.T) {
	cut := frames(3)[:2*frameLen+100]
	for name, tail := range map[string][]byte{
		"nothing":    nil,
		"id3v1":      id3v1(),
		"two id3v1":  slices.Concat(id3v1(), id3v1()),
		"long id3v1": slices.Concat(id3v1(), id3v1(), id3v1(), id3v1()), // the frame's length and more
	} {
		var o walkOpts
		w, _ := beginBare(t, slices.Concat(cut, tail), &o)
		if got := collect(t, w); len(got) != 2 {
			t.Errorf("%s: walked %d frames, want the 2 whole ones", name, len(got))
		}
		if len(o.msgs) != 1 || o.msgs[0] != "truncated final frame dropped" || o.offs[0] != 2*frameLen {
			t.Errorf("%s: findings = %v at %v", name, o.msgs, o.offs)
		}
	}
}

// TestALoneFrameInFrontOfTags: a single frame has no header behind it to
// confirm it, and a tag there is not one. The tags come off first.
func TestALoneFrameInFrontOfTags(t *testing.T) {
	var o walkOpts
	w, _ := beginBare(t, slices.Concat(frames(1), id3v1()), &o)
	if got := collect(t, w); len(got) != 1 || len(o.msgs) != 0 {
		t.Errorf("walked %d frames with %v, want the one and nothing", len(got), o.msgs)
	}
}

// TestJoinedStreamsCountTogether: a byte count that ends on another stream's
// first frame ends its own stream there. Where every stream states its
// counts the run's is their sum, each later metadata frame counted as the
// frame it is delivered as, and the padding is the last stream's.
func TestJoinedStreamsCountTogether(t *testing.T) {
	a, b, c := stream(5, 576, 1000), stream(7, 576, 1200), stream(3, 576, 1300)
	for _, tc := range []struct {
		name            string
		data            []byte
		frames, padding int64
		walked          int
		notes           int
	}{
		{"two streams", slices.Concat(a, b), 5 + 1 + 7, 1200, 13, 0},
		{"three streams", slices.Concat(a, b, c), 5 + 1 + 7 + 1 + 3, 1300, 17, 0},
		{"a tag between them", slices.Concat(a, id3v2(40), b), 5 + 1 + 7, 1200, 13, 1},
		{"two tags between them", slices.Concat(a, id3v2(40), id3v2(9), b), 5 + 1 + 7, 1200, 13, 1},
		{"tags behind the last", slices.Concat(a, b, id3v1()), 5 + 1 + 7, 1200, 13, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o walkOpts
			w, tag := beginBare(t, tc.data, &o)
			if !tag.Joined || tag.Frames != tc.frames || tag.Delay != 576 || tag.Padding != tc.padding {
				t.Errorf("tag = %+v, want joined, %d frames, delay 576, padding %d", tag, tc.frames, tc.padding)
			}
			if got := collect(t, w); len(got) != tc.walked {
				t.Errorf("walked %d frames, want %d", len(got), tc.walked)
			}
			if len(o.msgs) != 0 || len(o.notes) != tc.notes {
				t.Errorf("warnings %v, notes %v; want no damage and %d notes", o.msgs, o.notes, tc.notes)
			}
			if w.Truncated() != 0 {
				t.Errorf("truncated = %d", w.Truncated())
			}
		})
	}
}

// TestAJoinedRunWithNoCountPlaysToItsEnd: a stream joined on that states no
// counts of its own leaves the run's end unstated. The first stream's count
// and padding are dropped, its delay stays, and the length is the walk's.
func TestAJoinedRunWithNoCountPlaysToItsEnd(t *testing.T) {
	a := stream(5, 576, 1000)
	many := bytes.Repeat(stream(1, 576, 1000), maxJoins+2)
	for name, data := range map[string][]byte{
		"frames with no metadata frame":       slices.Concat(a, frames(4)),
		"a metadata frame with no byte count": slices.Concat(a, xingFrame(4, true, 576, 900), frames(4)),
		"more streams than are followed":      many,
	} {
		var o walkOpts
		_, tag := beginBare(t, data, &o)
		if !tag.Joined || tag.Frames != 0 || tag.Delay != 576 {
			t.Errorf("%s: tag = %+v, want joined with no count and the delay", name, tag)
		}
		if samples, delay, padding := tag.Gapless(frameSPF); samples != -1 || delay != 576+DecoderDelay || padding != 0 {
			t.Errorf("%s: Gapless = %d, %d, %d; want no length, the front trim, no end trim", name, samples, delay, padding)
		}
		if len(o.msgs) != 0 {
			t.Errorf("%s: warnings %v", name, o.msgs)
		}
	}
}

// TestAByteCountThatIsMerelyWrongDemotesNothing: only a count that ends on
// another stream's first frame is a join. One that ends inside a frame, past
// the data, at its end, in front of bytes that are no frame, or on a frame of
// its own stream short of where its frame count ends leaves the tag as it
// was, and so does any count in a run the container bounds.
func TestAByteCountThatIsMerelyWrongDemotesNothing(t *testing.T) {
	setBytes := func(data []byte, n int) []byte {
		data = bytes.Clone(data)
		binary.BigEndian.PutUint32(data[4+sideInfo+12:], uint32(n))
		return data
	}
	a := stream(5, 576, 1000)
	for name, data := range map[string][]byte{
		"exact":                  a,
		"short by whole frames":  setBytes(a, 3*frameLen),
		"short by one frame":     setBytes(a, 5*frameLen),
		"inside a frame":         setBytes(a, 3*frameLen+17),
		"past the data":          setBytes(a, 9*frameLen),
		"in front of junk":       slices.Concat(a, bytes.Repeat([]byte{0x11}, 300)),
		"in front of a lone tag": slices.Concat(a, id3v2(40)),
	} {
		var o walkOpts
		_, tag := beginBare(t, data, &o)
		if tag.Joined || tag.Frames != 5 || tag.Delay != 576 || tag.Padding != 1000 {
			t.Errorf("%s: tag = %+v, want the stream's own", name, tag)
		}
		if samples, _, padding := tag.Gapless(frameSPF); samples <= 0 || padding <= 0 {
			t.Errorf("%s: Gapless = %d samples, %d padding; want the stream's length and end trim", name, samples, padding)
		}
		if len(o.msgs) != 0 {
			t.Errorf("%s: warnings %v", name, o.msgs)
		}
	}
	var o walkOpts
	_, tag, _ := begin(t, slices.Concat(a, stream(7, 576, 1200)), &o)
	if tag.Joined || tag.Frames != 5 {
		t.Errorf("a wrapped run: tag = %+v, want the first stream's own", tag)
	}
}

// paddedStream is stream with the frame lengths a 128 kbit/s encoder writes
// at 44.1 kHz to hold its bit rate: 417 bytes, or 418 with the padding bit,
// about 24 frames in 25. It returns the length of each audio frame too.
func paddedStream(n int, delay, padding uint16) (data []byte, lens []int) {
	data = stream(0, delay, padding)
	for i := range n {
		f := oneFrame()
		// 144 * 128000 / 44100 bytes a frame, held as a running floor.
		if k := int64(i); (k+1)*144*128000/44100-k*144*128000/44100 == frameLen+1 {
			f[2] |= 0x02
			f = append(f, 0)
		}
		data = append(data, f...)
		lens = append(lens, len(f))
	}
	off := 4 + sideInfo
	binary.BigEndian.PutUint32(data[off+8:], uint32(n))
	binary.BigEndian.PutUint32(data[off+12:], uint32(len(data)))
	return data, lens
}

// TestAShortCountOnAPaddedStreamDemotesNothing: the frames of a constant
// bit rate stream are not all one length, and a byte count short by whole
// frames must not pass for the stream's own end by the slack between them.
// The same stream with frames joined on is a join still.
func TestAShortCountOnAPaddedStreamDemotesNothing(t *testing.T) {
	const n = 2000
	whole, lens := paddedStream(n, 576, 1000)
	if padded := len(whole) - (n+1)*frameLen; padded < n*9/10 || padded >= n {
		t.Fatalf("%d of %d frames are padded, want most and not all", padded, n)
	}
	short := 0
	for k := 1; k <= 24; k++ {
		short += lens[n-k]
		data := bytes.Clone(whole)
		binary.BigEndian.PutUint32(data[4+sideInfo+12:], uint32(len(whole)-short))
		var o walkOpts
		_, tag := beginBare(t, data, &o)
		if tag.Joined || tag.Frames != n || tag.Padding != 1000 {
			t.Errorf("short by %d frames: tag = %+v, want the stream's own", k, tag)
		}
	}
	var o walkOpts
	_, tag := beginBare(t, slices.Concat(whole, frames(4)), &o)
	if !tag.Joined || tag.Frames != 0 {
		t.Errorf("frames joined on: tag = %+v, want joined with no count", tag)
	}
	// An encoder that counts its metadata frame states one frame more.
	counted := bytes.Clone(whole)
	binary.BigEndian.PutUint32(counted[4+sideInfo+8:], n+1)
	_, tag = beginBare(t, slices.Concat(counted, frames(4)), &o)
	if !tag.Joined || tag.Frames != 0 {
		t.Errorf("frames joined on a stream that counts its metadata frame: tag = %+v, want joined", tag)
	}
}

// TestATagBetweenFramesIsSteppedOver: in a bare stream ID3v2 tags with the
// run's frames behind them are noted and stepped over, on a walk and on a
// read, and such a run keeps no snapshot, since a restored index would read
// it with the note unsaid. A wrapped run has no such baggage: there the
// bytes are damage.
func TestATagBetweenFramesIsSteppedOver(t *testing.T) {
	const n = IdxMinFrames + 50
	data := slices.Concat(frames(n/2), id3v2(40), frames(n-n/2))
	var o walkOpts
	w, _ := beginBare(t, data, &o)
	if err := w.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Frames() != n || len(o.msgs) != 0 || len(o.notes) != 1 || o.noffs[0] != int64(n/2*frameLen) {
		t.Errorf("walked %d frames, warnings %v, notes %v at %v; want %d frames and one note at the tag", w.Frames(), o.msgs, o.notes, o.noffs, n)
	}
	if blob := w.Snapshot(); blob != nil {
		t.Errorf("a run with a tag in it snapshotted %d bytes", len(blob))
	}
	var read walkOpts
	w, _ = beginBare(t, data, &read)
	if got := collect(t, w); len(got) != n || len(read.msgs) != 0 || len(read.notes) != 1 {
		t.Errorf("a read: %d frames, warnings %v, notes %v", len(got), read.msgs, read.notes)
	}
	var wrapped walkOpts
	w, _, _ = begin(t, data, &wrapped)
	if got := collect(t, w); len(got) != n || len(wrapped.msgs) != 1 || wrapped.msgs[0] != "50 unparsable bytes skipped" {
		t.Errorf("a wrapped run: %d frames, findings %v", len(got), wrapped.msgs)
	}
}

// TestAJoinedCountPastTheBytesIsIgnored: a joined stream's frame count is
// held to what the bytes behind it can hold, as a lone stream's is, and so is
// the sum. One past it is reported and ignored, which leaves the run with no
// count: nothing a metadata frame claims becomes a length no file could hold.
func TestAJoinedCountPastTheBytesIsIgnored(t *testing.T) {
	a := stream(5, 576, 1000)
	whole := (5 + 1 + 7) * frameLen
	for name, tc := range map[string]struct {
		data []byte
		msg  string
	}{
		"one stream's count": {slices.Concat(a, streamCounting(7, 0xFFFFFFF0, 576, 1200)),
			"the metadata frame declares 4294967280 frames, more than 2919 bytes can hold; ignored"},
		// Each count fits the bytes behind its own frame; together they do not.
		"the sum": {slices.Concat(streamCounting(5, uint32(MaxFrames(int64(whole))), 576, 1000), streamCounting(7, uint32(MaxFrames(7*frameLen)), 576, 1200)),
			"the joined metadata frames declare 347 frames, more than 5421 bytes can hold; ignored"},
	} {
		var o walkOpts
		w, tag := beginBare(t, tc.data, &o)
		if !tag.Joined || tag.Frames != 0 {
			t.Errorf("%s: tag = %+v, want joined with no count", name, tag)
		}
		if samples, delay, _ := tag.Gapless(frameSPF); samples != -1 || delay != 576+DecoderDelay {
			t.Errorf("%s: Gapless = %d, %d; want no length and the front trim", name, samples, delay)
		}
		if len(o.msgs) != 1 || o.msgs[0] != tc.msg {
			t.Errorf("%s: findings = %v, want %q", name, o.msgs, tc.msg)
		}
		if got := collect(t, w); len(got) != 13 {
			t.Errorf("%s: walked %d frames, want 13", name, len(got))
		}
		// An owner that refuses the finding stops the open.
		strict := walkOpts{fail: errors.New("strict")}
		ws := New(container.BytesSource(tc.data), int64(len(tc.data)), strict.bare())
		if _, _, err := ws.Begin(0); err == nil {
			t.Errorf("%s: a refusing owner opened the run", name)
		}
	}
}

// TestJoinedStreamsThatCountTheirOwnFrame: an encoder that counts its own
// metadata frame leaves an intact file one frame short of its count, which is
// a Note. Files of such an encoder joined are short by one frame each, and
// that is the same looseness, not damage: nothing is truncated. One frame
// more than that missing is a shortfall.
func TestJoinedStreamsThatCountTheirOwnFrame(t *testing.T) {
	a, b := streamCounting(5, 6, 576, 1000), streamCounting(7, 8, 576, 1200)
	for _, tc := range []struct {
		name        string
		data        []byte
		notes, msgs int
	}{
		{"one file", a, 1, 0},
		{"two joined", slices.Concat(a, b), 1, 0},
		{"two joined, a frame gone", slices.Concat(a, b[:len(b)-frameLen]), 0, 1},
	} {
		var o walkOpts
		w, _ := beginBare(t, tc.data, &o)
		if err := w.Complete(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(o.notes) != tc.notes || len(o.msgs) != tc.msgs || (w.Truncated() != 0) != (tc.msgs != 0) {
			t.Errorf("%s: notes %v, warnings %v, truncated %d; want %d notes and %d warnings", tc.name, o.notes, o.msgs, w.Truncated(), tc.notes, tc.msgs)
		}
	}
}
