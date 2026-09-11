//go:build !wmavoicetablesgen

package wmavoice_test

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/wmavoice"
	"github.com/colespringer/waxflow/waxerr"
)

// Every shape this build refuses, each by name, and beside it every shape it
// must NOT refuse. The second list is the one that costs a user a playable
// file if it goes wrong, so it is as long as the first
// (docs/notes/wma-voice-bitstream.md sections 14.8 and 14.9).

// TestConfigRefusals covers what is refused before a bit of bitstream is read.
func TestConfigRefusals(t *testing.T) {
	base := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses())
	for _, tc := range []struct {
		name string
		mut  func([]byte)
		want string
		code waxerr.Code
	}{
		{"the wrong wFormatTag", func(b []byte) { binary.LittleEndian.PutUint16(b, 0x0161) },
			"not WMA Voice", waxerr.CodeMalformedInput},
		{"the second registered tag", func(b []byte) { binary.LittleEndian.PutUint16(b, 0x000B) },
			"Voice 10", waxerr.CodeUnsupportedFormat},
		{"a cbSize other than 46 over 46 extra bytes", func(b []byte) { binary.LittleEndian.PutUint16(b[16:], 0) },
			"cbSize declares 0", waxerr.CodeMalformedInput},
		{"two channels", func(b []byte) { binary.LittleEndian.PutUint16(b[2:], 2) },
			"mono", waxerr.CodeUnsupportedFormat},
		{"a sample rate below the floor", func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 300) },
			"this format admits 322 to 22097", waxerr.CodeUnsupportedFormat},
		{"a sample rate above the ceiling", func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 24000) },
			"this format admits 322 to 22097", waxerr.CodeUnsupportedFormat},
		{"a rate whose delta-pitch field has no width", func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 7000) },
			"delta-pitch half-range", waxerr.CodeUnsupportedFormat},
		{"nBlockAlign of zero", func(b []byte) { binary.LittleEndian.PutUint16(b[12:], 0) },
			"nBlockAlign 0", waxerr.CodeMalformedInput},
		{"a denoise strength the table has no row for", func(b []byte) {
			f := binary.LittleEndian.Uint32(b[18+18:])
			binary.LittleEndian.PutUint32(b[18+18:], f&^0x3C|(12<<2))
		}, "denoise strength 12", waxerr.CodeUnsupportedFormat},
		{"a variable bit mode class with four frame types", func(b []byte) {
			// A class is a codeword LENGTH and each length has three
			// codewords, so a fourth entry has nowhere to go; the reference
			// lets it overwrite the next class's first slot.
			var w bitWriter
			for i := range 17 {
				c := i / 3
				if i < 4 {
					c = 0
				}
				w.put(uint64(c), 3)
			}
			copy(b[18+22:], w.buf)
		}, "holds more than 3 frame types", waxerr.CodeUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base.config()
			tc.mut(b)
			_, err := wmavoice.ParseConfig(b)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != tc.code {
				t.Errorf("code %q, want %q", code, tc.code)
			}
		})
	}
}

// TestExtradataSizeIsExact: this format defines 46 extra bytes and no other
// size, so a short or long blob is damage rather than a shape this build does
// not cover. It is also what turns a WMA v1 or v2 header retyped as Voice into
// a damage report instead of "unsupported".
func TestExtradataSizeIsExact(t *testing.T) {
	base := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses()).config()
	for _, n := range []int{0, 10, 18, 45, 47, 64} {
		b := append([]byte(nil), base[:18]...)
		b = append(b, make([]byte, n)...)
		binary.LittleEndian.PutUint16(b[16:], uint16(n))
		_, err := wmavoice.ParseConfig(b)
		if err == nil {
			t.Errorf("%d extra bytes accepted", n)
			continue
		}
		if !strings.Contains(err.Error(), "want exactly 46") {
			t.Errorf("%d extra bytes: error %q does not name the size", n, err)
		}
		if code := waxerr.CodeOf(err); code != waxerr.CodeMalformedInput {
			t.Errorf("%d extra bytes: code %q, want damage", n, code)
		}
	}
}

// TestHandBuiltConfigIsValidated: a Config a caller builds rather than parses
// reaches NewDecoder unchecked, and every geometry is derived from it. A zero
// rate would be a division by zero three calls later rather than a refusal.
func TestHandBuiltConfigIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  wmavoice.Config
	}{
		{"zero", wmavoice.Config{}},
		{"no rate", wmavoice.Config{BlockAlign: 450}},
		{"no nBlockAlign", wmavoice.Config{Rate: 16000}},
		{"an absurd nBlockAlign", wmavoice.Config{Rate: 16000, BlockAlign: 1 << 23}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wmavoice.NewDecoder(tc.cfg, tc.cfg.Format()); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestDecoderRefusesAFormatThatIsNotItsOwn: the container passes the track's
// format so a disagreement is caught here rather than by a downstream stage.
func TestDecoderRefusesAFormatThatIsNotItsOwn(t *testing.T) {
	s := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses())
	fm := s.cfg.Format()
	fm.Channels = 2
	if _, err := wmavoice.NewDecoder(s.cfg, fm); err == nil {
		t.Fatal("accepted a stereo format for a codec that is mono by construction")
	}
}

// TestStreamRefusals covers what is refused mid-stream, each from a hand-built
// packet because Windows' encoder writes none of them.
func TestStreamRefusals(t *testing.T) {
	s := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses())
	for _, tc := range []struct {
		name string
		pkt  func() []byte
		want string
	}{
		{"a superframe whose speech/music bit is clear", func() []byte {
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			w.put(2, 6)
			w.put(0, wmavoice.SpilloverBits(s.cfg))
			w.put(0, 1) // a WMA Pro payload
			return w.pad(s.align)
		}, "WMA Pro payload"},
		{"a superframe declaring more than 480 samples", func() []byte {
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			w.put(2, 6)
			w.put(0, wmavoice.SpilloverBits(s.cfg))
			w.put(1, 1)
			w.put(1, 1)
			w.put(481, 12)
			return w.pad(s.align)
		}, "declares 481 samples"},
		{"a frame type symbol the tree does not map", func() []byte {
			// Every class holds three types under defaultClasses, so symbols
			// 17 up are unmapped; the fourteen-bit codeword 0b11111111111100
			// is symbol 18.
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			w.put(2, 6)
			w.put(0, wmavoice.SpilloverBits(s.cfg))
			w.put(1, 1)
			w.put(0, 1)
			s.residualLSPs(&w)
			w.put(0x3FFC, 14)
			return w.pad(s.align)
		}, "is not in this stream's variable bit mode tree"},
		{"a packet too short for its own header", func() []byte {
			return []byte{0x00}
		}, "runs past the packet"},
		{"a superframe count run that outruns the packet", func() []byte {
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			// Escapes to the end of a packet this small.
			for range 8 {
				w.put(63, 6)
			}
			return w.buf
		}, "runs past the packet"},
		{"a spillover longer than the packet holds", func() []byte {
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			w.put(2, 6)
			w.put(1<<wmavoice.SpilloverBits(s.cfg)-1, wmavoice.SpilloverBits(s.cfg))
			return w.pad(20)
		}, "spillover bits"},
		{"a superframe whose fields outrun the bits available", func() []byte {
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			w.put(2, 6)
			w.put(0, wmavoice.SpilloverBits(s.cfg))
			w.put(1, 1)
			w.put(0, 1)
			return w.buf // stops mid-LSP-block
		}, "reads past the end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := s.decoder(t)
			defer dec.Release()
			_, err := decodeStream(t, dec, tc.pkt())
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestPacketLongerThanBlockAlignIsRefused: nothing has been read from it, so
// nothing has drifted; it is a container shape rather than a stream failure.
func TestPacketLongerThanBlockAlignIsRefused(t *testing.T) {
	s := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses())
	dec := s.decoder(t)
	defer dec.Release()
	_, err := decodeStream(t, dec, make([]byte, s.align+1))
	if err == nil || !strings.Contains(err.Error(), "longer than nBlockAlign") {
		t.Fatalf("error %v does not name the packet length", err)
	}
}

// TestNotRefused is the other half, and the one that costs a playable file if
// it goes wrong. None of these is damage.
func TestNotRefused(t *testing.T) {
	s := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses())
	for _, tc := range []struct {
		name string
		pkts func() [][]byte
	}{
		{"a packet whose count is 1, so its body decodes nothing", func() [][]byte {
			return [][]byte{s.packet(0, 1), s.packet(0, 1)}
		}},
		{"a spillover of zero with a carry pending", func() [][]byte {
			return [][]byte{s.packet(0, 2), s.packet(0, 2)}
		}},
		{"a spillover of zero with no carry pending", func() [][]byte {
			return [][]byte{s.packet(0, 2)}
		}},
		{"a packet shorter than nBlockAlign", func() [][]byte {
			p := s.packet(0, 1)
			return [][]byte{p[:len(p)-8]}
		}},
		{"a statistics block being present", func() [][]byte {
			var w bitWriter
			w.put(0, 4)
			w.put(1, 1)
			w.put(2, 6)
			w.put(0, wmavoice.SpilloverBits(s.cfg))
			// One superframe with a statistics block for the body, then a
			// plain one for the carry.
			w.put(1, 1)
			w.put(0, 1)
			s.residualLSPs(&w)
			for range wmavoice.FramesPerSuperframe {
				s.silenceFrame(&w, 128)
			}
			w.put(1, 1) // present
			w.put(3, 4) // 40 bits
			w.put(0, 40)
			s.silentSuperframe(&w, -1)
			return [][]byte{w.pad(s.align)}
		}},
		{"a sequence number that jumps", func() [][]byte {
			a, b := s.packet(0, 1), s.packet(0, 1)
			b[0] = b[0]&0x0F | 0x70
			return [][]byte{a, b}
		}},
		{"an empty packet, which flushes the carry", func() [][]byte {
			return [][]byte{s.packet(0, 1), {}, s.packet(0, 1)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := s.decoder(t)
			defer dec.Release()
			if _, err := decodeStream(t, dec, tc.pkts()...); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// TestEmitFailureDoesNotLatch: a caller's own failure is not the stream's, and
// the decoder carries on from the next packet.
func TestEmitFailureDoesNotLatch(t *testing.T) {
	s := newStream(t, 16000, 450, 0x0b1991a7, defaultClasses())
	dec := s.decoder(t)
	defer dec.Release()
	boom := errString("caller said no")
	err := dec.Decode(s.packet(0, 2), func(*audio.Buffer) error { return boom })
	if err != boom {
		t.Fatalf("error %v, want the caller's own", err)
	}
	n := 0
	if err := dec.Decode(s.packet(0, 2), func(*audio.Buffer) error { n++; return nil }); err != nil {
		t.Fatalf("the decoder latched: %v", err)
	}
	if n == 0 {
		t.Error("the decoder emitted nothing after the caller recovered")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
