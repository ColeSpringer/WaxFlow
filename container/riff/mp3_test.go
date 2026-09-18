package riff

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// The MP3-in-WAV fixture, one bitstream wrapped once more:
//
//	ffmpeg -i container/mp4/testdata/mp3.mp3 -c copy mp3.wav
//
// (ffmpeg 8.0.1, Ubuntu.) It is the same 41-frame libmp3lame encode the MP4
// fixtures carry, so the differential in tests/ compares one encode read
// through two containers rather than two encodes agreeing. `-c copy` into a
// WAV strips the Xing frame, so the file's only statement of its length is
// the fact chunk's 23616, the coded capacity with nothing said about trims.
const (
	wavMP3Frames = 41
	wavMP3SPF    = 576   // MPEG-2 Layer III
	wavMP3Raw    = 23616 // 41 frames, which is what fact declares
	// nCodecDelay as ffmpeg writes it, on every MP3 it wraps whatever the
	// stream's own LAME tag says. It is reported and never applied.
	wavMP3CodecDelay = 1393
)

func fixtureBytes(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// mp3Extra builds an MPEGLAYER3WAVEFORMAT cbSize region: wID 1 (MPEG),
// fdwFlags 2, nBlockSize 1152, nFramesPerBlock 1, and the codec delay.
func mp3Extra(codecDelay uint16) []byte {
	var b bytes.Buffer
	b.Write(u16le(1))
	b.Write(u32le(2))
	b.Write(u16le(1152))
	b.Write(u16le(1))
	b.Write(u16le(codecDelay))
	return b.Bytes()
}

// mp3Payload returns the fixture's data chunk: the bare frame run, which
// every hand-built cell here wraps in a header of its own.
func mp3Payload(t testing.TB) []byte {
	t.Helper()
	raw := fixtureBytes(t, "mp3.wav")
	i := bytes.Index(raw, []byte("data"))
	if i < 0 {
		t.Fatal("the fixture has no data chunk")
	}
	off := i + 8
	return raw[off : off+int(le.Uint32(raw[i+4:]))]
}

// xingFrame builds a Xing metadata frame in the geometry of the frame behind
// it, carrying a frame count and the LAME gapless extension. libmp3lame
// writes one at the head of an .mp3 and `-c copy` into a WAV drops it, so
// this is how the trimmed shape gets tested at all.
func xingFrame(t testing.TB, after []byte, frames, delay, padding uint16) []byte {
	t.Helper()
	h, err := mp3.ParseHeader(after)
	if err != nil {
		t.Fatal(err)
	}
	f := make([]byte, h.Size())
	copy(f, after[:mp3.HeaderLen])
	off := mp3.HeaderLen + h.SideInfoLen()
	if h.Protected {
		off += 2
	}
	copy(f[off:], "Xing")
	f[off+7] = 1 // flags: frame count present
	f[off+11] = byte(frames)
	f[off+10] = byte(frames >> 8)
	p := off + 12
	copy(f[p:], "LAME3.100")
	g := f[p+9+12:]
	g[0] = byte(delay >> 4)
	g[1] = byte(delay<<4) | byte(padding>>8)
	g[2] = byte(padding)
	return f
}

// walkMP3 reads every packet and returns the frame offsets and their
// durations, which is what a packet walk over this payload amounts to.
func walkMP3(t *testing.T, d *Demuxer) (data []byte, pts []int64) {
	t.Helper()
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if errors.Is(err, io.EOF) {
			return data, pts
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if pkt.Dur != wavMP3SPF {
			t.Fatalf("packet at %d carries %d samples, want %d", pkt.PTS, pkt.Dur, wavMP3SPF)
		}
		data = append(data, pkt.Data...)
		pts = append(pts, pkt.PTS)
	}
}

// TestMP3Track reads the fixture's track. Every field here is the file's own
// statement about itself except the format, which is the first frame's.
func TestMP3Track(t *testing.T) {
	d, err := NewDemuxer(container.BytesSource(fixtureBytes(t, "mp3.wav")), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Codec != codec.MP3 {
		t.Fatalf("codec = %q, want mp3", tr.Codec)
	}
	// The decoder's output format, not the chunk's: Layer III reconstruction
	// is floating point, and the fmt chunk writes 0 in wBitsPerSample.
	want := audio.Format{Rate: 22050, Channels: 1, Layout: audio.DefaultLayout(1),
		Type: audio.Float, BitDepth: 32}
	if tr.Fmt != want {
		t.Errorf("format = %v, want %v", tr.Fmt, want)
	}
	if len(tr.CodecConfig) != 0 {
		t.Errorf("codec config = %d bytes, want none: an MPEG frame states its own header", len(tr.CodecConfig))
	}
	if tr.Samples != wavMP3Raw || !tr.SamplesAdvisory || tr.SamplesExact {
		t.Errorf("length = %d (advisory %v, exact %v), want %d advisory",
			tr.Samples, tr.SamplesAdvisory, tr.SamplesExact, wavMP3Raw)
	}
	// Decision, not an omission: ffmpeg stamps the same nCodecDelay on every
	// MP3 it wraps (1393 here, where this stream's own LAME tag said 1105)
	// and ffmpeg's own WAV reader ignores the field. A trim comes from a
	// metadata frame or from nowhere.
	if tr.Delay != 0 || tr.Padding != 0 {
		t.Errorf("trims = %d/%d, want 0/0: nCodecDelay is not a gapless field", tr.Delay, tr.Padding)
	}
	warns := d.Warnings()
	if len(warns) != 1 || warns[0].Kind != container.Note {
		t.Fatalf("warnings = %v, want one Note", warns)
	}
	if !strings.Contains(warns[0].Msg, "1393") {
		t.Errorf("note = %q, want the declared delay named", warns[0].Msg)
	}
	// A Note must survive Strict: the file is well formed and this is a
	// thing the reader is choosing.
	if _, err := NewDemuxer(container.BytesSource(fixtureBytes(t, "mp3.wav")), &DemuxerOptions{Strict: true}); err != nil {
		t.Errorf("strict mode refused a well-formed file: %v", err)
	}
}

// TestMP3PacketsAreWholeFrames pins the packet model against the framing the
// codec itself reads: one packet per frame, delivered whole, at the timeline
// the frame index gives.
func TestMP3PacketsAreWholeFrames(t *testing.T) {
	payload := mp3Payload(t)
	d, err := NewDemuxer(container.BytesSource(fixtureBytes(t, "mp3.wav")), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, pts := walkMP3(t, d)
	if len(pts) != wavMP3Frames {
		t.Fatalf("walked %d packets, want %d", len(pts), wavMP3Frames)
	}
	for i, got := range pts {
		if want := int64(i) * wavMP3SPF; got != want {
			t.Fatalf("packet %d PTS = %d, want %d", i, got, want)
		}
	}
	if !bytes.Equal(data, payload) {
		t.Error("the packets are not the data chunk's bytes end to end")
	}
	// And the framing is the one mp3.ParseHeader hops: the walk must not
	// invent a boundary the decoder would not find, or miss one it would.
	hops := 0
	for off := 0; off < len(payload); hops++ {
		h, err := mp3.ParseHeader(payload[off:])
		if err != nil {
			t.Fatalf("frame %d at %d does not parse: %v", hops, off, err)
		}
		off += h.Size()
	}
	if hops != len(pts) {
		t.Errorf("the header hop finds %d frames, the walk delivered %d", hops, len(pts))
	}
}

// TestMP3XingLeadPayloadTakesItsTrims is the gapless shape, and the reason
// the fixture above cannot test it: `-c copy` into a WAV drops the metadata
// frame, so the trimmed track only exists where a writer kept one.
//
// The numbers are the source .mp3's own (576 encoder samples, 990 padding),
// so the track this reads is the track container/mpa reads from the same
// frames: 22050 samples behind a 1105-sample trim.
func TestMP3XingLeadPayloadTakesItsTrims(t *testing.T) {
	frames := mp3Payload(t)
	payload := append(xingFrame(t, frames, wavMP3Frames, 576, 990), frames...)
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(wavMP3CodecDelay), payload, -1)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Delay != 576+529 || tr.Padding != 990-529 {
		t.Errorf("trims = %d/%d, want %d/%d", tr.Delay, tr.Padding, 576+529, 990-529)
	}
	if tr.Samples != 22050 {
		t.Errorf("samples = %d, want 22050", tr.Samples)
	}
	if tr.SamplesAdvisory {
		t.Error("a LAME-stated length is not a rounded one")
	}
	// The metadata frame is not audio and must not be delivered.
	_, pts := walkMP3(t, d)
	if len(pts) != wavMP3Frames {
		t.Errorf("walked %d packets, want %d: the Xing frame was delivered as audio", len(pts), wavMP3Frames)
	}
}

// TestMP3XingCountIsSettledByTheWalk: the same wrapped run cut inside its
// final frame. The Xing count is a claim, so the walk that confirms it here
// shrinks it where the payload cannot fill it, and the settled length is what
// a read of the chunk delivers.
func TestMP3XingCountIsSettledByTheWalk(t *testing.T) {
	frames := mp3Payload(t)
	payload := append(xingFrame(t, frames, wavMP3Frames, 576, 990), frames...)
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(wavMP3CodecDelay), payload[:len(payload)-100], -1)

	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Walk(); err != nil {
		t.Fatalf("tolerant Walk: %v", err)
	}
	tr := d.Tracks()[0]
	if !tr.SamplesExact || tr.Samples >= 22050 {
		t.Errorf("settled to %d (exact %v), want less than the declared 22050, exact", tr.Samples, tr.SamplesExact)
	}
	var damage int
	for _, w := range d.Warnings() {
		if w.Kind == container.Damage {
			damage++
		}
	}
	if damage != 2 {
		t.Errorf("damage findings = %v, want the dropped frame and the shortfall", d.Warnings())
	}
	// The measurement is what a read hands out: the front trim comes off the
	// raw run and the declared length no longer caps it.
	d2, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, pts := walkMP3(t, d2)
	if want := int64(len(pts))*wavMP3SPF - tr.Delay; tr.Samples != want {
		t.Errorf("settled to %d, the %d packets a read delivers hold %d", tr.Samples, len(pts), want)
	}

	strict, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.Walk(); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict Walk = %v, want malformed", err)
	}
}

// TestMP3UntaggedLengthIsUnknown: with no metadata frame and no fact chunk
// nothing in the file counted the samples, and -1 says so rather than the
// walk's own guess.
func TestMP3UntaggedLengthIsUnknown(t *testing.T) {
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), mp3Payload(t), -1)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != -1 || tr.SamplesAdvisory {
		t.Errorf("length = %d (advisory %v), want -1", tr.Samples, tr.SamplesAdvisory)
	}
	if warns := d.Warnings(); len(warns) != 0 {
		t.Errorf("warnings = %v; a zero nCodecDelay has nothing to say", warns)
	}
}

// TestMP3ZeroFactIsNotALength: the walk found a frame, so the payload holds
// at least one frame's worth of audio, and a chunk saying zero is a writer
// that never patched it. Unknown beats a number that is provably wrong.
func TestMP3ZeroFactIsNotALength(t *testing.T) {
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), mp3Payload(t), 0)
	d, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != -1 {
		t.Errorf("samples = %d, want -1", tr.Samples)
	}
}

// TestMP3FactBeyondThePayloadIsIgnored: the exact frame count costs the walk
// the lazy index exists to avoid, but the payload's byte length bounds it for
// free, and a count above that bound is one the file cannot be describing.
// Without it a four-byte field states four billion samples and every caller
// that sizes a buffer from a track length believes it.
func TestMP3FactBeyondThePayloadIsIgnored(t *testing.T) {
	payload := mp3Payload(t)
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), payload, 1<<31)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != -1 {
		t.Errorf("samples = %d, want -1", tr.Samples)
	}
	warns := d.Warnings()
	if len(warns) != 1 || warns[0].Kind != container.Damage ||
		!strings.Contains(warns[0].Msg, "more than") {
		t.Fatalf("warnings = %v, want one Damage naming the bound", warns)
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted a length the payload cannot hold")
	}
	// The bound is loose on purpose: a count the payload COULD hold is the
	// file's to state, since checking it exactly is the walk.
	ceiling := int64(len(payload)) / 24 * wavMP3SPF
	raw = wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), payload, ceiling)
	d, err = NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != ceiling || !tr.SamplesAdvisory {
		t.Errorf("samples = %d advisory = %v, want %d advisory", tr.Samples, tr.SamplesAdvisory, ceiling)
	}
}

// TestMP3SeekBacksOffForTheReservoir holds the seek to both halves of Layer
// III's inter-frame state: the filterbank's overlap history and the bit
// reservoir, which lets a frame's main data begin up to 511 bytes earlier in
// the stream. A landing exactly on the target frame reads as exact and
// decodes to garbage.
func TestMP3SeekBacksOffForTheReservoir(t *testing.T) {
	// A payload long enough that the backoff has room to work: the fixture's
	// 41 frames are inside the backoff distance from end to end.
	frames := mp3Payload(t)
	payload := bytes.Repeat(frames, 20)
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), payload, -1)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := int64(400 * wavMP3SPF)
	landed, err := d.SeekSample(0, target)
	if err != nil {
		t.Fatal(err)
	}
	if landed > target {
		t.Fatalf("seek to %d landed at %d, past the target", target, landed)
	}
	if landed%wavMP3SPF != 0 {
		t.Errorf("landing %d is not a frame boundary", landed)
	}
	// Three frames is the filterbank's share alone. Anything at or above
	// that landing means the reservoir walk contributed nothing, which at
	// this bit rate it must.
	if want := target - 3*wavMP3SPF; landed >= want {
		t.Errorf("seek to %d landed at %d; the filterbank's own backoff reaches %d, so the reservoir walk did nothing",
			target, landed, want)
	}
	if landed == 0 {
		t.Errorf("seek to %d landed at 0; the backoff is bounded, not a rewind", target)
	}
	// A target inside the backoff distance clamps at the top of the payload
	// rather than going negative, and the next packet starts there.
	if landed, err := d.SeekSample(0, wavMP3SPF); err != nil || landed != 0 {
		t.Errorf("seek to %d = %d, %v; want 0", wavMP3SPF, landed, err)
	}
	var pkt container.Packet
	if err := d.ReadPacket(&pkt); err != nil || pkt.PTS != 0 {
		t.Errorf("post-seek packet PTS = %d (%v), want 0", pkt.PTS, err)
	}
}

// TestMP3FmtChunkDisagreeingWithTheFrames pins which side wins. The fmt
// chunk is the container's claim and the frame header is the stream's own;
// codec/mp3 refuses a frame that disagrees with the track it was built for,
// so believing the chunk opens a file that then fails at its first packet.
func TestMP3FmtChunkDisagreeingWithTheFrames(t *testing.T) {
	raw := wavHeader(tagMP3, 2, 44100, wavMP3SPF, 0, mp3Extra(0), mp3Payload(t), -1)
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := d.Tracks()[0]
	if tr.Fmt.Rate != 22050 || tr.Fmt.Channels != 1 {
		t.Errorf("format = %v, want the frame's 22050 mono", tr.Fmt)
	}
	warns := d.Warnings()
	if len(warns) != 1 || warns[0].Kind != container.Damage || !strings.Contains(warns[0].Msg, "the frame wins") {
		t.Fatalf("warnings = %v, want one Damage naming the winner", warns)
	}
	if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); !errors.Is(err, waxerr.ErrMalformedInput) {
		t.Errorf("strict mode error = %v, want malformed", err)
	}
}

// TestMP3ShortExtraIsDamage: the twelve bytes behind the WAVEFORMATEX are
// what makes the tag's own structure, and a header without them is out of
// spec. Nothing here needs them, so it is damage and not a refusal.
func TestMP3ShortExtraIsDamage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []byte
	}{
		{"no cbSize at all", nil},
		{"cbSize 0", []byte{}},
		{"half a header", mp3Extra(1393)[:6]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, tc.extra, mp3Payload(t), wavMP3Raw)
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tr := d.Tracks()[0]; tr.Samples != wavMP3Raw {
				t.Errorf("samples = %d, want the fact chunk's %d", tr.Samples, wavMP3Raw)
			}
			warns := d.Warnings()
			if len(warns) != 1 || warns[0].Kind != container.Damage {
				t.Fatalf("warnings = %v, want one Damage", warns)
			}
			if _, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true}); err == nil {
				t.Error("strict mode accepted a header missing its own extra")
			}
		})
	}
}

// frameOffsets hops the payload the way the codec does, so a test that
// wants to cut between two frames can say which two.
func frameOffsets(t testing.TB, payload []byte) []int {
	t.Helper()
	var offs []int
	for off := 0; off < len(payload); {
		h, err := mp3.ParseHeader(payload[off:])
		if err != nil {
			t.Fatalf("frame at %d does not parse: %v", off, err)
		}
		offs = append(offs, off)
		off += h.Size()
	}
	return offs
}

// TestMP3ToleratedDamage walks payloads a writer or a disk damaged. The
// wording is pinned because it reaches users, and because it is the same
// wording the bare-stream reader has always used for the same finding.
//
// Every finding but the leading one arrives during the walk rather than at
// open, which is what a lazily built index means: the frames past the head
// are not looked at until something asks for them, so Strict fails at the
// packet that reaches the damage. The bare stream behaves the same way.
// A strict probe finishes that walk through container.Walker before its
// verdict, so every row here refuses it; a tolerant walk leaves the finding
// in Warnings.
func TestMP3ToleratedDamage(t *testing.T) {
	frames := mp3Payload(t)
	offs := frameOffsets(t, frames)
	junk := func(n int) []byte { return bytes.Repeat([]byte{0x11}, n) }
	cut := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	for _, tc := range []struct {
		name    string
		payload []byte
		want    string
		atOpen  bool
		frames  int
	}{
		{"leading junk", cut(junk(32), frames),
			"32 unparsable bytes before the first frame", true, wavMP3Frames},
		{"junk between frames", cut(frames[:offs[2]], junk(16), frames[offs[2]:]),
			"16 unparsable bytes skipped", false, wavMP3Frames},
		{"truncated final frame", frames[:offs[len(offs)-1]+8],
			"truncated final frame dropped", false, wavMP3Frames - 1},
		{"bytes that are not frames at the end", cut(frames, junk(40)),
			"40 trailing bytes are not frames, dropped", false, wavMP3Frames},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), tc.payload, -1)
			d, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, pts := walkMP3(t, d); len(pts) != tc.frames {
				t.Errorf("walked %d frames, want %d", len(pts), tc.frames)
			}
			warns := d.Warnings()
			if len(warns) != 1 || warns[0].Msg != tc.want {
				t.Fatalf("warnings = %v, want %q", warns, tc.want)
			}
			if warns[0].Kind != container.Damage {
				t.Errorf("kind = %v, want Damage", warns[0].Kind)
			}
			// A walk finishes the index without a read: under tolerance the
			// finding lands in Warnings the same way, and Walked flips.
			walked, err := NewDemuxer(container.BytesSource(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if walked.Walked() && !tc.atOpen {
				t.Error("Walked before any walk")
			}
			if err := walked.Walk(); err != nil {
				t.Fatalf("a tolerant Walk failed: %v", err)
			}
			if !walked.Walked() {
				t.Error("not Walked after Walk")
			}
			if ws := walked.Warnings(); len(ws) != 1 || ws[0].Msg != tc.want {
				t.Errorf("warnings after Walk = %v, want %q", ws, tc.want)
			}

			strict, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
			switch {
			case tc.atOpen:
				if err == nil {
					t.Fatal("strict mode accepted damage at the head")
				}
				return
			case err != nil:
				t.Fatalf("strict mode refused at open, before the walk reached the damage: %v", err)
			}
			var pkt container.Packet
			for {
				err := strict.ReadPacket(&pkt)
				if err == nil {
					continue
				}
				if errors.Is(err, io.EOF) {
					t.Fatal("strict mode walked past the damage")
				}
				if !errors.Is(err, waxerr.ErrMalformedInput) {
					t.Fatalf("strict mode failed with %v, want malformed", err)
				}
				break
			}
			// The strict probe's path: Walk refuses with the same error.
			strict, err = NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := strict.Walk(); !errors.Is(err, waxerr.ErrMalformedInput) {
				t.Errorf("strict Walk returned %v, want malformed", err)
			}
		})
	}
}

// TestMP3IndexRoundTrips takes the frame index through the sidecar blob. The
// payload is synthesized rather than committed because the snapshot has a
// floor: below a few thousand frames rebuilding the index beats a disk round
// trip, which is what the fixture's 41 frames pin on the other side.
func TestMP3IndexRoundTrips(t *testing.T) {
	frames := mp3Payload(t)
	payload := bytes.Repeat(frames, 120) // 4920 frames
	raw := wavHeader(tagMP3, 1, 22050, wavMP3SPF, 0, mp3Extra(0), payload, -1)
	src := container.BytesSource(raw)

	d, err := NewDemuxer(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if blob := d.IndexSnapshot(); blob != nil {
		t.Fatal("snapshot before any walk should be nil")
	}
	if _, err := d.SeekSample(0, 4919*wavMP3SPF); err != nil {
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
	l1, err := d.SeekSample(0, 3_000_000)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := d2.SeekSample(0, 3_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if l1 != l2 {
		t.Errorf("the restored index seeks to %d, the walked one to %d", l2, l1)
	}
	var p1, p2 container.Packet
	if err := d.ReadPacket(&p1); err != nil {
		t.Fatal(err)
	}
	if err := d2.ReadPacket(&p2); err != nil {
		t.Fatal(err)
	}
	if p1.PTS != p2.PTS || !bytes.Equal(p1.Data, p2.Data) {
		t.Error("the restored index delivers different packets")
	}
	// A walk that has already delivered frames declines: a restored index
	// would move them under the caller.
	if d2.RestoreIndex(blob) {
		t.Error("a progressed walk accepted an index")
	}
}

// TestByteLinearPayloadsHaveNoIndex is the other half of the capability
// gate: format.Media advertises container.Indexer by type assertion, so a
// PCM WAV must answer nil and false rather than nothing at all.
func TestByteLinearPayloadsHaveNoIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"pcm", wavHeader(tagPCM, 1, 8000, 2, 16, nil, make([]byte, 64), -1)},
		{"mp3, below the floor", fixtureBytes(t, "mp3.wav")},
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
			// The same per-file answer on container.Walker: the byte-linear
			// payload has nothing to walk, and the short frame run was just
			// walked to its end by the seek.
			if err := d.Walk(); err != nil || !d.Walked() {
				t.Errorf("Walk = %v, Walked = %v; want nil and true for a payload with nothing left to walk", err, d.Walked())
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
