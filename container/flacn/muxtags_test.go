package flacn_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/internal/testutil"
)

// encodeTagged is encodeStream with a tag set: src encoded at the given
// level into w with the tags in the muxer options.
func encodeTagged(t *testing.T, src *audio.Buffer, level int, w io.Writer, samplesHint int64, tags []container.Tag) *flac.Encoder {
	t.Helper()
	enc, err := flac.NewEncoder(src.Fmt, &flac.EncoderOptions{Level: level})
	if err != nil {
		t.Fatal(err)
	}
	mux := flacn.NewMuxer(w, &flacn.MuxerOptions{MD5: enc.MD5, Tags: tags})
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

// vcHeader returns the metadata block header byte and body length of the
// block after STREAMINFO, asserting it is a VORBIS_COMMENT.
func vcHeader(t *testing.T, raw []byte) (hdr byte, bodyLen int) {
	t.Helper()
	off := 8 + flac.StreamInfoLen
	hdr = raw[off]
	if hdr&0x7F != 4 {
		t.Fatalf("block after STREAMINFO has type %d, want 4 (VORBIS_COMMENT)", hdr&0x7F)
	}
	return hdr, int(raw[off+1])<<16 | int(raw[off+2])<<8 | int(raw[off+3])
}

// TestMuxTagsRoundTrip checks the VORBIS_COMMENT block sits between
// STREAMINFO and the rest of the stream without disturbing either side:
// the demuxer decodes the same samples as a no-tags run, the STREAMINFO
// back-patch (fixed offset 8) still lands on a seekable writer, the
// last-metadata-block flag follows whether a seek table trails the
// comments. The waxlabel read-back cell lives in the oracletest module.
func TestMuxTagsRoundTrip(t *testing.T) {
	tags := []container.Tag{
		{Key: "TITLE", Value: "Flac Title"},
		{Key: "ARTIST", Value: "Flac Artist"},
	}
	f := muxFmt(44100, 2, 16)
	src := testutil.Sine(f, 3*4096+123, 997, 0.8)
	defer audio.Put(src)
	base := 8 + flac.StreamInfoLen // where the block after STREAMINFO starts

	t.Run("seekable", func(t *testing.T) {
		w := &memWS{}
		enc := encodeTagged(t, src, 5, w, int64(src.N), tags)
		wp := &memWS{}
		encodeStream(t, src, 5, wp, int64(src.N))
		raw, plain := w.b, wp.b

		si := decodeStream(t, raw, src) // demuxer skips the block, samples intact
		if si.MD5 != enc.MD5() {
			t.Errorf("STREAMINFO MD5 %x, want %x (back-patch must land past the comment block)", si.MD5, enc.MD5())
		}
		if raw[4]&0x80 != 0 {
			t.Fatal("STREAMINFO marked last with a comment block following")
		}
		hdr, vcLen := vcHeader(t, raw)
		if hdr&0x80 != 0 {
			t.Error("VORBIS_COMMENT marked last with a seek table following")
		}
		if next := raw[base+4+vcLen]; next != 0x80|3 {
			t.Errorf("block after VORBIS_COMMENT has header %#x, want last SEEKTABLE", next)
		}
		// The comment block must be pure insertion: everything outside it
		// matches the no-tags run byte for byte (seek offsets count from
		// the first frame, so the table is unaffected).
		if !bytes.Equal(raw[:base], plain[:base]) {
			t.Error("marker or STREAMINFO differ from a no-tags run")
		}
		if !bytes.Equal(raw[base+4+vcLen:], plain[base:]) {
			t.Error("stream after the comment block differs from a no-tags run")
		}
	})

	t.Run("streaming", func(t *testing.T) {
		var buf, plainBuf bytes.Buffer
		encodeTagged(t, src, 5, &buf, int64(src.N), tags)
		encodeStream(t, src, 5, &plainBuf, int64(src.N))
		raw, plain := buf.Bytes(), plainBuf.Bytes()

		decodeStream(t, raw, src) // demuxer skips the block, samples intact
		if raw[4]&0x80 != 0 {
			t.Fatal("STREAMINFO marked last with a comment block following")
		}
		hdr, vcLen := vcHeader(t, raw)
		if hdr&0x80 == 0 {
			t.Error("VORBIS_COMMENT not marked last on a plain writer (nothing follows it)")
		}
		if fr := raw[base+4+vcLen:]; fr[0] != 0xFF || fr[1]&0xFC != 0xF8 {
			t.Errorf("bytes after the comment block %x %x are not a frame sync", fr[0], fr[1])
		}
		// A plain no-tags run marks STREAMINFO itself last; past that one
		// bit the two streams must agree outside the inserted block.
		if plain[4]&0x80 == 0 {
			t.Fatal("no-tags STREAMINFO not marked last on a plain writer")
		}
		if !bytes.Equal(raw[8:base], plain[8:base]) {
			t.Error("STREAMINFO body differs from a no-tags run")
		}
		if !bytes.Equal(raw[base+4+vcLen:], plain[base:]) {
			t.Error("frames after the comment block differ from a no-tags run")
		}
	})
}
