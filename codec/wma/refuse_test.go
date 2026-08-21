//go:build !wmatablesgen

package wma_test

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/waxerr"
)

// wfx builds a WAVEFORMATEX plus the codec extra bytes an ffmpeg-written v2
// file carries, so a refusal test can vary one field at a time.
func wfx(tag uint16, channels, rate, bytesPerSec, blockAlign int, extra []byte) []byte {
	b := make([]byte, 18+len(extra))
	binary.LittleEndian.PutUint16(b, tag)
	binary.LittleEndian.PutUint16(b[2:], uint16(channels))
	binary.LittleEndian.PutUint32(b[4:], uint32(rate))
	binary.LittleEndian.PutUint32(b[8:], uint32(bytesPerSec))
	binary.LittleEndian.PutUint16(b[12:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(b[14:], 16)
	binary.LittleEndian.PutUint16(b[16:], uint16(len(extra)))
	copy(b[18:], extra)
	return b
}

// v2Extra is the ten extra bytes ffmpeg's ASF muxer writes for wmav2, with
// flags2 at offset 4.
func v2Extra(flags2 uint16) []byte {
	e := make([]byte, 10)
	binary.LittleEndian.PutUint16(e[4:], flags2)
	return e
}

// v1Extra is the four extra bytes a v1 stream carries, with flags2 at offset 2.
func v1Extra(flags2 uint16) []byte {
	e := make([]byte, 4)
	binary.LittleEndian.PutUint16(e[2:], flags2)
	return e
}

// TestConfigRefusals: every shape this decoder does not cover is named before
// a bit is read. nAvgBytesPerSec and nBlockAlign matter most, because
// container/asf validates neither and the frame layout divides by the first
// and steps by the second.
func TestConfigRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  []byte
		want string
	}{
		{"rate above the ceiling", wfx(0x161, 2, 96000, 16000, 743, v2Extra(1)), "above the 50000 Hz"},
		{"zero rate", wfx(0x161, 2, 0, 16000, 743, v2Extra(1)), "sample rate 0"},
		{"three channels", wfx(0x161, 3, 44100, 16000, 743, v2Extra(1)), "3 channels"},
		{"no channels", wfx(0x161, 0, 44100, 16000, 743, v2Extra(1)), "0 channels"},
		{"zero block align", wfx(0x161, 2, 44100, 16000, 0, v2Extra(1)), "nBlockAlign is 0"},
		{"zero byte rate", wfx(0x161, 2, 44100, 0, 743, v2Extra(1)), "nAvgBytesPerSec is 0"},
		{"wma pro tag", wfx(0x162, 2, 44100, 16000, 743, v2Extra(1)), "0x0162 is not Windows Media Audio 1 or 2"},
		{"pcm tag", wfx(0x0001, 2, 44100, 16000, 743, nil), "0x0001 is not Windows Media Audio 1 or 2"},
		{"short config", []byte{0x61, 0x01}, "want at least the 18-byte"},
		// v1 plus variable block lengths has no coherent reference behaviour:
		// the band layout is computed into one slot and indexed from another,
		// which agree only because every real v1 file has one block size.
		{"v1 with variable block lengths", wfx(0x160, 1, 44100, 4000, 185, v1Extra(0x0005)), "Windows Media Audio 1 with variable block lengths"},
		// Stereo v1 aligns per channel slot, and an align is relative to the
		// buffer a frame is read from; a reservoir moves frames between
		// buffers, so the two together have no defined meaning.
		{"stereo v1 with a reservoir", wfx(0x160, 2, 44100, 8000, 371, v1Extra(0x0003)), "stereo Windows Media Audio 1 with a bit reservoir"},
		// A bit rate absurd for the frame length asks for a superframe offset
		// field wider than the bit reader's word.
		{"absurd bit rate", wfx(0x161, 1, 8000, 1<<30, 4096, v2Extra(3)), "superframe offset field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wma.ParseConfig(tc.cfg)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if got := waxerr.CodeOf(err); got != waxerr.CodeUnsupportedFormat {
				t.Errorf("code %q, want %q", got, waxerr.CodeUnsupportedFormat)
			}
		})
	}
}

// TestConfigAccepts covers the shapes that look odd and are not.
func TestConfigAccepts(t *testing.T) {
	// Fewer extra bytes than the version needs means flags2 0, which is
	// LSP-coded exponents and no reservoir, not a broken file.
	cfg, err := wma.ParseConfig(wfx(0x161, 1, 44100, 4000, 185, nil))
	if err != nil {
		t.Fatalf("a v2 stream with no extra bytes: %v", err)
	}
	if cfg.Flags2 != 0 {
		t.Errorf("flags2 = %#04x, want 0", cfg.Flags2)
	}
	// One writer claims variable block lengths it does not use, always with
	// this exact word, and readers clear the bit for it.
	cfg, err = wma.ParseConfig(wfx(0x161, 1, 44100, 4000, 185, v2Extra(0x000d)))
	if err != nil {
		t.Fatalf("the 0x000d quirk: %v", err)
	}
	if cfg.Flags2 != 0x0009 {
		t.Errorf("flags2 = %#04x, want 0x0009 (the variable-block-length bit cleared)", cfg.Flags2)
	}
	// The same bits on any other word are honoured.
	cfg, err = wma.ParseConfig(wfx(0x161, 1, 44100, 4000, 185, v2Extra(0x000f)))
	if err != nil {
		t.Fatalf("0x000f: %v", err)
	}
	if cfg.Flags2 != 0x000f {
		t.Errorf("flags2 = %#04x, want 0x000f left alone", cfg.Flags2)
	}
	// Mono v1 with a reservoir is fine: mono never aligns, so a frame means
	// the same thing wherever the reservoir puts it.
	if _, err := wma.ParseConfig(wfx(0x160, 1, 44100, 4000, 185, v1Extra(0x0003))); err != nil {
		t.Fatalf("mono v1 with a reservoir: %v", err)
	}
	// A cbSize larger than the bytes present is clamped: writers overstate it,
	// and the fixed fields are what select the codec.
	over := wfx(0x161, 1, 44100, 4000, 185, v2Extra(1))
	binary.LittleEndian.PutUint16(over[16:], 4096)
	if _, err := wma.ParseConfig(over); err != nil {
		t.Fatalf("an overstated cbSize: %v", err)
	}
}

// TestDecodeRefusesDamagedPackets: a packet that does not decode is refused
// rather than turned into garbage samples. WMA has no sync word and no frame
// header, so an overrun is the only signal a walk has gone wrong.
func TestDecodeRefusesDamagedPackets(t *testing.T) {
	c := corpusCells[16] // v2 44.1 kHz stereo 128k, the widest packet
	track, pkts := demux(t, corpusFile(t, c))
	cfg, err := wma.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	// A packet cut short mid-frame: the frame runs off the end of what is
	// there.
	dec, err := wma.NewDecoder(cfg, track.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	short := pkts[0][:8]
	if err := dec.Decode(short, func(*audio.Buffer) error { return nil }); err == nil {
		t.Error("accepted a packet cut to 8 bytes")
	}
	if err := dec.Decode(nil, func(*audio.Buffer) error { return nil }); err == nil {
		t.Error("accepted an empty packet")
	}
}
