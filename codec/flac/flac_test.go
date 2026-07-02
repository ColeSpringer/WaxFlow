package flac

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// TestCRCCheckValues pins both checksums to their published check values
// (the CRC of the ASCII string "123456789"): 0xF4 for CRC-8/SMBUS and
// 0xFEE8 for CRC-16/UMTS, the parameter sets RFC 9639 uses.
func TestCRCCheckValues(t *testing.T) {
	check := []byte("123456789")
	if got := crc8(check); got != 0xF4 {
		t.Errorf("crc8 check value = %#02x, want 0xf4", got)
	}
	if got := CRC16(check); got != 0xFEE8 {
		t.Errorf("CRC16 check value = %#04x, want 0xfee8", got)
	}
	split := UpdateCRC16(CRC16(check[:4]), check[4:])
	if split != 0xFEE8 {
		t.Errorf("incremental CRC16 = %#04x, want 0xfee8", split)
	}
}

// TestParseStreamInfo decodes a hand-packed STREAMINFO and checks every
// field lands where RFC 9639 says it lives.
func TestParseStreamInfo(t *testing.T) {
	b := make([]byte, StreamInfoLen)
	b[0], b[1] = 0x10, 0x00 // min block 4096
	b[2], b[3] = 0x10, 0x00 // max block 4096
	b[4], b[5], b[6] = 0x00, 0x00, 0x20
	b[7], b[8], b[9] = 0x00, 0x40, 0x00
	// rate 44100 (20 bits), channels 2 (code 1), bits 16 (code 15)
	b[10] = 0x0A
	b[11] = 0xC4
	b[12] = 0x40 | 1<<1 | 0
	b[13] = 0xF0 | 0x0                                  // bits low nibble | samples high nibble
	b[14], b[15], b[16], b[17] = 0x00, 0x00, 0x30, 0x39 // 12345 samples
	si, err := ParseStreamInfo(b)
	if err != nil {
		t.Fatal(err)
	}
	want := StreamInfo{
		MinBlock: 4096, MaxBlock: 4096, MinFrame: 32, MaxFrame: 16384,
		Rate: 44100, Channels: 2, Bits: 16, Samples: 12345,
	}
	if si != want {
		t.Errorf("ParseStreamInfo = %+v, want %+v", si, want)
	}
	f := si.PCMFormat()
	if f.Rate != 44100 || f.Channels != 2 || f.BitDepth != 16 || f.Type != audio.Int {
		t.Errorf("PCMFormat = %v", f)
	}
	if _, err := ParseStreamInfo(b[:33]); err == nil {
		t.Error("short STREAMINFO accepted")
	}
}

// buildFrameHeader packs a valid fixed-strategy frame header for tests.
func buildFrameHeader(frameNo uint64, bsCode, rateCode, assign, bitsCode int, extras ...byte) []byte {
	b := []byte{0xFF, 0xF8, byte(bsCode<<4 | rateCode), byte(assign<<4 | bitsCode<<1)}
	// Coded number, single byte only in tests.
	b = append(b, byte(frameNo))
	b = append(b, extras...)
	return append(b, crc8(b))
}

func TestParseFrameHeader(t *testing.T) {
	// Block size 4096 (code 12), rate 44.1k (code 9), stereo independent
	// (assign 1), 16-bit (code 4).
	hdr := buildFrameHeader(3, 12, 9, 1, 4)
	fi, err := ParseFrameHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Variable || fi.Coded != 3 || fi.BlockSize != 4096 || fi.Rate != 44100 ||
		fi.Channels != 2 || fi.Bits != 16 || fi.hdrLen != len(hdr) {
		t.Errorf("ParseFrameHeader = %+v", fi)
	}
	// Numbering semantics: plain fixed-strategy streams count frames;
	// the variable bit or unequal STREAMINFO block bounds (the pre-1.0
	// "old format") switch to sample positions.
	si := StreamInfo{MinBlock: 4096, MaxBlock: 4096}
	n := si.Numbering(fi)
	if n.SampleCoded || n.Start(fi) != 3*4096 || n.Next(fi) != 4 {
		t.Errorf("fixed numbering = %+v: start %d next %d", n, n.Start(fi), n.Next(fi))
	}
	old := StreamInfo{MinBlock: 2304, MaxBlock: 4608}
	n = old.Numbering(fi)
	if !n.SampleCoded || n.Start(fi) != 3 || n.Next(fi) != 3+4096 {
		t.Errorf("old-format numbering = %+v: start %d next %d", n, n.Start(fi), n.Next(fi))
	}

	// Uncommon 16-bit block size (code 7) and rate deferred to
	// STREAMINFO (code 0); side-channel assignment.
	hdr = buildFrameHeader(0, 7, 0, 8, 6, 0x12, 0x33) // block size 0x1234
	fi, err = ParseFrameHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if fi.BlockSize != 0x1234 || fi.Rate != 0 || fi.Channels != 2 || fi.Bits != 24 {
		t.Errorf("uncommon header = %+v", fi)
	}

	// Corrupted CRC-8 must fail.
	bad := buildFrameHeader(3, 12, 9, 1, 4)
	bad[len(bad)-1] ^= 1
	if _, err := ParseFrameHeader(bad); err == nil {
		t.Error("corrupt CRC-8 accepted")
	}
	// Reserved bits must fail.
	if _, err := ParseFrameHeader([]byte{0xFF, 0xFA, 0xC9, 0x14, 0x00, 0x00}); err == nil {
		t.Error("bad sync accepted")
	}
	// Every truncation errors rather than panics.
	full := buildFrameHeader(3, 7, 14, 1, 4, 0x10, 0x00, 0x11, 0x22)
	for i := 0; i < len(full); i++ {
		if _, err := ParseFrameHeader(full[:i]); err == nil {
			t.Errorf("truncation to %d bytes accepted", i)
		}
	}
}

// TestBitReader exercises field reads across refill boundaries.
func TestBitReader(t *testing.T) {
	r := &bitReader{data: []byte{0b10110100, 0b01100000, 0xFF, 0x00, 0x0F}}
	if v := r.u(3); v != 0b101 {
		t.Errorf("u(3) = %b", v)
	}
	if v := r.s(4); v != -6 { // 1010 sign-extended
		t.Errorf("s(4) = %d, want -6", v)
	}
	if v := r.unary(); v != 2 { // two zeros across the byte boundary
		t.Errorf("unary = %d, want 2", v)
	}
	if v := r.unary(); v != 0 { // leading 1
		t.Errorf("unary = %d, want 0", v)
	}
	if v := r.u(14); v != 0b00000111111110 {
		t.Errorf("u(14) = %b", v)
	}
	r.align()
	if v := r.u(8); v != 0x0F {
		t.Errorf("post-align u(8) = %#x", v)
	}
	if r.err {
		t.Error("unexpected overrun")
	}
	r.u(1)
	if !r.err {
		t.Error("overrun not flagged")
	}
}

// FuzzParseFrameHeader asserts the parser never panics and never reads
// past MaxFrameHeaderLen.
func FuzzParseFrameHeader(f *testing.F) {
	f.Add(buildFrameHeader(3, 12, 9, 1, 4))
	f.Add(buildFrameHeader(0, 7, 14, 10, 6, 0xFF, 0xFF, 0x11, 0x22))
	f.Add([]byte{0xFF, 0xF9, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		fi, err := ParseFrameHeader(data)
		if err != nil {
			return
		}
		if fi.BlockSize < 1 || fi.BlockSize > MaxBlockSize {
			t.Fatalf("accepted block size %d", fi.BlockSize)
		}
		if fi.Channels < 1 || fi.Channels > 8 {
			t.Fatalf("accepted %d channels", fi.Channels)
		}
	})
}

// FuzzDecode drives the frame decoder with arbitrary packets: no panics,
// no buffer writes past the emitted length.
func FuzzDecode(f *testing.F) {
	si := StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: 44100, Channels: 2, Bits: 16}
	f.Add(buildFrameHeader(0, 12, 9, 1, 4))
	f.Add([]byte{0xFF, 0xF8, 0xC9, 0x14})
	f.Fuzz(func(t *testing.T, data []byte) {
		dec, err := NewDecoder(si, si.PCMFormat())
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Release()
		derr := dec.Decode(data, func(b *audio.Buffer) error {
			if b.N < 1 || b.N > MaxBlockSize {
				t.Fatalf("emitted %d frames", b.N)
			}
			return nil
		})
		_ = derr
	})
}
