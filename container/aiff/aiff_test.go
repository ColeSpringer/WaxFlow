package aiff

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

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

func wireBytes(cfg pcm.Config, channels, frames int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, cfg.BytesPerFrame(channels)*frames)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func muxAIFF(t *testing.T, w io.Writer, cfg pcm.Config, f audio.Format, wire []byte, announce int64) {
	t.Helper()
	frameBytes := cfg.BytesPerFrame(f.Channels)
	cfgBytes, err := cfg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	m := NewMuxer(w)
	track := container.Track{Codec: codec.PCM, CodecConfig: cfgBytes, Fmt: f, Samples: announce, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	frames := int64(len(wire) / frameBytes)
	half := frames / 2 * int64(frameBytes)
	pts := int64(0)
	for _, data := range [][]byte{wire[:half], wire[half:]} {
		if len(data) == 0 {
			continue
		}
		dur := int64(len(data) / frameBytes)
		err := m.WritePacket(container.Packet{Track: 0, Packet: codec.Packet{Data: data, PTS: pts, Dur: dur, Sync: true}})
		if err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		pts += dur
	}
	if err := m.End(codec.Trailer{Samples: frames}); err != nil {
		t.Fatalf("End: %v", err)
	}
}

func demuxAll(t *testing.T, src container.Source, opts *DemuxerOptions) (container.Track, []byte, []container.Warning) {
	t.Helper()
	d, err := NewDemuxer(src, opts)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	var data []byte
	var pkt container.Packet
	wantPTS := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if pkt.PTS != wantPTS || !pkt.Sync {
			t.Fatalf("packet pts=%d sync=%v, want pts=%d sync=true", pkt.PTS, pkt.Sync, wantPTS)
		}
		wantPTS += pkt.Dur
		data = append(data, pkt.Data...)
	}
	return d.Tracks()[0], data, d.Warnings()
}

func TestExt80RoundTrip(t *testing.T) {
	rates := []int{8000, 11025, 16000, 22050, 32000, 44100, 48000, 88200, 96000, 176400, 192000, 352800, 1}
	for _, rate := range rates {
		enc := toExt80(float64(rate))
		got := fromExt80(enc[:])
		if got != float64(rate) {
			t.Errorf("ext80 round trip of %d = %v", rate, got)
		}
	}
	// The canonical 44100 encoding, byte for byte.
	want := [10]byte{0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0}
	if got := toExt80(44100); got != want {
		t.Errorf("toExt80(44100) = % X, want % X", got, want)
	}
	if v := fromExt80(make([]byte, 10)); v != 0 {
		t.Errorf("fromExt80(zeros) = %v, want 0", v)
	}
	inf := [10]byte{0x7F, 0xFF, 0x80, 0, 0, 0, 0, 0, 0, 0}
	if v := fromExt80(inf[:]); !math.IsInf(v, 1) {
		t.Errorf("fromExt80(inf) = %v, want +Inf", v)
	}
}

var muxMatrix = []struct {
	name     string
	cfg      pcm.Config
	channels int
}{
	{"s8 mono", pcm.Config{Encoding: pcm.SignedInt, Bits: 8}, 1},
	{"s16be stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}, 2},
	{"s24be stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 24, BigEndian: true}, 2},
	{"s32be mono", pcm.Config{Encoding: pcm.SignedInt, Bits: 32, BigEndian: true}, 1},
	{"f32be stereo", pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}, 2},
	{"s16be 5.1", pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}, 6},
}

func TestMuxDemuxRoundTrip(t *testing.T) {
	for _, tt := range muxMatrix {
		for _, announce := range []int64{999, -1} {
			name := tt.name + " known"
			if announce < 0 {
				name = tt.name + " unknown"
			}
			t.Run(name, func(t *testing.T) {
				f := tt.cfg.PCMFormat(44100, tt.channels, audio.DefaultLayout(tt.channels))
				const frames = 999
				wire := wireBytes(tt.cfg, tt.channels, frames, 7)
				ws := &memWS{}
				muxAIFF(t, ws, tt.cfg, f, wire, announce)

				wantForm := "AIFF"
				if tt.cfg.Encoding == pcm.Float {
					wantForm = "AIFC"
				}
				if string(ws.b[8:12]) != wantForm {
					t.Fatalf("form = %q, want %q", ws.b[8:12], wantForm)
				}

				track, data, _ := demuxAll(t, container.BytesSource(ws.b), &DemuxerOptions{Strict: true})
				if track.Fmt != f {
					t.Errorf("format = %v, want %v", track.Fmt, f)
				}
				if track.Samples != frames {
					t.Errorf("samples = %d, want %d", track.Samples, frames)
				}
				gotCfg, err := pcm.ParseConfig(track.CodecConfig)
				if err != nil {
					t.Fatal(err)
				}
				if gotCfg != tt.cfg {
					t.Errorf("wire config = %+v, want %+v", gotCfg, tt.cfg)
				}
				if !bytes.Equal(data, wire) {
					t.Error("payload bytes differ after round trip")
				}
			})
		}
	}
}

// TestSowtAndRawDecode covers read-only compression types by hand-building
// headers the muxer never writes.
func TestSowtAndRawDecode(t *testing.T) {
	build := func(comp string, bits int, payload []byte, frames uint32) []byte {
		var b bytes.Buffer
		b.WriteString("FORM")
		b.Write(u32be(0)) // patched below
		b.WriteString("AIFC")
		b.WriteString("FVER")
		b.Write(u32be(4))
		b.Write(u32be(fverTimestamp))
		b.WriteString("COMM")
		b.Write(u32be(24))
		b.Write(u16be(1)) // mono
		b.Write(u32be(frames))
		b.Write(u16be(uint16(bits)))
		rate := toExt80(8000)
		b.Write(rate[:])
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

	t.Run("sowt", func(t *testing.T) {
		payload := []byte{0x02, 0x01, 0xFE, 0xFF} // 0x0102, -2 little-endian
		track, data, _ := demuxAll(t, container.BytesSource(build("sowt", 16, payload, 2)), &DemuxerOptions{Strict: true})
		cfg, _ := pcm.ParseConfig(track.CodecConfig)
		if cfg.BigEndian || cfg.Bits != 16 {
			t.Errorf("sowt config = %+v, want little-endian 16", cfg)
		}
		if !bytes.Equal(data, payload) {
			t.Error("sowt payload differs")
		}
	})

	t.Run("raw", func(t *testing.T) {
		payload := []byte{0x80, 0x00, 0xFF}
		track, _, _ := demuxAll(t, container.BytesSource(build("raw ", 8, payload, 3)), &DemuxerOptions{Strict: true})
		cfg, _ := pcm.ParseConfig(track.CodecConfig)
		if cfg.Encoding != pcm.UnsignedInt {
			t.Errorf("raw config = %+v, want unsigned", cfg)
		}
	})

	t.Run("unknown compression", func(t *testing.T) {
		_, err := NewDemuxer(container.BytesSource(build("ima4", 16, nil, 0)), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
			t.Errorf("ima4 error = %v, want unsupported-format", err)
		}
	})
}

func TestSeekSample(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}
	f := cfg.PCMFormat(8000, 2, audio.DefaultLayout(2))
	const frames = 9000
	wire := wireBytes(cfg, 2, frames, 4)
	ws := &memWS{}
	muxAIFF(t, ws, cfg, f, wire, frames)

	d, err := NewDemuxer(container.BytesSource(ws.b), nil)
	if err != nil {
		t.Fatal(err)
	}
	landed, err := d.SeekSample(0, 4321)
	if err != nil || landed != 4321 {
		t.Fatalf("SeekSample = %d, %v; want 4321, nil", landed, err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.PTS != 4321 || !bytes.Equal(pkt.Data, wire[4321*4:4321*4+len(pkt.Data)]) {
		t.Error("post-seek packet mismatch")
	}
	if landed, _ = d.SeekSample(0, frames+1); landed != frames {
		t.Errorf("past-end seek landed %d, want %d", landed, frames)
	}
	if err := d.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
		t.Errorf("read after past-end seek = %v, want EOF", err)
	}
}

// TestPartialTrailingFrameWarns covers SSND payloads holding whole frames
// plus a fragment: the fragment must surface as a warning (or a strict
// failure) even when the floored count matches what COMM declares.
func TestPartialTrailingFrameWarns(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("FORM")
	b.Write(u32be(0)) // patched below
	b.WriteString("AIFF")
	b.WriteString("COMM")
	b.Write(u32be(18))
	b.Write(u16be(1)) // mono s16be: 2-byte frames
	b.Write(u32be(2)) // COMM agrees with the floored frame count
	b.Write(u16be(16))
	rate := toExt80(8000)
	b.Write(rate[:])
	b.WriteString("SSND")
	b.Write(u32be(8 + 5)) // 2 whole frames + 1 trailing byte
	b.Write(u32be(0))
	b.Write(u32be(0))
	b.Write([]byte{1, 2, 3, 4, 5})
	b.WriteByte(0) // chunk pad
	raw := b.Bytes()
	be.PutUint32(raw[4:], uint32(len(raw)-8))

	track, _, warns := demuxAll(t, container.BytesSource(raw), nil)
	if track.Samples != 2 {
		t.Errorf("samples = %d, want 2", track.Samples)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w.Msg, "whole frame") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a partial-frame warning, got %v", warns)
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict demux of a partial trailing frame must fail")
	}
}

func TestCOMMFramesDisagreeWithSSND(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}
	f := cfg.PCMFormat(8000, 1, audio.DefaultLayout(1))
	wire := wireBytes(cfg, 1, 100, 5)
	ws := &memWS{}
	muxAIFF(t, ws, cfg, f, wire, 100)

	// Inflate the COMM frame count past what SSND holds.
	commOff := bytes.Index(ws.b, []byte("COMM"))
	be.PutUint32(ws.b[commOff+8+2:], 150)
	track, _, warns := demuxAll(t, container.BytesSource(ws.b), nil)
	if track.Samples != 100 {
		t.Errorf("samples = %d, want clamp to 100", track.Samples)
	}
	if len(warns) == 0 {
		t.Error("expected a clamp warning")
	}
	if _, err := NewDemuxer(container.BytesSource(ws.b), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict demux of disagreeing counts must fail")
	}
}

func TestMuxRejects(t *testing.T) {
	cfgBE, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}.MarshalBinary()
	cfgLE, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}.MarshalBinary()
	cfgU8, _ := pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}.MarshalBinary()
	cfg24in32, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24, BigEndian: true}.MarshalBinary()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	f8 := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 8}
	base := container.Track{Codec: codec.PCM, CodecConfig: cfgBE, Fmt: f, Samples: 10}

	t.Run("unseekable writer", func(t *testing.T) {
		var buf bytes.Buffer
		m := NewMuxer(&buf)
		if err := m.Begin([]container.Track{base}); err == nil {
			t.Error("Begin on a plain writer must fail")
		}
	})
	tests := []struct {
		name   string
		tracks []container.Track
	}{
		{"no tracks", nil},
		{"wrong codec", []container.Track{{Codec: codec.MP3, Fmt: f, Samples: 10}}},
		{"little endian", []container.Track{{Codec: codec.PCM, CodecConfig: cfgLE, Fmt: f, Samples: 10}}},
		{"unsigned", []container.Track{{Codec: codec.PCM, CodecConfig: cfgU8, Fmt: f8, Samples: 10}}},
		{"gapless trims", []container.Track{{Codec: codec.PCM, CodecConfig: cfgBE, Fmt: f, Samples: 10, Padding: 3}}},
		// COMM sampleSize implies storage of ceil(sampleSize/8) bytes, so
		// a wider container word (24 valid bits in 32-bit words) has no
		// AIFF representation and must be refused, not written misaligned.
		{"valid bits narrower than words", []container.Track{{Codec: codec.PCM, CodecConfig: cfg24in32,
			Fmt: audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 24}, Samples: 10}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := NewMuxer(&memWS{}).Begin(tt.tracks); err == nil {
				t.Error("Begin must fail")
			}
		})
	}
}

func TestNeedsSeek(t *testing.T) {
	if !NewMuxer(&memWS{}).NeedsSeek() {
		t.Error("AIFF muxer must require seeking")
	}
}

// TestWritePacketFailsFastPastSizeLimit simulates an output sitting just
// under AIFF's 4 GiB ceiling (white-box: bumping the write offset instead
// of writing 4 GiB) and expects the next packet to be refused up front,
// not gigabytes later at End.
func TestWritePacketFailsFastPastSizeLimit(t *testing.T) {
	cfgBE, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}.MarshalBinary()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	m := NewMuxer(&memWS{})
	track := container.Track{Codec: codec.PCM, CodecConfig: cfgBE, Fmt: f, Samples: -1}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	m.off = size32Max - 100
	pkt := container.Packet{Packet: codec.Packet{Data: make([]byte, 400), Dur: 100, Sync: true}}
	if err := m.WritePacket(pkt); !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("WritePacket past the limit = %v, want unsupported-format", err)
	}
}

func TestOddPayloadPads(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 8}
	f := cfg.PCMFormat(8000, 1, audio.DefaultLayout(1))
	wire := wireBytes(cfg, 1, 33, 6)
	ws := &memWS{}
	muxAIFF(t, ws, cfg, f, wire, 33)
	if len(ws.b)%2 != 0 {
		t.Errorf("file length %d is odd; SSND must be padded", len(ws.b))
	}
	track, data, _ := demuxAll(t, container.BytesSource(ws.b), &DemuxerOptions{Strict: true})
	if track.Samples != 33 || !bytes.Equal(data, wire) {
		t.Error("odd payload did not round-trip")
	}
}
