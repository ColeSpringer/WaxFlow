package waxflow_test

// Engine-level ALAC coverage without an external oracle: transcode a known
// signal to ALAC-in-fragmented-MP4 through the whole engine, re-read the
// fragments with a compact standalone parser (independent of the production
// demuxer, which reads only non-fragmented movies), decode with our ALAC
// decoder, and assert bit-exact reconstruction. The ffmpeg differential in
// differential_test.go proves the same output is a real fMP4 a third-party
// tool decodes; this proves the engine path is lossless with no ffmpeg.

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

// fmp4Reader extracts the ALAC magic cookie and per-fragment samples from a
// fragmented MP4. It walks boxes by hand so it shares no code with the
// muxer under test.
type fmp4Reader struct {
	data []byte
}

func be32t(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// children iterates the boxes packed in body, calling fn(type, payload).
func children(body []byte, fn func(typ string, payload []byte)) {
	for len(body) >= 8 {
		size := int(be32t(body))
		if size < 8 || size > len(body) {
			return
		}
		fn(string(body[4:8]), body[8:size])
		body = body[size:]
	}
}

// find returns the payload of the first child of the given type.
func find(body []byte, typ string) []byte {
	var out []byte
	children(body, func(t string, p []byte) {
		if out == nil && t == typ {
			out = p
		}
	})
	return out
}

// cookie descends moov to the ALAC magic cookie.
func (r fmp4Reader) cookie(t *testing.T) []byte {
	t.Helper()
	moov := find(r.data, "moov")
	stsd := find(find(find(find(find(moov, "trak"), "mdia"), "minf"), "stbl"), "stsd")
	if len(stsd) < 8 {
		t.Fatal("no stsd")
	}
	entry := find(stsd[8:], "alac") // skip version/flags(4)+entry_count(4)
	if len(entry) < 28 {
		t.Fatal("no alac sample entry")
	}
	inner := find(entry[28:], "alac")
	if len(inner) < 28 {
		t.Fatalf("no alac cookie box (%d bytes)", len(inner))
	}
	return inner[4:] // skip the cookie box's version/flags
}

// samples returns every fragment's ALAC packets in order.
func (r fmp4Reader) samples(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	body := r.data
	for len(body) >= 8 {
		size := int(be32t(body))
		if size < 8 || size > len(body) {
			t.Fatalf("bad box size %d", size)
		}
		typ := string(body[4:8])
		if typ == "moof" {
			moof := body[8:size]
			trun := find(find(moof, "traf"), "trun")
			flags := be32t(trun) & 0xFFFFFF
			p := trun[4:]
			count := int(be32t(p))
			p = p[4:]
			if flags&0x1 != 0 {
				p = p[4:] // data_offset
			}
			// samples begin at the mdat payload following this moof.
			mdat := body[size:]
			if string(mdat[4:8]) != "mdat" {
				t.Fatalf("box after moof is %q", mdat[4:8])
			}
			data := mdat[8:int(be32t(mdat))]
			off := 0
			for i := 0; i < count; i++ {
				if flags&0x100 != 0 {
					p = p[4:] // duration
				}
				sz := int(be32t(p))
				p = p[4:]
				out = append(out, data[off:off+sz])
				off += sz
			}
		}
		body = body[size:]
	}
	return out
}

// decodeALAC transcodes src to ALAC through the engine and decodes it back.
func decodeALAC(t *testing.T, e *waxflow.Engine, wav []byte, wantFmt audio.Format, frames int) *audio.Buffer {
	t.Helper()
	out := &memWS{}
	res, err := e.Transcode(context.Background(), container.BytesSource(wav), "", out, waxflow.TranscodeOptions{Format: "alac"})
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if res.Format != wantFmt {
		t.Fatalf("output format %v, want %v", res.Format, wantFmt)
	}
	if res.Samples != int64(frames) {
		t.Fatalf("samples = %d, want %d", res.Samples, frames)
	}

	r := fmp4Reader{data: out.b}
	cfg, err := alac.ParseMagicCookie(r.cookie(t))
	if err != nil {
		t.Fatalf("cookie: %v", err)
	}
	if cfg.Format() != wantFmt {
		t.Fatalf("cookie format %v, want %v", cfg.Format(), wantFmt)
	}
	dec, err := alac.NewDecoder(cfg, wantFmt)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Release()

	got := audio.Get(wantFmt, frames)
	pos := 0
	for _, pkt := range r.samples(t) {
		if err := dec.Decode(pkt, func(b *audio.Buffer) error {
			for c := 0; c < wantFmt.Channels; c++ {
				copy(got.I[c*got.Stride+pos:c*got.Stride+pos+b.N], b.ChanI(c))
			}
			pos += b.N
			return nil
		}); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	if pos != frames {
		t.Fatalf("decoded %d samples, want %d", pos, frames)
	}
	got.N = pos
	return got
}

// TestTranscodeALACRoundTrips pins the ALAC output path to the lossless
// promise: integer sources survive WAV to ALAC and back bit for bit across
// depths and channel counts, and the depth-snapping rules (float to 24-bit,
// 8-bit widened to 16) hold.
func TestTranscodeALACRoundTrips(t *testing.T) {
	e := waxflow.New()
	const frames = 9111 // not a multiple of the encoder frame size

	t.Run("lossless depths", func(t *testing.T) {
		matrix := []struct {
			cfg      pcm.Config
			channels int
		}{
			{pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 1},
			{pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2},
			{pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 2},
			{pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, 2},
		}
		for _, tt := range matrix {
			f := tt.cfg.PCMFormat(48000, tt.channels, audio.DefaultLayout(tt.channels))
			t.Run(f.String(), func(t *testing.T) {
				wav, src := makeWAV(t, tt.cfg, tt.channels, frames, 41)
				defer audio.Put(src)
				got := decodeALAC(t, e, wav, f, frames)
				defer audio.Put(got)
				equalPCM(t, src, got)
			})
		}
	})

	// A float source has no lossless ALAC form; the engine quantizes to
	// 24-bit integers, and that 24-bit stream round-trips exactly.
	t.Run("f32 defaults to 24-bit", func(t *testing.T) {
		wav, src := makeWAV(t, pcm.Config{Encoding: pcm.Float, Bits: 32}, 2, frames, 43)
		defer audio.Put(src)
		got := decodeALAC(t, e, wav, audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}, frames)
		audio.Put(got)
	})

	// An 8-bit source is widened to 16-bit (ALAC's floor) losslessly.
	t.Run("u8 widens to 16-bit", func(t *testing.T) {
		wav, src := makeWAV(t, pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, 1, frames, 47)
		defer audio.Put(src)
		got := decodeALAC(t, e, wav, audio.Format{Rate: 48000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}, frames)
		audio.Put(got)
	})
}
