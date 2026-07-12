package mka

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// muxToBytes runs a track and packets through the muxer and returns the file.
func muxToBytes(t *testing.T, track container.Track, opts *MuxerOptions, pkts []codec.Packet, trailer codec.Trailer) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewMuxer(&buf, opts)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, p := range pkts {
		if err := m.WritePacket(container.Packet{Track: 0, Packet: p}); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := m.End(trailer); err != nil {
		t.Fatalf("End: %v", err)
	}
	return buf.Bytes()
}

// readAll drains a demuxer's packets into copied payloads.
func readAll(t *testing.T, d *Demuxer) [][]byte {
	t.Helper()
	var out [][]byte
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, append([]byte(nil), pkt.Data...))
	}
}

// TestMuxPCMRoundTrip mux/demuxes raw PCM: payloads and format survive.
func TestMuxPCMRoundTrip(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	track := container.Track{Codec: codec.PCM, Fmt: f, Default: true}
	// Each packet is 480 stereo int16 frames (1920 bytes) of distinct data.
	var pkts []codec.Packet
	for i := 0; i < 6; i++ {
		data := make([]byte, 480*4)
		for j := range data {
			data[j] = byte(i*7 + j)
		}
		pkts = append(pkts, codec.Packet{Data: data, PTS: int64(i * 480), Dur: 480})
	}
	file := muxToBytes(t, track, nil, pkts, codec.Trailer{Samples: 6 * 480})

	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.PCM || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 {
		t.Errorf("track = %+v", tr)
	}
	got := readAll(t, d)
	if len(got) != len(pkts) {
		t.Fatalf("read %d packets, wrote %d", len(got), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i].Data) {
			t.Errorf("packet %d payload mismatch", i)
		}
	}
}

// TestMuxOpusGapless checks the Opus gapless round-trip: CodecDelay carries the
// pre-skip and the last block's DiscardPadding carries the tail trim, so the
// demuxer recovers Delay, Padding, and the exact sample count.
func TestMuxOpusGapless(t *testing.T) {
	const preSkip = 312
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1 // version
	head[9] = 2 // channels
	binary.LittleEndian.PutUint16(head[10:], preSkip)
	binary.LittleEndian.PutUint32(head[12:], 48000)
	// gain 0, family 0 already zero.

	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	track := container.Track{Codec: codec.Opus, CodecConfig: head, Fmt: f, Delay: preSkip, Default: true}

	// Five 480-sample packets (TOC 0x00 = SILK NB 10 ms, one frame).
	var pkts []codec.Packet
	for i := 0; i < 5; i++ {
		pkts = append(pkts, codec.Packet{Data: []byte{0x00, byte(i), 0x55}, Dur: 480, PTS: int64(i * 480)})
	}
	const padding = 88
	raw := int64(5 * 480)
	trailer := codec.Trailer{Samples: raw - preSkip - padding, Delay: preSkip, Padding: padding}

	file := muxToBytes(t, track, nil, pkts, trailer)
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	tr := d.Tracks()[0]
	if tr.Delay != preSkip {
		t.Errorf("Delay = %d, want %d", tr.Delay, preSkip)
	}
	if !tr.SamplesExact {
		t.Error("SamplesExact = false, want true for a CodecDelay track")
	}
	if want := raw - preSkip - padding; tr.Samples != want {
		t.Errorf("Samples = %d, want %d", tr.Samples, want)
	}
	if got := readAll(t, d); len(got) != len(pkts) {
		t.Errorf("read %d packets, wrote %d", len(got), len(pkts))
	}
}

// TestMuxDeterministic checks that equal inputs yield byte-identical output
// (fixed TrackUID, fixed app strings, no DateUTC).
func TestMuxDeterministic(t *testing.T) {
	f := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}
	track := container.Track{Codec: codec.PCM, Fmt: f, Default: true}
	pkts := []codec.Packet{{Data: bytes.Repeat([]byte{1, 2}, 256), Dur: 256, PTS: 0}}
	a := muxToBytes(t, track, nil, pkts, codec.Trailer{})
	b := muxToBytes(t, track, nil, pkts, codec.Trailer{})
	if !bytes.Equal(a, b) {
		t.Error("muxer output is not deterministic")
	}
}

// TestMuxWebMRejectsFLAC checks that a webm request for a codec webm does not
// carry is refused up front rather than emitting an invalid file.
func TestMuxWebMRejectsFLAC(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	si := make([]byte, 34) // shape does not matter; the webm check precedes it
	track := container.Track{Codec: codec.FLAC, CodecConfig: si, Fmt: f, Default: true}
	var buf bytes.Buffer
	m := NewMuxer(&buf, &MuxerOptions{WebM: true})
	if err := m.Begin([]container.Track{track}); err == nil {
		t.Error("webm + FLAC accepted; want rejection")
	}
}
