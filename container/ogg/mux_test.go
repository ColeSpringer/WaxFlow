package ogg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
)

// muxOpusHead builds a minimal stereo OpusHead for muxer tests.
func muxOpusHead(preSkip int) []byte {
	head := append([]byte("OpusHead"), 1, 2)
	head = binary.LittleEndian.AppendUint16(head, uint16(preSkip))
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = binary.LittleEndian.AppendUint16(head, 0)
	return append(head, 0)
}

// TestMuxerPageBatching pins the muxer's paging policy: the two header pages
// and the first audio page stay small (TTFA), later packets batch into large
// pages so framing overhead stays under one percent, the final page carries
// the end-trim granule with the EOS flag, and every packet round-trips
// byte-identically through our own demuxer in order.
func TestMuxerPageBatching(t *testing.T) {
	const (
		packets = 250
		pktLen  = 240 // ~96 kbit/s worth of 20 ms packets
		dur     = 960
		preSkip = 120
	)
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	track := container.Track{Codec: codec.Opus, CodecConfig: muxOpusHead(preSkip)}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	var want [][]byte
	payloadBytes := 0
	for i := 0; i < packets; i++ {
		data := make([]byte, pktLen)
		data[0] = byte(31<<3 | 0x4) // TOC: CELT FB 20 ms, stereo, code 0
		for j := 1; j < pktLen; j++ {
			data[j] = byte(i + j)
		}
		want = append(want, data)
		payloadBytes += pktLen
		if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: data, Dur: dur, Sync: true}}); err != nil {
			t.Fatal(err)
		}
	}
	samples := int64(packets*dur - preSkip - 100) // some end-trim
	if err := m.End(codec.Trailer{Samples: samples, Delay: preSkip}); err != nil {
		t.Fatal(err)
	}

	stream := out.Bytes()
	pages := bytes.Count(stream, []byte("OggS"))
	// 2 header pages + 1 single-packet first audio page + batched pages: with
	// a 4096-byte target and 240-byte packets, 17 packets per page.
	if pages > 2+1+packets/16 {
		t.Errorf("%d packets produced %d pages: batching is not engaging", packets, pages)
	}
	// Bound the total framing overhead: the unavoidable floor is one lacing
	// byte per packet plus the header pages, ~1.3% on this short stream; the
	// old page-per-packet muxer sat near 12%.
	if overhead := float64(len(stream)-payloadBytes) / float64(payloadBytes); overhead > 0.02 {
		t.Errorf("framing overhead %.2f%% exceeds 2%%", 100*overhead)
	}

	// The stream must round-trip through our demuxer: same packets, same
	// order, and the end-trim granule surfacing as the track's sample count.
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Samples != samples {
		t.Errorf("demuxed Samples = %d, want %d (end-trim granule)", tr.Samples, samples)
	}
	if tr.Delay != preSkip {
		t.Errorf("demuxed Delay = %d, want %d", tr.Delay, preSkip)
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			if i != packets {
				t.Fatalf("demuxed %d packets, want %d", i, packets)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if i >= packets {
			t.Fatalf("demuxed more than %d packets", packets)
		}
		if !bytes.Equal(pkt.Data, want[i]) {
			t.Fatalf("packet %d differs after mux round trip", i)
		}
	}

	// TTFA: the first audio page must arrive right after the headers and
	// carry exactly one packet, so headers+first-audio is small.
	third := bytes.Index(stream[1:], []byte("OggS"))
	_ = third
	offs := pageOffsets(stream)
	if len(offs) < 4 {
		t.Fatalf("expected at least 4 pages, got %d", len(offs))
	}
	firstAudioLen := offs[3] - offs[2]
	if firstAudioLen > headerLen+1+pktLen+8 {
		t.Errorf("first audio page is %d bytes; it should hold a single packet", firstAudioLen)
	}
}

// pageOffsets returns the byte offset of every page header in the stream.
func pageOffsets(b []byte) []int {
	var offs []int
	for i := 0; i+4 <= len(b); {
		if bytes.Equal(b[i:i+4], []byte("OggS")) {
			offs = append(offs, i)
			i += headerLen
			continue
		}
		i++
	}
	return offs
}
