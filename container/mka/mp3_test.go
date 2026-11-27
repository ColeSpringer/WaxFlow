package mka

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// testdata/mp3.mka is one second of a 440 Hz tone under noise, 44.1 kHz
// mono at 32 kbit/s, as LAME and ffmpeg's Matroska muxer write it:
//
//	ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=1" \
//	    -f lavfi -i "anoisesrc=r=44100:a=0.1:d=1:seed=1" \
//	    -filter_complex amix=inputs=2 -ac 1 -c:a libmp3lame -b:a 32k \
//	    -cluster_time_limit 100 mp3.mka
//
// (ffmpeg 8.0.1, Ubuntu.) Forty 1152-sample frames in thirteen clusters, a
// CodecDelay of LAME's 1105 samples (576 encoder, 529 decoder), a
// DiscardPadding of 875 on the last block, and Cues. The noise keeps the bit
// reservoir in use, and at 32 kbit/s three frames hold less main data than
// it can reach back over, which is what a seek's backoff has to cover.
const (
	mp3Frames  = 40
	mp3Raw     = mp3Frames * 1152
	mp3Delay   = 1105
	mp3Pad     = 875
	mp3Samples = mp3Raw - mp3Delay - mp3Pad // the 44100 the signal lasts
)

// TestMP3InMatroska: an A_MPEG/L3 track is MP3, one whole frame a packet, at
// the format the frames state, with the gapless trims the file writes.
func TestMP3InMatroska(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixture(t, "mp3.mka")), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	want := audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}
	if tr.Codec != codec.MP3 || tr.Fmt != want || len(tr.CodecConfig) != 0 {
		t.Fatalf("track %q %v with %d config bytes, want mp3 %v and none", tr.Codec, tr.Fmt, len(tr.CodecConfig), want)
	}
	if tr.Delay != mp3Delay {
		t.Errorf("Delay = %d, want the CodecDelay's %d", tr.Delay, mp3Delay)
	}
	var raw, n int64
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		h, err := mp3.ParseHeader(pkt.Data)
		if err != nil || h.Size() != len(pkt.Data) {
			t.Fatalf("packet %d is not one whole frame: %d bytes, header %+v (%v)", n, len(pkt.Data), h, err)
		}
		if pkt.PTS != raw || pkt.Dur != 1152 {
			t.Fatalf("packet %d at %d for %d, want %d for 1152", n, pkt.PTS, pkt.Dur, raw)
		}
		raw += pkt.Dur
		n++
	}
	if n != mp3Frames || pkt.Padding != mp3Pad {
		t.Errorf("%d packets, the last trimming %d; want %d and %d", n, pkt.Padding, mp3Frames, mp3Pad)
	}
	if err := d.Walk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != mp3Samples || !tr.SamplesExact || tr.Padding != mp3Pad {
		t.Errorf("the walk settled %d samples (exact %v), Padding %d; want %d, %d", tr.Samples, tr.SamplesExact, tr.Padding, mp3Samples, mp3Pad)
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("warnings on a clean file: %v", d.Warnings())
	}
}

// TestMP3FrameDecidesTheFormat: the first frame's header is the track's
// format. An Audio element that states another is damage, read past with the
// frame's format and refused under Strict; one that states nothing held only
// the format's defaults and is no disagreement.
func TestMP3FrameDecidesTheFormat(t *testing.T) {
	file := fixture(t, "mp3.mka")
	var rate [8]byte
	binary.BigEndian.PutUint64(rate[:], math.Float64bits(22050))
	wrong := restated(t, restated(t, file, idChannels, []byte{2}), idSamplingFreq, rate[:])
	d, err := NewDemuxer(container.BytesSource(wrong), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := d.Tracks()[0].Fmt; f.Rate != 44100 || f.Channels != 1 {
		t.Errorf("track reads %d Hz %d channels, want the frame's 44100 and 1", f.Rate, f.Channels)
	}
	if dmg, _ := countWarnings(d.Warnings()); dmg != 1 {
		t.Errorf("%d Damage warnings for an Audio element the frames contradict, want 1: %v", dmg, d.Warnings())
	}
	if _, err := NewDemuxer(container.BytesSource(wrong), &container.DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a strict open = %v, want malformed", err)
	}

	// A first frame that says stereo where Channels is left out disagrees
	// only with the default, which is no damage: the frame's count is taken
	// without a word, strict or not. Where Channels states 1, it is damage.
	// Only the open reads the patched header, so nothing decodes it.
	probe, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, at, ok := probe.firstFrameHead(probe.sel)
	if !ok {
		t.Fatal("the fixture's first frame was not found")
	}
	stereo := append([]byte(nil), file...)
	stereo[at+3] &^= 0xC0 // channel mode 0, stereo
	quiet, err := NewDemuxer(container.BytesSource(voided(t, stereo, idChannels)), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a strict open with no Channels = %v", err)
	}
	if f := quiet.Tracks()[0].Fmt; f.Rate != 44100 || f.Channels != 2 {
		t.Errorf("track reads %d Hz %d channels, want the frame's 44100 and 2", f.Rate, f.Channels)
	}
	if _, err := NewDemuxer(container.BytesSource(stereo), &container.DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("a strict open where Channels states 1 and the frame 2 = %v, want malformed", err)
	}
}

// TestMP3FirstFrameInAnUnsizedCluster: the first frame is found in a Cluster
// of unknown size too, so its header still has the last word over an Audio
// element that disagrees with it.
func TestMP3FirstFrameInAnUnsizedCluster(t *testing.T) {
	unsized := unsizeClusters(fixture(t, "mp3.mka"))
	if unsized == nil {
		t.Fatal("the fixture's clusters were not rewritten")
	}
	d, err := NewDemuxer(container.BytesSource(unsized), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, at, ok := d.firstFrameHead(d.sel); !ok || at <= d.firstClusterOff {
		t.Fatalf("the first frame was not found in the unsized cluster (at %d, %v)", at, ok)
	}
	wrong, err := NewDemuxer(container.BytesSource(restated(t, unsized, idChannels, []byte{2})), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := wrong.Tracks()[0].Fmt; f.Channels != 1 {
		t.Errorf("track reads %d channels, want the frame's 1", f.Channels)
	}
	if dmg, _ := countWarnings(wrong.Warnings()); dmg != 1 {
		t.Errorf("%d Damage warnings for Channels the frame contradicts, want 1: %v", dmg, wrong.Warnings())
	}
}

// TestMPEGLayersIAndIIAreRefusedByName: only Layer III has a decoder here, so
// a Layer I or II track is refused as unsupported, named.
func TestMPEGLayersIAndIIAreRefusedByName(t *testing.T) {
	file := fixture(t, "mp3.mka")
	for _, tc := range []struct{ id, name string }{{"A_MPEG/L2", "MPEG/L2 (MP2)"}, {"A_MPEG/L1", "MPEG/L1 (MP1)"}} {
		bad := bytes.Replace(file, []byte("A_MPEG/L3"), []byte(tc.id), 1)
		_, err := NewDemuxer(container.BytesSource(bad), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) || err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s: %v, want unsupported naming %q", tc.id, err, tc.name)
		}
	}
}

// TestFramesOfAnotherLayerAreNamedForWhatTheyAre: a CodecID that says Layer
// III over Layer I or II frames is wrong about them. The first frame names
// the track, which is passed over like any codec with no decoder here, so a
// file's other track is still chosen.
func TestFramesOfAnotherLayerAreNamedForWhatTheyAre(t *testing.T) {
	file := fixture(t, "mp3.mka")
	probe, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, at, ok := probe.firstFrameHead(probe.sel)
	if !ok {
		t.Fatal("the fixture's first frame was not found")
	}
	for _, tc := range []struct {
		name  string
		layer byte // the header's two layer bits
	}{{"MPEG/L2 (MP2)", 2}, {"MPEG/L1 (MP1)", 3}} {
		other := append([]byte(nil), file...)
		other[at+1] = other[at+1]&^0x06 | tc.layer<<1
		_, err := NewDemuxer(container.BytesSource(other), nil)
		if !errors.Is(err, waxerr.ErrUnsupportedFormat) || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("layer bits %d: %v, want unsupported naming %q", tc.layer, err, tc.name)
		}
	}

	// A Layer II track ahead of a PCM one: the PCM track is read.
	var tracks, cluster []byte
	cluster = appendUint(cluster, idTimestamp, 0)
	pcm := bytes.Repeat([]byte{0x20}, 64)
	for i, tr := range []struct {
		codecID string
		frame   []byte
	}{
		{"A_MPEG/L3", append([]byte{0xFF, 0xFD, 0x90, 0x00}, make([]byte, 60)...)},
		{"A_PCM/INT/LIT", pcm},
	} {
		number := uint64(i + 1)
		var e, a []byte
		e = appendUint(e, idTrackNumber, number)
		e = appendUint(e, idTrackUID, number)
		e = appendUint(e, idTrackType, trackTypeAudio)
		e = appendString(e, idCodecID, tr.codecID)
		a = appendFloat(a, idSamplingFreq, 44100)
		a = appendUint(a, idChannels, 2)
		a = appendUint(a, idBitDepth, 16)
		e = appendElement(e, idAudio, a)
		tracks = appendElement(tracks, idTrackEntry, e)
		block := blockBody(0, tr.frame, 0x80)
		block[0] = 0x80 | byte(number)
		cluster = appendElement(cluster, idSimpleBlock, block)
	}
	var seg []byte
	seg = appendElement(seg, idInfo, appendUint(nil, idTimestampScale, 1_000_000))
	seg = appendElement(seg, idTracks, tracks)
	seg = appendElement(seg, idCluster, cluster)
	two := appendElement((&Muxer{opts: MuxerOptions{}}).ebmlHeader(), idSegment, seg)
	d, err := NewDemuxer(container.BytesSource(two), &container.DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatalf("a Layer II track ahead of a PCM one: %v", err)
	}
	if got := d.Tracks()[0].Codec; got != codec.PCM {
		t.Fatalf("selected the %s track, want the PCM one", got)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || !bytes.Equal(pkt.Data, pcm) {
		t.Errorf("the first packet is not the PCM track's block (%v)", err)
	}
}

// TestMP3BackoffCoversTheReservoir: a landing steps back over whole clusters
// until the track's frames between hold 575 bytes of main data past their
// overhead, and no further; the step is bounded.
func TestMP3BackoffCoversTheReservoir(t *testing.T) {
	d := &Demuxer{setup: codecSetup{id: codec.MP3, fmt: mp3.Header{Rate: 8000, Channels: 1}.PCMFormat()}}
	overhead := mp3FrameOverhead(d.setup.fmt) // 15 bytes
	// index lays out n clusters of framesPer frames of frameBytes, each
	// cluster also holding foreign bytes of another track.
	index := func(frameBytes, framesPer, foreign int64, n int) []clusterPos {
		var out []clusterPos
		for i := range int64(n) {
			out = append(out, clusterPos{
				off: i * (framesPer*frameBytes + foreign), sample: i * framesPer * 576,
				frames: i * framesPer, bytes: i * framesPer * frameBytes,
			})
		}
		return out
	}

	// Two 144-byte frames a cluster credit 2*(144-15) = 258 bytes, so 575
	// takes three clusters, whatever else the clusters hold.
	for _, foreign := range []int64{0, 3000} {
		d.clusterIndex = index(144, 2, foreign, 20)
		if got := d.mp3Backoff(10); got != 7 {
			t.Errorf("144-byte frames, two a cluster, %d foreign bytes: landed on cluster %d, want 7", foreign, got)
		}
	}
	// One 800-byte frame a cluster covers it in one.
	d.clusterIndex = index(800, 1, 0, 20)
	if got := d.mp3Backoff(10); got != 9 {
		t.Errorf("800-byte frames: landed on cluster %d, want 9", got)
	}
	// Frames no larger than their overhead never cover it; the step stops at
	// the bound, and never passes the first cluster.
	d.clusterIndex = index(overhead, 1, 0, 400)
	if got := d.mp3Backoff(300); got != 300-mp3BackoffClusters {
		t.Errorf("overhead-sized frames: landed on cluster %d, want %d", got, 300-mp3BackoffClusters)
	}
	if got := d.mp3Backoff(5); got != 0 {
		t.Errorf("near the start: landed on cluster %d, want 0", got)
	}
}

// TwoTrackMP3 rebuilds testdata/mp3.mka with one MP3 frame a cluster, each
// cluster also holding a 3000-byte block of track 2, the shape of an .mkv
// whose video shares the clusters. The Segment states no size, so the stale
// Cues are not taken, and the end trim is dropped with its BlockGroup.
func TwoTrackMP3(t testing.TB) []byte {
	t.Helper()
	src := fixture(t, "mp3.mka")
	d, err := NewDemuxer(container.BytesSource(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), src[:d.firstClusterOff]...)
	seg := bytes.Index(out, appendID(nil, idSegment)) + 4
	copy(out[seg:], unknownSizeVint)
	block := func(track byte, frame []byte) []byte {
		return appendElement(nil, idSimpleBlock, append([]byte{0x80 | track, 0, 0, 0x80}, frame...))
	}
	other := make([]byte, 3000)
	for i := range other {
		other[i] = byte(i * 7)
	}
	for {
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			return out
		}
		body := appendUint(nil, idTimestamp, uint64(pkt.PTS*1000/44100))
		body = append(body, block(1, pkt.Data)...)
		body = append(body, block(2, other)...)
		out = appendElement(out, idCluster, body)
	}
}
