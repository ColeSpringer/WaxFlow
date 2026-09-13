package adpcm_test

import (
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/waxerr"
)

// The reference decoders below are written the plain way: one nibble at a
// time, no block abstraction, the tables inline. They exist to check the
// package's block walk (the three channel interleaves and the two nibble
// orders, which is where a layout mix-up hides) against something that
// cannot share its bugs. The arithmetic itself is pinned against ffmpeg by
// the differential tests in tests/.
var refStep = [89]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230,
	253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
	1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327,
	3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442,
	11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

var refIndexAdj = [16]int{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}

func refClip(v int) int { return min(max(v, -32768), 32767) }

// refIMA advances one channel by a nibble. mul selects the WAV layout's
// multiply form over the QuickTime layout's shift-add one.
func refIMA(pred, idx *int, nib byte, mul bool) int {
	step := refStep[*idx]
	var diff int
	if mul {
		diff = (2*int(nib&7) + 1) * step / 8
	} else {
		diff = step / 8
		if nib&4 != 0 {
			diff += step
		}
		if nib&2 != 0 {
			diff += step / 2
		}
		if nib&1 != 0 {
			diff += step / 4
		}
	}
	if nib&8 != 0 {
		diff = -diff
	}
	*pred = refClip(*pred + diff)
	*idx = min(max(*idx+refIndexAdj[nib], 0), 88)
	return *pred
}

// imaWavBlock builds one WAV-layout block and returns it with the samples it
// must decode to, interleaved. A tail too short for a whole round-robin word
// is padding at every channel count, which is the measured reference rule and
// not the format's samples-per-block formula; see TestIMAWavTailIsPadding.
func imaWavBlock(channels, blockAlign int, seed byte) ([]byte, []int32) {
	blk := make([]byte, blockAlign)
	pred := make([]int, channels)
	idx := make([]int, channels)
	out := make([][]int, channels)
	for c := range channels {
		pred[c] = 100*c - 50
		idx[c] = 3 + c
		binary.LittleEndian.PutUint16(blk[4*c:], uint16(int16(pred[c])))
		blk[4*c+2] = byte(idx[c])
		out[c] = append(out[c], pred[c])
	}
	body := blk[4*channels:]
	for i := range body {
		body[i] = seed + byte(i*7)
	}
	word := 4 * channels
	for off := 0; off+word <= len(body); off += word {
		for c := range channels {
			for _, b := range body[off+4*c : off+4*c+4] {
				out[c] = append(out[c], refIMA(&pred[c], &idx[c], b&0x0F, true))
				out[c] = append(out[c], refIMA(&pred[c], &idx[c], b>>4, true))
			}
		}
	}
	return blk, interleave(out)
}

// imaQTBlocks builds n QuickTime blocks per channel and returns them with
// the samples they decode to under each of the two candidate rules: carry,
// where a header that agrees with the running state does not reload it, and
// reload, where every block starts from its header. The headers are written
// from the running state, which is what an encoder does and what makes the
// two rules diverge.
func imaQTBlocks(channels, n int, seed byte) (data []byte, carry, reload []int32) {
	pred := make([]int, channels)
	idx := make([]int, channels)
	for b := range n {
		for c := range channels {
			part := make([]byte, 34)
			binary.BigEndian.PutUint16(part, uint16(int16(pred[c]&^0x7F|idx[c])))
			for i := 2; i < 34; i++ {
				part[i] = seed + byte(b*31+c*13+i*5)
			}
			for _, by := range part[2:] {
				refIMA(&pred[c], &idx[c], by&0x0F, false)
				refIMA(&pred[c], &idx[c], by>>4, false)
			}
			data = append(data, part...)
		}
	}
	return data, refQT(data, channels, true), refQT(data, channels, false)
}

// refQT decodes a run of QuickTime blocks, optionally carrying the predictor
// across a block whose header agrees with the running state.
func refQT(data []byte, channels int, carry bool) []int32 {
	pred := make([]int, channels)
	idx := make([]int, channels)
	out := make([][]int, channels)
	for off := 0; off+34*channels <= len(data); off += 34 * channels {
		for c := range channels {
			part := data[off+34*c:]
			v := int(int16(binary.BigEndian.Uint16(part)))
			hdrIdx, hdrPred := v&0x7F, v&^0x7F
			if !carry || hdrIdx != idx[c] || abs(pred[c]-hdrPred) > 127 {
				pred[c], idx[c] = hdrPred, hdrIdx
			}
			for _, by := range part[2:34] {
				out[c] = append(out[c], refIMA(&pred[c], &idx[c], by&0x0F, false))
				out[c] = append(out[c], refIMA(&pred[c], &idx[c], by>>4, false))
			}
		}
	}
	return interleave(out)
}

// msBlock builds one Microsoft ADPCM block and the samples it decodes to.
// seed is the repeating nibble pattern its body carries, and it is chosen
// rather than arbitrary: the step size grows by up to 3x per nibble with
// nothing in the format to bound it, so an arbitrary body runs it past any
// fixed width within a few dozen samples. What a decoder does there is
// TestMSRunawayStep's business; this one checks the block walk on a body
// whose step stays where a real encoder would keep it.
func msBlock(channels, blockAlign int, coefs [7][2]int16, seed []byte) ([]byte, []int32) {
	adapt := [16]int{230, 230, 230, 230, 307, 409, 512, 614, 768, 614, 512, 409, 307, 230, 230, 230}
	blk := make([]byte, blockAlign)
	s1 := make([]int, channels)
	s2 := make([]int, channels)
	delta := make([]int, channels)
	c1 := make([]int, channels)
	c2 := make([]int, channels)
	out := make([][]int, channels)
	for c := range channels {
		pi := (c + 3) % 7
		blk[c] = byte(pi)
		c1[c], c2[c] = int(coefs[pi][0]), int(coefs[pi][1])
		delta[c] = 200 + 37*c
		s1[c] = 1000 - 500*c
		s2[c] = -700 + 300*c
		binary.LittleEndian.PutUint16(blk[channels+2*c:], uint16(int16(delta[c])))
		binary.LittleEndian.PutUint16(blk[3*channels+2*c:], uint16(int16(s1[c])))
		binary.LittleEndian.PutUint16(blk[5*channels+2*c:], uint16(int16(s2[c])))
		out[c] = append(out[c], s2[c], s1[c])
	}
	body := blk[7*channels:]
	for i := range body {
		body[i] = seed[2*i%len(seed)]<<4 | seed[(2*i+1)%len(seed)]
	}
	c := 0
	for _, b := range body {
		for _, nib := range [2]byte{b >> 4, b & 0x0F} {
			pv := s1[c]*c1[c] + s2[c]*c2[c]
			// Truncating division toward zero, as the specification writes it.
			q := pv / 256
			if pv < 0 && pv%256 != 0 {
				q = -((-pv) / 256)
			}
			v := refClip(q + delta[c]*int(int8(nib<<4)>>4))
			s2[c], s1[c] = s1[c], v
			out[c] = append(out[c], v)
			d := adapt[nib] * delta[c] / 256
			delta[c] = max(d, 16)
			c = (c + 1) % channels
		}
	}
	return blk, interleave(out)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func interleave(chans [][]int) []int32 {
	var out []int32
	for i := range chans[0] {
		for c := range chans {
			out = append(out, int32(chans[c][i]))
		}
	}
	return out
}

// decode runs a packet through a fresh decoder and returns interleaved
// output, checking the emit contract on the way.
func decode(t *testing.T, cfg adpcm.Config, pkt []byte) []int32 {
	t.Helper()
	f := cfg.Format(44100, audio.DefaultLayout(cfg.Channels))
	dec, err := adpcm.NewDecoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	var out []int32
	err = dec.Decode(pkt, func(b *audio.Buffer) error {
		if b.N > audio.StandardChunk {
			t.Errorf("emitted %d frames, more than the %d standard chunk", b.N, audio.StandardChunk)
		}
		for i := range b.N {
			for c := range cfg.Channels {
				out = append(out, b.ChanI(c)[i])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compare(t *testing.T, got, want []int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("decoded %d samples, reference decodes %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("interleaved sample %d = %d, reference says %d", i, got[i], want[i])
		}
	}
}

func TestIMAWavAgainstReference(t *testing.T) {
	for _, tt := range []struct{ channels, blockAlign int }{
		{1, 256}, {2, 1024}, {1, 8}, {2, 16}, {1, 255}, {4, 512},
	} {
		cfg := adpcm.Config{Layout: adpcm.IMAWav, Channels: tt.channels, BlockAlign: tt.blockAlign}
		cfg.SamplesPerBlock = cfg.BlockFrames()
		blk, want := imaWavBlock(tt.channels, tt.blockAlign, 0x5A)
		t.Run(cfg.Layout.String(), func(t *testing.T) {
			compare(t, decode(t, cfg, blk), want)
		})
	}
}

func TestIMAQuickTimeAgainstReference(t *testing.T) {
	for _, channels := range []int{1, 2} {
		cfg := adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: channels,
			BlockAlign: 34 * channels, SamplesPerBlock: 64}
		data, carry, _ := imaQTBlocks(channels, 4, 0x33)
		compare(t, decode(t, cfg, data), carry)
	}
}

// TestIMAQuickTimeCarriesState pins the rule that makes an 'ima4' decode
// match the reference: a block header whose step index and truncated
// predictor agree with the running state does not reload it, so the decode
// keeps the seven bits the header cannot hold. The reload reading is decoded
// alongside and must differ, so the test cannot pass by the two rules
// happening to agree on this data.
//
// This is also why a seek into one of these streams cannot start at a block:
// the state a block inherits is not recoverable from its own header. The
// demuxers restart the decode from the file's first block instead.
func TestIMAQuickTimeCarriesState(t *testing.T) {
	cfg := adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: 1, BlockAlign: 34, SamplesPerBlock: 64}
	data, carry, reload := imaQTBlocks(1, 6, 0x71)
	if len(carry) != len(reload) {
		t.Fatalf("the two readings decoded %d and %d samples", len(carry), len(reload))
	}
	same := true
	for i := range carry {
		if carry[i] != reload[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("the carry and reload readings agree on this data; the test would pass either way")
	}
	compare(t, decode(t, cfg, data), carry)
}

// msNibbles is a repeating nibble pattern whose step size neither runs away
// nor pins to its floor over a block: it stays between 110 and 200, which is
// where a real encoder's does.
var msNibbles = []byte{15, 1, 13, 1, 3, 4, 14, 1, 12, 1, 11, 15, 12}

func TestMSAgainstReference(t *testing.T) {
	coefs := adpcm.DefaultCoefs
	for _, tt := range []struct{ channels, blockAlign int }{{1, 256}, {2, 1024}, {1, 8}, {2, 15}} {
		cfg := adpcm.Config{Layout: adpcm.MS, Channels: tt.channels, BlockAlign: tt.blockAlign, Coefs: coefs}
		cfg.SamplesPerBlock = cfg.BlockFrames()
		blk, want := msBlock(tt.channels, tt.blockAlign, coefs, msNibbles)
		compare(t, decode(t, cfg, blk), want)
	}
}

// TestMSHonoursTheFilesTable pins decision 13: the coefficients come from
// the file. A stream with a table of its own decodes differently from the
// same bytes read with the default one, which is what readers that ignore
// the field do.
func TestMSHonoursTheFilesTable(t *testing.T) {
	odd := adpcm.DefaultCoefs
	odd[3] = [2]int16{300, -100}
	cfg := adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 64, Coefs: odd}
	cfg.SamplesPerBlock = cfg.BlockFrames()
	blk, want := msBlock(1, 64, odd, msNibbles)
	compare(t, decode(t, cfg, blk), want)

	std := cfg
	std.Coefs = adpcm.DefaultCoefs
	if got := decode(t, std, blk); got[2] == want[2] {
		t.Error("the default table decoded the same first sample as the file's; the table is not being read")
	}
}

func TestMalformedBlocks(t *testing.T) {
	imaWav := adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 64}
	imaWav.SamplesPerBlock = imaWav.BlockFrames()
	qt := adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: 1, BlockAlign: 34, SamplesPerBlock: 64}
	ms := adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 64, Coefs: adpcm.DefaultCoefs}
	ms.SamplesPerBlock = ms.BlockFrames()

	badIMA := make([]byte, 64)
	badIMA[2] = 89 // one past the last step index
	badQT := make([]byte, 34)
	badQT[0], badQT[1] = 0x00, 0x59 // step index 89 in the low seven bits
	badMS := make([]byte, 64)
	badMS[0] = 7 // one past the last coefficient pair

	for _, tt := range []struct {
		name string
		cfg  adpcm.Config
		pkt  []byte
	}{
		{"IMA WAV step index", imaWav, badIMA},
		{"QuickTime step index", qt, badQT},
		{"MS predictor index", ms, badMS},
		{"ragged packet", imaWav, make([]byte, 63)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dec, err := adpcm.NewDecoder(tt.cfg, tt.cfg.Format(8000, audio.DefaultLayout(1)))
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Release()
			err = dec.Decode(tt.pkt, func(*audio.Buffer) error { return nil })
			if waxerr.CodeOf(err) != waxerr.CodeMalformedInput {
				t.Errorf("err = %v (code %v), want malformed", err, waxerr.CodeOf(err))
			}
		})
	}
}

// TestConfigRoundTrip pins the marshaled blob, which is a cache key.
func TestConfigRoundTrip(t *testing.T) {
	cases := []adpcm.Config{
		{Layout: adpcm.IMAWav, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1017},
		{Layout: adpcm.IMAQuickTime, Channels: 1, BlockAlign: 34, SamplesPerBlock: 64},
		{Layout: adpcm.MS, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1012, Coefs: adpcm.DefaultCoefs},
	}
	for _, cfg := range cases {
		t.Run(cfg.Layout.String(), func(t *testing.T) {
			b, err := cfg.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			back, err := adpcm.ParseConfig(b)
			if err != nil {
				t.Fatal(err)
			}
			if back != cfg {
				t.Errorf("round trip gave %+v, want %+v", back, cfg)
			}
			// An IMA blob must not carry the MS table's 28 bytes.
			if want := 9; cfg.Layout != adpcm.MS && len(b) != want {
				t.Errorf("blob of %d bytes for %v, want %d", len(b), cfg.Layout, want)
			}
		})
	}
	for _, bad := range [][]byte{
		nil,
		{1},
		{2, 0, 1, 34, 0, 64, 0, 0, 0}, // a version this package did not write
		{1, 2, 1, 34, 0, 64, 0, 0, 0}, // an unknown layout
		{1, 0, 1, 34, 0, 64, 0},       // the pre-widening length
	} {
		if _, err := adpcm.ParseConfig(bad); err == nil {
			t.Errorf("ParseConfig(%v) accepted a blob this package did not write", bad)
		}
	}
}

// TestGeometryRefusals pins the shapes Validate rejects, which is what keeps
// the decoder's loops from having to check them.
func TestGeometryRefusals(t *testing.T) {
	cases := []struct {
		name string
		cfg  adpcm.Config
	}{
		{"unknown layout", adpcm.Config{Layout: adpcm.Layout(9), Channels: 1, BlockAlign: 34, SamplesPerBlock: 64}},
		{"no channels", adpcm.Config{Layout: adpcm.IMAWav, Channels: 0, BlockAlign: 64, SamplesPerBlock: 121}},
		{"MS beyond stereo", adpcm.Config{Layout: adpcm.MS, Channels: 3, BlockAlign: 64, SamplesPerBlock: 30}},
		{"QuickTime block size", adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: 1, BlockAlign: 68, SamplesPerBlock: 64}},
		{"block holds no header", adpcm.Config{Layout: adpcm.IMAWav, Channels: 2, BlockAlign: 4, SamplesPerBlock: 1}},
		{"samples disagree", adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 64, SamplesPerBlock: 100}},
		{"IMA with a table", adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 64, SamplesPerBlock: 121, Coefs: adpcm.DefaultCoefs}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Error("Validate accepted a geometry this package cannot decode")
			}
		})
	}
}

// TestMSRunawayStep pins what happens where the specification stops: a block
// whose nibbles all drive the predictor one way runs the step size up by 3x
// apiece, past any width a header could state it in and past any sample it
// could still change.
//
// The whole block must then sit on the domain floor. That is the sharp part:
// an implementation computing this in the 16-bit samples' own width wraps
// instead, and a wrap shows up as a sample at the opposite rail, so the
// assertion is "every one of them, exactly" rather than "bounded".
func TestMSRunawayStep(t *testing.T) {
	cfg := adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 512, Coefs: adpcm.DefaultCoefs}
	cfg.SamplesPerBlock = cfg.BlockFrames()
	blk := make([]byte, cfg.BlockAlign)
	binary.LittleEndian.PutUint16(blk[1:], uint16(int16(32767))) // the largest step a header can state
	binary.LittleEndian.PutUint16(blk[3:], uint16(int16(32767))) // sample1
	binary.LittleEndian.PutUint16(blk[5:], uint16(0x8000))       // sample2, the domain floor
	body := blk[7:]
	for i := range body {
		body[i] = 0x88 // two -0 nibbles, the table's largest growth
	}
	got := decode(t, cfg, blk)
	if len(got) != cfg.SamplesPerBlock {
		t.Fatalf("decoded %d samples, want %d", len(got), cfg.SamplesPerBlock)
	}
	// The first two are the header's own samples; every nibble after them
	// subtracts a step that only grows.
	for i, v := range got[2:] {
		if v != -32768 {
			t.Fatalf("sample %d = %d, want the domain floor", i+2, v)
		}
	}
}

// TestIMAWavTailIsPadding pins the geometry against the reference rather than
// against the format's formula, at the block aligns where the two disagree.
//
// wSamplesPerBlock is defined as (blockAlign - 4*ch)*2/ch + 1, which counts a
// tail too short for a whole round-robin word. ffmpeg 8.0.1 does not: it
// decodes 9 frames from a 10-byte mono block where the formula says 13, and
// 1009 from a 1020-byte stereo block where it says 1013. Every block align an
// encoder writes divides evenly, so nothing in the fixture corpus can see the
// difference, and a channel count that took the formula's answer would have
// diverged from the reference at the first sample past the last whole word.
func TestIMAWavTailIsPadding(t *testing.T) {
	for _, tt := range []struct {
		channels, blockAlign, want int
	}{
		{1, 10, 9},      // the formula says 13
		{2, 1020, 1009}, // the formula says 1013
		{1, 255, 497},   // the formula says 503
		{1, 1024, 2041}, // word-aligned: the two agree
		{2, 1024, 1017},
	} {
		cfg := adpcm.Config{Layout: adpcm.IMAWav, Channels: tt.channels, BlockAlign: tt.blockAlign}
		if got := cfg.BlockFrames(); got != tt.want {
			t.Errorf("%d channels of %d bytes hold %d frames, want %d",
				tt.channels, tt.blockAlign, got, tt.want)
		}
	}
}

// TestBlockFramesSurvivesNoChannels pins that the derivation does not trap on
// a channel count its callers are supposed to have rejected. It is the shared
// geometry for three containers, and a panic there is reachable from any
// caller that validates in a different order.
func TestBlockFramesSurvivesNoChannels(t *testing.T) {
	for _, layout := range []adpcm.Layout{adpcm.IMAWav, adpcm.IMAQuickTime, adpcm.MS} {
		cfg := adpcm.Config{Layout: layout, Channels: 0, BlockAlign: 1024}
		if got := cfg.BlockFrames(); got != 0 {
			t.Errorf("%v with no channels derives %d frames, want 0", layout, got)
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%v with no channels validated", layout)
		}
	}
}

// TestLargeBlockRoundTrips pins the widened samples-per-block field. A block
// align is a uint16 in every container that states one, and at the top of
// that range a mono IMA block decodes to more frames than a uint16 can hold;
// storing it in one made ParseConfig reject a config Validate had accepted,
// which surfaced as a decoder refusal on a track probe called healthy.
func TestLargeBlockRoundTrips(t *testing.T) {
	for _, tt := range []struct{ channels, blockAlign int }{{1, 40000}, {1, 65535}} {
		cfg := adpcm.Config{Layout: adpcm.IMAWav, Channels: tt.channels, BlockAlign: tt.blockAlign}
		cfg.SamplesPerBlock = cfg.BlockFrames()
		if cfg.SamplesPerBlock <= 65535 {
			t.Fatalf("%d channels of %d bytes hold %d frames, which a uint16 could have held",
				tt.channels, tt.blockAlign, cfg.SamplesPerBlock)
		}
		b, err := cfg.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary: %v", err)
		}
		back, err := adpcm.ParseConfig(b)
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		if back != cfg {
			t.Errorf("round trip gave %+v, want %+v", back, cfg)
		}
	}
}

// TestMSPredictionTruncatesTowardZero pins the direction of the division the
// specification writes, at the only values where it differs from a shift: a
// negative prediction whose quotient is not exact. The two agree on every
// fixture in this tree, so nothing else can tell them apart, and the
// expression is written branchlessly, which is exactly the kind of rewrite a
// test that never reaches a negative value would wave through.
//
// The table is chosen, not conventional: coefficient pair {1, 0} makes the
// prediction the previous sample itself, so a -1 becomes the whole
// numerator and the two roundings give 0 and -1.
func TestMSPredictionTruncatesTowardZero(t *testing.T) {
	coefs := adpcm.DefaultCoefs
	coefs[0] = [2]int16{1, 0}
	cfg := adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 16, Coefs: coefs}
	cfg.SamplesPerBlock = cfg.BlockFrames()

	blk := make([]byte, cfg.BlockAlign)
	blk[0] = 0                                                // the {1, 0} pair
	binary.LittleEndian.PutUint16(blk[1:], uint16(int16(16))) // the smallest step
	binary.LittleEndian.PutUint16(blk[3:], uint16(0xFFFF))    // sample1 = -1
	binary.LittleEndian.PutUint16(blk[5:], uint16(int16(0)))  // sample2
	for i := 7; i < len(blk); i++ {
		blk[i] = 0x00 // nibble 0: the step contributes nothing
	}
	got := decode(t, cfg, blk)
	// got[0] and got[1] are the header's own samples; the first coded one is
	// the prediction alone.
	if got[2] != 0 {
		t.Errorf("a prediction of -1/256 gave %d, want 0: the division floors instead of truncating", got[2])
	}
}
