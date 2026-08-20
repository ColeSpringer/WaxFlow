package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// muxAAC encodes n samples of a deterministic stereo signal into a
// fragmented MP4 via w, declaring declaredLen as the track length.
func muxAAC(t *testing.T, w io.Writer, n int, declaredLen int64) (src [][]float32, trailer codec.Trailer) {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	enc, err := aac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	src = make([][]float32, 2)
	for c := range src {
		src[c] = make([]float32, n)
		for i := range src[c] {
			ti := float64(i) / 44100
			src[c][i] = float32(0.3*math.Sin(2*math.Pi*440*ti) +
				0.1*math.Sin(2*math.Pi*1870*ti+float64(c)))
		}
	}
	m := NewMuxer(w, nil)
	track := container.Track{Codec: codec.AACLC, CodecConfig: enc.CodecConfig(),
		Fmt: f, Samples: declaredLen, Delay: int64(enc.Delay()), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	buf := audio.Get(f, 1024)
	defer audio.Put(buf)
	for off := 0; off < n; off += 1024 {
		end := min(off+1024, n)
		buf.N = end - off
		for c := 0; c < 2; c++ {
			copy(buf.ChanF(c), src[c][off:end])
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatal(err)
		}
	}
	trailer, err = enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}
	return src, trailer
}

// elstEntry extracts the single edit-list entry (duration, mediaTime)
// from a produced stream.
func elstEntry(t *testing.T, data []byte) (duration, mediaTime int64) {
	t.Helper()
	i := bytes.Index(data, []byte("elst"))
	if i < 0 {
		t.Fatal("no elst box in output")
	}
	return int64(binary.BigEndian.Uint64(data[i+12:])),
		int64(binary.BigEndian.Uint64(data[i+20:]))
}

// TestMuxAACGaplessKnownLength checks the streaming path with a known
// length: the init header's edit list carries the delay AND the exact
// duration (the CMAF gapless convention Apple's stack honors). ffmpeg's
// CLI decode applies the start trim but not the tail edit on fragmented
// input, so the tail assertion here is on the signaling bytes, and the
// decode assertion covers the exactly-trimmed front.
func TestMuxAACGaplessKnownLength(t *testing.T) {
	const n = 44100 + 123
	var out bytes.Buffer
	src, tr := muxAAC(t, &out, n, n)

	dur, mediaTime := elstEntry(t, out.Bytes())
	if mediaTime != aac.EncoderDelay || dur != n {
		t.Fatalf("elst (dur %d, mediaTime %d), want (%d, %d)", dur, mediaTime, n, aac.EncoderDelay)
	}
	if tr.Samples != n {
		t.Fatalf("trailer samples %d", tr.Samples)
	}

	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "enc.m4a")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	want := int(tr.Samples+tr.Padding) * 2 // front-trimmed, tail padding present
	if len(got) != want {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want %d (front trim honored)", len(got), want)
	}
	var sig, errE float64
	for i := 0; i < n; i++ {
		for c := 0; c < 2; c++ {
			s := float64(src[c][i])
			e := s - float64(got[i*2+c])
			sig += s * s
			errE += e * e
		}
	}
	snr := 10 * math.Log10(sig/errE)
	t.Logf("fMP4 gapless ffmpeg SNR %.1f dB", snr)
	if snr < 15 {
		t.Fatalf("SNR %.1f dB below 15", snr)
	}
}

// TestMuxAACEndPatchesUnknownLength checks the seekable path: Begin
// writes an edit list with delay and unknown duration, and End patches
// the exact length from the trailer.
func TestMuxAACEndPatchesUnknownLength(t *testing.T) {
	const n = 3*44100/2 + 41
	path := filepath.Join(t.TempDir(), "enc.m4a")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, tr := muxAAC(t, fh, n, -1)
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dur, mediaTime := elstEntry(t, data)
	if mediaTime != aac.EncoderDelay || dur != n {
		t.Fatalf("patched elst (dur %d, mediaTime %d), want (%d, %d)", dur, mediaTime, n, aac.EncoderDelay)
	}
	if tr.Samples != n {
		t.Fatalf("trailer samples %d", tr.Samples)
	}
	testutil.FFmpeg(t)
	got := testutil.FFmpegDecodeF32(t, path)
	want := int(tr.Samples+tr.Padding) * 2
	if len(got) != want {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want %d", len(got), want)
	}
}

// TestMuxAACUnknownLengthStreaming checks the pure-stream fallback: no
// seeking, unknown length, so the edit list carries the delay alone
// (duration 0 reads as "to end of movie") and only the front trims.
func TestMuxAACUnknownLengthStreaming(t *testing.T) {
	const n = 44100
	var out bytes.Buffer
	_, tr := muxAAC(t, &out, n, -1)
	dur, mediaTime := elstEntry(t, out.Bytes())
	if mediaTime != aac.EncoderDelay || dur != 0 {
		t.Fatalf("elst (dur %d, mediaTime %d), want (0, %d)", dur, mediaTime, aac.EncoderDelay)
	}
	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "enc.m4a")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	// Delay-only: the decode covers everything past the priming,
	// including the tail padding.
	want := int(tr.Samples+tr.Padding) * 2
	if len(got) != want {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want %d (delay-only trim)", len(got), want)
	}
}

// TestMuxAACRejects pins the validation: a wrong-format track and a
// truncated ASC must fail Begin.
func TestMuxAACRejects(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2),
		Type: audio.Float, BitDepth: 32}
	enc, err := aac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	wrong := f
	wrong.Rate = 48000
	if err := NewMuxer(&out, nil).Begin([]container.Track{{
		Codec: codec.AACLC, CodecConfig: enc.CodecConfig(), Fmt: wrong}}); err == nil {
		t.Fatal("rate-mismatched track accepted")
	}
	if err := NewMuxer(&out, nil).Begin([]container.Track{{
		Codec: codec.AACLC, CodecConfig: []byte{0x11}, Fmt: f}}); err == nil {
		t.Fatal("truncated ASC accepted")
	}
}

// TestElstDurOffset pins the back-patch offset constant against the
// builder's actual layout: the 64-bit duration must sit exactly
// elstDurOffset bytes into the blob elstBox returns.
func TestElstDurOffset(t *testing.T) {
	const mediaTime, duration = 1024, 0x656C7374 // the duration spells "elst": the tag-scan trap
	blob := elstBox(mediaTime, duration)
	if got := int64(binary.BigEndian.Uint64(blob[elstDurOffset:])); got != duration {
		t.Fatalf("duration at elstDurOffset = %#x, want %#x", got, duration)
	}
	if got := int64(binary.BigEndian.Uint64(blob[elstDurOffset+8:])); got != mediaTime {
		t.Fatalf("media_time after the duration = %d, want %d", got, mediaTime)
	}
}

// TestMuxAtANonZeroStart writes into a destination the caller had already
// written to. The edit-list duration and the iTunSMPB payload are patched
// where the moov put them, at offsets into the stream rather than the file.
func TestMuxAtANonZeroStart(t *testing.T) {
	const n = 8192
	raw := testutil.MuxAtOffset(t, 97, func(w io.Writer) {
		muxAAC(t, w, n, -1)
	})
	if dur, _ := elstEntry(t, raw); dur == 0 {
		t.Error("the edit list kept its unknown duration")
	}
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	if tr := d.Tracks()[0]; tr.Codec != codec.AACLC || tr.Samples <= 0 {
		t.Errorf("read back as %+v", tr)
	}
}

// TestMuxPipeSkipsTheGaplessAtom: seekability is probed, not inferred.
// *os.File has Seek for a pipe too, and a muxer that trusted the method set
// would write the iTunSMPB template and then fail to fill it, leaving a
// gapless atom claiming zero padding over a whole encode.
func TestMuxPipeSkipsTheGaplessAtom(t *testing.T) {
	w := &testutil.PipeWriteSeeker{}
	muxAAC(t, w, 8192, -1)
	if bytes.Contains(w.Buf, []byte("iTunSMPB")) {
		t.Error("a writer whose Seek fails got an iTunSMPB atom it could never fill")
	}
}
