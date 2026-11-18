package flac

import "testing"

// numbered is a fixed-strategy frame header carrying coded, of the block
// size bsCode names (12 is 4096, 1 is 192, 5 is 4608).
func numbered(t *testing.T, coded uint64, bsCode int) FrameInfo {
	t.Helper()
	b := appendCoded([]byte{0xFF, 0xF8, byte(bsCode<<4 | 9), 1<<4 | 4<<1}, coded)
	fi, err := ParseFrameHeader(append(b, crc8(b)))
	if err != nil || fi.Coded != coded {
		t.Fatalf("built a header numbered %d (%v), want %d", fi.Coded, err, coded)
	}
	return fi
}

// TestNumberingTakesTheBlockSizeSTREAMINFOStates: equal, valid bounds are the
// stream's block size (RFC 9639 section 8.2), and they place a frame shorter
// than it that is known to be the stream's last. With a frame behind it, or
// nothing known of what follows, the frame's own size is the block size as
// it was.
func TestNumberingTakesTheBlockSizeSTREAMINFOStates(t *testing.T) {
	si := StreamInfo{MinBlock: 4096, MaxBlock: 4096}
	short, next := numbered(t, 1, 1), numbered(t, 2, 12) // 192 samples
	if n := si.NumberingLast(short); n.SampleCoded || n.ConstBlock != 4096 || n.Start(short) != 4096 {
		t.Errorf("a short last frame numbered 1 under 4096-sample bounds: %+v starting at %d, want frame 1 of 4096", n, n.Start(short))
	}
	if n := si.Numbering(short); n.SampleCoded || n.ConstBlock != 192 {
		t.Errorf("a short frame with nothing known behind it: %+v, want its own 192", n)
	}
	if !si.NumberingUnsettled(short) {
		t.Error("a frame shorter than the stated block size is settled by what follows it")
	}
	if n := si.NumberingBy(short, next); n.SampleCoded || n.ConstBlock != 192 {
		t.Errorf("a short frame with a frame behind it: %+v, want its own 192", n)
	}
	// A frame numbered that many on is no evidence under bounds that state
	// a block size: the frames are counted.
	if n := si.NumberingBy(short, numbered(t, 1+192, 12)); n.SampleCoded || n.ConstBlock != 192 {
		t.Errorf("a short frame and one numbered its size on: %+v, want frame numbers", n)
	}
	wide := numbered(t, 1, 5) // 4608 samples
	if n := si.NumberingLast(wide); n.SampleCoded || n.ConstBlock != 4608 || si.NumberingUnsettled(wide) {
		t.Errorf("a last frame larger than the stated block size: %+v, unsettled %v, want its own 4608, settled", n, si.NumberingUnsettled(wide))
	}
	whole := numbered(t, 3, 12)
	if n := si.Numbering(whole); n != si.NumberingBy(whole, numbered(t, 4, 12)) || n != si.NumberingLast(whole) || n.ConstBlock != 4096 || si.NumberingUnsettled(whole) {
		t.Errorf("a frame of the stated block size: %+v, unsettled %v", n, si.NumberingUnsettled(whole))
	}
	// Bounds STREAMINFO may not state give no block size.
	for _, bad := range []StreamInfo{{MinBlock: 8, MaxBlock: 8}, {MinBlock: 0, MaxBlock: 0}, {MinBlock: 2304, MaxBlock: 4608}} {
		if got := bad.FixedBlock(); got != 0 {
			t.Errorf("bounds %d..%d state a block size of %d", bad.MinBlock, bad.MaxBlock, got)
		}
		if n := bad.NumberingLast(short); n.ConstBlock != 192 || n != bad.Numbering(short) {
			t.Errorf("bounds %d..%d: %+v, want the last frame's own 192", bad.MinBlock, bad.MaxBlock, n)
		}
	}
	if got := si.FixedBlock(); got != 4096 {
		t.Errorf("bounds 4096..4096 state a block size of %d", got)
	}
	// The variable strategy counts samples, whatever the last frame's size.
	variable := short
	variable.Variable = true
	if n := si.NumberingLast(variable); !n.SampleCoded || n.Start(variable) != 1 {
		t.Errorf("a variable-blocksize last frame: %+v", n)
	}
}

// TestNumberingReadsTheFramesWhereTheBoundsAreUnequal: with the blocking
// strategy bit clear, unequal bounds mean sample numbers (the old format),
// whether a stream may state them or not, until the frame behind the first
// says how the stream counts.
func TestNumberingReadsTheFramesWhereTheBoundsAreUnequal(t *testing.T) {
	first := numbered(t, 3, 12)
	frameNext, sampleNext, other := numbered(t, 4, 12), numbered(t, 3+4096, 12), numbered(t, 9, 12)
	for _, tc := range []struct {
		name     string
		si       StreamInfo
		alone    bool // sample-coded by STREAMINFO and the first frame alone
		unsettle bool
	}{
		{"unequal bounds", StreamInfo{MinBlock: 2304, MaxBlock: 4608}, true, true},
		{"a minimum above the maximum", StreamInfo{MinBlock: 4608, MaxBlock: 256}, true, true},
		{"a minimum under 16", StreamInfo{MinBlock: 8, MaxBlock: 4096}, true, true},
		{"equal bounds", StreamInfo{MinBlock: 4096, MaxBlock: 4096}, false, false},
	} {
		if n := tc.si.Numbering(first); n.SampleCoded != tc.alone {
			t.Errorf("%s: sample-coded %v by the first frame alone, want %v", tc.name, n.SampleCoded, tc.alone)
		}
		if got := tc.si.NumberingUnsettled(first); got != tc.unsettle {
			t.Errorf("%s: unsettled %v, want %v", tc.name, got, tc.unsettle)
		}
		if n := tc.si.NumberingBy(first, frameNext); n.SampleCoded || n.ConstBlock != 4096 || n.Start(first) != 3*4096 {
			t.Errorf("%s, the next frame numbered one higher: %+v, want frame numbers of 4096", tc.name, n)
		}
		if n := tc.si.NumberingBy(first, sampleNext); n.SampleCoded != tc.unsettle {
			t.Errorf("%s, the next frame numbered a block higher: sample-coded %v, want %v", tc.name, n.SampleCoded, tc.unsettle)
		}
		if n := tc.si.NumberingBy(first, other); n != tc.si.Numbering(first) {
			t.Errorf("%s, the next frame numbered neither: %+v, want what the first frame alone says", tc.name, n)
		}
	}
	// A frame of the other blocking strategy says nothing of this one's.
	unequal, strayed := StreamInfo{MinBlock: 2304, MaxBlock: 4608}, frameNext
	strayed.Variable = true
	if n := unequal.NumberingBy(first, strayed); !n.SampleCoded {
		t.Error("a variable-blocksize frame behind a fixed one made its numbers frame numbers")
	}
	// The blocking strategy bit set is sample numbers, whatever else.
	variable := first
	variable.Variable = true
	for _, si := range []StreamInfo{{MinBlock: 4096, MaxBlock: 4096}, {MinBlock: 4608, MaxBlock: 256}, {MinBlock: 16, MaxBlock: 65535}} {
		if n := si.NumberingBy(variable, frameNext); !n.SampleCoded || !si.Numbering(variable).SampleCoded || si.NumberingUnsettled(variable) {
			t.Errorf("bounds %d..%d: a variable-blocksize frame is not sample-coded", si.MinBlock, si.MaxBlock)
		}
	}
}

// TestNumbererWritesSampleNumbersUnderUnequalBounds: a copy numbers a
// fixed-strategy frame by its first sample wherever the bounds differ,
// valid or not, as it did.
func TestNumbererWritesSampleNumbersUnderUnequalBounds(t *testing.T) {
	frame := func(coded uint64) []byte {
		h := buildFrameHeader(coded, 12, 9, 1, 4)
		crc := CRC16(h)
		return append(h, byte(crc>>8), byte(crc))
	}
	for _, tc := range []struct {
		si   StreamInfo
		want uint64 // the second frame's number
	}{
		{StreamInfo{MinBlock: 4096, MaxBlock: 4096}, 1},
		{StreamInfo{MinBlock: 2304, MaxBlock: 4608}, 4096},
		{StreamInfo{MinBlock: 4608, MaxBlock: 256}, 4096},
		{StreamInfo{MinBlock: 8, MaxBlock: 4096}, 4096},
	} {
		n := NewNumberer(tc.si, 0, 0)
		if _, _, err := n.Number(frame(7)); err != nil {
			t.Fatal(err)
		}
		out, start, err := n.Number(frame(7))
		if err != nil {
			t.Fatal(err)
		}
		fi, err := ParseFrameHeader(out)
		if err != nil || fi.Coded != tc.want || start != 4096 {
			t.Errorf("bounds %d..%d: the second frame is numbered %d at sample %d (%v), want %d at 4096",
				tc.si.MinBlock, tc.si.MaxBlock, fi.Coded, start, err, tc.want)
		}
	}
}

// TestAOneSampleLastFrameIsPlacedByTheStatedBlockSize: a short last frame of
// one sample read first is settled by what follows it as any short frame is.
func TestAOneSampleLastFrameIsPlacedByTheStatedBlockSize(t *testing.T) {
	b := appendCoded([]byte{0xFF, 0xF8, 6<<4 | 9, 1<<4 | 4<<1}, 7)
	b = append(b, 0) // an 8-bit block size of 1
	one, err := ParseFrameHeader(append(b, crc8(b)))
	if err != nil || one.BlockSize != 1 {
		t.Fatalf("built a header of %d samples (%v), want 1", one.BlockSize, err)
	}
	si := StreamInfo{MinBlock: 4096, MaxBlock: 4096}
	if !si.NumberingUnsettled(one) {
		t.Error("a one-sample frame under 4096-sample bounds is settled by itself")
	}
	if n := si.NumberingLast(one); n.ConstBlock != 4096 || n.Start(one) != 7*4096 {
		t.Errorf("as the last frame: %+v starting at %d, want frame 7 of 4096", n, n.Start(one))
	}
	// Under bounds that state no block size its number one on says nothing.
	if (StreamInfo{MinBlock: 16, MaxBlock: 4096}).NumberingUnsettled(one) {
		t.Error("a one-sample frame under unequal bounds asks for evidence that cannot tell")
	}
}
