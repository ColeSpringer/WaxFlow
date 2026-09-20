package mpa

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
)

// TestCRC16ARC checks the table against the polynomial's published check
// value, so a transcription slip in either direction shows up here rather
// than as a tag no reader trusts.
func TestCRC16ARC(t *testing.T) {
	if got := updateCRC16(0, []byte("123456789")); got != 0xBB3D {
		t.Errorf("CRC-16/ARC check value = %#04x, want 0xbb3d", got)
	}
	split := updateCRC16(updateCRC16(0, []byte("12345")), []byte("6789"))
	if split != 0xBB3D {
		t.Errorf("incremental CRC-16 = %#04x, want 0xbb3d", split)
	}
}

// tagAt returns the metadata frame's payload offset and its LAME extension,
// located by walking the flags the way a reader does.
func tagAt(t *testing.T, out []byte) (off int, lame []byte) {
	t.Helper()
	h, err := mp3.ParseHeader(out)
	if err != nil {
		t.Fatalf("first frame header: %v", err)
	}
	off = mp3.HeaderLen + h.SideInfoLen()
	flags := binary.BigEndian.Uint32(out[off+4:])
	p := off + 8
	for _, f := range xingFields {
		if flags&f.flag != 0 {
			p += f.n
		}
	}
	if p+lamePrefixLen > h.Size() {
		return off, nil
	}
	return off, out[p:h.Size()]
}

// TestLAMETagCanonicalLayout pins what every MP3 this muxer writes at a
// normal bit rate must carry: the LAME-prefixed encoder string at the
// canonical magic+120, all four Xing fields ahead of it, and the block's
// own fields filled in rather than left zero.
func TestLAMETagCanonicalLayout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vbr    bool
		magic  string
		method byte
	}{
		{"cbr", false, "Info", 1},
		{"vbr", true, "Xing", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const rate, channels, n = 44100, 2, 40000
			var pkts [][]byte
			var tr codec.Trailer
			var samples int64
			if tc.vbr {
				pkts, tr, samples = encodeVBR(t, rate, channels, 128000, n)
			} else {
				pkts, tr, samples = encodeTone(t, rate, channels, 128000, n)
			}
			ws := &memWS{}
			if tc.vbr {
				muxVBR(t, ws, pkts, tr, samples, rate, channels)
			} else {
				muxPackets(t, ws, pkts, tr, samples, rate, channels)
			}
			out := ws.Buf

			h, err := mp3.ParseHeader(out)
			if err != nil {
				t.Fatal(err)
			}
			off := mp3.HeaderLen + h.SideInfoLen()
			if got := string(out[off : off+4]); got != tc.magic {
				t.Fatalf("magic %q, want %q", got, tc.magic)
			}
			flags := binary.BigEndian.Uint32(out[off+4:])
			if want := uint32(xingFlagFrames | xingFlagBytes | xingFlagTOC | xingFlagQuality); flags != want {
				t.Fatalf("flags %#x, want %#x (all four, so the block lands at magic+120)", flags, want)
			}
			b := out[off+120:]
			if got := string(b[:9]); got != encoderTag {
				t.Errorf("encoder string %q, want %q", got, encoderTag)
			}
			if b[9] != tc.method {
				t.Errorf("revision/VBR-method byte %#02x, want %#02x", b[9], tc.method)
			}
			if want := minKbpsOf(t, pkts); int(b[20]) != want {
				t.Errorf("bitrate byte %d, want %d kbit/s", b[20], want)
			}
			if got := b[24] >> 6; got != 1 {
				t.Errorf("misc source frequency %d, want 1 (44.1 kHz)", got)
			}
			// Bits 4-2 are the stereo mode, and 0 there means MONO: a
			// stereo file whose misc byte is left zero claims a mono
			// encode to every tag inspector that reads the block.
			if got := (b[24] >> 2) & 7; got != 3 {
				t.Errorf("misc stereo mode %d, want 3 (joint stereo)", got)
			}

			// Music length and CRC cover the audio frames alone: this
			// frame is excluded, because the tag CRC below is written
			// into the frame the music CRC would otherwise cover.
			audio := out[h.Size():]
			if got := binary.BigEndian.Uint32(b[28:]); int(got) != len(audio) {
				t.Errorf("music length %d, want %d (the audio frames)", got, len(audio))
			}
			if got, want := binary.BigEndian.Uint16(b[32:]), updateCRC16(0, audio); got != want {
				t.Errorf("music CRC %#04x, want %#04x", got, want)
			}
			if got, want := binary.BigEndian.Uint16(b[34:]), updateCRC16(0, out[:off+154]); got != want {
				t.Errorf("info tag CRC %#04x, want %#04x over the frame's first %d bytes", got, want, off+154)
			}

			// The reader still gets the gapless fields out of it.
			tag, ok := mpegframes.ParseVBRTag(h, out[:h.Size()])
			if !ok || tag.Delay != int64(mp3.EncoderDelay) {
				t.Errorf("ParseVBRTag = %+v, %v; want delay %d", tag, ok, mp3.EncoderDelay)
			}
		})
	}
}

// TestLAMETagFitRule walks the three shapes a frame too small for the
// canonical layout falls back through. The cliff matters because a frame
// that silently drops the extension loses the gapless trims, and one that
// keeps a flag for a field it did not write misplaces the extension for
// every reader that walks the flags.
func TestLAMETagFitRule(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hdr       [mp3.HeaderLen]byte
		wantFlags uint32
		wantLame  int
	}{
		// MPEG-1 stereo, 128 kbit/s, 44.1 kHz: 417 bytes, canonical.
		{"canonical", [mp3.HeaderLen]byte{0xFF, 0xFB, 0x90, 0x00},
			xingFlagFrames | xingFlagBytes | xingFlagTOC | xingFlagQuality, lameBlockLen},
		// MPEG-2.5 stereo, 8 kbit/s, 8 kHz: 72 bytes, 43 past the flag
		// word. Only the frame count fits ahead of the whole block.
		{"fields-dropped", [mp3.HeaderLen]byte{0xFF, 0xE3, 0x18, 0x00},
			xingFlagFrames, lameBlockLen},
		// MPEG-2.5 mono, 8 kbit/s, 12 kHz: 27 bytes past the flag word,
		// which holds the 24-byte prefix and no field ahead of it. The
		// frame count loses to the gapless trims here, which is the one
		// window where they compete; see buildInfoFrame.
		{"prefix-only", [mp3.HeaderLen]byte{0xFF, 0xE3, 0x14, 0xC0},
			0, lamePrefixLen},
		// MPEG-2.5 stereo, 8 kbit/s, 11.025 kHz: 23 bytes, too small for
		// even the prefix, so the frame keeps its form's own fields.
		{"marker-only", [mp3.HeaderLen]byte{0xFF, 0xE3, 0x10, 0x00},
			xingFlagFrames, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := mp3.ParseHeader(tc.hdr[:])
			if err != nil {
				t.Fatal(err)
			}
			m := NewMuxer(nil, &MuxerOptions{})
			m.h, m.hdr, m.rate, m.minKbps = h, tc.hdr, h.Rate, h.Bitrate/1000
			frame := m.buildInfoFrame(infoFields{delay: mp3.EncoderDelay, padding: 100, frames: 7})
			if frame == nil {
				t.Fatalf("no metadata frame for a %d-byte frame", h.Size())
			}
			if len(frame) != h.Size() {
				t.Fatalf("frame is %d bytes, want %d", len(frame), h.Size())
			}
			off := mp3.HeaderLen + h.SideInfoLen()
			if got := binary.BigEndian.Uint32(frame[off+4:]); got != tc.wantFlags {
				t.Errorf("flags %#x, want %#x", got, tc.wantFlags)
			}
			// Every flag set must have its field, with the extension
			// starting right after the last of them.
			p := off + 8
			for _, f := range xingFields {
				if binary.BigEndian.Uint32(frame[off+4:])&f.flag != 0 {
					p += f.n
				}
			}
			if p+tc.wantLame > h.Size() {
				t.Fatalf("extension of %d bytes at %d overruns the %d-byte frame", tc.wantLame, p, h.Size())
			}
			if tc.wantLame == 0 {
				if p+lamePrefixLen <= h.Size() {
					t.Errorf("the frame had room for the prefix at %d, it should carry it", p)
				}
				return
			}
			if got := string(frame[p : p+9]); got != encoderTag {
				t.Errorf("encoder string %q at %d, want %q", got, p, encoderTag)
			}
			tag, ok := mpegframes.ParseVBRTag(h, frame)
			if !ok || tag.Delay != int64(mp3.EncoderDelay) || tag.Padding != 100 {
				t.Errorf("ParseVBRTag = %+v, %v; want delay %d padding 100", tag, ok, mp3.EncoderDelay)
			}
			// A short frame keeps the prefix and nothing past it, so the
			// bytes the block's tail would occupy must not be there.
			if tc.wantLame == lamePrefixLen && p+lameBlockLen <= h.Size() {
				t.Errorf("the frame had room for the whole block at %d", p)
			}
		})
	}
}

// TestLAMETagOldPrefixStillReads holds the reader to accepting the
// WaxFlow01 string this muxer used to write: files already on disk carry
// it, and their gapless trims have to survive a round trip.
func TestLAMETagOldPrefixStillReads(t *testing.T) {
	const rate, channels, n = 44100, 2, 40000
	pkts, tr, samples := encodeTone(t, rate, channels, 128000, n)
	ws := &memWS{}
	muxPackets(t, ws, pkts, tr, samples, rate, channels)
	out := ws.Buf

	h, err := mp3.ParseHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	off := mp3.HeaderLen + h.SideInfoLen()
	want, ok := mpegframes.ParseVBRTag(h, out[:h.Size()])
	if !ok {
		t.Fatal("the metadata frame carries no readable tag")
	}
	old := bytes.Clone(out)
	copy(old[off+120:], "WaxFlow01")
	got, ok := mpegframes.ParseVBRTag(h, old[:h.Size()])
	if !ok || got != want {
		t.Errorf("WaxFlow01-tagged frame reads %+v, %v; want %+v", got, ok, want)
	}
}

// minKbpsOf is the lowest bit rate any of the audio frames declares, which
// is what the extension's bitrate byte states for a CBR stream (where they
// all agree) and for a VBR one (where LAME's field is the minimum).
func minKbpsOf(t *testing.T, pkts [][]byte) int {
	t.Helper()
	lo := 0
	for i, p := range pkts {
		h, err := mp3.ParseHeader(p)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if i == 0 || h.Bitrate/1000 < lo {
			lo = h.Bitrate / 1000
		}
	}
	return lo
}

// TestLAMETagMiscByteNamesTheStereoMode walks the modes a source can carry
// into the tag. The field's zero is mono, so leaving the byte alone is not
// the neutral choice it looks like.
func TestLAMETagMiscByteNamesTheStereoMode(t *testing.T) {
	for _, tc := range []struct {
		mode mp3.ChannelMode
		rate int
		want byte
	}{
		{mp3.ModeMono, 44100, 1<<6 | 0<<2},
		{mp3.ModeStereo, 44100, 1<<6 | 1<<2},
		{mp3.ModeDual, 48000, 2<<6 | 2<<2},
		{mp3.ModeJoint, 48000, 2<<6 | 3<<2},
		{mp3.ModeJoint, 24000, 0<<6 | 3<<2},
		{mp3.ModeStereo, 96000, 3<<6 | 1<<2},
	} {
		if got := miscByte(mp3.Header{Mode: tc.mode, Rate: tc.rate}); got != tc.want {
			t.Errorf("miscByte(%v, %d Hz) = %#02x, want %#02x", tc.mode, tc.rate, got, tc.want)
		}
	}
}
