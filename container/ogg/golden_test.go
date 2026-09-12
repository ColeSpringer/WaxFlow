package ogg

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files (make goldens)")

// TestGoldenMuxOutputs pins the muxer's byte-exact output, which is what the
// ADR-0004 MuxerVersion stands for: every byte of the page layout, the comment
// headers and the gapless granules. Regenerate with `make goldens` and review
// the diff. Each case says what it pins; all three are deterministic on every
// architecture (the Vorbis case re-muxes a committed libvorbis stream rather
// than running the arch-pinned encoder).
func TestGoldenMuxOutputs(t *testing.T) {
	t.Run("golden-opus.opus", func(t *testing.T) {
		testutil.Golden(t, filepath.Join("testdata", "golden-opus.opus"), goldenOpus(t), *update)
	})
	t.Run("golden-flac.oga", func(t *testing.T) {
		testutil.Golden(t, filepath.Join("testdata", "golden-flac.oga"), goldenFLAC(t), *update)
	})
	t.Run("golden-vorbis.ogg", func(t *testing.T) {
		testutil.Golden(t, filepath.Join("testdata", "golden-vorbis.ogg"), goldenVorbis(t), *update)
	})
}

// goldenOpus muxes canned Opus packets in four phases, so the golden pins
// every flush rule and both sides of the byte target:
//
//   - 256-byte packets fill a page exactly at 4096 bytes, so LOWERING the
//     target by one moves a page boundary;
//   - 241-byte packets overshoot it by one at 4097, so RAISING it by one moves
//     a boundary too (no single packet size catches both directions, since
//     4096 and 4097 share no factor);
//   - 15-byte packets fill the 255-entry segment table first;
//   - 1920-granule packets reach exactly 48000 granules on the 25th, so the
//     granule cap binds before the other two.
//
// It also pins the single-packet first audio page (TTFA), the comment-header
// layout with a skipped invalid key, and the EOS page's end-trim granule.
//
// The payloads are canned rather than encoded, but each phase's TOC byte
// declares the frame duration that phase stamps, so the granulepos timeline
// the muxer builds and the timing a reader derives from the packets agree.
// TestGoldenOpusIsSelfConsistent holds that.
func goldenOpus(t *testing.T) []byte {
	t.Helper()
	const preSkip = 312
	// toc packs a TOC byte: config in the top five bits, the stereo bit, and
	// frame code 0 (one frame per packet). The config's frame length is the
	// packet's duration, so it has to match dur.
	phases := []struct {
		packets, size int
		toc           byte
		dur           int64
	}{
		{40, 256, 31<<3 | 0x4, 960}, // CELT FB 20 ms: the 4096-byte target
		{40, 241, 31<<3 | 0x4, 960}, // the same, one byte past it
		{600, 15, 28<<3 | 0x4, 120}, // CELT FB 2.5 ms: the 255-segment table
		{60, 10, 2<<3 | 0x4, 1920},  // SILK NB 40 ms: the 48000-granule cap
	}
	tags := []container.Tag{
		{Key: "TITLE", Value: "Golden"},
		{Key: "ARTIST", Value: "First"},
		{Key: "ARTIST", Value: "Second"},
		{Key: "BAD=KEY", Value: "skipped"}, // '=' is not a legal field name
	}
	var out bytes.Buffer
	m := NewMuxer(&out, &MuxerOptions{Tags: tags})
	track := container.Track{Codec: codec.Opus, CodecConfig: muxOpusHead(preSkip)}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, ph := range phases {
		for i := 0; i < ph.packets; i++ {
			data := make([]byte, ph.size)
			data[0] = ph.toc
			for j := 1; j < ph.size; j++ {
				data[j] = byte(i + j)
			}
			pkt := container.Packet{Packet: codec.Packet{Data: data, Dur: ph.dur, Sync: true}}
			if err := m.WritePacket(pkt); err != nil {
				t.Fatal(err)
			}
			total += ph.dur
		}
	}
	if err := m.End(codec.Trailer{Samples: total - preSkip - 100, Delay: preSkip}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestGoldenOpusIsSelfConsistent reads the committed golden back with this
// package's own demuxer, which times a packet from its TOC rather than from
// what the muxer was told. A golden whose granulepos timeline disagreed with
// its packets would be a stream no reader could make sense of, committed as if
// it were one.
func TestGoldenOpusIsSelfConsistent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden-opus.opus"))
	if err != nil {
		t.Fatal(err)
	}
	track, pkts := demuxAll(t, raw)
	if len(pkts) != 40+40+600+60 {
		t.Fatalf("demuxed %d packets, muxed %d", len(pkts), 40+40+600+60)
	}
	want := int64(40*960 + 40*960 + 600*120 + 60*1920)
	var got int64
	for i, p := range pkts {
		if p.pts != got {
			t.Fatalf("packet %d starts at %d, want %d", i, p.pts, got)
		}
		got += p.dur
	}
	if got != want {
		t.Errorf("packets total %d samples, the muxer was told %d", got, want)
	}
	if track.Samples != want-312-100 {
		t.Errorf("track length %d, want the end-trimmed %d", track.Samples, want-312-100)
	}
}

// goldenFLAC encodes a sine through the real FLAC encoder and frames it as
// Ogg FLAC, pinning the 0x7F"FLAC" identification BOS page, the 0x84 comment
// block and the final page's granule (the sample total, since FLAC self-times).
// The encoder is integer-only and deterministic, so the bytes are the same on
// every architecture.
func goldenFLAC(t *testing.T) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	src := testutil.Sine(f, 4410, 440, 0.5)
	defer audio.Put(src)

	enc, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: 5})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	m := NewMuxer(&out, &MuxerOptions{Tags: []container.Tag{{Key: "TITLE", Value: "Golden"}}})
	track := container.Track{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error {
		return m.WritePacket(container.Packet{Packet: p})
	}
	chunk := audio.Get(f, enc.FrameSize())
	defer audio.Put(chunk)
	for off := 0; off < src.N; off += enc.FrameSize() {
		n := min(enc.FrameSize(), src.N-off)
		audio.CopyFrames(chunk, 0, src, off, n)
		chunk.N = n
		if err := enc.Encode(chunk, emit); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(tr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// goldenVorbis re-muxes a committed libvorbis stream: this package's demuxer
// supplies the CodecConfig and the audio packets, and the muxer frames them
// again. That keeps the golden architecture-independent, since our own Vorbis
// encoder is arch-pinned (goldenEncodeArch) and must not feed a golden. It pins
// the id-alone BOS page, the comment-plus-setup header page with its framing
// bit, the (prev+cur)/4 granule rule with its zero first increment, and the
// end granule. The input fixture is never touched by -update.
//
// Fixture: ffmpeg 8.0.1
//
//	ffmpeg -f lavfi -i sine=frequency=440:sample_rate=8000:duration=0.25 \
//	  -c:a libvorbis -q:a 0 libvorbis-8k-mono.ogg
func goldenVorbis(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "libvorbis-8k-mono.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	track, pkts := demuxAll(t, raw)
	if track.Codec != codec.Vorbis || len(pkts) == 0 {
		t.Fatalf("fixture demuxed as %q with %d packets", track.Codec, len(pkts))
	}
	var out bytes.Buffer
	m := NewMuxer(&out, &MuxerOptions{Tags: []container.Tag{{Key: "TITLE", Value: "Golden"}}})
	remux := container.Track{Codec: codec.Vorbis, CodecConfig: track.CodecConfig, Fmt: track.Fmt}
	if err := m.Begin([]container.Track{remux}); err != nil {
		t.Fatal(err)
	}
	for i, p := range pkts {
		pkt := container.Packet{Packet: codec.Packet{Data: p.data, Dur: p.dur, Sync: true}}
		if err := m.WritePacket(pkt); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(codec.Trailer{Samples: track.Samples}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
