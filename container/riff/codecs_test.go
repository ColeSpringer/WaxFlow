package riff

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// wavHeader builds a WAV around one fmt chunk and one data chunk, with an
// optional fact chunk. extra is the cbSize region a compressed tag states its
// geometry in; factSamples is written only when it is not negative.
func wavHeader(tag uint16, channels, rate, blockAlign, bits int, extra, payload []byte, factSamples int64) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	b.Write(u32le(0)) // patched below
	b.WriteString("WAVE")

	fmtPayload := new(bytes.Buffer)
	fmtPayload.Write(u16le(tag))
	fmtPayload.Write(u16le(uint16(channels)))
	fmtPayload.Write(u32le(uint32(rate)))
	fmtPayload.Write(u32le(uint32(rate * blockAlign)))
	fmtPayload.Write(u16le(uint16(blockAlign)))
	fmtPayload.Write(u16le(uint16(bits)))
	if extra != nil {
		fmtPayload.Write(u16le(uint16(len(extra))))
		fmtPayload.Write(extra)
	}
	b.WriteString("fmt ")
	b.Write(u32le(uint32(fmtPayload.Len())))
	b.Write(fmtPayload.Bytes())
	if fmtPayload.Len()%2 == 1 {
		b.WriteByte(0)
	}
	if factSamples >= 0 {
		b.WriteString("fact")
		b.Write(u32le(4))
		b.Write(u32le(uint32(factSamples)))
	}
	b.WriteString("data")
	b.Write(u32le(uint32(len(payload))))
	b.Write(payload)
	if len(payload)%2 == 1 {
		b.WriteByte(0)
	}
	raw := b.Bytes()
	le.PutUint32(raw[4:], uint32(len(raw)-8))
	return raw
}

func u16le(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

func u32le(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// imaExtra and msExtra are the cbSize regions the two ADPCM tags define.
func imaExtra(samplesPerBlock int) []byte { return u16le(uint16(samplesPerBlock)) }

func msExtra(samplesPerBlock int, coefs [7][2]int16) []byte {
	b := append(u16le(uint16(samplesPerBlock)), u16le(7)...)
	for _, pair := range coefs {
		b = append(b, u16le(uint16(pair[0]))...)
		b = append(b, u16le(uint16(pair[1]))...)
	}
	return b
}

// TestG711Tags reads the two companding laws out of a fmt chunk. One byte is
// one sample per channel, so the payload's length is the sample count and the
// packet walk is the PCM walk.
func TestG711Tags(t *testing.T) {
	for _, tc := range []struct {
		tag  uint16
		want codec.ID
	}{{tagALaw, codec.ALaw}, {tagMuLaw, codec.MuLaw}} {
		t.Run(string(tc.want), func(t *testing.T) {
			payload := []byte{0xD5, 0x55, 0xFF, 0x7F, 0x00, 0x80}
			raw := wavHeader(tc.tag, 2, 8000, 2, 8, nil, payload, -1)
			track, data, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if track.Codec != tc.want {
				t.Fatalf("codec = %q, want %q", track.Codec, tc.want)
			}
			if len(track.CodecConfig) != 0 {
				t.Errorf("codec config = %v, want none: the law is the codec ID", track.CodecConfig)
			}
			want := audio.Format{Rate: 8000, Channels: 2, Layout: audio.DefaultLayout(2),
				Type: audio.Int, BitDepth: 16}
			if track.Fmt != want {
				t.Errorf("format = %v, want %v", track.Fmt, want)
			}
			if track.Samples != 3 || track.SourceBitDepth != 8 {
				t.Errorf("samples = %d, source depth = %d; want 3 and 8", track.Samples, track.SourceBitDepth)
			}
			if !bytes.Equal(data, payload) {
				t.Error("the packet walk did not deliver the payload")
			}
			if len(warns) != 0 {
				t.Errorf("warnings on a well-formed header: %v", warns)
			}
		})
	}
}

// TestG711WidthIsNotTheFieldsToState pins that a header declaring another
// width is damage rather than a different stream: a companding law has one
// width, so the bytes are where 8 bits say they are either way.
func TestG711WidthIsNotTheFieldsToState(t *testing.T) {
	raw := wavHeader(tagALaw, 1, 8000, 1, 16, nil, []byte{0xD5, 0x55}, -1)
	track, _, warns := demuxAll(t, container.BytesSource(raw), nil)
	if track.Samples != 2 {
		t.Errorf("samples = %d, want 2", track.Samples)
	}
	if len(warns) == 0 {
		t.Error("a 16-bit A-law header must warn")
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict demux of a 16-bit A-law header must fail")
	}
}

// TestADPCMTags reads both block codecs, and pins the geometry each block is
// walked at.
func TestADPCMTags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tag        uint16
		blockAlign int
		channels   int
		extra      func(spb int) []byte
		want       adpcm.Config
		codecID    codec.ID
	}{{
		name: "ima-mono", tag: tagIMAADPCM, blockAlign: 20, channels: 1, extra: imaExtra,
		want:    adpcm.Config{Layout: adpcm.IMAWav, Channels: 1, BlockAlign: 20, SamplesPerBlock: 33},
		codecID: codec.IMAADPCM,
	}, {
		name: "ima-stereo", tag: tagIMAADPCM, blockAlign: 24, channels: 2, extra: imaExtra,
		want:    adpcm.Config{Layout: adpcm.IMAWav, Channels: 2, BlockAlign: 24, SamplesPerBlock: 17},
		codecID: codec.IMAADPCM,
	}, {
		name: "ms-mono", tag: tagMSADPCM, blockAlign: 20, channels: 1,
		extra:   func(spb int) []byte { return msExtra(spb, adpcm.DefaultCoefs) },
		want:    adpcm.Config{Layout: adpcm.MS, Channels: 1, BlockAlign: 20, SamplesPerBlock: 28, Coefs: adpcm.DefaultCoefs},
		codecID: codec.MSADPCM,
	}, {
		name: "ms-stereo", tag: tagMSADPCM, blockAlign: 32, channels: 2,
		extra:   func(spb int) []byte { return msExtra(spb, adpcm.DefaultCoefs) },
		want:    adpcm.Config{Layout: adpcm.MS, Channels: 2, BlockAlign: 32, SamplesPerBlock: 20, Coefs: adpcm.DefaultCoefs},
		codecID: codec.MSADPCM,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := make([]byte, tc.blockAlign*3)
			for i := range payload {
				payload[i] = byte(i)
			}
			raw := wavHeader(tc.tag, tc.channels, 8000, tc.blockAlign, 4,
				tc.extra(tc.want.SamplesPerBlock), payload, -1)
			track, data, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if track.Codec != tc.codecID {
				t.Fatalf("codec = %q, want %q", track.Codec, tc.codecID)
			}
			cfg, err := adpcm.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if cfg != tc.want {
				t.Errorf("config = %+v, want %+v", cfg, tc.want)
			}
			if want := int64(3 * tc.want.SamplesPerBlock); track.Samples != want {
				t.Errorf("samples = %d, want %d", track.Samples, want)
			}
			if track.SourceBitDepth != adpcm.SourceBitDepth {
				t.Errorf("source depth = %d, want %d", track.SourceBitDepth, adpcm.SourceBitDepth)
			}
			if !bytes.Equal(data, payload) {
				t.Error("the packet walk did not deliver the payload in block order")
			}
			if len(warns) != 0 {
				t.Errorf("warnings on a well-formed header: %v", warns)
			}
		})
	}
}

// TestFactChunk pins what the sample count does per codec: it is the only
// statement of a block codec's true length, a cross-check for a byte-linear
// one, and ignored where PCM's payload already says the same thing.
func TestFactChunk(t *testing.T) {
	const blockAlign, spb = 20, 33
	blocks := func(n int) []byte { return make([]byte, blockAlign*n) }

	t.Run("trims a block codec", func(t *testing.T) {
		raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), 100)
		track, _, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
		if track.Samples != 100 {
			t.Errorf("samples = %d, want the fact chunk's 100", track.Samples)
		}
		if !track.SamplesExact {
			t.Error("a fact-trimmed length is a truncation instruction and must be exact")
		}
		if len(warns) != 0 {
			t.Errorf("warnings: %v", warns)
		}
		// The walk still delivers whole blocks; the trim is the decoder's.
		if _, data, _ := demuxAll(t, container.BytesSource(raw), nil); len(data) != blockAlign*4 {
			t.Errorf("the walk delivered %d bytes, want all %d", len(data), blockAlign*4)
		}
	})

	t.Run("absent leaves the capacity", func(t *testing.T) {
		raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), -1)
		track, _, _ := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
		if track.Samples != 4*spb {
			t.Errorf("samples = %d, want the capacity %d", track.Samples, 4*spb)
		}
		if !track.SamplesExact {
			t.Error("a capacity a read delivers exactly is exact")
		}
	})

	// A count more than one block short of the payload is not a truncation
	// any encoder could have written: it disclaims blocks the file coded.
	// Believing it would empty a track on a zero or unpatched field. The
	// capacity stays, and stays exact: the read delivers exactly that many.
	t.Run("far below the capacity is ignored", func(t *testing.T) {
		for _, fact := range []int64{0, 3, 99} {
			raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), fact)
			track, _, warns := demuxAll(t, container.BytesSource(raw), nil)
			if track.Samples != 4*spb {
				t.Errorf("fact %d was believed: %d samples, want the capacity %d", fact, track.Samples, 4*spb)
			}
			if !track.SamplesExact {
				t.Errorf("fact %d left the capacity inexact", fact)
			}
			if len(warns) == 0 {
				t.Errorf("fact %d passed without a warning", fact)
			}
		}
		// One block short is the boundary: still ignored.
		raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), int64(3*spb))
		if track, _, _ := demuxAll(t, container.BytesSource(raw), nil); track.Samples != 4*spb || !track.SamplesExact {
			t.Errorf("a count exactly one block short gave %d samples exact = %v, want the capacity %d and exact",
				track.Samples, track.SamplesExact, 4*spb)
		}
		// One sample inside the last block is a real truncation.
		raw = wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), int64(3*spb+1))
		track, _, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
		if track.Samples != int64(3*spb+1) || !track.SamplesExact {
			t.Errorf("samples = %d exact = %v, want %d and exact", track.Samples, track.SamplesExact, 3*spb+1)
		}
		if len(warns) != 0 {
			t.Errorf("warnings on a real truncation: %v", warns)
		}
	})

	t.Run("oversized is clamped", func(t *testing.T) {
		raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), 1000)
		track, _, warns := demuxAll(t, container.BytesSource(raw), nil)
		if track.Samples != 4*spb {
			t.Errorf("samples = %d, want the capacity %d", track.Samples, 4*spb)
		}
		if !track.SamplesExact {
			t.Error("a clamped count is the capacity, which is exact")
		}
		if len(warns) == 0 {
			t.Error("a fact chunk past the capacity must warn")
		}
		if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
			t.Error("strict demux of an oversized fact must fail")
		}
	})

	t.Run("cross-checks a byte-linear codec", func(t *testing.T) {
		raw := wavHeader(tagALaw, 1, 8000, 1, 8, nil, make([]byte, 10), 7)
		track, _, warns := demuxAll(t, container.BytesSource(raw), nil)
		if track.Samples != 10 {
			t.Errorf("samples = %d; a byte-linear payload states its own length", track.Samples)
		}
		if len(warns) == 0 {
			t.Error("a fact chunk disagreeing with the payload must warn")
		}
	})

	// The format specification makes the chunk required for every tag but
	// PCM and optional for that one, so a PCM file's is a field writers fill
	// in when they feel like it. Believing it, or even naming it, would put
	// "input damage" on ordinary files whose payload says the right thing.
	t.Run("ignored for PCM", func(t *testing.T) {
		raw := wavHeader(tagPCM, 1, 8000, 2, 16, nil, make([]byte, 20), 3)
		track, _, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
		if track.Samples != 10 {
			t.Errorf("samples = %d, want 10", track.Samples)
		}
		if len(warns) != 0 {
			t.Errorf("warnings = %v; a PCM fact chunk is not read", warns)
		}
	})

	// Not read means not judged either: the chunk is optional for PCM, so a
	// writer that emits an empty one has written a legal file and must not
	// be refused for it.
	t.Run("a zero-length fact chunk is not damage", func(t *testing.T) {
		raw := wavHeader(tagPCM, 1, 8000, 2, 16, nil, make([]byte, 20), -1)
		at := bytes.Index(raw, []byte("data"))
		out := append([]byte(nil), raw[:at]...)
		out = append(out, []byte("fact")...)
		out = append(out, u32le(0)...)
		out = append(out, raw[at:]...)
		le.PutUint32(out[4:], uint32(len(out)-8))
		track, _, warns := demuxAll(t, container.BytesSource(out), &DemuxerOptions{Strict: true})
		if track.Samples != 10 {
			t.Errorf("samples = %d, want 10", track.Samples)
		}
		if len(warns) != 0 {
			t.Errorf("warnings = %v", warns)
		}
	})

	// The field is authoritative for a block codec, so the first one wins
	// rather than the last, which is what idFmt and idData already do.
	t.Run("a duplicate is ignored", func(t *testing.T) {
		raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), blocks(4), int64(3*spb+5))
		at := bytes.Index(raw, []byte("data"))
		out := append([]byte(nil), raw[:at]...)
		out = append(out, []byte("fact")...)
		out = append(out, u32le(4)...)
		out = append(out, u32le(1)...)
		out = append(out, raw[at:]...)
		le.PutUint32(out[4:], uint32(len(out)-8))
		track, _, _ := demuxAll(t, container.BytesSource(out), nil)
		if track.Samples != int64(3*spb+5) {
			t.Errorf("samples = %d, want the first fact's %d", track.Samples, 3*spb+5)
		}
	})
}

// TestLengthIsExact pins the claim a block codec's track makes about its own
// length, at the sample level: every packet the walk delivers is decoded, and
// the frames that come out must number exactly Samples. That is the
// probe-equals-read invariant the FLAC and WavPack readers carry, and it is
// what lets a caller that needs an authoritative length take the number
// instead of decoding the file to find one. A fact chunk that was ignored
// changes nothing, since the capacity is what the read delivers either way.
func TestLengthIsExact(t *testing.T) {
	const blockAlign = 20
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"ima no fact", wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(33), make([]byte, blockAlign*4), -1)},
		{"ima ignored fact", wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(33), make([]byte, blockAlign*4), 3)},
		{"ms no fact", wavHeader(tagMSADPCM, 1, 8000, blockAlign, 4, msExtra(28, adpcm.DefaultCoefs), make([]byte, blockAlign*4), -1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(tc.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			track := d.Tracks()[0]
			if !track.SamplesExact {
				t.Error("the track does not claim an exact length")
			}
			cfg, err := adpcm.ParseConfig(track.CodecConfig)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := adpcm.NewDecoder(cfg, track.Fmt)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Release()
			var frames int64
			count := func(b *audio.Buffer) error { frames += int64(b.N); return nil }
			var pkt container.Packet
			for {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := dec.Decode(pkt.Data, count); err != nil {
					t.Fatal(err)
				}
			}
			if err := dec.Drain(count); err != nil {
				t.Fatal(err)
			}
			if frames != track.Samples {
				t.Errorf("the decode delivered %d frames, the track claims %d exactly", frames, track.Samples)
			}
		})
	}
}

// TestTrailingPartialBlock pins what a data chunk that stops mid-block does:
// the same thing a PCM payload that stops mid-frame does, which is warn and
// leave the fragment out of the walk.
func TestTrailingPartialBlock(t *testing.T) {
	const blockAlign, spb = 20, 33
	raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), make([]byte, blockAlign*2+7), -1)
	track, data, warns := demuxAll(t, container.BytesSource(raw), nil)
	if track.Samples != 2*spb {
		t.Errorf("samples = %d, want the two whole blocks' %d", track.Samples, 2*spb)
	}
	if len(data) != blockAlign*2 {
		t.Errorf("the walk delivered %d bytes, want %d", len(data), blockAlign*2)
	}
	if len(warns) == 0 || !strings.Contains(warns[0].Msg, "block") {
		t.Errorf("warnings = %v, want one naming the block boundary", warns)
	}
}

// TestSamplesPerBlockIsDerived pins that the declared count loses to the
// layout: the decoder's loops are written to the derivation, so a header
// stating another number is describing a block boundary its payload does not
// have.
func TestSamplesPerBlockIsDerived(t *testing.T) {
	raw := wavHeader(tagIMAADPCM, 1, 8000, 20, 4, imaExtra(99), make([]byte, 40), -1)
	track, _, warns := demuxAll(t, container.BytesSource(raw), nil)
	cfg, err := adpcm.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SamplesPerBlock != 33 {
		t.Errorf("samples per block = %d, want the derived 33", cfg.SamplesPerBlock)
	}
	if len(warns) == 0 {
		t.Error("a declared count that disagrees with the layout must warn")
	}
}

// TestNonStandardCoefTableIsANote pins decision 13's visible half: the file's
// own predictor table is used, and because readers that ignore it decode such
// a file differently, that is worth saying. A Note and not damage: the file is
// well formed and the specification says its table is the one to use, so
// --strict must not refuse it.
func TestNonStandardCoefTableIsANote(t *testing.T) {
	odd := adpcm.DefaultCoefs
	odd[3] = [2]int16{300, -100}
	raw := wavHeader(tagMSADPCM, 1, 8000, 20, 4, msExtra(28, odd), make([]byte, 40), -1)
	track, _, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
	cfg, err := adpcm.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Coefs != odd {
		t.Errorf("coefficients = %v, want the file's %v", cfg.Coefs, odd)
	}
	if len(warns) != 1 || warns[0].Kind != container.Note {
		t.Fatalf("warnings = %v, want one Note", warns)
	}
	if !strings.Contains(warns[0].Msg, "coefficient table") {
		t.Errorf("note %q does not name the table", warns[0].Msg)
	}
}

// TestCompressedTagRefusals covers headers that are well formed as chunks and
// state a geometry this reader will not decode, or state one impossibly.
func TestCompressedTagRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		code waxerr.Code
		want string
	}{{
		name: "ima-without-extra",
		raw:  wavHeader(tagIMAADPCM, 1, 8000, 20, 4, nil, make([]byte, 20), -1),
		code: waxerr.CodeMalformedInput, want: "no samples per block",
	}, {
		name: "ms-without-a-coefficient-count",
		raw:  wavHeader(tagMSADPCM, 1, 8000, 20, 4, u16le(28), make([]byte, 20), -1),
		code: waxerr.CodeMalformedInput, want: "no coefficient count",
	}, {
		name: "ms-with-four-coefficient-pairs",
		raw: wavHeader(tagMSADPCM, 1, 8000, 20, 4,
			append(append(u16le(28), u16le(4)...), make([]byte, 16)...), make([]byte, 20), -1),
		code: waxerr.CodeUnsupportedFormat, want: "4 predictor coefficient pairs",
	}, {
		name: "ms-with-a-truncated-table",
		raw: wavHeader(tagMSADPCM, 1, 8000, 20, 4,
			append(append(u16le(28), u16le(7)...), make([]byte, 8)...), make([]byte, 20), -1),
		code: waxerr.CodeMalformedInput, want: "coefficient pairs",
	}, {
		name: "block-shorter-than-its-header",
		raw:  wavHeader(tagIMAADPCM, 2, 8000, 4, 4, imaExtra(1), make([]byte, 8), -1),
		code: waxerr.CodeMalformedInput, want: "too small for the 8 bytes",
	}, {
		name: "ms-beyond-stereo",
		raw:  wavHeader(tagMSADPCM, 3, 8000, 60, 4, msExtra(28, adpcm.DefaultCoefs), make([]byte, 60), -1),
		code: waxerr.CodeUnsupportedFormat, want: "alternate between at most 2",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDemuxer(container.BytesSource(tc.raw), nil)
			if got := waxerr.CodeOf(err); got != tc.code {
				t.Fatalf("error = %v (code %v), want %v", err, got, tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v does not name %q", err, tc.want)
			}
		})
	}
}

// TestExtensibleRefusesABlockCodec pins the one place the extensible wrapper
// and a block codec cannot coexist: the extra bytes ARE the extensible
// struct, so the block geometry has nowhere left to live.
//
// The container width is 8 and not 4 because the wrapper's own rule bites
// first at 4 (it requires whole bytes), and that refusal would pass this test
// for the wrong reason.
func TestExtensibleRefusesABlockCodec(t *testing.T) {
	extra := make([]byte, 22)
	le.PutUint16(extra, 8)          // valid bits
	le.PutUint16(extra[6:], 0x0011) // the sub-format tag
	copy(extra[8:], guidTail[:])
	raw := wavHeader(tagExtensible, 1, 8000, 20, 8, extra, make([]byte, 20), -1)
	_, err := NewDemuxer(container.BytesSource(raw), nil)
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Fatalf("error = %v, want unsupported-format", err)
	}
	if !strings.Contains(err.Error(), "WAVE_FORMAT_EXTENSIBLE") {
		t.Errorf("%v does not name the wrapper", err)
	}
}

// TestBlockSeekLandsOnABlock pins the landing rule: a block codec's unit is
// several samples, so the demuxer lands on the block at or before the target
// and format.Media discards the remainder. A target past the end lands on the
// last block the TRACK has, not on the one the payload has: the two differ
// once a fact chunk trims the length, and a landing past the track's end is a
// position its callers cannot use.
func TestBlockSeekLandsOnABlock(t *testing.T) {
	const blockAlign, spb = 20, 33
	raw := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), make([]byte, blockAlign*4), -1)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A fact-trimmed sibling, where the payload holds a block the track does
	// not: every landing must still be one the track has.
	trimmed := wavHeader(tagIMAADPCM, 1, 8000, blockAlign, 4, imaExtra(spb), make([]byte, blockAlign*4), int64(3*spb+1))
	td, err := NewDemuxer(container.BytesSource(trimmed), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := td.SeekSample(0, 1<<20); got > td.Tracks()[0].Samples {
		t.Errorf("a past-the-end seek landed at %d, past the track's %d", got, td.Tracks()[0].Samples)
	}
	for _, tc := range []struct{ target, want int64 }{
		{0, 0}, {1, 0}, {32, 0}, {33, 33}, {70, 66}, {1 << 20, 4 * spb},
	} {
		got, err := d.SeekSample(0, tc.target)
		if err != nil {
			t.Fatalf("SeekSample(%d): %v", tc.target, err)
		}
		if got != tc.want {
			t.Errorf("SeekSample(%d) landed at %d, want %d", tc.target, got, tc.want)
		}
	}
}

// TestGeometryDefectsCarryTheContainersName pins that a defect in a WAV
// reports as a WAV's. The block rules live in a shared package so the two
// containers that state the same geometry cannot drift, and the first version
// of that sharing let the codec package's own error through, so a bad fmt
// chunk told the user about "adpcm:", a package the request never named.
func TestGeometryDefectsCarryTheContainersName(t *testing.T) {
	for _, raw := range [][]byte{
		wavHeader(tagIMAADPCM, 2, 8000, 4, 4, imaExtra(1), make([]byte, 8), -1),
		wavHeader(tagMSADPCM, 2, 8000, 8, 4, msExtra(2, adpcm.DefaultCoefs), make([]byte, 16), -1),
		// The other shapes the shared package can refuse: a channel count
		// past what MS ADPCM's alternating nibbles spell, and one past what
		// the pipeline carries at all.
		wavHeader(tagMSADPCM, 3, 8000, 60, 4, msExtra(28, adpcm.DefaultCoefs), make([]byte, 60), -1),
		wavHeader(tagIMAADPCM, 100, 8000, 1024, 4, imaExtra(9), make([]byte, 1024), -1),
	} {
		_, err := NewDemuxer(container.BytesSource(raw), nil)
		if err == nil {
			t.Fatal("a block too small for its header was accepted")
		}
		if !strings.HasPrefix(err.Error(), "wav: ") {
			t.Errorf("error %q does not carry the container's name", err)
		}
		if strings.Contains(err.Error(), "adpcm:") {
			t.Errorf("error %q names the codec package", err)
		}
	}
}
