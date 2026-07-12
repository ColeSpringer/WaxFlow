package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
)

// trafBox builds one traf (tfhd default-base-is-moof + tfdt + trun with
// per-sample duration and size) for crafting fragmented-reader inputs.
func trafBox(trackID uint32, baseTime uint64, durs, sizes []uint32) []byte {
	tfhd := makeFullBox("tfhd", 0, 0x020000, u32(trackID)) // default-base-is-moof
	tfdt := makeFullBox("tfdt", 1, 0, u64(baseTime))
	body := []byte{0, 0x00, 0x03, 0x01} // version, flags: data-offset+dur+size
	body = append(body, u32(uint32(len(durs)))...)
	body = append(body, u32(0)...) // data_offset (unused by these tests)
	for i := range durs {
		body = append(body, u32(durs[i])...)
		body = append(body, u32(sizes[i])...)
	}
	return makeBox("traf", tfhd, tfdt, makeBox("trun", body))
}

// craftDemuxer builds a one-moof+mdat media source and a Demuxer wired to a
// synthetic selected track, so the fragmented read path can be exercised with
// crafted durations, timescales, and track IDs without a full init segment. The
// trun data_offset stays 0, so samples read filler bytes from the moof head:
// these tests check the timeline and track routing, not the sample payloads.
func craftDemuxer(trackID uint32, timescale int64, rate int, durs, sizes []uint32) *Demuxer {
	mfhd := makeFullBox("mfhd", 0, 0, u32(1))
	moof := makeBox("moof", mfhd, trafBox(trackID, 0, durs, sizes))
	file := append(moof, makeBox("mdat", make([]byte, 64))...)
	src := container.BytesSource(file)
	return &Demuxer{
		src:        src,
		size:       int64(len(file)),
		w:          srcwin.New(src, int64(len(file)), "mp4: test"),
		fragmented: true,
		trex:       trexDefaults{have: true},
		sel: &track{
			id:        int(trackID),
			timescale: timescale,
			fmt:       audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		},
	}
}

// opusTrackFor builds a synthetic Opus track (a valid OpusHead the sample-entry
// builder accepts) plus a run of packets, for exercising the fragmented read
// path without pulling in the Opus encoder.
func opusTrackFor(preSkip, samples int64, npkt int) (container.Track, []codec.Packet) {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint16(head[10:], uint16(preSkip))
	binary.LittleEndian.PutUint32(head[12:], 48000)
	t := container.Track{
		Codec:       codec.Opus,
		CodecConfig: head,
		Fmt:         audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Delay:       preSkip,
		Samples:     samples,
	}
	var pkts []codec.Packet
	for i := 0; i < npkt; i++ {
		pkts = append(pkts, codec.Packet{Data: []byte{0x00, byte(i), byte(i >> 8), 0x5A}, Dur: 960, Sync: true})
	}
	return t, pkts
}

// flacTrackFor builds a synthetic FLAC track (a valid STREAMINFO) plus packets.
func flacTrackFor(t *testing.T, npkt int) (container.Track, []codec.Packet) {
	t.Helper()
	si := flac.StreamInfo{MinBlock: 4096, MaxBlock: 4096, Rate: 48000, Channels: 2, Bits: 16, Samples: int64(npkt) * 4096}
	cfg, err := si.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	track := container.Track{
		Codec:       codec.FLAC,
		CodecConfig: cfg,
		Fmt:         si.PCMFormat(),
		Samples:     int64(npkt) * 4096,
	}
	var pkts []codec.Packet
	for i := 0; i < npkt; i++ {
		pkts = append(pkts, codec.Packet{Data: bytes.Repeat([]byte{byte(i + 1)}, 200), Dur: 4096, Sync: true})
	}
	return track, pkts
}

// segmentize runs packets through the Segmenter and returns the init header and
// the concatenated media segments.
func segmentize(t *testing.T, track container.Track, pkts []codec.Packet, segSamples int) (init, media []byte) {
	t.Helper()
	init, err := InitSegment(track)
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	seg, err := NewSegmenter(track, &SegmenterOptions{SegmentSamples: segSamples})
	if err != nil {
		t.Fatalf("NewSegmenter: %v", err)
	}
	var buf bytes.Buffer
	emit := func(s Segment) error { buf.Write(s.Data); return nil }
	for _, p := range pkts {
		if err := seg.WritePacket(p, emit); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := seg.End(emit); err != nil {
		t.Fatalf("End: %v", err)
	}
	return init, buf.Bytes()
}

// readFrag drains a demuxer's packets into copied payloads and their PTS.
func readFrag(t *testing.T, d *Demuxer) ([][]byte, []int64) {
	t.Helper()
	var data [][]byte
	var pts []int64
	for {
		var pkt container.Packet
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return data, pts
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		data = append(data, append([]byte(nil), pkt.Data...))
		pts = append(pts, pkt.PTS)
	}
}

// TestFragmentedSelfContained reads a self-contained fragmented file (init +
// fragments concatenated) through the normal demuxer, exercising the dOps/dfLa
// sample entries and the fragment iterator.
func TestFragmentedSelfContained(t *testing.T) {
	type fragCase struct {
		name    string
		track   container.Track
		pkts    []codec.Packet
		segSamp int
		codec   codec.ID
		delay   int64
	}
	ot, opkts := opusTrackFor(312, 12*960-312, 12)
	ft, fpkts := flacTrackFor(t, 6)
	cases := []fragCase{
		{"opus", ot, opkts, 4 * 960, codec.Opus, 312},
		{"flac", ft, fpkts, 2 * 4096, codec.FLAC, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			init, media := segmentize(t, tc.track, tc.pkts, tc.segSamp)
			file := append(append([]byte(nil), init...), media...)

			d, err := NewDemuxer(container.BytesSource(file), nil)
			if err != nil {
				t.Fatalf("NewDemuxer: %v", err)
			}
			tr := d.Tracks()[0]
			if tr.Codec != tc.codec {
				t.Errorf("codec = %q, want %q", tr.Codec, tc.codec)
			}
			if tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
				t.Errorf("fmt = %+v", tr.Fmt)
			}
			if tr.Delay != tc.delay {
				t.Errorf("Delay = %d, want %d", tr.Delay, tc.delay)
			}
			got, pts := readFrag(t, d)
			if len(got) != len(tc.pkts) {
				t.Fatalf("read %d packets, wrote %d", len(got), len(tc.pkts))
			}
			var wantPTS int64
			for i := range tc.pkts {
				if !bytes.Equal(got[i], tc.pkts[i].Data) {
					t.Errorf("packet %d payload mismatch", i)
				}
				if pts[i] != wantPTS {
					t.Errorf("packet %d PTS = %d, want %d", i, pts[i], wantPTS)
				}
				wantPTS += tc.pkts[i].Dur
			}
		})
	}
}

// TestFragmentedBareSegment reads a bare media segment (no ftyp/moov) through
// NewFragmentedDemuxer with an out-of-band init segment, the HLS-client entry
// point.
func TestFragmentedBareSegment(t *testing.T) {
	track, pkts := opusTrackFor(312, 10*960-312, 10)
	init, media := segmentize(t, track, pkts, 4*960)

	d, err := NewFragmentedDemuxer(init, container.BytesSource(media))
	if err != nil {
		t.Fatalf("NewFragmentedDemuxer: %v", err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.Opus || tr.Delay != 312 {
		t.Errorf("track = %+v", tr)
	}
	got, _ := readFrag(t, d)
	if len(got) != len(pkts) {
		t.Fatalf("read %d packets, wrote %d", len(got), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i].Data) {
			t.Errorf("packet %d payload mismatch", i)
		}
	}
}

// TestFragmentedSeek checks that seeking a fragmented stream lands on the
// fragment at or before the target decode time.
func TestFragmentedSeek(t *testing.T) {
	track, pkts := opusTrackFor(312, 12*960-312, 12)
	const segSamp = 4 * 960 // 3 fragments of 4 packets each
	init, media := segmentize(t, track, pkts, segSamp)
	file := append(append([]byte(nil), init...), media...)
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}
	// Target inside the second fragment (decode time 4*960..8*960); landing
	// should be the second fragment's base decode time.
	landed, err := d.SeekSample(0, 5*960)
	if err != nil {
		t.Fatalf("SeekSample: %v", err)
	}
	if landed != segSamp {
		t.Errorf("landed at %d, want %d (second fragment base)", landed, segSamp)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil {
		t.Fatalf("ReadPacket after seek: %v", err)
	}
	if pkt.PTS != segSamp {
		t.Errorf("first packet after seek PTS = %d, want %d", pkt.PTS, segSamp)
	}
}

// TestFragmentedDurClamp guards finding 1: a trun with zero sample durations
// (a malformed segment, or trex defaults of 0) must not yield Dur=0 packets;
// the reader clamps each to at least one sample, like the stbl reader.
func TestFragmentedDurClamp(t *testing.T) {
	d := craftDemuxer(1, 48000, 48000, []uint32{0, 0, 0}, []uint32{4, 4, 4})
	for i := 0; i < 3; i++ {
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("ReadPacket %d: %v", i, err)
		}
		if pkt.Dur <= 0 {
			t.Fatalf("packet %d Dur = %d, want >= 1 (zero trun duration must clamp)", i, pkt.Dur)
		}
		if pkt.PTS != int64(i) {
			t.Errorf("packet %d PTS = %d, want %d (clamped durations accumulate)", i, pkt.PTS, i)
		}
	}
}

// TestFragmentedTimescaleRescale guards finding 2: when the media timescale is
// not the sample rate, the trun durations and tfdt base are in ticks and must
// be rescaled to output samples, matching the progressive stbl reader.
func TestFragmentedTimescaleRescale(t *testing.T) {
	// timescale 96000 = 2x rate 48000; a 1920-tick sample is 960 output samples.
	d := craftDemuxer(1, 96000, 48000, []uint32{1920, 1920}, []uint32{8, 8})
	var pts int64
	for i := 0; i < 2; i++ {
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("ReadPacket %d: %v", i, err)
		}
		if pkt.Dur != 960 {
			t.Errorf("packet %d Dur = %d, want 960 (1920 ticks at 2x timescale)", i, pkt.Dur)
		}
		if pkt.PTS != pts {
			t.Errorf("packet %d PTS = %d, want %d", i, pkt.PTS, pts)
		}
		pts += pkt.Dur
	}
}

// TestFragmentedTrackMatch guards finding 3: parseFragment must take the traf
// whose track_ID is the selected track's, not simply the first traf, so an
// interleaved moof does not feed another track's samples to the audio decoder.
func TestFragmentedTrackMatch(t *testing.T) {
	// A moof body with a non-audio traf (track 99) before the audio traf (1).
	mfhd := makeFullBox("mfhd", 0, 0, u32(1))
	other := trafBox(99, 0, []uint32{100, 100, 100}, []uint32{4, 4, 4}) // 3 samples
	audioTraf := trafBox(1, 0, []uint32{960, 960}, []uint32{8, 8})      // 2 samples
	body := append(append(append([]byte{}, mfhd...), other...), audioTraf...)

	fi, err := parseFragment(body, trexDefaults{have: true}, 1)
	if err != nil {
		t.Fatalf("parseFragment: %v", err)
	}
	if len(fi.samples) != 2 || fi.samples[0].dur != 960 {
		t.Errorf("selected the wrong traf: got %d samples %+v, want the 2-sample audio traf", len(fi.samples), fi.samples)
	}
	// Selecting the other track picks the 3-sample traf.
	if fi99, _ := parseFragment(body, trexDefaults{have: true}, 99); len(fi99.samples) != 3 {
		t.Errorf("track 99 selected %d samples, want 3", len(fi99.samples))
	}
	// A moof with only a non-matching traf yields no samples (skipped), not the
	// wrong track's.
	onlyOther := append(append([]byte{}, mfhd...), other...)
	if fiNone, _ := parseFragment(onlyOther, trexDefaults{have: true}, 1); len(fiNone.samples) != 0 {
		t.Errorf("non-matching moof yielded %d samples, want 0", len(fiNone.samples))
	}
}
