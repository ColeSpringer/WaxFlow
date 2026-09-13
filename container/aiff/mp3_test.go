package aiff

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// The frames these cells walk are synthesized rather than encoded, and they
// can be: nothing in this package decodes one. What it reads is the four-byte
// header, so a valid header with payload bytes behind it exercises every path
// here, and a real encode would only tie these assertions to a fixture the
// decode differential in tests/ already owns.
//
// MPEG-1 Layer III, 128 kbit/s, 44.1 kHz, stereo, no CRC.
const (
	mp3FrameCount = 8
	mp3FrameLen   = 417
	mp3SPF        = 1152
	mp3Rate       = 44100
	mp3Channels   = 2
)

// mp3Payload is a run of whole frames. The payload bytes avoid 0xFF so a
// resync scan cannot mistake the middle of one for a frame boundary.
func mp3Payload() []byte {
	f := make([]byte, mp3FrameLen)
	copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
	for i := 4; i < len(f); i++ {
		f[i] = byte(i)
	}
	return bytes.Repeat(f, mp3FrameCount)
}

// xingFrame builds a metadata frame in the payload's geometry, carrying a
// frame count and the LAME gapless extension: the only thing in an AIFF-C
// that can state this payload's length.
func xingFrame(t testing.TB, frames, delay, padding uint16) []byte {
	t.Helper()
	one := mp3Payload()[:mp3FrameLen]
	h, err := mp3.ParseHeader(one)
	if err != nil {
		t.Fatal(err)
	}
	f := make([]byte, mp3FrameLen)
	copy(f, one[:mp3.HeaderLen])
	off := mp3.HeaderLen + h.SideInfoLen()
	copy(f[off:], "Xing")
	f[off+7] = 1 // flags: frame count present
	f[off+10] = byte(frames >> 8)
	f[off+11] = byte(frames)
	p := off + 12
	copy(f[p:], "LAME3.100")
	g := f[p+9+12:]
	g[0] = byte(delay >> 4)
	g[1] = byte(delay<<4) | byte(padding>>8)
	g[2] = byte(padding)
	return f
}

// walkMP3 reads every packet and returns the bytes and the frame count.
func walkMP3(t *testing.T, d *Demuxer) ([]byte, int) {
	t.Helper()
	var data []byte
	var pkt container.Packet
	for n := 0; ; n++ {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return data, n
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if pkt.Dur != mp3SPF || pkt.PTS != int64(n)*mp3SPF {
			t.Fatalf("packet %d: pts %d dur %d, want %d and %d", n, pkt.PTS, pkt.Dur, int64(n)*mp3SPF, mp3SPF)
		}
		data = append(data, pkt.Data...)
	}
}

// TestMP3CompressionTypes reads both spellings of the same coding: the
// '.mp3' fourcc and QuickTime's "ms" escape hatch with MP3's WAVE format
// tag behind it. They are one track, read one way.
func TestMP3CompressionTypes(t *testing.T) {
	payload := mp3Payload()
	for _, comp := range []string{compMP3, ".MP3", "ms\x00\x55"} {
		t.Run(comp, func(t *testing.T) {
			raw := buildAIFCrate(comp, mp3Channels, 0, mp3Rate, payload, mp3FrameCount)
			d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			tr := d.Tracks()[0]
			if tr.Codec != codec.MP3 {
				t.Fatalf("codec = %q, want mp3", tr.Codec)
			}
			// The decoder's output format: Layer III reconstruction is
			// floating point, so COMM's sampleSize is not the output depth
			// and is not read at all.
			want := audio.Format{Rate: mp3Rate, Channels: mp3Channels,
				Layout: audio.DefaultLayout(mp3Channels), Type: audio.Float, BitDepth: 32}
			if tr.Fmt != want {
				t.Errorf("format = %v, want %v", tr.Fmt, want)
			}
			if len(tr.CodecConfig) != 0 {
				t.Errorf("codec config = %d bytes, want none", len(tr.CodecConfig))
			}
			data, n := walkMP3(t, d)
			if n != mp3FrameCount || !bytes.Equal(data, payload) {
				t.Errorf("walked %d frames, want %d whole ones", n, mp3FrameCount)
			}
		})
	}
}

// TestMP3LengthIsUnknownWithoutAMetadataFrame is decision 7 of this stream,
// and the note is why it is not a silent shrug: COMM declares a number, this
// reader does not use it, and a caller looking at "unknown" deserves to know
// which of the two counts nobody defined.
func TestMP3LengthIsUnknownWithoutAMetadataFrame(t *testing.T) {
	raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, mp3Payload(), mp3FrameCount)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Samples != -1 || tr.SamplesExact || tr.SamplesAdvisory {
		t.Errorf("length = %d (exact %v, advisory %v), want -1", tr.Samples, tr.SamplesExact, tr.SamplesAdvisory)
	}
	warns := d.Warnings()
	if len(warns) != 1 || warns[0].Kind != container.Note {
		t.Fatalf("warnings = %v, want one Note", warns)
	}
	if !strings.Contains(warns[0].Msg, "8 sample frames") {
		t.Errorf("note = %q, want COMM's own number named", warns[0].Msg)
	}
}

// TestMP3XingLeadPayloadTakesItsTrims: a metadata frame is the one thing in
// an AIFF-C that states this payload's length, and it states the trims with
// it or not at all.
func TestMP3XingLeadPayloadTakesItsTrims(t *testing.T) {
	payload := append(xingFrame(t, mp3FrameCount, 576, 990), mp3Payload()...)
	raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, payload, mp3FrameCount+1)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Delay != 576+529 || tr.Padding != 990-529 {
		t.Errorf("trims = %d/%d, want %d/%d", tr.Delay, tr.Padding, 576+529, 990-529)
	}
	if want := int64(mp3FrameCount*mp3SPF) - tr.Delay - tr.Padding; tr.Samples != want {
		t.Errorf("samples = %d, want %d", tr.Samples, want)
	}
	if warns := d.Warnings(); len(warns) != 0 {
		t.Errorf("warnings = %v; a stated length has nothing to note", warns)
	}
	// The metadata frame is not audio and must not be delivered.
	if _, n := walkMP3(t, d); n != mp3FrameCount {
		t.Errorf("walked %d frames, want %d: the Xing frame was delivered as audio", n, mp3FrameCount)
	}
}

// TestMP3COMMDisagreeingWithTheFrames pins which side wins: a decoder reads
// the frame, so the frame does.
func TestMP3COMMDisagreeingWithTheFrames(t *testing.T) {
	raw := buildAIFCrate(compMP3, 1, 0, mp3Rate, mp3Payload(), mp3FrameCount)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Tracks()[0].Fmt.Channels; got != mp3Channels {
		t.Errorf("channels = %d, want the frame's %d", got, mp3Channels)
	}
	warns := d.Warnings()
	if len(warns) == 0 || warns[0].Kind != container.Damage || !strings.Contains(warns[0].Msg, "the frame wins") {
		t.Fatalf("warnings = %v, want a Damage naming the winner", warns)
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict mode error = %v, want malformed", err)
	}
}

// TestMP3SeekBacksOffForTheReservoir: a landing exactly on the target frame
// reads as exact and decodes to garbage, since Layer III carries filterbank
// overlap across frames and a frame's main data can begin up to 511 bytes
// earlier in the stream.
func TestMP3SeekBacksOffForTheReservoir(t *testing.T) {
	raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, bytes.Repeat(mp3Payload(), 60), mp3FrameCount*60)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := int64(400 * mp3SPF)
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatal(err)
	}
	if landed > target || landed%mp3SPF != 0 {
		t.Fatalf("seek to %d landed at %d", target, landed)
	}
	if want := target - 3*mp3SPF; landed >= want {
		t.Errorf("seek to %d landed at %d; the filterbank's own backoff reaches %d, so the reservoir walk did nothing",
			target, landed, want)
	}
	if landed == 0 {
		t.Errorf("seek to %d landed at 0; the backoff is bounded, not a rewind", target)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != landed {
		t.Errorf("post-seek packet PTS = %d (%v), want %d", pkt.PTS, err, landed)
	}
}

// TestMP3ToleratedDamage walks payloads a writer or a disk damaged, in the
// same wording the bare-stream reader uses for the same findings. Everything
// but the leading one arrives during the walk, which is what a lazily built
// index means.
func TestMP3ToleratedDamage(t *testing.T) {
	payload := mp3Payload()
	junk := func(n int) []byte { return bytes.Repeat([]byte{0x11}, n) }
	cut := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	for _, tc := range []struct {
		name    string
		payload []byte
		want    string
		atOpen  bool
		frames  int
	}{
		{"leading junk", cut(junk(32), payload),
			"32 unparsable bytes before the first frame", true, mp3FrameCount},
		{"junk between frames", cut(payload[:2*mp3FrameLen], junk(16), payload[2*mp3FrameLen:]),
			"16 unparsable bytes skipped", false, mp3FrameCount},
		{"truncated final frame", payload[:len(payload)-mp3FrameLen/2],
			"truncated final frame dropped", false, mp3FrameCount - 1},
		{"bytes that are not frames at the end", cut(payload, junk(40)),
			"40 trailing bytes are not frames, dropped", false, mp3FrameCount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, tc.payload, mp3FrameCount)
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, n := walkMP3(t, d); n != tc.frames {
				t.Errorf("walked %d frames, want %d", n, tc.frames)
			}
			var found bool
			for _, w := range d.Warnings() {
				if w.Msg == tc.want && w.Kind == container.Damage {
					found = true
				}
			}
			if !found {
				t.Fatalf("warnings = %v, want a Damage saying %q", d.Warnings(), tc.want)
			}
			strict, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if tc.atOpen {
				if err == nil {
					t.Fatal("strict mode accepted damage at the head")
				}
				return
			}
			if err != nil {
				t.Fatalf("strict mode refused at open, before the walk reached the damage: %v", err)
			}
			var pkt container.Packet
			for {
				err := strict.ReadPacket(&pkt)
				if err == nil {
					continue
				}
				if !errors.Is(err, waxerr.ErrMalformedInput) {
					t.Fatalf("strict mode failed with %v, want malformed", err)
				}
				break
			}
		})
	}
}

// TestMP3IndexRoundTrips takes the frame index through the sidecar blob, and
// pins the floor under it: below a few thousand frames rebuilding the index
// beats a disk round trip, which is what a short payload answers nil for.
func TestMP3IndexRoundTrips(t *testing.T) {
	raw := buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, bytes.Repeat(mp3Payload(), 600), mp3FrameCount*600)
	src := container.BytesSource(raw)
	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SeekSample(0, int64(mp3FrameCount)*600*mp3SPF); err != nil {
		t.Fatal(err)
	}
	blob := d.IndexSnapshot()
	if blob == nil {
		t.Fatal("no snapshot after a full walk")
	}
	d2, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d2.RestoreIndex(blob) {
		t.Fatal("snapshot rejected by an identical source")
	}
	l1, err := d.SeekSample(0, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := d2.SeekSample(0, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if l1 != l2 {
		t.Errorf("the restored index seeks to %d, the walked one to %d", l2, l1)
	}
}

// TestByteLinearPayloadsHaveNoIndex is the other half of the capability
// gate: format.Media advertises container.Indexer by type assertion, so a
// PCM track must answer nil and false rather than nothing at all.
func TestByteLinearPayloadsHaveNoIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"pcm", buildAIFCn(compNONE, 1, 16, make([]byte, 64), 32)},
		{"mp3, below the floor", buildAIFCrate(compMP3, mp3Channels, 0, mp3Rate, mp3Payload(), mp3FrameCount)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDemuxer(container.BytesSource(tc.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.SeekSample(0, 1<<20); err != nil {
				t.Fatal(err)
			}
			if blob := d.IndexSnapshot(); blob != nil {
				t.Errorf("snapshotted %d bytes", len(blob))
			}
			// On a demuxer that has not moved, so the refusal is the one
			// being tested rather than the guard against restoring over a
			// progressed walk.
			fresh, err := NewDemuxer(container.BytesSource(tc.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.RestoreIndex([]byte("WXMPAIDX1\x00\x01\x01\x00")) {
				t.Error("restored an index")
			}
		})
	}
}
