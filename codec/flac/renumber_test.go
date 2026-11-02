package flac_test

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// encodedFrames encodes a looped sine with the frames numbered from first and
// returns each frame.
func encodedFrames(t testing.TB, first int64) [][]byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	src := testutil.Sine(f, 30*4096+1000, 440, 0.5)
	defer audio.Put(src)
	enc, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: 5, FirstFrame: first})
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	emit := func(p codec.Packet) error {
		frames = append(frames, bytes.Clone(p.Data))
		return nil
	}
	chunk := audio.Get(f, enc.FrameSize())
	defer audio.Put(chunk)
	for off := 0; off < src.N; off += enc.FrameSize() {
		n := min(enc.FrameSize(), src.N-off)
		audio.CopyFrames(chunk, 0, src, off, n)
		chunk.N = n
		if err := enc.Encode(chunk, emit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := enc.Finish(emit); err != nil {
		t.Fatal(err)
	}
	return frames
}

// requireRenumbered holds RenumberFrame's output for from, renumbered to coded,
// to want byte for byte, appended after a prefix that must survive.
func requireRenumbered(t *testing.T, from []byte, coded uint64, want []byte) {
	t.Helper()
	prefix := []byte("prefix")
	got, err := flac.RenumberFrame(bytes.Clone(prefix), from, coded)
	if err != nil {
		t.Fatalf("renumbering to %d: %v", coded, err)
	}
	if !bytes.HasPrefix(got, prefix) {
		t.Fatalf("renumbering to %d lost what dst held", coded)
	}
	if got = got[len(prefix):]; !bytes.Equal(got, want) {
		t.Fatalf("renumbered to %d: % x..., want % x...", coded, got[:min(len(got), 12)], want[:min(len(want), 12)])
	}
}

// TestRenumberFrameFixed: a fixed-blocksize frame renumbered is the frame the
// encoder writes under that number, byte for byte, across coded numbers one,
// two, three and six bytes long, in both directions.
func TestRenumberFrameFixed(t *testing.T) {
	zero := encodedFrames(t, 0)
	for _, first := range []int64{100, 2000, 1<<31 - 40} {
		moved := encodedFrames(t, first)
		if len(moved) != len(zero) {
			t.Fatalf("%d frames from %d, %d from 0", len(moved), first, len(zero))
		}
		for i := range zero {
			requireRenumbered(t, moved[i], uint64(i), zero[i])
			requireRenumbered(t, zero[i], uint64(first)+uint64(i), moved[i])
		}
	}
}

// TestRenumberFrameVariable: a variable-blocksize frame takes its first
// sample's number, up to the seven-byte form, and renumbered is the frame
// built with that number independently, byte for byte.
func TestRenumberFrameVariable(t *testing.T) {
	zero := encodedFrames(t, 0)
	for _, from := range []uint64{0, 1000, 1 << 35} {
		for i, frame := range zero {
			sample := uint64(i) * 4096
			at := testutil.SetFLACFrameNumber(t, frame, true, from+sample)
			fi, err := flac.ParseFrameHeader(at)
			if err != nil || !fi.Variable || fi.Coded != from+sample {
				t.Fatalf("the variable frame does not read as sample %d: %+v, %v", from+sample, fi, err)
			}
			requireRenumbered(t, at, sample, testutil.SetFLACFrameNumber(t, frame, true, sample))
			requireRenumbered(t, testutil.SetFLACFrameNumber(t, frame, true, sample), from+sample, at)
		}
	}
}

// TestRenumberFrameCodedWidths: every coded width, at both edges, reads back
// as the number written, and the frame grows or shrinks by the width change.
func TestRenumberFrameCodedWidths(t *testing.T) {
	frame := testutil.SetFLACFrameNumber(t, encodedFrames(t, 0)[1], true, 0)
	for _, tc := range []struct {
		coded uint64
		width int
	}{
		{0, 1}, {0x7F, 1}, {0x80, 2}, {1<<11 - 1, 2}, {1 << 11, 3}, {1<<16 - 1, 3}, {1 << 16, 4},
		{1<<21 - 1, 4}, {1 << 21, 5}, {1<<26 - 1, 5}, {1 << 26, 6}, {1<<31 - 1, 6}, {1 << 31, 7}, {1<<36 - 1, 7},
	} {
		out, err := flac.RenumberFrame(nil, frame, tc.coded)
		if err != nil {
			t.Fatalf("renumbering to %d: %v", tc.coded, err)
		}
		fi, err := flac.ParseFrameHeader(out)
		if err != nil || fi.Coded != tc.coded {
			t.Fatalf("renumbered to %d, reads %d (%v)", tc.coded, fi.Coded, err)
		}
		if got := len(out) - len(frame) + 1; got != tc.width {
			t.Errorf("%d is coded in %d bytes, want %d", tc.coded, got, tc.width)
		}
	}
}

// TestRenumberFrameDecodes: the audio is untouched, so a renumbered frame
// decodes as the original does.
func TestRenumberFrameDecodes(t *testing.T) {
	frames := encodedFrames(t, 5000)
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	si := flac.StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: 44100, Channels: 2, Bits: 16}
	decode := func(frame []byte) [][]int32 {
		t.Helper()
		dec, err := flac.NewDecoder(si, f)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()
		var chans [][]int32
		if err := dec.Decode(frame, func(b *audio.Buffer) error {
			for c := range f.Channels {
				chans = append(chans, append([]int32(nil), b.ChanI(c)...))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return chans
	}
	for i, frame := range frames {
		out, err := flac.RenumberFrame(nil, frame, uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		want, got := decode(frame), decode(out)
		for c := range want {
			if testutil.DiffI32(got[c], want[c]) >= 0 {
				t.Fatalf("frame %d channel %d decodes differently renumbered", i, c)
			}
		}
	}
}

// TestRenumberFrameRefuses: a frame that does not parse, or whose checksum
// does not check, is refused with an error and dst is left as it was;
// recomputing a damaged frame's CRC-16 would hide the damage. So is a packet
// of two frames, whose CRC-16 checks as a whole, and a number past the coded
// form's 36 bits.
func TestRenumberFrameRefuses(t *testing.T) {
	frames := encodedFrames(t, 0)
	frame := frames[3]
	damaged := func(at int) []byte {
		b := bytes.Clone(frame)
		b[at] ^= 0x10
		return b
	}
	for _, tc := range []struct {
		name  string
		frame []byte
		coded uint64
		code  waxerr.Code
	}{
		{"empty", nil, 0, waxerr.CodeMalformedInput},
		{"a header cut short", frame[:4], 0, waxerr.CodeMalformedInput},
		{"no sync", damaged(0), 0, waxerr.CodeMalformedInput},
		{"a header CRC-8 that fails", damaged(5), 0, waxerr.CodeMalformedInput},
		{"a payload that fails the CRC-16", damaged(len(frame) / 2), 0, waxerr.CodeMalformedInput},
		{"a CRC-16 that fails", damaged(len(frame) - 1), 0, waxerr.CodeMalformedInput},
		{"no CRC-16", frame[:6], 0, waxerr.CodeMalformedInput},
		{"a frame cut short", frame[:len(frame)-1], 0, waxerr.CodeMalformedInput},
		{"two frames glued", append(bytes.Clone(frames[1]), frames[2]...), 0, waxerr.CodeMalformedInput},
		{"a number past 36 bits", frame, 1 << 36, waxerr.CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := []byte("prefix")
			got, err := flac.RenumberFrame(dst, tc.frame, tc.coded)
			if err == nil {
				t.Fatal("renumbered")
			}
			if code := waxerr.CodeOf(err); code != tc.code {
				t.Errorf("error %v has code %q, want %q", err, code, tc.code)
			}
			if !bytes.Equal(got, dst) {
				t.Errorf("dst became %q", got)
			}
		})
	}
}

// testStreamInfo is the STREAMINFO of encodedFrames' stream.
var testStreamInfo = flac.StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: 44100, Channels: 2, Bits: 16}

// TestNumbererFromZero: a Numberer from sample 0 hands back frames numbered
// from 0 that are the encoder's own frames from 0, byte for byte, with where
// each starts, and passes a frame already numbered so through untouched.
func TestNumbererFromZero(t *testing.T) {
	zero, moved := encodedFrames(t, 0), encodedFrames(t, 1000)
	n := flac.NewNumberer(testStreamInfo, 0, 0)
	for i, frame := range moved {
		got, start, err := n.Number(frame)
		if err != nil {
			t.Fatal(err)
		}
		if start != int64(i)*4096 || !bytes.Equal(got, zero[i]) {
			t.Fatalf("frame %d numbered to start at %d: % x..., want %d", i, start, got[:min(len(got), 8)], i*4096)
		}
	}
	n = flac.NewNumberer(testStreamInfo, 0, 0)
	for i, frame := range zero {
		got, _, err := n.Number(frame)
		if err != nil {
			t.Fatal(err)
		}
		if &got[0] != &frame[0] {
			t.Fatalf("frame %d, already numbered, was rewritten", i)
		}
	}
}

// TestNumbererFromAPosition: a Numberer from a position past 0 numbers the
// frames it is handed where they are in the stream, as a run restarted
// mid-stream must, including a short final frame alone, whose number comes
// from the stream's block size rather than its own, and whatever block
// bounds STREAMINFO states.
func TestNumbererFromAPosition(t *testing.T) {
	zero, moved := encodedFrames(t, 0), encodedFrames(t, 1000)
	misstated := testStreamInfo
	misstated.MinBlock, misstated.MaxBlock = 1152, 1152
	for _, si := range []flac.StreamInfo{testStreamInfo, misstated} {
		for _, from := range []int{5, len(zero) - 1} {
			n := flac.NewNumberer(si, int64(from)*4096, 4096)
			for i := from; i < len(moved); i++ {
				got, start, err := n.Number(moved[i])
				if err != nil {
					t.Fatal(err)
				}
				if start != int64(i)*4096 || !bytes.Equal(got, zero[i]) {
					t.Fatalf("from frame %d: frame %d starts at %d and is not the encoder's", from, i, start)
				}
			}
		}
	}
	if _, _, err := flac.NewNumberer(testStreamInfo, 1000, 4096).Number(moved[0]); err == nil {
		t.Error("numbered a fixed-blocksize frame at sample 1000, between frames")
	}
	if _, _, err := flac.NewNumberer(testStreamInfo, 5*4096, 0).Number(moved[5]); err == nil {
		t.Error("numbered a fixed-blocksize frame past 0 with no block size to number it by")
	}
}

// TestNumbererVariable: under the variable blocking strategy each frame takes
// the number of its first sample, from wherever the Numberer starts.
func TestNumbererVariable(t *testing.T) {
	frames := encodedFrames(t, 0)
	for _, from := range []int{0, 7} {
		n := flac.NewNumberer(testStreamInfo, int64(from)*4096, 0)
		for i := from; i < len(frames); i++ {
			sample := uint64(i) * 4096
			got, start, err := n.Number(testutil.SetFLACFrameNumber(t, frames[i], true, sample+1<<33))
			if err != nil {
				t.Fatal(err)
			}
			if want := testutil.SetFLACFrameNumber(t, frames[i], true, sample); start != int64(sample) || !bytes.Equal(got, want) {
				t.Fatalf("from frame %d: frame %d starts at %d and is not numbered %d", from, i, start, sample)
			}
		}
	}
}

// TestClearMD5: ClearMD5 zeroes the signature of a copy of a STREAMINFO block
// and nothing else, and refuses what is not one.
func TestClearMD5(t *testing.T) {
	si := testStreamInfo
	si.Samples = 123456
	for i := range si.MD5 {
		si.MD5[i] = byte(i + 1)
	}
	raw, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	in := bytes.Clone(raw)
	out, err := flac.ClearMD5(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, in) {
		t.Error("ClearMD5 wrote to its input")
	}
	got, err := flac.ParseStreamInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	want := si
	want.MD5 = [16]byte{}
	if got != want {
		t.Errorf("ClearMD5 left %+v, want %+v", got, want)
	}
	if _, err := flac.ClearMD5(raw[:20]); err == nil {
		t.Error("ClearMD5 accepted 20 bytes")
	}
}

// TestRestateStreamInfo: RestateStreamInfo states a total and no MD5 in a copy
// of a STREAMINFO block and leaves the rest byte for byte, bounds no header
// writer states included; a total past 36 bits is stated as unknown.
func TestRestateStreamInfo(t *testing.T) {
	si := testStreamInfo
	si.Samples = 123456
	for i := range si.MD5 {
		si.MD5[i] = byte(i + 1)
	}
	raw, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	raw[0], raw[1] = 0, 8 // a minimum block size MarshalBinary refuses
	in := bytes.Clone(raw)
	for _, tc := range []struct{ samples, want int64 }{
		{99, 99}, {1<<36 - 1, 1<<36 - 1}, {1 << 36, 0}, {-1, 0},
	} {
		out, err := flac.RestateStreamInfo(raw, tc.samples)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, in) {
			t.Fatal("RestateStreamInfo wrote to its input")
		}
		got, err := flac.ParseStreamInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		want := si
		want.MinBlock, want.Samples, want.MD5 = 8, tc.want, [16]byte{}
		if got != want {
			t.Errorf("restated to %d: %+v, want %+v", tc.samples, got, want)
		}
	}
	if _, err := flac.RestateStreamInfo(raw[:20], 1); err == nil {
		t.Error("RestateStreamInfo accepted 20 bytes")
	}
}
