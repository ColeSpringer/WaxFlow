package riff

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

// memWS is an in-memory io.WriteSeeker for exercising the back-patch path.
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

// onlyWriter hides Seek from a writer, forcing the streaming path.
type onlyWriter struct{ w io.Writer }

func (o onlyWriter) Write(p []byte) (int, error) { return o.w.Write(p) }

// wireBytes builds deterministic raw wire data for a config.
func wireBytes(cfg pcm.Config, channels, frames int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, cfg.BytesPerFrame(channels)*frames)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	// Raw random bytes are not valid for every encoding (float NaN
	// payloads are fine to carry, sign-extension is what matters for
	// ints); the container never interprets them, so any bytes do.
	return b
}

// muxWAV writes a WAV holding wire, announcing announceSamples in the
// track (-1 for unknown length).
func muxWAV(t *testing.T, w io.Writer, cfg pcm.Config, f audio.Format, wire []byte, announceSamples int64, opts *MuxerOptions) {
	t.Helper()
	frameBytes := cfg.BytesPerFrame(f.Channels)
	cfgBytes, err := cfg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	m := NewMuxer(w, opts)
	track := container.Track{Codec: codec.PCM, CodecConfig: cfgBytes, Fmt: f, Samples: announceSamples, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	frames := int64(len(wire) / frameBytes)
	// Split into two packets to exercise multi-packet accounting.
	half := frames / 2 * int64(frameBytes)
	pkts := [][]byte{wire[:half], wire[half:]}
	pts := int64(0)
	for _, data := range pkts {
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

// demuxAll parses src and returns the track and concatenated packet data.
func demuxAll(t *testing.T, src container.Source, opts *DemuxerOptions) (container.Track, []byte, []container.Warning) {
	t.Helper()
	d, err := NewDemuxer(src, opts)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	tracks := d.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("Tracks() = %d, want 1", len(tracks))
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
		if pkt.PTS != wantPTS || !pkt.Sync || pkt.Track != 0 {
			t.Fatalf("packet pts=%d sync=%v track=%d, want pts=%d sync=true track=0", pkt.PTS, pkt.Sync, pkt.Track, wantPTS)
		}
		wantPTS += pkt.Dur
		data = append(data, pkt.Data...)
	}
	return tracks[0], data, d.Warnings()
}

var muxMatrix = []struct {
	name     string
	cfg      pcm.Config
	channels int
}{
	{"u8 mono", pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, 1},
	{"s16 stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, 2},
	{"s24 stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 2},
	{"s32 mono", pcm.Config{Encoding: pcm.SignedInt, Bits: 32}, 1},
	{"s24in32 stereo", pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24}, 2},
	{"f32 stereo", pcm.Config{Encoding: pcm.Float, Bits: 32}, 2},
	{"s24 5.1", pcm.Config{Encoding: pcm.SignedInt, Bits: 24}, 6},
}

func TestMuxDemuxRoundTrip(t *testing.T) {
	for _, tt := range muxMatrix {
		for _, mode := range []string{"seekable known", "seekable unknown", "stream known"} {
			t.Run(tt.name+" "+mode, func(t *testing.T) {
				f := tt.cfg.PCMFormat(44100, tt.channels, audio.DefaultLayout(tt.channels))
				const frames = 999
				wire := wireBytes(tt.cfg, tt.channels, frames, 7)

				var raw []byte
				announce := int64(frames)
				if mode == "seekable unknown" {
					announce = -1
				}
				if mode == "stream known" {
					var buf bytes.Buffer
					muxWAV(t, onlyWriter{&buf}, tt.cfg, f, wire, announce, nil)
					raw = buf.Bytes()
				} else {
					ws := &memWS{}
					muxWAV(t, ws, tt.cfg, f, wire, announce, nil)
					raw = ws.b
				}

				track, data, warns := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
				_ = warns
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

func TestRF64AutoWriteKnownLength(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	const frames = 500
	wire := wireBytes(cfg, 2, frames, 1)

	// A limit far below the payload forces the RF64 projection up front,
	// on both seekable and unseekable writers.
	opts := &MuxerOptions{SizeLimit: 512}
	for _, name := range []string{"seekable", "stream"} {
		t.Run(name, func(t *testing.T) {
			var raw []byte
			if name == "seekable" {
				ws := &memWS{}
				muxWAV(t, ws, cfg, f, wire, frames, opts)
				raw = ws.b
			} else {
				var buf bytes.Buffer
				muxWAV(t, onlyWriter{&buf}, cfg, f, wire, frames, opts)
				raw = buf.Bytes()
			}
			if string(raw[:4]) != "RF64" {
				t.Fatalf("header = %q, want RF64", raw[:4])
			}
			if string(raw[12:16]) != "ds64" {
				t.Fatalf("first chunk = %q, want ds64", raw[12:16])
			}
			track, data, _ := demuxAll(t, container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if track.Samples != frames {
				t.Errorf("samples = %d, want %d", track.Samples, frames)
			}
			if !bytes.Equal(data, wire) {
				t.Error("payload differs")
			}
		})
	}
}

func TestRF64RewriteUnknownLength(t *testing.T) {
	// Unknown length on a seekable writer starts as RIFF with a JUNK
	// reservation; crossing the limit must rewrite it to RF64 at End.
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	wire := wireBytes(cfg, 2, 500, 2)

	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wire, -1, &MuxerOptions{SizeLimit: 512})
	if string(ws.b[:4]) != "RF64" {
		t.Fatalf("header = %q, want RF64 after rewrite", ws.b[:4])
	}
	if string(ws.b[12:16]) != "ds64" {
		t.Fatalf("reservation = %q, want ds64 after rewrite", ws.b[12:16])
	}
	track, data, _ := demuxAll(t, container.BytesSource(ws.b), &DemuxerOptions{Strict: true})
	if track.Samples != 500 || !bytes.Equal(data, wire) {
		t.Error("RF64 rewrite did not round-trip")
	}

	// Same shape but under the limit: stays RIFF, JUNK stays JUNK.
	ws = &memWS{}
	muxWAV(t, ws, cfg, f, wire, -1, nil)
	if string(ws.b[:4]) != "RIFF" || string(ws.b[12:16]) != "JUNK" {
		t.Errorf("header/reservation = %q/%q, want RIFF/JUNK", ws.b[:4], ws.b[12:16])
	}
}

func TestStreamingUnknownLengthPlaceholders(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(44100, 1, audio.DefaultLayout(1))
	wire := wireBytes(cfg, 1, 100, 3)

	var buf bytes.Buffer
	muxWAV(t, onlyWriter{&buf}, cfg, f, wire, -1, nil)
	raw := buf.Bytes()
	if le.Uint32(raw[4:]) != 0xFFFFFFFF {
		t.Errorf("riff size = %#x, want streaming placeholder", le.Uint32(raw[4:]))
	}

	// Tolerant demux clamps to EOF with a warning; strict mode refuses.
	track, data, warns := demuxAll(t, container.BytesSource(raw), nil)
	if track.Samples != 100 || !bytes.Equal(data, wire) {
		t.Error("streaming WAV did not round-trip")
	}
	if len(warns) == 0 {
		t.Error("expected a streaming-size warning")
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict demux of streaming sizes must fail")
	}
}

func TestSeekSample(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(8000, 2, audio.DefaultLayout(2))
	const frames = 9000 // spans multiple packets
	wire := wireBytes(cfg, 2, frames, 4)
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wire, frames, nil)

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
	if pkt.PTS != 4321 {
		t.Errorf("post-seek PTS = %d, want 4321", pkt.PTS)
	}
	want := wire[4321*4 : 4321*4+len(pkt.Data)]
	if !bytes.Equal(pkt.Data, want) {
		t.Error("post-seek payload mismatch")
	}

	if landed, err = d.SeekSample(0, frames+50); err != nil || landed != frames {
		t.Errorf("past-end seek = %d, %v; want clamp to %d", landed, err, frames)
	}
	if err := d.ReadPacket(&pkt); !errors.Is(err, io.EOF) {
		t.Errorf("read after past-end seek = %v, want EOF", err)
	}
	if _, err := d.SeekSample(0, -1); err == nil {
		t.Error("negative seek must fail")
	}
	if _, err := d.SeekSample(1, 0); err == nil {
		t.Error("seek on unknown track must fail")
	}
}

func TestExtensibleLayoutRoundTrip(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 32, ValidBits: 24}
	layout := audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.LowFrequency | audio.SideLeft | audio.SideRight
	f := cfg.PCMFormat(96000, 6, layout)
	wire := wireBytes(cfg, 6, 64, 5)
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wire, 64, nil)

	track, data, _ := demuxAll(t, container.BytesSource(ws.b), &DemuxerOptions{Strict: true})
	if track.Fmt.Layout != layout {
		t.Errorf("layout = %v, want %v", track.Fmt.Layout, layout)
	}
	if track.Fmt.BitDepth != 24 {
		t.Errorf("bit depth = %d, want 24", track.Fmt.BitDepth)
	}
	if !bytes.Equal(data, wire) {
		t.Error("payload differs")
	}
}

func TestOddDataSizePads(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}
	f := cfg.PCMFormat(8000, 1, audio.DefaultLayout(1))
	wire := wireBytes(cfg, 1, 33, 6) // odd byte count
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wire, 33, nil)
	if len(ws.b)%2 != 0 {
		t.Errorf("file length %d is odd; data chunk must be padded", len(ws.b))
	}
	track, data, _ := demuxAll(t, container.BytesSource(ws.b), &DemuxerOptions{Strict: true})
	if track.Samples != 33 || !bytes.Equal(data, wire) {
		t.Error("odd-sized payload did not round-trip")
	}
}

func TestDemuxRejectsUnsupported(t *testing.T) {
	// Build a header for MS-ADPCM (tag 2): structurally valid, not PCM.
	ws := &memWS{}
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(8000, 1, audio.DefaultLayout(1))
	muxWAV(t, ws, cfg, f, wireBytes(cfg, 1, 10, 8), 10, nil)
	fmtOff := bytes.Index(ws.b, []byte("fmt "))
	le.PutUint16(ws.b[fmtOff+8:], 0x0002)

	_, err := NewDemuxer(container.BytesSource(ws.b), nil)
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("ADPCM demux error = %v, want unsupported-format", err)
	}

	if _, err := NewDemuxer(container.BytesSource([]byte("not a wav file at all")), nil); !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("garbage demux error = %v, want unsupported-format", err)
	}
}

func TestDemuxClampsTruncatedData(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(8000, 2, audio.DefaultLayout(2))
	wire := wireBytes(cfg, 2, 100, 9)
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wire, 100, nil)
	cut := ws.b[:len(ws.b)-37] // truncate mid-frame

	track, _, warns := demuxAll(t, container.BytesSource(cut), nil)
	if track.Samples >= 100 {
		t.Errorf("samples = %d, want fewer than 100 after truncation", track.Samples)
	}
	if len(warns) == 0 {
		t.Error("expected truncation warnings")
	}
	if _, err := NewDemuxer(container.BytesSource(cut), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict demux of truncated file must fail")
	}
}

// TestDS64HugeDataSizeClamped pins the 64-bit bound: a file-supplied ds64
// data size near MaxUint64 must clamp with a warning (or fail strict),
// never wrap negative past the bounds check.
func TestDS64HugeDataSizeClamped(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	wire := wireBytes(cfg, 2, 300, 21)
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wire, 300, &MuxerOptions{SizeLimit: 128}) // forces RF64
	if string(ws.b[:4]) != "RF64" {
		t.Fatal("fixture is not RF64")
	}
	// ds64 payload starts at offset 20: riffSize, then dataSize at +8.
	le.PutUint64(ws.b[20+8:], 0xFFFFFFFFFFFFFFF0)

	track, data, warns := demuxAll(t, container.BytesSource(ws.b), nil)
	if track.Samples != 300 || !bytes.Equal(data, wire) {
		t.Error("huge ds64 size did not clamp to the real payload")
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w.Msg, "exceeds file") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a clamp warning, got %v", warns)
	}
	if _, err := NewDemuxer(container.BytesSource(ws.b), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict demux of a lying ds64 must fail")
	}
}

// TestRateBoundIsPlatformIndependent pins that rates above MaxInt32 are
// rejected by an explicit int64 bound, not by int overflow that only
// 32-bit builds would hit.
func TestRateBoundIsPlatformIndependent(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 1, audio.DefaultLayout(1))
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wireBytes(cfg, 1, 10, 22), 10, nil)
	fmtOff := bytes.Index(ws.b, []byte("fmt "))
	le.PutUint32(ws.b[fmtOff+8+4:], 0x80000000) // nSamplesPerSec = 2^31

	_, err := NewDemuxer(container.BytesSource(ws.b), nil)
	if !errors.Is(err, waxerr.ErrUnsupportedFormat) {
		t.Errorf("2^31 rate error = %v, want unsupported-format", err)
	}
}

func TestMuxRejects(t *testing.T) {
	cfg16, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}.MarshalBinary()
	cfgBE, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 16, BigEndian: true}.MarshalBinary()
	cfgS8, _ := pcm.Config{Encoding: pcm.SignedInt, Bits: 8}.MarshalBinary()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	f8 := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 8}
	base := container.Track{Codec: codec.PCM, CodecConfig: cfg16, Fmt: f, Samples: 10}

	tests := []struct {
		name   string
		tracks []container.Track
	}{
		{"no tracks", nil},
		{"two tracks", []container.Track{base, base}},
		{"wrong codec", []container.Track{{Codec: codec.FLAC, Fmt: f, Samples: 10}}},
		{"gapless trims", []container.Track{{Codec: codec.PCM, CodecConfig: cfg16, Fmt: f, Samples: 10, Delay: 100}}},
		{"big endian", []container.Track{{Codec: codec.PCM, CodecConfig: cfgBE, Fmt: f, Samples: 10}}},
		{"signed 8-bit", []container.Track{{Codec: codec.PCM, CodecConfig: cfgS8, Fmt: f8, Samples: 10}}},
		// A length whose byte size overflows int64 must fail closed
		// instead of wrapping the RF64 projection negative.
		{"overflowing length", []container.Track{{Codec: codec.PCM, CodecConfig: cfg16, Fmt: f, Samples: math.MaxInt64/2 + 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMuxer(&memWS{}, nil)
			if err := m.Begin(tt.tracks); err == nil {
				t.Error("Begin must fail")
			}
		})
	}

	t.Run("trailer mismatch", func(t *testing.T) {
		m := NewMuxer(&memWS{}, nil)
		if err := m.Begin([]container.Track{base}); err != nil {
			t.Fatal(err)
		}
		pkt := container.Packet{Packet: codec.Packet{Data: make([]byte, 40), Dur: 10, Sync: true}}
		if err := m.WritePacket(pkt); err != nil {
			t.Fatal(err)
		}
		if err := m.End(codec.Trailer{Samples: 11}); err == nil {
			t.Error("End with wrong sample count must fail")
		}
	})

	t.Run("projection missed on stream", func(t *testing.T) {
		var buf bytes.Buffer
		m := NewMuxer(onlyWriter{&buf}, nil)
		if err := m.Begin([]container.Track{base}); err != nil {
			t.Fatal(err)
		}
		pkt := container.Packet{Packet: codec.Packet{Data: make([]byte, 36), Dur: 9, Sync: true}}
		if err := m.WritePacket(pkt); err != nil {
			t.Fatal(err)
		}
		if err := m.End(codec.Trailer{Samples: 9}); err == nil {
			t.Error("streamed headers promised 10 samples; End with 9 must fail")
		}
	})
}

func TestNeedsSeek(t *testing.T) {
	if NewMuxer(&memWS{}, nil).NeedsSeek() {
		t.Error("WAV muxer must not require seeking (it has a streaming form)")
	}
}
