package ogg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
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

// muxedPage is one page of a muxer's output, read back by hand.
type muxedPage struct {
	flags   byte
	granule int64
	seq     uint32
	lacing  []byte
	body    []byte
}

// readPages parses every page of stream by its own framing and checks each
// one's checksum, so a page whose segment count is wrong fails here.
func readPages(t *testing.T, stream []byte) []muxedPage {
	t.Helper()
	var pages []muxedPage
	for off := 0; off < len(stream); {
		if off+headerLen > len(stream) || string(stream[off:off+4]) != "OggS" {
			t.Fatalf("no page at offset %d", off)
		}
		body := off + headerLen + int(stream[off+26])
		if body > len(stream) {
			t.Fatalf("the page at %d is truncated", off)
		}
		size := 0
		for _, l := range stream[off+headerLen : body] {
			size += int(l)
		}
		if body+size > len(stream) {
			t.Fatalf("the page at %d runs past the stream", off)
		}
		page := append([]byte(nil), stream[off:body+size]...)
		clear(page[22:26])
		if crc32(0, page) != binary.LittleEndian.Uint32(stream[off+22:]) {
			t.Fatalf("the page at %d fails its checksum", off)
		}
		pages = append(pages, muxedPage{
			flags:   stream[off+5],
			granule: int64(binary.LittleEndian.Uint64(stream[off+6:])),
			seq:     binary.LittleEndian.Uint32(stream[off+18:]),
			lacing:  stream[off+headerLen : body],
			body:    stream[body : body+size],
		})
		off = body + size
	}
	return pages
}

// requirePageFlow holds an Opus stream's pages to the packets muxed into it,
// whose durations are durs: sequence numbers run on, a page is flagged
// continued exactly when the one before it ends mid-packet, only the last page
// ends the stream, and a page states the granule its last completed packet
// ends at, or -1 when it completes none.
func requirePageFlow(t *testing.T, pages []muxedPage, durs []int64) {
	t.Helper()
	const headers = 2
	done, granule, open := 0, int64(0), false
	for i, p := range pages {
		if p.seq != uint32(i) {
			t.Errorf("page %d has sequence number %d", i, p.seq)
		}
		if cont := p.flags&flagContinued != 0; cont != open {
			t.Errorf("page %d: continued %v, but the page before it ends mid-packet: %v", i, cont, open)
		}
		if eos := p.flags&flagEOS != 0; eos != (i == len(pages)-1) {
			t.Errorf("page %d of %d: end of stream %v", i, len(pages), eos)
		}
		completes := false
		for _, l := range p.lacing {
			open = l == 255
			if l < 255 {
				if done >= headers {
					granule += durs[done-headers]
				}
				done++
				completes = true
			}
		}
		want := int64(-1)
		if completes {
			want = granule
		}
		if p.granule != want {
			t.Errorf("page %d states granule %d, want %d", i, p.granule, want)
		}
	}
	if done != headers+len(durs) {
		t.Errorf("the pages complete %d packets, want %d", done, headers+len(durs))
	}
}

// TestMuxerSplitsLargePackets: a packet longer than one page's 255 segments
// continues onto as many pages as it needs, each flagged continued, and every
// page that completes no packet states granule -1. The packets read back whole.
func TestMuxerSplitsLargePackets(t *testing.T) {
	const dur = 960
	for _, sizes := range [][]int{
		{65024}, {65025}, {70000}, {130050}, {200000},
		{70000, 240, 65024, 240, 240, 65025, 130050, 240, 200000},
	} {
		t.Run(fmt.Sprint(sizes), func(t *testing.T) {
			var out bytes.Buffer
			m := NewMuxer(&out, nil)
			if err := m.Begin([]container.Track{{Codec: codec.Opus, CodecConfig: muxOpusHead(0)}}); err != nil {
				t.Fatal(err)
			}
			var want [][]byte
			var durs []int64
			for i, n := range sizes {
				data := make([]byte, n)
				data[0] = byte(31<<3 | 0x4) // TOC: CELT FB 20 ms, stereo, code 0
				for j := 1; j < n; j++ {
					data[j] = byte(i*31 + j*7)
				}
				want, durs = append(want, data), append(durs, dur)
				if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: data, Dur: dur, Sync: true}}); err != nil {
					t.Fatal(err)
				}
			}
			total := int64(len(sizes) * dur)
			if err := m.End(codec.Trailer{Samples: total}); err != nil {
				t.Fatal(err)
			}
			requirePageFlow(t, readPages(t, out.Bytes()), durs)

			d, err := NewDemuxer(container.BytesSource(out.Bytes()), &DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.Samples != total {
				t.Errorf("the track says %d samples, want %d", tr.Samples, total)
			}
			var pkt container.Packet
			for i := 0; ; i++ {
				err := d.ReadPacket(&pkt)
				if errors.Is(err, io.EOF) {
					if i != len(want) {
						t.Fatalf("read %d packets back, want %d", i, len(want))
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if i >= len(want) || !bytes.Equal(pkt.Data, want[i]) {
					t.Fatalf("packet %d does not read back as written", i)
				}
			}
			if ws := d.Warnings(); len(ws) != 0 {
				t.Errorf("warnings %v", ws)
			}
		})
	}
}

// TestMuxerBatchesEmptyPackets: empty packets still take a segment each, so a
// run of them closes pages at 255 segments like any other.
func TestMuxerBatchesEmptyPackets(t *testing.T) {
	var out bytes.Buffer
	m := NewMuxer(&out, nil)
	if err := m.Begin([]container.Track{{Codec: codec.Opus, CodecConfig: muxOpusHead(0)}}); err != nil {
		t.Fatal(err)
	}
	var durs []int64
	write := func(data []byte, dur int64) {
		t.Helper()
		durs = append(durs, dur)
		if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: data, Dur: dur, Sync: true}}); err != nil {
			t.Fatal(err)
		}
	}
	write([]byte{31<<3 | 0x4, 1, 2, 3}, 960)
	for range 600 {
		write(nil, 0)
	}
	write([]byte{31<<3 | 0x4, 4, 5, 6}, 960)
	if err := m.End(codec.Trailer{Samples: 1920}); err != nil {
		t.Fatal(err)
	}
	requirePageFlow(t, readPages(t, out.Bytes()), durs)
}

// muxFLACFrom encodes a sine, its frames numbered from first, into an Ogg FLAC
// on a destination the muxer can seek, so the stream ends fully declared.
func muxFLACFrom(t *testing.T, first int64) []byte {
	t.Helper()
	f := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	src := testutil.Sine(f, 4410*100+1000, 440, 0.5)
	defer audio.Put(src)
	enc, err := flac.NewEncoder(f, &flac.EncoderOptions{Level: 5, FirstFrame: first})
	if err != nil {
		t.Fatal(err)
	}
	ws := &testutil.MemWriteSeeker{}
	m := NewMuxer(ws, &MuxerOptions{MD5: enc.MD5})
	track := container.Track{Codec: codec.FLAC, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(src.N)}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Packet: p}) }
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
	return ws.Buf
}

// TestMuxerNumbersFLACFramesFromZero: FLAC frames that arrive numbered from
// past 0 are written numbered from 0, which the granules, counted from 0,
// agree with: an encode whose frames start at 1000 muxes to the bytes of one
// whose frames start at 0.
func TestMuxerNumbersFLACFramesFromZero(t *testing.T) {
	want, got := muxFLACFrom(t, 0), muxFLACFrom(t, 1000)
	if !bytes.Equal(got, want) {
		t.Fatalf("frames numbered from 1000 mux to %d bytes unlike those numbered from 0 (%d)", len(got), len(want))
	}
}

// TestOggFLACMuxRefusesAnUnreadableStreamInfo: the frames' numbering rests on
// STREAMINFO's block bounds, so a STREAMINFO that does not parse is refused at
// Begin, before anything is written, and the muxer takes no packet after.
func TestOggFLACMuxRefusesAnUnreadableStreamInfo(t *testing.T) {
	frame := muxFLACFrom(t, 0)
	frame = frame[bytes.Index(frame, []byte{0xFF, 0xF8}):][:16]
	for _, cfg := range [][]byte{make([]byte, flacStreamInfoLen), make([]byte, 10)} { // rate 0; short
		var out bytes.Buffer
		m := NewMuxer(&out, nil)
		if err := m.Begin([]container.Track{{Codec: codec.FLAC, CodecConfig: cfg}}); err == nil {
			t.Fatal("Begin accepted a STREAMINFO that does not parse")
		}
		if out.Len() != 0 {
			t.Errorf("Begin wrote %d bytes", out.Len())
		}
		if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: frame, Dur: 4096}}); err == nil {
			t.Error("WritePacket took a packet after Begin refused")
		}
	}
}
