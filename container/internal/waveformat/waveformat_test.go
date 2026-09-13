package waveformat

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/waxerr"
)

// ex builds a WAVEFORMATEX: the fixed header, then cbSize and the extra
// bytes when extra is not nil. cbSize is len(extra) unless declared is set,
// which is how a header that lies about its own extra is built.
func ex(tag uint16, channels, rate, blockAlign, bits int, extra []byte, declared int) []byte {
	b := make([]byte, headerLen)
	le := binary.LittleEndian
	le.PutUint16(b, tag)
	le.PutUint16(b[2:], uint16(channels))
	le.PutUint32(b[4:], uint32(rate))
	le.PutUint32(b[8:], uint32(rate*blockAlign))
	le.PutUint16(b[12:], uint16(blockAlign))
	le.PutUint16(b[14:], uint16(bits))
	if extra == nil {
		return b
	}
	n := len(extra)
	if declared >= 0 {
		n = declared
	}
	b = binary.LittleEndian.AppendUint16(b, uint16(n))
	return append(b, extra...)
}

func imaExtra(spb int) []byte {
	return binary.LittleEndian.AppendUint16(nil, uint16(spb))
}

func msExtra(spb int, coefs [7][2]int16) []byte {
	b := binary.LittleEndian.AppendUint16(imaExtra(spb), 7)
	for _, pair := range coefs {
		b = binary.LittleEndian.AppendUint16(b, uint16(pair[0]))
		b = binary.LittleEndian.AppendUint16(b, uint16(pair[1]))
	}
	return b
}

func TestParse(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		for _, n := range []int{0, 1, headerLen - 1} {
			if _, ok := Parse(make([]byte, n)); ok {
				t.Errorf("%d bytes parsed as a WAVEFORMATEX", n)
			}
		}
	})

	t.Run("fields", func(t *testing.T) {
		got, ok := Parse(ex(TagIMAADPCM, 2, 44100, 1024, 4, imaExtra(1017), -1))
		if !ok {
			t.Fatal("a full header did not parse")
		}
		want := Ex{Tag: TagIMAADPCM, Channels: 2, Rate: 44100, BlockAlign: 1024, Bits: 4}
		if got.Tag != want.Tag || got.Channels != want.Channels || got.Rate != want.Rate ||
			got.BlockAlign != want.BlockAlign || got.Bits != want.Bits {
			t.Errorf("fields = %+v, wanted %+v before the extra", got, want)
		}
		if len(got.Extra) != 2 {
			t.Errorf("extra = %v, want the two samples-per-block bytes", got.Extra)
		}
	})

	// cbSize is the file's own number, so the extra is bounded by what the
	// buffer holds and not by what the field claims; without that a header
	// declaring 4096 bytes of extra it does not carry reads past its end.
	t.Run("cbSize is clamped to the buffer", func(t *testing.T) {
		got, ok := Parse(ex(TagMSADPCM, 1, 8000, 1024, 4, imaExtra(2036), 4096))
		if !ok {
			t.Fatal("a header with an oversized cbSize did not parse")
		}
		if len(got.Extra) != 2 {
			t.Errorf("extra of %d bytes from a 2-byte tail", len(got.Extra))
		}
	})

	t.Run("no cbSize at all", func(t *testing.T) {
		got, ok := Parse(ex(1, 2, 44100, 4, 16, nil, -1))
		if !ok {
			t.Fatal("a bare 16-byte header did not parse")
		}
		if len(got.Extra) != 0 {
			t.Errorf("extra = %v, want none", got.Extra)
		}
	})
}

func TestADPCMGeometry(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want adpcm.Config
	}{{
		name: "ima mono",
		raw:  ex(TagIMAADPCM, 1, 8000, 1024, 4, imaExtra(2041), -1),
		want: adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 1024, SamplesPerBlock: 2041},
	}, {
		name: "ima stereo",
		raw:  ex(TagIMAADPCM, 2, 44100, 1024, 4, imaExtra(1017), -1),
		want: adpcm.Config{Layout: adpcm.IMAWav, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1017},
	}, {
		name: "ms mono",
		raw:  ex(TagMSADPCM, 1, 8000, 1024, 4, msExtra(2036, adpcm.DefaultCoefs), -1),
		want: adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 1024, SamplesPerBlock: 2036, Coefs: adpcm.DefaultCoefs},
	}, {
		name: "ms stereo",
		raw:  ex(TagMSADPCM, 2, 44100, 1024, 4, msExtra(1012, adpcm.DefaultCoefs), -1),
		want: adpcm.Config{Layout: adpcm.MS, Channels: 2, BlockAlign: 1024, SamplesPerBlock: 1012, Coefs: adpcm.DefaultCoefs},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := Parse(tc.raw)
			if !ok {
				t.Fatal("header did not parse")
			}
			cfg, found, err := ADPCM(parsed, "test: ")
			if err != nil {
				t.Fatalf("ADPCM: %v", err)
			}
			if len(found) != 0 {
				t.Errorf("findings on a well-formed header: %v", found)
			}
			if cfg != tc.want {
				t.Errorf("config = %+v, want %+v", cfg, tc.want)
			}
		})
	}
}

// TestADPCMFindings pins what comes back as a finding rather than an error:
// the caller reports these at its own offset, and the Note/damage split is
// what decides whether Strict refuses.
func TestADPCMFindings(t *testing.T) {
	odd := adpcm.DefaultCoefs
	odd[0] = [2]int16{300, -100}
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
		note bool
	}{{
		name: "samples per block disagrees",
		raw:  ex(TagIMAADPCM, 1, 8000, 1024, 4, imaExtra(99), -1),
		want: "declares 99 samples per block",
	}, {
		name: "bits per sample is not four",
		raw:  ex(TagIMAADPCM, 1, 8000, 1024, 16, imaExtra(2041), -1),
		want: "16 bits per sample",
	}, {
		name: "coefficient table is not the conventional one",
		raw:  ex(TagMSADPCM, 1, 8000, 1024, 4, msExtra(2036, odd), -1),
		want: "coefficient table", note: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, _ := Parse(tc.raw)
			cfg, found, err := ADPCM(parsed, "test: ")
			if err != nil {
				t.Fatalf("ADPCM: %v", err)
			}
			if len(found) != 1 {
				t.Fatalf("findings = %v, want one", found)
			}
			if !strings.Contains(found[0].Msg, tc.want) {
				t.Errorf("finding %q does not name %q", found[0].Msg, tc.want)
			}
			if found[0].Note != tc.note {
				t.Errorf("finding Note = %v, want %v", found[0].Note, tc.note)
			}
			// A finding is not a refusal: the geometry still comes back
			// usable, derived rather than believed.
			if cfg.SamplesPerBlock != cfg.BlockFrames() {
				t.Errorf("samples per block = %d, want the derived %d", cfg.SamplesPerBlock, cfg.BlockFrames())
			}
		})
	}
}

func TestADPCMRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		code waxerr.Code
		want string
	}{{
		name: "no extra bytes",
		raw:  ex(TagIMAADPCM, 1, 8000, 1024, 4, nil, -1),
		code: waxerr.CodeMalformedInput, want: "no samples per block",
	}, {
		name: "ms with no coefficient count",
		raw:  ex(TagMSADPCM, 1, 8000, 1024, 4, imaExtra(2036), -1),
		code: waxerr.CodeMalformedInput, want: "no coefficient count",
	}, {
		name: "ms with four coefficient pairs",
		raw: ex(TagMSADPCM, 1, 8000, 1024, 4,
			append(binary.LittleEndian.AppendUint16(imaExtra(2036), 4), make([]byte, 16)...), -1),
		code: waxerr.CodeUnsupportedFormat, want: "4 predictor coefficient pairs",
	}, {
		name: "ms with a truncated table",
		raw: ex(TagMSADPCM, 1, 8000, 1024, 4,
			append(binary.LittleEndian.AppendUint16(imaExtra(2036), 7), make([]byte, 8)...), -1),
		code: waxerr.CodeMalformedInput, want: "2 of its 7 coefficient pairs",
	}, {
		// A block size that cannot hold the header its own tag defines is
		// two fields of one header contradicting each other, which is damage
		// rather than a shape this build declines.
		name: "block shorter than its header",
		raw:  ex(TagIMAADPCM, 2, 8000, 4, 4, imaExtra(1), -1),
		code: waxerr.CodeMalformedInput, want: "too small for the 8 bytes",
	}, {
		name: "ms beyond stereo",
		raw:  ex(TagMSADPCM, 3, 8000, 1024, 4, msExtra(676, adpcm.DefaultCoefs), -1),
		code: waxerr.CodeUnsupportedFormat, want: "alternate between at most 2",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := Parse(tc.raw)
			if !ok {
				t.Fatal("header did not parse")
			}
			_, _, err := ADPCM(parsed, "test: ")
			if got := waxerr.CodeOf(err); got != tc.code {
				t.Fatalf("error = %v (code %v), want %v", err, got, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v does not name %q", err, tc.want)
			}
		})
	}
}
