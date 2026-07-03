package flacn_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// memWS is an in-memory io.WriteSeeker for exercising the back-patch
// path.
type memWS struct {
	b   []byte
	pos int64
}

func (w *memWS) Write(p []byte) (int, error) {
	if need := w.pos + int64(len(p)); need > int64(len(w.b)) {
		grown := make([]byte, need)
		copy(grown, w.b)
		w.b = grown
	}
	copy(w.b[w.pos:], p)
	w.pos += int64(len(p))
	return len(p), nil
}

func (w *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = off
	case io.SeekCurrent:
		w.pos += off
	case io.SeekEnd:
		w.pos = int64(len(w.b)) + off
	}
	return w.pos, nil
}

func muxFmt(rate, channels, bits int) audio.Format {
	return audio.Format{
		Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels),
		Type: audio.Int, BitDepth: bits,
	}
}

// encodeStream encodes src at the given level into w through the muxer,
// with samplesHint as the track projection (as the engine supplies it;
// -1 means unknown). It returns the finished encoder for MD5 checks.
func encodeStream(t *testing.T, src *audio.Buffer, level int, w io.Writer, samplesHint int64) *flac.Encoder {
	t.Helper()
	enc, err := flac.NewEncoder(src.Fmt, &flac.EncoderOptions{Level: level})
	if err != nil {
		t.Fatal(err)
	}
	mux := flacn.NewMuxer(w, &flacn.MuxerOptions{MD5: enc.MD5})
	track := container.Track{
		Codec:       codec.FLAC,
		CodecConfig: enc.CodecConfig(),
		Fmt:         src.Fmt,
		Samples:     samplesHint,
		Default:     true,
	}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return mux.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	chunk := audio.Get(src.Fmt, enc.FrameSize())
	defer audio.Put(chunk)
	for off := 0; off < src.N; off += enc.FrameSize() {
		n := min(enc.FrameSize(), src.N-off)
		audio.CopyFrames(chunk, 0, src, off, n)
		chunk.N = n
		if err := enc.Encode(chunk, emit); err != nil {
			t.Fatal(err)
		}
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	return enc
}

// decodeStream demuxes and decodes a complete native FLAC stream,
// asserting it equals src.
func decodeStream(t *testing.T, raw []byte, src *audio.Buffer) flac.StreamInfo {
	t.Helper()
	d, err := flacn.NewDemuxer(container.BytesSource(raw), &flacn.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	track := d.Tracks()[0]
	si, err := flac.ParseStreamInfo(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := flac.NewDecoder(si, track.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()

	pos := 0
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		err = dec.Decode(pkt.Data, func(out *audio.Buffer) error {
			for c := 0; c < src.Fmt.Channels; c++ {
				if d := testutil.DiffI32(out.ChanI(c), src.ChanI(c)[pos:pos+out.N]); d >= 0 {
					t.Fatalf("channel %d differs at sample %d", c, pos+d)
				}
			}
			pos += out.N
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if pos != src.N {
		t.Fatalf("decoded %d samples, want %d", pos, src.N)
	}
	return si
}

func TestMuxRoundTripSeekable(t *testing.T) {
	f := muxFmt(44100, 2, 16)
	src := testutil.Sine(f, 3*4096+123, 997, 0.8)
	defer audio.Put(src)

	w := &memWS{}
	enc := encodeStream(t, src, 5, w, int64(src.N))
	si := decodeStream(t, w.b, src)

	if si.Samples != int64(src.N) {
		t.Errorf("STREAMINFO samples %d, want %d", si.Samples, src.N)
	}
	if si.MinFrame == 0 || si.MaxFrame == 0 || si.MinFrame > si.MaxFrame {
		t.Errorf("frame bounds %d..%d not patched", si.MinFrame, si.MaxFrame)
	}
	if si.MD5 != enc.MD5() {
		t.Errorf("STREAMINFO MD5 %x, want %x", si.MD5, enc.MD5())
	}
}

func TestMuxRoundTripUnseekable(t *testing.T) {
	f := muxFmt(48000, 2, 24)
	src := testutil.Noise(f, 2*4096+77, 9)
	defer audio.Put(src)

	var buf bytes.Buffer
	encodeStream(t, src, 5, &buf, int64(src.N))
	si := decodeStream(t, buf.Bytes(), src)

	// The streaming form carries the projected totals and nothing that
	// needs a back-patch.
	if si.Samples != int64(src.N) {
		t.Errorf("STREAMINFO samples %d, want projected %d", si.Samples, src.N)
	}
	if si.MinFrame != 0 || si.MaxFrame != 0 {
		t.Errorf("frame bounds %d..%d on an unseekable stream, want 0", si.MinFrame, si.MaxFrame)
	}
	if si.MD5 != [16]byte{} {
		t.Errorf("MD5 %x on an unseekable stream, want unset", si.MD5)
	}
}

func TestMuxUnknownLength(t *testing.T) {
	f := muxFmt(44100, 1, 16)
	src := testutil.Sine(f, 5000, 500, 0.5)
	defer audio.Put(src)

	var buf bytes.Buffer
	encodeStream(t, src, 2, &buf, -1)
	si := decodeStream(t, buf.Bytes(), src)
	if si.Samples != 0 {
		t.Errorf("STREAMINFO samples %d, want 0 (unknown)", si.Samples)
	}

	// The same unknown-length stream on a seekable writer patches exact
	// totals at End.
	w := &memWS{}
	encodeStream(t, src, 2, w, -1)
	si = decodeStream(t, w.b, src)
	if si.Samples != int64(src.N) {
		t.Errorf("patched STREAMINFO samples %d, want %d", si.Samples, src.N)
	}
}

func TestMuxSeekTable(t *testing.T) {
	f := muxFmt(44100, 2, 16)
	frames := 44100 * 25 // three 10-second intervals
	src := testutil.Sine(f, frames, 220, 0.6)
	defer audio.Put(src)

	w := &memWS{}
	encodeStream(t, src, 5, w, int64(src.N))

	// The table must parse: type 3, 18-byte points, real points sorted
	// and placeholders trailing.
	raw := w.b
	if string(raw[:4]) != "fLaC" {
		t.Fatal("missing stream marker")
	}
	if raw[4]&0x80 != 0 {
		t.Fatal("STREAMINFO marked last; expected a SEEKTABLE to follow")
	}
	off := 8 + flac.StreamInfoLen
	hdr := raw[off : off+4]
	if hdr[0] != 0x80|3 {
		t.Fatalf("second block type %#x, want last SEEKTABLE", hdr[0])
	}
	size := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if size%18 != 0 || size == 0 {
		t.Fatalf("SEEKTABLE size %d not a point multiple", size)
	}
	points := size / 18
	if want := (frames + 441000 - 1) / 441000; points != want {
		t.Errorf("%d seek points, want %d", points, want)
	}
	body := raw[off+4 : off+4+size]
	last := int64(-1)
	placeholders := false
	for p := 0; p < points; p++ {
		sample := binary.BigEndian.Uint64(body[p*18:])
		if sample == ^uint64(0) {
			placeholders = true
			continue
		}
		if placeholders {
			t.Fatal("real point after a placeholder")
		}
		if int64(sample) <= last {
			t.Fatalf("seek points not ascending at %d", p)
		}
		last = int64(sample)
	}

	// The demuxer must be able to seek through the table it wrote.
	d, err := flacn.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	landed, err := d.SeekSample(0, 44100*20)
	if err != nil {
		t.Fatal(err)
	}
	if landed > 44100*20 || landed < 0 {
		t.Fatalf("landed %d past target", landed)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.PTS != landed {
		t.Fatalf("first packet after seek at %d, landed says %d", pkt.PTS, landed)
	}
}

// TestMuxSeekPointOnShortFrame pins the seek point's frame-length
// field: a point landing on the short final frame must advertise that
// frame's true sample count, not the constant block size.
func TestMuxSeekPointOnShortFrame(t *testing.T) {
	f := muxFmt(44100, 1, 16)
	// 108 full blocks end at sample 442368, just past the 441000-sample
	// second interval target, so the second seek point lands on the
	// 100-sample final frame.
	frames := 108*4096 + 100
	src := testutil.Sine(f, frames, 220, 0.5)
	defer audio.Put(src)

	w := &memWS{}
	encodeStream(t, src, 5, w, int64(src.N))

	hdr := w.b[8+flac.StreamInfoLen:]
	size := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if size != 2*18 {
		t.Fatalf("SEEKTABLE of %d bytes, want 2 points", size)
	}
	off := 8 + flac.StreamInfoLen + 4
	points := 0
	var lastSample, lastLen uint64
	for ; points < size/18; points++ {
		b := w.b[off+points*18:]
		sample := binary.BigEndian.Uint64(b)
		if sample == ^uint64(0) {
			break
		}
		lastSample = sample
		lastLen = uint64(b[16])<<8 | uint64(b[17])
		if points == 0 && lastLen != 4096 {
			t.Fatalf("full-frame point advertises %d samples, want 4096", lastLen)
		}
	}
	if points != 2 {
		t.Fatalf("%d real seek points, want 2", points)
	}
	if lastSample != 108*4096 || lastLen != 100 {
		t.Fatalf("short-frame point = sample %d, %d samples; want 442368, 100", lastSample, lastLen)
	}
}

func TestMuxProjectionMismatchUnseekable(t *testing.T) {
	f := muxFmt(44100, 2, 16)
	src := testutil.Sine(f, 1000, 500, 0.5)
	defer audio.Put(src)

	enc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mux := flacn.NewMuxer(&buf, nil)
	track := container.Track{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: 9999, Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return mux.WritePacket(container.Packet{Track: 0, Packet: p})
	}
	src.N = 1000
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err == nil {
		t.Fatal("End accepted a header projection the stream missed on an unseekable writer")
	} else if waxerr.CodeOf(err) != waxerr.CodeInternal {
		t.Fatalf("unexpected error class: %v", err)
	}
}

func TestMuxRejects(t *testing.T) {
	f := muxFmt(44100, 2, 16)
	enc, err := flac.NewEncoder(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := enc.CodecConfig()
	good := container.Track{Codec: codec.FLAC, CodecConfig: cfg, Fmt: f, Samples: -1, Default: true}

	cases := []struct {
		name   string
		tracks []container.Track
	}{
		{"two tracks", []container.Track{good, good}},
		{"wrong codec", []container.Track{{Codec: codec.PCM, CodecConfig: cfg, Fmt: f}}},
		{"gapless trims", []container.Track{{Codec: codec.FLAC, CodecConfig: cfg, Fmt: f, Delay: 100}}},
		{"bad config", []container.Track{{Codec: codec.FLAC, CodecConfig: []byte{1, 2}, Fmt: f}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := flacn.NewMuxer(&bytes.Buffer{}, nil).Begin(tc.tracks); err == nil {
				t.Error("Begin accepted")
			}
		})
	}

	mux := flacn.NewMuxer(&bytes.Buffer{}, nil)
	if err := mux.WritePacket(container.Packet{}); err == nil {
		t.Error("WritePacket before Begin accepted")
	}
	if err := mux.Begin([]container.Track{good}); err != nil {
		t.Fatal(err)
	}
	if err := mux.Begin([]container.Track{good}); err == nil {
		t.Error("second Begin accepted")
	}
	if err := mux.WritePacket(container.Packet{Track: 1, Packet: codec.Packet{Data: []byte{1}, Dur: 1}}); err == nil {
		t.Error("unknown track accepted")
	}
	if err := mux.WritePacket(container.Packet{Track: 0}); err == nil {
		t.Error("empty packet accepted")
	}
	if err := mux.End(codec.Trailer{Delay: 1}); err == nil {
		t.Error("trailer with trims accepted")
	}
}
