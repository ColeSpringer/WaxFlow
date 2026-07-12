package ogg

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/container"
)

// encodeVorbis runs a signal through the real Vorbis encoder and returns the
// audio packets, the codec-config blob, and the gapless trailer, so the muxer
// is exercised with genuine block-size-bearing packets rather than hand-rolled
// ones.
func encodeVorbis(t *testing.T, f audio.Format, src [][]float32) ([][]byte, []byte, codec.Trailer) {
	t.Helper()
	enc, err := vorbis.NewEncoder(f, &vorbis.EncoderOptions{Quality: 3})
	if err != nil {
		t.Fatalf("vorbis.NewEncoder: %v", err)
	}
	var packets [][]byte
	emit := func(p codec.Packet) error {
		packets = append(packets, append([]byte(nil), p.Data...))
		return nil
	}
	const chunk = 2048
	n := len(src[0])
	for off := 0; off < n; off += chunk {
		end := min(off+chunk, n)
		buf := audio.Get(f, end-off)
		buf.N = end - off // ChanF's length is N, so set it before copying in
		for c := range src {
			copy(buf.ChanF(c), src[c][off:end])
		}
		if err := enc.Encode(buf, emit); err != nil {
			t.Fatalf("encode: %v", err)
		}
		audio.Put(buf)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return packets, enc.CodecConfig(), tr
}

// muxVorbis frames encoded Vorbis packets into an Ogg stream through the muxer.
func muxVorbis(t *testing.T, f audio.Format, cfg []byte, packets [][]byte, tr codec.Trailer, tags []container.Tag) []byte {
	t.Helper()
	var out bytes.Buffer
	m := NewMuxer(&out, &MuxerOptions{Tags: tags})
	track := container.Track{Codec: codec.Vorbis, CodecConfig: cfg, Fmt: f}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i, p := range packets {
		if err := m.WritePacket(container.Packet{Packet: codec.Packet{Data: p, Sync: true}}); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.End(tr); err != nil {
		t.Fatalf("End: %v", err)
	}
	return out.Bytes()
}

// TestMuxVorbisRoundTrip is the phase-5 acceptance gate: a mux->demux round trip
// of real Vorbis packets recovers the true sample count exactly (SamplesExact),
// which only holds when the muxer's granulepos accounting carries the
// firstBlock/2 priming shift the demuxer subtracts back off. It also confirms
// every audio packet survives the framing byte-identically and in order.
func TestMuxVorbisRoundTrip(t *testing.T) {
	for _, ch := range []int{1, 2} {
		f := audio.Format{Rate: 48000, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
		const n = 24000 // half a second: several long blocks plus a partial tail
		src := make([][]float32, ch)
		for c := 0; c < ch; c++ {
			src[c] = make([]float32, n)
			freq := 440.0 * float64(c+1)
			for i := range src[c] {
				src[c][i] = 0.3 * float32(math.Sin(2*math.Pi*freq*float64(i)/48000))
			}
		}
		packets, cfg, tr := encodeVorbis(t, f, src)
		if tr.Samples != n {
			t.Fatalf("ch=%d: encoder trailer Samples = %d, want %d", ch, tr.Samples, n)
		}
		stream := muxVorbis(t, f, cfg, packets, tr, nil)
		// The muxer has no clock and a fixed serial, so re-muxing the same
		// packets must be byte-identical (deterministic output).
		if again := muxVorbis(t, f, cfg, packets, tr, nil); !bytes.Equal(stream, again) {
			t.Errorf("ch=%d: muxer output is not deterministic", ch)
		}

		d, err := NewDemuxer(container.BytesSource(stream), nil)
		if err != nil {
			t.Fatalf("ch=%d: NewDemuxer: %v", ch, err)
		}
		track := d.Tracks()[0]
		if track.Codec != codec.Vorbis {
			t.Fatalf("ch=%d: demuxed codec %q, want vorbis", ch, track.Codec)
		}
		if !track.SamplesExact || track.Samples != n {
			t.Errorf("ch=%d: demuxed Samples = %d exact=%v, want %d exact (gapless round trip)", ch, track.Samples, track.SamplesExact, n)
		}
		if track.Fmt.Channels != ch || track.Fmt.Rate != 48000 {
			t.Errorf("ch=%d: demuxed format %dch %dHz", ch, track.Fmt.Channels, track.Fmt.Rate)
		}
		// Every audio packet must survive framing byte-for-byte and in order.
		var pkt container.Packet
		for i := 0; ; i++ {
			err := d.ReadPacket(&pkt)
			if errors.Is(err, io.EOF) {
				if i != len(packets) {
					t.Fatalf("ch=%d: demuxed %d packets, want %d", ch, i, len(packets))
				}
				break
			}
			if err != nil {
				t.Fatalf("ch=%d: ReadPacket %d: %v", ch, i, err)
			}
			if i >= len(packets) {
				t.Fatalf("ch=%d: demuxed more than %d packets", ch, len(packets))
			}
			if !bytes.Equal(pkt.Data, packets[i]) {
				t.Fatalf("ch=%d: packet %d differs after mux round trip", ch, i)
			}
		}
	}
}

// TestEmitHeaderPagesSpill exercises the header-page packer's spill path: a
// packet larger than one page's 255x255-byte segment table must continue onto a
// flagContinued page, and the emitted pages must reassemble the original packets
// exactly (the demuxer's generic reassembly relies on this). Real Vorbis setups
// fit one page, so this covers the otherwise-unreached large-setup branch.
func TestEmitHeaderPagesSpill(t *testing.T) {
	small := bytes.Repeat([]byte{0xAB}, 300)  // 2 segments
	huge := bytes.Repeat([]byte{0xCD}, 70000) // 275 segments: spans >1 page
	pkts := [][]byte{small, huge}

	type emitted struct {
		payload, seg []byte
		flags        byte
	}
	var pages []emitted
	err := emitHeaderPages(pkts, func(payload, seg []byte, granule int64, headerType byte) error {
		if granule != 0 {
			t.Errorf("header page granule = %d, want 0", granule)
		}
		if len(seg) > maxPageSegEntries {
			t.Errorf("page has %d segments, over the %d cap", len(seg), maxPageSegEntries)
		}
		pages = append(pages, emitted{append([]byte(nil), payload...), append([]byte(nil), seg...), headerType})
		return nil
	})
	if err != nil {
		t.Fatalf("emitHeaderPages: %v", err)
	}
	if len(pages) < 2 {
		t.Fatalf("huge packet did not spill: %d page(s)", len(pages))
	}
	// A page whose predecessor ended mid-packet (last lacing 255) must be
	// flagged continued; the first page never is.
	if pages[0].flags&flagContinued != 0 {
		t.Error("first page must not be flagged continued")
	}
	for i := 1; i < len(pages); i++ {
		wantCont := pages[i-1].seg[len(pages[i-1].seg)-1] == 255
		if got := pages[i].flags&flagContinued != 0; got != wantCont {
			t.Errorf("page %d continued=%v, want %v", i, got, wantCont)
		}
	}
	// Reassemble packets from the pages via the lacing table and compare.
	var body []byte
	var laces []byte
	for _, p := range pages {
		body = append(body, p.payload...)
		laces = append(laces, p.seg...)
	}
	var got [][]byte
	var cur []byte
	off := 0
	for _, l := range laces {
		cur = append(cur, body[off:off+int(l)]...)
		off += int(l)
		if l < 255 {
			got = append(got, cur)
			cur = nil
		}
	}
	if len(got) != len(pkts) {
		t.Fatalf("reassembled %d packets, want %d", len(got), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i]) {
			t.Errorf("packet %d did not reassemble to the original", i)
		}
	}
}

// TestMuxVorbisComment confirms the muxer builds a fresh Vorbis comment header
// from the caller's tags (the encoder never sees them), that the header parses
// back through the decoder's config path, and that the identification and setup
// headers pass through unchanged.
func TestMuxVorbisComment(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	n := 8000
	src := [][]float32{make([]float32, n), make([]float32, n)}
	for i := 0; i < n; i++ {
		v := 0.2 * float32(math.Sin(2*math.Pi*330*float64(i)/48000))
		src[0][i], src[1][i] = v, v
	}
	packets, cfg, tr := encodeVorbis(t, f, src)
	tags := []container.Tag{{Key: "TITLE", Value: "Floofy Pony"}, {Key: "ARTIST", Value: "WaxFlow"}}
	stream := muxVorbis(t, f, cfg, packets, tr, tags)

	// The muxed stream's three headers must parse as a valid Vorbis config, and
	// the id/setup halves must be byte-identical to the encoder's (only the
	// comment is rebuilt).
	d, err := NewDemuxer(container.BytesSource(stream), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	gotID, gotComment, gotSetup, err := vorbis.SplitConfig(d.Tracks()[0].CodecConfig)
	if err != nil {
		t.Fatalf("SplitConfig of muxed config: %v", err)
	}
	wantID, _, wantSetup, err := vorbis.SplitConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotID, wantID) {
		t.Error("identification header changed through the muxer")
	}
	if !bytes.Equal(gotSetup, wantSetup) {
		t.Error("setup header changed through the muxer")
	}
	// The rebuilt comment header must be a well-formed Vorbis comment packet
	// (type 0x03 + "vorbis") and carry the caller's tags.
	if len(gotComment) < 7 || gotComment[0] != 0x03 || string(gotComment[1:7]) != "vorbis" {
		t.Fatalf("comment header lacks the type/signature")
	}
	if !bytes.Contains(gotComment, []byte("TITLE=Floofy Pony")) || !bytes.Contains(gotComment, []byte("ARTIST=WaxFlow")) {
		t.Error("rebuilt comment header is missing the caller's tags")
	}
}
