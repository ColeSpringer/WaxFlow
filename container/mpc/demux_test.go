package mpc_test

import (
	"io"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mpc"
	"github.com/colespringer/waxflow/waxerr"
)

// TestSV7HeaderFields parses a hand-built header, every field with a value
// that would show a swapped or shifted read.
func TestSV7HeaderFields(t *testing.T) {
	h := sv7Header{frames: 3, ms: true, maxBand: 27, profile: 10, freq: 1, gapless: true, last: 700,
		encVersion: 116, pns: true, titleGain: 0, titlePeak: 0}
	frames := []sv7Frame{randomFrame(1, 300), randomFrame(2, 517), randomFrame(3, 33)}
	decay := randomFrame(4, 100)
	raw := buildSV7(h, frames, 700, &decay)
	d := open(t, raw, true)
	track := d.Tracks()[0]
	cfg, err := musepack.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := musepack.Config{StreamVersion: 7, Rate: 48000, Channels: 2, MaxBand: 27, MS: true, PNS: 1, TrueGapless: true, LastFrameSamples: 700}
	if cfg != want {
		t.Errorf("config %+v, want %+v", cfg, want)
	}
	if track.Codec != codec.Musepack || track.Delay != musepack.SynthDelay || !track.SamplesExact {
		t.Errorf("track %+v", track)
	}
	if want := int64(2*musepack.FrameLength + 700); track.Samples != want {
		t.Errorf("Samples = %d, want %d", track.Samples, want)
	}
	if d.TotalForTest() != 4 {
		t.Errorf("delivers %d packets, want 3 frames plus the decay frame", d.TotalForTest())
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("warnings on a clean stream: %v", d.Warnings())
	}
}

// TestSV7Realignment pins the packet payloads: each is the frame's bits
// realigned to a byte boundary, pad bits zero, the header stating the length,
// and the same bits the stream carried.
func TestSV7Realignment(t *testing.T) {
	h := sv7Header{frames: 4, maxBand: 20, freq: 0, gapless: true, last: 480}
	frames := []sv7Frame{randomFrame(11, 8), randomFrame(12, 4091), randomFrame(13, 9), randomFrame(14, 2999)}
	raw := buildSV7(h, frames, 480, nil)
	d := open(t, raw, true)
	cfg, _ := musepack.ParseConfig(d.Tracks()[0].CodecConfig)
	pkts := readAll(t, d)
	if len(pkts) != len(frames) {
		t.Fatalf("%d packets for %d frames", len(pkts), len(frames))
	}
	for i, p := range pkts {
		hd, state, payload, err := musepack.ParsePacketHeader(p.Data, cfg)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if hd.Frames != 1 || hd.BitLen != frames[i].n || state != nil {
			t.Errorf("packet %d header %+v, want one frame of %d bits and no state", i, hd, frames[i].n)
		}
		if len(payload) != (frames[i].n+7)/8 {
			t.Errorf("packet %d payload is %d bytes for %d bits", i, len(payload), frames[i].n)
		}
		for b := 0; b < len(payload)*8; b++ {
			want := uint64(0)
			if b < frames[i].n {
				want = bitAt(frames[i].bits, b)
			}
			if got := bitAt(payload, b); got != want {
				t.Fatalf("packet %d bit %d = %d, want %d", i, b, got, want)
			}
		}
		if p.PTS != int64(i)*musepack.FrameLength || p.Dur != musepack.FrameLength || p.Sync != (i == 0) {
			t.Errorf("packet %d PTS %d Dur %d Sync %v", i, p.PTS, p.Dur, p.Sync)
		}
	}
}

// TestSV7TailRegimes pins the decay-frame rule and the length that follows
// from it: the frame past the header's count exists exactly when the last
// frame's sample count exceeds what the synthesis delay leaves in the frame
// before it, and the stream's own count wins over the header's.
func TestSV7TailRegimes(t *testing.T) {
	frames := []sv7Frame{randomFrame(1, 200), randomFrame(2, 300), randomFrame(3, 250)}
	decay := randomFrame(4, 120)
	cases := []struct {
		name        string
		gapless     bool
		headerLast  int
		tail        int
		decay       bool
		wantSamples int64
		wantTotal   int64
		warn        string
	}{
		{"short tail, no decay frame", true, 480, 480, false, 2*1152 + 480, 3, ""},
		{"exactly the threshold", true, 671, 671, false, 2*1152 + 671, 3, ""},
		{"long tail with its decay frame", true, 700, 700, true, 2*1152 + 700, 4, ""},
		{"full last frame written as zero", true, 0, 0, true, 3 * 1152, 4, ""},
		{"stream and header disagree", true, 500, 700, true, 2*1152 + 700, 4, "the header says the last frame holds 500 samples, the stream says 700"},
		{"decay frame missing", true, 700, 700, false, 3*1152 - 481, 3, "need a decay frame the stream does not hold"},
		{"not gapless", false, 700, 700, false, 3*1152 - 481, 3, ""},
		{"tail past a frame", true, 480, 1500, false, 2*1152 + 480, 3, "more than a frame holds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := sv7Header{frames: 3, maxBand: 20, gapless: c.gapless, last: c.headerLast}
			var dec *sv7Frame
			if c.decay {
				dec = &decay
			}
			raw := buildSV7(h, frames, c.tail, dec)
			d := open(t, raw, false)
			if got := d.Tracks()[0].Samples; got != c.wantSamples {
				t.Errorf("Samples = %d, want %d", got, c.wantSamples)
			}
			if got := d.TotalForTest(); got != c.wantTotal {
				t.Errorf("delivers %d packets, want %d", got, c.wantTotal)
			}
			pkts := readAll(t, d)
			if int64(len(pkts)) != c.wantTotal {
				t.Errorf("read %d packets, want %d", len(pkts), c.wantTotal)
			}
			warnings := d.Warnings()
			switch {
			case c.warn == "" && len(warnings) != 0:
				t.Errorf("unexpected warnings: %v", warnings)
			case c.warn != "" && !hasWarning(warnings, c.warn):
				t.Errorf("warnings %v lack %q", warnings, c.warn)
			}
			if c.warn != "" {
				if _, err := mpc.NewDemuxer(container.BytesSource(raw), &mpc.DemuxerOptions{Strict: true}); err == nil {
					t.Error("strict mode accepted a stream that warned")
				}
			}
		})
	}
}

func hasWarning(ws []container.Warning, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w.Msg, sub) {
			return true
		}
	}
	return false
}

// TestSV8Packets parses a hand-built packet stream: SH, RG, EI, SO pointing at
// an ST, audio blocks, chapters, SE. The frame total follows from the count
// the way the reference decodes, and the last block's frame count lands in
// its packet header.
func TestSV8Packets(t *testing.T) {
	const count, silence = 3000, 100
	h := sv8Header{count: count, silence: silence, freq: 2, maxBand: 25, channels: 1, ms: false, blockPwr: 2}
	// 3000+481 samples need 4 frames; with 4 frames per block that is one
	// block. Two blocks are written: the second is tolerated and unread.
	stream := buildSV8(
		sv8Packet("SH", shPayload(h)),
		sv8Packet("RG", rgPayload(1, 17000, 21000, 18000, 22000)),
		sv8Packet("EI", eiPayload(80, true, 1, 30, 1)),
		sv8Packet("XX", []byte{1, 2, 3}), // an unknown key is skipped by size
		sv8Packet("AP", apBlock(50)),
		sv8Packet("AP", apBlock(60)),
		sv8Packet("SE", nil),
	)
	d := open(t, stream, true)
	track := d.Tracks()[0]
	cfg, err := musepack.ParseConfig(track.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := musepack.Config{StreamVersion: 8, Rate: 37800, Channels: 1, MaxBand: 25, BlockPwr: 2, PNS: 1, TrueGapless: true}
	if cfg != want {
		t.Errorf("config %+v, want %+v", cfg, want)
	}
	if track.Samples != count-silence || track.Delay != musepack.SynthDelay+silence || !track.SamplesExact {
		t.Errorf("track Samples %d Delay %d exact %v", track.Samples, track.Delay, track.SamplesExact)
	}
	pkts := readAll(t, d)
	if len(pkts) != 1 {
		t.Fatalf("%d packets, want 1", len(pkts))
	}
	hd, _, payload, err := musepack.ParsePacketHeader(pkts[0].Data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if hd.Frames != framesFor(count) || hd.BitLen != 0 || string(payload) != string(apBlock(50)) {
		t.Errorf("packet header %+v with %d payload bytes", hd, len(payload))
	}
	if !pkts[0].Sync || pkts[0].PTS != 0 || pkts[0].Dur != int64(hd.Frames)*musepack.FrameLength {
		t.Errorf("packet PTS %d Dur %d Sync %v", pkts[0].PTS, pkts[0].Dur, pkts[0].Sync)
	}
	tags := d.Tags()
	if got := tags["REPLAYGAIN_TRACK_GAIN"]; len(got) != 1 || got[0] != gainString(17000) {
		t.Errorf("REPLAYGAIN_TRACK_GAIN = %v, want %q", got, gainString(17000))
	}
	if got := tags["REPLAYGAIN_ALBUM_PEAK"]; len(got) != 1 || got[0] != peakString(22000) {
		t.Errorf("REPLAYGAIN_ALBUM_PEAK = %v, want %q", got, peakString(22000))
	}

	// Fewer blocks than the count needs: a warning, and the length shrinks to
	// what the blocks deliver.
	h2 := sv8Header{count: 20000, silence: 0, freq: 0, maxBand: 25, channels: 2, ms: true, blockPwr: 0}
	short := buildSV8(sv8Packet("SH", shPayload(h2)), sv8Packet("AP", apBlock(10)), sv8Packet("AP", apBlock(10)), sv8Packet("SE", nil))
	d = open(t, short, false)
	if got := d.Tracks()[0].Samples; got != 2*musepack.FrameLength-musepack.SynthDelay {
		t.Errorf("truncated stream Samples = %d, want %d", got, 2*musepack.FrameLength-musepack.SynthDelay)
	}
	if !hasWarning(d.Warnings(), "only 2 blocks are present") {
		t.Errorf("warnings %v", d.Warnings())
	}
	if _, err := mpc.NewDemuxer(container.BytesSource(short), &mpc.DemuxerOptions{Strict: true}); err == nil {
		t.Error("strict mode accepted a stream short of its declared blocks")
	}

	// An unknown length whose last block is not parsable (the payloads here
	// are arbitrary bytes): every block is read as a full one, the length is
	// what those frames deliver and is advisory, and the shape warns.
	h3 := sv8Header{count: 0, freq: 0, maxBand: 25, channels: 2, blockPwr: 2}
	unknown := buildSV8(sv8Packet("SH", shPayload(h3)), sv8Packet("AP", apBlock(10)), sv8Packet("AP", apBlock(10)), sv8Packet("SE", nil))
	d = open(t, unknown, false)
	if tr := d.Tracks()[0]; tr.Samples != 2*4*musepack.FrameLength-musepack.SynthDelay || tr.SamplesExact || d.TotalForTest() != 2 {
		t.Errorf("unknown-length track %+v delivering %d packets", tr, d.TotalForTest())
	}
	if !hasWarning(d.Warnings(), "does not parse") {
		t.Errorf("warnings %v", d.Warnings())
	}
	pkts = readAll(t, d)
	if hd, _, _, _ := musepack.ParsePacketHeader(pkts[1].Data, musepack.Config{StreamVersion: 8, Rate: 44100, Channels: 2, MaxBand: 25, BlockPwr: 2}); hd.Frames != 4 {
		t.Errorf("unknown-length blocks carry %d frames, want the full 4", hd.Frames)
	}
}

// TestUnknownLengthCountsTheLastBlock pins the unknown-length shape on real
// frames: a fixture's header rewritten to state no count derives its last
// block's frame count by parsing, delivers every frame less the synthesis
// delay as an exact length, and decodes exactly as the counted stream does.
func TestUnknownLengthCountsTheLastBlock(t *testing.T) {
	raw := codecFixture(t, "sv8-blocks.mpc")
	counted := open(t, raw, true)
	cfg, _ := musepack.ParseConfig(counted.Tracks()[0].CodecConfig)
	// Rewrite SH: the same fields with the count zeroed and the CRC redone.
	blk, ok := counted.SV8HeaderForTest(4)
	if !ok || blk.Key != "SH" {
		t.Fatal("no SH at offset 4")
	}
	sh := shPayload(sv8Header{count: 0, silence: 0, freq: 0, maxBand: cfg.MaxBand, channels: cfg.Channels, ms: cfg.MS, blockPwr: cfg.BlockPwr})
	rewritten := append([]byte("MPCK"), sv8Packet("SH", sh)...)
	rewritten = append(rewritten, raw[4+blk.HdrLen+int(blk.Payload):]...)
	d := open(t, rewritten, true)
	track := d.Tracks()[0]
	if !track.SamplesExact || d.TotalForTest() != counted.TotalForTest() {
		t.Fatalf("unknown-length track %+v delivering %d packets, the counted stream %d", track, d.TotalForTest(), counted.TotalForTest())
	}
	got := decodeAll(t, d)
	want := decodeAll(t, counted)
	if len(got) != len(want) {
		t.Fatalf("%d vs %d raw samples", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sample %d differs", i)
		}
	}
	// Every frame less the delay: the counted stream's own frames end past
	// its count by the encoder's flush, and that is what an uncounted stream
	// delivers.
	if want := int64(len(got)/cfg.Channels) - musepack.SynthDelay; track.Samples != want {
		t.Errorf("Samples = %d, want %d", track.Samples, want)
	}
}

// gainString and peakString spell the replay gain tags the way the demuxer
// does from the storage form.
func gainString(gain uint16) string {
	return strconv.FormatFloat(64.82-float64(gain)/256, 'f', 2, 64) + " dB"
}

func peakString(peak uint16) string {
	return strconv.FormatFloat(math.Pow(10, float64(peak)/256/20)/32768, 'f', 6, 64)
}

// TestDemuxPackets pins PTS, duration and sync on the committed fixtures.
func TestDemuxPackets(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc", "seek-pns.mpc", "gapless-sv7.mpc"} {
		t.Run(name, func(t *testing.T) {
			d := open(t, fixture(t, name), true)
			track := d.Tracks()[0]
			cfg, _ := musepack.ParseConfig(track.CodecConfig)
			fpb := int64(cfg.FramesPerBlock())
			pkts := readAll(t, d)
			if len(pkts) < 3 {
				t.Fatalf("%d packets; the fixture is meant to have several", len(pkts))
			}
			var pts int64
			for i, p := range pkts {
				hd, _, _, err := musepack.ParsePacketHeader(p.Data, cfg)
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
				if p.PTS != pts || p.Dur != int64(hd.Frames)*musepack.FrameLength {
					t.Errorf("packet %d PTS %d Dur %d, want PTS %d Dur %d", i, p.PTS, p.Dur, pts, int64(hd.Frames)*musepack.FrameLength)
				}
				if cfg.StreamVersion == 8 {
					// A block is an exact resume point when nothing draws from
					// the noise generator, or at the stream's start.
					if want := i == 0 || cfg.PNS == 0; p.Sync != want {
						t.Errorf("SV8 packet %d Sync %v, want %v (PNS %d)", i, p.Sync, want, cfg.PNS)
					}
					if i < len(pkts)-1 && int64(hd.Frames) != fpb {
						t.Errorf("SV8 packet %d carries %d frames, want a full block of %d", i, hd.Frames, fpb)
					}
				} else if p.Sync != (i == 0) {
					t.Errorf("SV7 packet %d Sync %v", i, p.Sync)
				}
				pts += p.Dur
			}
			if pts < track.Delay+track.Samples {
				t.Errorf("packets cover %d raw samples, the track needs %d", pts, track.Delay+track.Samples)
			}
			if cfg.StreamVersion == 8 {
				last, _, _, _ := musepack.ParsePacketHeader(pkts[len(pkts)-1].Data, cfg)
				if int64(last.Frames) >= fpb {
					t.Errorf("the last block is full (%d frames); the fixture is meant to end short", last.Frames)
				}
			}
		})
	}
}

// TestSeekLandsEarlyEnough pins the landing rule: at or before the frame that
// starts synthWarmup samples ahead of the target, never past the target, and
// a past-the-end target lands on the last packet.
func TestSeekLandsEarlyEnough(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc", "seek-pns.mpc"} {
		t.Run(name, func(t *testing.T) {
			d := open(t, fixture(t, name), true)
			track := d.Tracks()[0]
			raw := track.Delay + track.Samples
			for _, target := range []int64{0, 1, 511, 512, 513, 1152, 1663, 1664, 5000, 9999, raw / 2, raw - 1} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				early := max(target-mpc.SynthWarmupForTest, 0)
				if landed > target || (landed > early && landed != 0) {
					t.Errorf("seek to %d landed at %d; want at or before %d", target, landed, early)
				}
				if landed%musepack.FrameLength != 0 {
					t.Errorf("seek to %d landed at %d, not a frame boundary", target, landed)
				}
				var pkt container.Packet
				if err := d.ReadPacket(&pkt); err != nil {
					t.Fatalf("read after seek to %d: %v", target, err)
				}
				if pkt.PTS != landed || !pkt.Sync {
					t.Errorf("seek to %d: first packet PTS %d Sync %v, landed %d", target, pkt.PTS, pkt.Sync, landed)
				}
			}
			landed, err := d.SeekSample(0, raw+100000)
			if err != nil {
				t.Fatal(err)
			}
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatalf("read after a past-the-end seek: %v", err)
			}
			if pkt.PTS != landed || pkt.PTS+pkt.Dur < raw {
				t.Errorf("past-the-end seek landed at %d on a packet %d..%d, raw end %d", landed, pkt.PTS, pkt.PTS+pkt.Dur, raw)
			}
			if err := d.ReadPacket(&pkt); err != io.EOF {
				t.Errorf("read past the last packet = %v, want io.EOF", err)
			}
		})
	}
}

// TestSeekDecodesTheRightSamples is the seek gate: a decode resumed at the
// landing, with a fresh decoder and the state the packet carries, is
// bit-identical to the linear decode from the landing on, once the synthesis
// history has refilled. Both noise-substitution fixtures are in the matrix, so
// the generator state the landing carries is what makes them pass.
func TestSeekDecodesTheRightSamples(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc", "seek-pns.mpc", "gapless-sv7.mpc"} {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)
			d := open(t, raw, true)
			track := d.Tracks()[0]
			cfg, _ := musepack.ParseConfig(track.CodecConfig)
			ch := cfg.Channels
			linear := decodeAll(t, d)
			end := track.Delay + track.Samples
			for _, target := range []int64{0, 600, 1700, 4000, end / 3, end / 2, end - 2000, end - 1} {
				landed, err := d.SeekSample(0, target)
				if err != nil {
					t.Fatalf("seek to %d: %v", target, err)
				}
				got := decodeAll(t, d)
				ref := linear[int(landed)*ch:]
				if len(got) != len(ref) {
					t.Fatalf("seek to %d: resumed decode is %d samples, the linear tail %d", target, len(got), len(ref))
				}
				warm := min(mpc.SynthWarmupForTest*ch, len(got))
				for i := warm; i < len(got); i++ {
					if got[i] != ref[i] {
						t.Fatalf("seek to %d (landed %d): sample %d differs, %v vs %v", target, landed, i, got[i], ref[i])
					}
				}
				if target >= mpc.SynthWarmupForTest && landed > target-mpc.SynthWarmupForTest {
					t.Errorf("seek to %d landed at %d, inside the warm-up", target, landed)
				}
			}
		})
	}
}

// TestTags reads the APEv2 tag mppenc wrote and the ID3v1 the generator
// appended; both are peeled as trailers, neither draws a warning.
func TestTags(t *testing.T) {
	d := open(t, fixture(t, "tagged.mpc"), true)
	tags := d.Tags()
	for key, want := range map[string]string{
		"ARTIST": "Wax Test", "ALBUM": "Fixtures", "TITLE": "Tagged", "RECORDINGDATE": "2026", "TRACKNUMBER": "3",
	} {
		if got := tags[key]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %q", key, got, want)
		}
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("warnings on a tagged fixture: %v", d.Warnings())
	}
	if got := d.Tracks()[0].Samples; got != 8000 {
		t.Errorf("Samples = %d, want 8000", got)
	}
	// The tag block must not be read as audio: every packet decodes.
	decodeAll(t, d)
}

// TestReplayGain pins the gain conversion from both header forms against the
// tag spellings: the SV7 header carries the gain in hundredths of a dB and the
// peak as a 16-bit sample value, the SV8 packet the storage form directly, and
// the tag surfaces both as replay gain dB and a linear peak. A tag that
// already carries the keys wins.
func TestReplayGain(t *testing.T) {
	// SV7: -6.50 dB and a peak of half scale.
	titleGain := int16(-650)
	h := sv7Header{frames: 1, maxBand: 20, gapless: true, last: 480, titleGain: uint16(titleGain), titlePeak: 16384,
		albumGain: 325, albumPeak: 32767}
	d := open(t, buildSV7(h, []sv7Frame{randomFrame(1, 40)}, 480, nil), true)
	tags := d.Tags()
	if got := tags["REPLAYGAIN_TRACK_GAIN"]; len(got) != 1 || got[0] != "-6.50 dB" {
		t.Errorf("SV7 REPLAYGAIN_TRACK_GAIN = %v, want -6.50 dB", got)
	}
	if got := tags["REPLAYGAIN_ALBUM_GAIN"]; len(got) != 1 || got[0] != "+3.25 dB" {
		t.Errorf("SV7 REPLAYGAIN_ALBUM_GAIN = %v, want +3.25 dB", got)
	}
	peak := func(key string) float64 {
		got := tags[key]
		if len(got) != 1 {
			t.Fatalf("%s = %v", key, got)
		}
		v, err := strconv.ParseFloat(got[0], 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if p := peak("REPLAYGAIN_TRACK_PEAK"); math.Abs(p-0.5) > 1e-3 {
		t.Errorf("SV7 track peak %v, want about 0.5", p)
	}
	if p := peak("REPLAYGAIN_ALBUM_PEAK"); math.Abs(p-1) > 1e-3 {
		t.Errorf("SV7 album peak %v, want about 1", p)
	}
	// SV8: version 1 read, any other version ignored.
	sh := sv8Header{count: 1000, freq: 0, maxBand: 20, channels: 2, blockPwr: 0}
	d = open(t, buildSV8(sv8Packet("SH", shPayload(sh)), sv8Packet("RG", rgPayload(1, 18258, 21578, 0, 0)), sv8Packet("AP", apBlock(8)), sv8Packet("AP", apBlock(8)), sv8Packet("SE", nil)), true)
	tags = d.Tags()
	if got := tags["REPLAYGAIN_TRACK_GAIN"]; len(got) != 1 || got[0] != "-6.50 dB" {
		t.Errorf("SV8 REPLAYGAIN_TRACK_GAIN = %v, want -6.50 dB", got)
	}
	if _, ok := tags["REPLAYGAIN_ALBUM_GAIN"]; ok {
		t.Error("a zero album gain surfaced as a tag")
	}
	d = open(t, buildSV8(sv8Packet("SH", shPayload(sh)), sv8Packet("RG", rgPayload(2, 18258, 21578, 0, 0)), sv8Packet("AP", apBlock(8)), sv8Packet("AP", apBlock(8)), sv8Packet("SE", nil)), true)
	if got := d.Tags(); got != nil {
		t.Errorf("an RG packet of version 2 produced tags %v; the reference ignores it", got)
	}
}

// TestChapters pins the chapter search the reference performs: the CT run
// right after the seek table an SO packet points at, or else the run ending at
// the end marker; a run interrupted by another packet is not seen.
func TestChapters(t *testing.T) {
	sh := sv8Header{count: 10 * 1152, freq: 0, maxBand: 20, channels: 2, blockPwr: 0}
	ct := func(sample uint64, title string) []byte { return sv8Packet("CT", ctPayload(t, sample, title)) }
	blocks := func(n int) [][]byte {
		var out [][]byte
		for i := 0; i < n; i++ {
			out = append(out, sv8Packet("AP", apBlock(8+i)))
		}
		return out
	}
	// Chapters before SE, after the last audio block.
	tail := append(blocks(11), ct(0, "one"), ct(3000, "two"), sv8Packet("SE", nil))
	d := open(t, buildSV8(append([][]byte{sv8Packet("SH", shPayload(sh))}, tail...)...), true)
	chapters := d.Chapters()
	if len(chapters) != 2 || chapters[0].Title != "one" || chapters[1].Title != "two" || chapters[1].Start.Seconds() < 0.068 || chapters[1].Start.Seconds() > 0.069 {
		t.Errorf("chapters %+v", chapters)
	}
	// A run interrupted by an audio block is not the run before SE.
	mid := append([][]byte{sv8Packet("SH", shPayload(sh))}, blocks(5)...)
	mid = append(mid, ct(0, "lost"))
	mid = append(mid, blocks(6)...)
	mid = append(mid, sv8Packet("SE", nil))
	if got := open(t, buildSV8(mid...), true).Chapters(); len(got) != 0 {
		t.Errorf("a CT between audio blocks surfaced: %+v", got)
	}
	// SO pointing at an ST: the run after the ST is the one read, even with
	// audio blocks after it before SE.
	st := sv8Packet("ST", stPayload())
	head := [][]byte{sv8Packet("SH", shPayload(sh))}
	body := blocks(11)
	var bodyLen int
	for _, p := range body {
		bodyLen += len(p)
	}
	// The SO offset is from the SO packet's own start to the ST packet's, and
	// the packet's own length depends on the offset's width: iterate to the
	// fixed point.
	ptr := bodyLen
	for {
		so := sv8Packet("SO", varint(uint64(ptr)))
		if len(so)+bodyLen == ptr {
			break
		}
		ptr = len(so) + bodyLen
	}
	so := sv8Packet("SO", varint(uint64(ptr)))
	stream := buildSV8(append(append(append(head, so), body...), st, ct(1152, "after st"), sv8Packet("AP", apBlock(3)), sv8Packet("SE", nil))...)
	d = open(t, stream, false)
	chapters = d.Chapters()
	if len(chapters) != 1 || chapters[0].Title != "after st" {
		t.Errorf("chapters after the seek table: %+v", chapters)
	}
}

// TestBeginningSilence pins the SV8 field only mpccut writes: the cut fixture
// starts on a block boundary and declares the remainder as beginning silence,
// so its delivered samples are the origin's from the cut point, bit for bit.
func TestBeginningSilence(t *testing.T) {
	cut := open(t, codecFixture(t, "sv8-cut.mpc"), true)
	origin := open(t, codecFixture(t, "sv8-blocks.mpc"), true)
	ct, ot := cut.Tracks()[0], origin.Tracks()[0]
	if ct.Delay <= musepack.SynthDelay {
		t.Fatalf("the cut fixture declares no beginning silence (Delay %d)", ct.Delay)
	}
	const from = 5000
	if ct.Samples != ot.Samples-from {
		t.Errorf("cut delivers %d samples, the origin's tail is %d", ct.Samples, ot.Samples-from)
	}
	ch := ct.Fmt.Channels
	got := decodeAll(t, cut)[int(ct.Delay)*ch : int(ct.Delay+ct.Samples)*ch]
	want := decodeAll(t, origin)[int(ot.Delay+from)*ch : int(ot.Delay+ot.Samples)*ch]
	if len(got) != len(want) {
		t.Fatalf("%d vs %d samples", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sample %d: cut %v, origin %v", i, got[i], want[i])
		}
	}
}

// TestLeadingID3v2 rebases every position on a tag in front of the stream.
func TestLeadingID3v2(t *testing.T) {
	for _, name := range []string{"seek.mpc", "seek-sv7.mpc"} {
		raw := fixture(t, name)
		tag := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 1, 20}, make([]byte, 148)...)
		tagged := append(tag, raw...)
		plain := readAll(t, open(t, raw, true))
		shifted := readAll(t, open(t, tagged, true))
		if len(plain) != len(shifted) {
			t.Fatalf("%s: %d packets plain, %d behind a tag", name, len(plain), len(shifted))
		}
		for i := range plain {
			if string(plain[i].Data) != string(shifted[i].Data) || plain[i].PTS != shifted[i].PTS {
				t.Fatalf("%s: packet %d differs behind a leading tag", name, i)
			}
		}
	}
}

// TestTruncatedTailIsReported pins the declared-length invariant: a stream cut
// short warns and fails strict mode, reports what it can deliver, and what
// the probe reports is what a read delivers. A cut that loses audio shrinks
// the length; a cut that only clips the SV8 end marker leaves it standing,
// since every block is still there.
func TestTruncatedTailIsReported(t *testing.T) {
	for _, name := range []string{"seek.mpc", "gapless-sv7.mpc", "seek-sv7.mpc"} {
		full := fixture(t, name)
		whole := open(t, full, true).Tracks()[0]
		for _, cut := range []int{len(full) - 1, len(full) * 3 / 4, len(full) / 2} {
			t.Run(name+"/"+strconv.Itoa(cut), func(t *testing.T) {
				raw := full[:cut]
				d, err := mpc.NewDemuxer(container.BytesSource(raw), nil)
				if err != nil {
					t.Fatalf("a truncated stream refused outright: %v", err)
				}
				track := d.Tracks()[0]
				if track.Samples > whole.Samples {
					t.Errorf("truncated to %d bytes and declares %d samples, more than the whole file's %d", cut, track.Samples, whole.Samples)
				}
				if cut <= len(full)*3/4 && track.Samples >= whole.Samples {
					t.Errorf("truncated to %d bytes and still declares %d samples (whole %d)", cut, track.Samples, whole.Samples)
				}
				if len(d.Warnings()) == 0 {
					t.Error("no warning on a truncated stream")
				}
				delivered := int64(len(decodeAll(t, d)) / track.Fmt.Channels)
				if delivered < track.Delay+track.Samples {
					t.Errorf("probe reports %d samples after %d of delay, the read delivers %d raw", track.Samples, track.Delay, delivered)
				}
				if _, err := mpc.NewDemuxer(container.BytesSource(raw), &mpc.DemuxerOptions{Strict: true}); err == nil {
					t.Error("strict mode accepted a truncated stream")
				}
			})
		}
	}
}

// TestRefusals pins every by-name refusal through the demuxer.
func TestRefusals(t *testing.T) {
	sh := sv8Header{count: 1000, freq: 0, maxBand: 20, channels: 2, blockPwr: 0}
	badCRC := shPayload(sh)
	badCRC[0] ^= 0xFF
	cases := map[string]struct {
		raw  []byte
		want string
	}{
		"SV4":            {append([]byte("MP+\x04"), make([]byte, 64)...), "Musepack SV4 is not supported"},
		"SV6":            {append([]byte("MP+\x06"), make([]byte, 64)...), "Musepack SV6 is not supported"},
		"three channels": {buildSV8(sv8Packet("SH", shPayload(sv8Header{count: 1000, maxBand: 20, channels: 3})), sv8Packet("AP", apBlock(4)), sv8Packet("SE", nil)), "3 channels"},
		"bad SH CRC":     {buildSV8(sv8Packet("SH", badCRC), sv8Packet("AP", apBlock(4)), sv8Packet("SE", nil)), "CRC"},
		"reserved rate":  {buildSV8(sv8Packet("SH", shPayload(sv8Header{count: 1000, freq: 5, maxBand: 20, channels: 2})), sv8Packet("AP", apBlock(4)), sv8Packet("SE", nil)), "reserved"},
		"no SH":          {buildSV8(sv8Packet("RG", rgPayload(1, 0, 0, 0, 0)), sv8Packet("AP", apBlock(4)), sv8Packet("SE", nil)), "no stream header"},
		"SH version 7":   {buildSV8(sv8Packet("SH", func() []byte { p := shPayload(sh); p[4] = 7; return p }()), sv8Packet("AP", apBlock(4))), ""},
		"oversized varint": {append([]byte("MPCK"), append([]byte("SH"), []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}...)...),
			"no whole packet"},
		"last frame past a frame": {buildSV7(sv7Header{frames: 1, maxBand: 20, gapless: true, last: 1200}, []sv7Frame{randomFrame(1, 40)}, 1200, nil), "more than a frame holds"},
		"max band 0":              {buildSV7(sv7Header{frames: 1, maxBand: 0, gapless: true, last: 480}, []sv7Frame{randomFrame(1, 40)}, 480, nil), "max band"},
		"junk":                    {[]byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"), "not a Musepack stream"},
		"oversized SH packet":     {buildSV8(sv8Packet("SH", make([]byte, mpc.MaxHeaderPacketForTest+1))), "exceeds"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := mpc.NewDemuxer(container.BytesSource(c.raw), nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.HasPrefix(err.Error(), "musepack: ") {
				t.Errorf("error %q lacks the container name", err)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %q", err, c.want)
			}
			if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat {
				t.Errorf("code %v, want %v", waxerr.CodeOf(err), waxerr.CodeUnsupportedFormat)
			}
		})
	}
}

// TestSubMinimumFrameEndsTheStream pins the fuzzer's finding: an SV7 length
// field below the two band-zero resolutions is damage, and the stream ends at
// the frame before it rather than emitting a packet no decoder could parse.
func TestSubMinimumFrameEndsTheStream(t *testing.T) {
	frames := []sv7Frame{randomFrame(1, 100), randomFrame(2, 0), randomFrame(3, 100)}
	raw := buildSV7(sv7Header{frames: 3, maxBand: 20, gapless: true, last: 480}, frames, 480, nil)
	d := open(t, raw, false)
	if d.TotalForTest() != 1 || !hasWarning(d.Warnings(), "no frame can hold") {
		t.Errorf("delivers %d packets with warnings %v", d.TotalForTest(), d.Warnings())
	}
	cfg, _ := musepack.ParseConfig(d.Tracks()[0].CodecConfig)
	for _, p := range readAll(t, d) {
		if _, _, _, err := musepack.ParsePacketHeader(p.Data, cfg); err != nil {
			t.Errorf("emitted a packet the decoder cannot parse: %v", err)
		}
	}
}

// TestOversizedSeekTableIsSubsampledNotRefused pins the block table's
// retention: past the cap the table halves its density and keeps every
// entry at a multiple of its stride, as the reference does with its own.
func TestOversizedSeekTableIsSubsampledNotRefused(t *testing.T) {
	n := mpc.MaxSeekEntriesKeptForTest*2 + 5
	offsets := make([]int64, n)
	for i := range offsets {
		offsets[i] = int64(i) * 100
	}
	kept, stride := mpc.RetainForTest(offsets)
	if len(kept) > mpc.MaxSeekEntriesKeptForTest {
		t.Errorf("kept %d entries, the cap is %d", len(kept), mpc.MaxSeekEntriesKeptForTest)
	}
	if stride == 0 {
		t.Error("the stride never grew")
	}
	for i, off := range kept {
		if want := int64(i<<stride) * 100; off != want {
			t.Fatalf("entry %d is block %d, want block %d", i, off/100, i<<stride)
		}
	}
}

// TestEOFIsTheSentinel pins the read contract: the bare io.EOF, repeatedly.
func TestEOFIsTheSentinel(t *testing.T) {
	d := open(t, fixture(t, "gapless-sv7.mpc"), true)
	readAll(t, d)
	var pkt container.Packet
	for range 3 {
		if err := d.ReadPacket(&pkt); err != io.EOF {
			t.Fatalf("ReadPacket after the end = %v, want io.EOF", err)
		}
	}
}

// TestSTAgreesWithTheWalk cross-checks the seek table an encoder wrote against
// the block positions the walk found: every table entry names a block header
// at the table's spacing. Positions are never taken from the table, so this is
// what proves the table was read right (the Golomb sign rule included).
func TestSTAgreesWithTheWalk(t *testing.T) {
	check := func(t *testing.T, raw []byte) {
		d := open(t, raw, true)
		st, exp := d.SeekTableForTest()
		if len(st) < 2 {
			t.Skipf("the stream carries a seek table of %d entries", len(st))
		}
		blocks, err := d.BlockOffsetsForTest()
		if err != nil {
			t.Fatal(err)
		}
		every := 1 << exp
		for i, off := range st {
			b := i * every
			if b >= len(blocks) {
				break
			}
			if off != blocks[b] {
				t.Errorf("seek table entry %d says block %d is at %d, the walk found it at %d", i, b, off, blocks[b])
			}
		}
	}
	for _, name := range []string{"seek.mpc", "seek-pns.mpc"} {
		t.Run(name, func(t *testing.T) { check(t, fixture(t, name)) })
	}
	for _, name := range []string{"sv8-stereo.mpc", "sv8-frames0.mpc", "sv8-repack.mpc"} {
		t.Run(name, func(t *testing.T) { check(t, codecFixture(t, name)) })
	}
	t.Run("inside-mp8.mpc", func(t *testing.T) {
		raw, err := readVector(t, "musepack/inside-mp8.mpc")
		if err != nil {
			t.Skip(err)
		}
		check(t, raw)
	})
}
