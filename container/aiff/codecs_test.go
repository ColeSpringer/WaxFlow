package aiff

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// buildAIFCn is buildAIFC with a channel count, which the compressed
// compression types need: their unit spans every channel.
func buildAIFCn(comp string, channels, bits int, payload []byte, frames uint32) []byte {
	return buildAIFCrate(comp, channels, bits, 8000, payload, frames)
}

// buildAIFCrate is buildAIFCn at a stated sample rate, which the compression
// types that carry their own framing need: their frames state a rate of their
// own, and a COMM that disagrees is a different test.
func buildAIFCrate(comp string, channels, bits, rate int, payload []byte, frames uint32) []byte {
	var b bytes.Buffer
	b.WriteString("FORM")
	b.Write(u32be(0)) // patched below
	b.WriteString("AIFC")
	b.WriteString("FVER")
	b.Write(u32be(4))
	b.Write(u32be(fverTimestamp))
	b.WriteString("COMM")
	b.Write(u32be(24))
	b.Write(u16be(uint16(channels)))
	b.Write(u32be(frames))
	b.Write(u16be(uint16(bits)))
	ext := toExt80(float64(rate))
	b.Write(ext[:])
	b.WriteString(comp)
	b.Write([]byte{0, 0})
	b.WriteString("SSND")
	b.Write(u32be(uint32(8 + len(payload))))
	b.Write(u32be(0))
	b.Write(u32be(0))
	b.Write(payload)
	raw := b.Bytes()
	be.PutUint32(raw[4:], uint32(len(raw)-8))
	return raw
}

// TestG711CompressionTypes reads the two companding laws. COMM's sampleSize
// is the DECODED width for a compressed type and the writers disagree about
// it (ffmpeg writes 8, QuickTime 16), so both are accepted as they stand.
func TestG711CompressionTypes(t *testing.T) {
	for _, tc := range []struct {
		comp string
		want codec.ID
	}{{compALaw, codec.ALaw}, {compULaw, codec.MuLaw}} {
		for _, bits := range []int{8, 16} {
			t.Run(string(tc.want), func(t *testing.T) {
				payload := []byte{0xD5, 0x55, 0xFF, 0x7F}
				raw := buildAIFCn(tc.comp, 1, bits, payload, 4)
				track, data, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
				if track.Codec != tc.want {
					t.Fatalf("codec = %q, want %q", track.Codec, tc.want)
				}
				want := audio.Format{Rate: 8000, Channels: 1, Layout: audio.DefaultLayout(1),
					Type: audio.Int, BitDepth: 16}
				if track.Fmt != want {
					t.Errorf("format = %v, want %v", track.Fmt, want)
				}
				if track.Samples != 4 || track.SourceBitDepth != 8 {
					t.Errorf("samples = %d, source depth = %d; want 4 and 8", track.Samples, track.SourceBitDepth)
				}
				if !bytes.Equal(data, payload) {
					t.Error("the packet walk did not deliver the payload")
				}
				if len(warns) != 0 {
					t.Errorf("warnings on a well-formed COMM: %v", warns)
				}
			})
		}
	}
	t.Run("other widths warn", func(t *testing.T) {
		raw := buildAIFCn(compALaw, 1, 24, []byte{0xD5, 0x55}, 2)
		if _, _, warns := demuxAll(t, container.BytesSource(raw), nil); len(warns) == 0 {
			t.Error("a 24-bit A-law COMM must warn")
		}
	})
}

// TestIMA4CountsPackets pins the field that changes meaning with the
// compression type: numSampleFrames counts PACKETS for ima4, so a file
// declaring 3 holds 192 samples, not 3.
func TestIMA4CountsPackets(t *testing.T) {
	for _, channels := range []int{1, 2} {
		payload := make([]byte, adpcm.QuickTimeBlockBytes*channels*3)
		for i := range payload {
			payload[i] = byte(i)
		}
		raw := buildAIFCn(compIMA4, channels, 4, payload, 3)
		track, data, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
		if track.Codec != codec.IMAADPCM {
			t.Fatalf("codec = %q, want %q", track.Codec, codec.IMAADPCM)
		}
		if want := int64(3 * adpcm.QuickTimeBlockFrames); track.Samples != want {
			t.Errorf("samples = %d, want %d: COMM counts packets for ima4", track.Samples, want)
		}
		cfg, err := adpcm.ParseConfig(track.CodecConfig)
		if err != nil {
			t.Fatal(err)
		}
		want := adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: channels,
			BlockAlign: adpcm.QuickTimeBlockBytes * channels, SamplesPerBlock: adpcm.QuickTimeBlockFrames}
		if cfg != want {
			t.Errorf("config = %+v, want %+v", cfg, want)
		}
		if track.SourceBitDepth != adpcm.SourceBitDepth {
			t.Errorf("source depth = %d, want %d", track.SourceBitDepth, adpcm.SourceBitDepth)
		}
		if !bytes.Equal(data, payload) {
			t.Error("the packet walk did not deliver the payload in packet order")
		}
		if len(warns) != 0 {
			t.Errorf("warnings on a well-formed COMM: %v", warns)
		}
	}
}

// TestIMA4SeeksToTheStart pins the landing rule Apple's layout forces: its
// decoder carries a predictor across packets and a packet header restates
// only its top nine bits, so a decode begun at a packet never converges to the
// linear one. Landing at zero is the only exact answer; format.Media's
// pre-roll walks up to the target from there.
func TestIMA4SeeksToTheStart(t *testing.T) {
	raw := buildAIFCn(compIMA4, 1, 4, make([]byte, adpcm.QuickTimeBlockBytes*8), 8)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 64, 300, 1 << 20} {
		got, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("SeekSample(%d): %v", target, err)
		}
		if got != 0 {
			t.Errorf("SeekSample(%d) landed at %d, want 0", target, got)
		}
	}
	// A companded type in the same container lands exactly, so the assertion
	// above is about ima4 and not about the demuxer.
	g, err := NewDemuxer(container.BytesSource(buildAIFCn(compULaw, 1, 8, make([]byte, 500), 500)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := g.SeekSample(0, 321); err != nil || got != 321 {
		t.Errorf("mu-law SeekSample(321) = %d, %v; want an exact landing", got, err)
	}
}

// TestTrailingPartialPacket pins what an SSND that stops mid-packet does: the
// same thing one that stops mid-frame does.
func TestTrailingPartialPacket(t *testing.T) {
	raw := buildAIFCn(compIMA4, 1, 4, make([]byte, adpcm.QuickTimeBlockBytes*2+9), 3)
	track, data, warns := demuxAll(t, container.BytesSource(raw), nil)
	if want := int64(2 * adpcm.QuickTimeBlockFrames); track.Samples != want {
		t.Errorf("samples = %d, want the two whole packets' %d", track.Samples, want)
	}
	if len(data) != adpcm.QuickTimeBlockBytes*2 {
		t.Errorf("the walk delivered %d bytes, want %d", len(data), adpcm.QuickTimeBlockBytes*2)
	}
	if len(warns) == 0 || !strings.Contains(warns[0].Msg, "packet") {
		t.Errorf("warnings = %v, want one naming the packet boundary", warns)
	}
}

// TestWidthStatingTypes reads in24 and in32, whose fourcc names their width
// and whose sampleSize field is conventionally 16 whatever they hold.
func TestWidthStatingTypes(t *testing.T) {
	for _, tc := range []struct {
		comp string
		bits int
	}{{compIn24, 24}, {compIn32, 32}} {
		t.Run(tc.comp, func(t *testing.T) {
			payload := make([]byte, tc.bits/8*4)
			raw := buildAIFCn(tc.comp, 1, 16, payload, 4)
			track, data, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
			cfg, err := pcm.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			want := pcm.Config{Encoding: pcm.SignedInt, Bits: tc.bits, BigEndian: true}
			if !cfg.Equal(want) {
				t.Errorf("config = %+v, want %+v", cfg, want)
			}
			if track.Samples != 4 || len(data) != len(payload) {
				t.Errorf("samples = %d over %d bytes, want 4 over %d", track.Samples, len(data), len(payload))
			}
			if len(warns) != 0 {
				t.Errorf("warnings: %v", warns)
			}
		})
	}
}

// TestCompressionTypesFoldCase pins that a file spelling a compression type in
// capitals is the same file. Every reference reader folds, so matching exactly
// would refuse files that play elsewhere, and refuse them while naming the
// codec this package decodes.
func TestCompressionTypesFoldCase(t *testing.T) {
	for _, tc := range []struct {
		comp  string
		bits  int
		want  codec.ID
		bytes int
	}{
		{"SOWT", 16, codec.PCM, 8},
		{"NONE", 16, codec.PCM, 8},
		{"IN24", 16, codec.PCM, 12},
		{"ALAW", 8, codec.ALaw, 4},
		{"IMA4", 4, codec.IMAADPCM, adpcm.QuickTimeBlockBytes},
		{"FL32", 32, codec.PCM, 16},
	} {
		t.Run(tc.comp, func(t *testing.T) {
			frames := uint32(4)
			if tc.want == codec.IMAADPCM {
				frames = 1
			}
			raw := buildAIFCn(tc.comp, 1, tc.bits, make([]byte, tc.bytes), frames)
			track, _, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if track.Codec != tc.want {
				t.Errorf("codec = %q, want %q", track.Codec, tc.want)
			}
			if len(warns) != 0 {
				t.Errorf("warnings: %v", warns)
			}
		})
	}
}

// TestSowtIsLittleEndianAtEveryWidth pins the one thing folding must not lose:
// the byte order is the type's, not the case's.
func TestSowtIsLittleEndianAtEveryWidth(t *testing.T) {
	for _, comp := range []string{"sowt", "SOWT"} {
		raw := buildAIFCn(comp, 1, 16, []byte{0x02, 0x01}, 1)
		track, _, _ := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
		cfg, err := pcm.ParseConfig(track.CodecConfig)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BigEndian {
			t.Errorf("%s decoded big-endian", comp)
		}
	}
}

// TestCompressedTypeRefusals covers the types this build still declines, and
// the message each carries: a refusal of a well-formed file is the whole
// answer a caller gets, so it has to name what the file holds.
func TestCompressedTypeRefusals(t *testing.T) {
	for _, tc := range []struct{ comp, want string }{
		{"MAC3", `MAC3 (compression type "MAC3")`},
		{"MAC6", `MAC6 (compression type "MAC6")`},
		// No name for this one, so the type is the whole answer.
		{"QDM2", `compression type "QDM2"`},
	} {
		t.Run(tc.comp, func(t *testing.T) {
			_, err := NewDemuxer(container.BytesSource(buildAIFCn(tc.comp, 1, 16, nil, 0)), nil)
			if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
				t.Fatalf("error = %v, want unsupported-format", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v does not name %q", err, tc.want)
			}
		})
	}
}

// TestLengthIsExact pins the claim the track makes about its own length. The
// count is units the payload holds, clamped to what COMM declares, and the
// walk stops at exactly that many, so there is no room for the number and the
// decode to differ. Saying so is what keeps a caller that needs an
// authoritative length from decoding the file to find one, which for an ima4
// source is the whole file.
func TestLengthIsExact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     []byte
		samples int64
	}{
		{"pcm", buildAIFCn(compNONE, 1, 16, make([]byte, 20), 10), 10},
		{"ulaw", buildAIFCn(compULaw, 2, 8, make([]byte, 20), 10), 10},
		{"ima4", buildAIFCn(compIMA4, 1, 4, make([]byte, adpcm.QuickTimeBlockBytes*3), 3), 192},
		// COMM over-declares: the payload is the ceiling and the clamped
		// count is still exact.
		{"clamped", buildAIFCn(compIMA4, 1, 4, make([]byte, adpcm.QuickTimeBlockBytes*2), 3), 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track, data, _ := demuxAll(t, container.BytesSource(tc.raw), nil)
			if !track.SamplesExact {
				t.Error("the track does not claim an exact length")
			}
			if track.Samples != tc.samples {
				t.Errorf("samples = %d, want %d", track.Samples, tc.samples)
			}
			// The walk delivers exactly the units that count is made of.
			if want := int(track.Samples) / adpcm.QuickTimeBlockFrames * adpcm.QuickTimeBlockBytes; track.Codec == codec.IMAADPCM && len(data) != want {
				t.Errorf("the walk delivered %d bytes for %d samples, want %d", len(data), track.Samples, want)
			}
		})
	}
}
