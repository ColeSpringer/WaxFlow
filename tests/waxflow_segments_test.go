package waxflow_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/waxerr"
)

// parsedSegment is a media segment cracked back into packets for
// verification: trun durations/sizes plus the mdat payload, the fragment
// sequence number, and the decode time.
type parsedSegment struct {
	baseTime int64
	seq      uint32
	durs     []uint32
	packets  [][]byte
}

func (p *parsedSegment) samples() int64 {
	var n int64
	for _, d := range p.durs {
		n += int64(d)
	}
	return n
}

// parseSegment walks styp + (moof+mdat)... and extracts every sample. It
// validates just enough structure to make test failures precise; ffprobe
// (gated) and the golden bytes cover full conformance.
func parseSegment(t *testing.T, data []byte) *parsedSegment {
	t.Helper()
	seg := &parsedSegment{baseTime: -1}
	var durs []uint32
	var sizes []uint32
	off := 0
	readBox := func() (string, []byte) {
		if off+8 > len(data) {
			t.Fatalf("truncated box header at %d", off)
		}
		size := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if size < 8 || off+size > len(data) {
			t.Fatalf("box %q size %d at %d out of bounds", typ, size, off)
		}
		body := data[off+8 : off+size]
		off += size
		return typ, body
	}
	typ, _ := readBox()
	if typ != "styp" {
		t.Fatalf("segment starts with %q, want styp", typ)
	}
	for off < len(data) {
		typ, body := readBox()
		switch typ {
		case "moof":
			durs, sizes = nil, nil
			walk := func(b []byte, fn func(string, []byte)) {
				for len(b) >= 8 {
					size := int(binary.BigEndian.Uint32(b))
					if size < 8 || size > len(b) {
						t.Fatalf("child box size %d out of bounds", size)
					}
					fn(string(b[4:8]), b[8:size])
					b = b[size:]
				}
			}
			walk(body, func(typ string, b []byte) {
				if typ == "mfhd" && seg.seq == 0 {
					seg.seq = binary.BigEndian.Uint32(b[4:])
				}
				if typ != "traf" {
					return
				}
				walk(b, func(typ string, b []byte) {
					switch typ {
					case "tfdt":
						if b[0] != 1 {
							t.Fatalf("tfdt version %d, want 1", b[0])
						}
						if base := int64(binary.BigEndian.Uint64(b[4:])); seg.baseTime < 0 {
							seg.baseTime = base
						}
					case "trun":
						if flags := binary.BigEndian.Uint32(b[:4]) & 0xFFFFFF; flags != 0x000301 {
							t.Fatalf("trun flags %#x, want 0x000301", flags)
						}
						n := int(binary.BigEndian.Uint32(b[4:]))
						at := 12 // past sample_count and data_offset
						for i := 0; i < n; i++ {
							durs = append(durs, binary.BigEndian.Uint32(b[at:]))
							sizes = append(sizes, binary.BigEndian.Uint32(b[at+4:]))
							at += 8
						}
					}
				})
			})
		case "mdat":
			at := 0
			for i, size := range sizes {
				if at+int(size) > len(body) {
					t.Fatalf("mdat too short for sample %d", i)
				}
				seg.packets = append(seg.packets, bytes.Clone(body[at:at+int(size)]))
				seg.durs = append(seg.durs, durs[i])
				at += int(size)
			}
			if at != len(body) {
				t.Fatalf("mdat has %d bytes past the trun samples", len(body)-at)
			}
		default:
			t.Fatalf("unexpected top-level box %q in segment", typ)
		}
	}
	return seg
}

// collectSegments runs a segmented transcode and returns the segments in
// order, asserting the indices are sequential from start.
func collectSegments(t *testing.T, e *waxflow.Engine, raw []byte, opts waxflow.TranscodeOptions,
	segSamples int, start int64) ([]mp4.Segment, *waxflow.SegmentedResult) {
	t.Helper()
	var segs []mp4.Segment
	res, err := e.TranscodeSegments(context.Background(), container.BytesSource(raw), "wav", opts,
		waxflow.SegmentedOptions{SegmentSamples: segSamples, StartSegment: start},
		func(s mp4.Segment) error {
			if want := start + int64(len(segs)); s.Index != want {
				t.Fatalf("segment index %d, want %d", s.Index, want)
			}
			segs = append(segs, s)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return segs, res
}

func TestPlanSegments(t *testing.T) {
	e := waxflow.New()
	track := container.Track{Codec: codec.PCM, Fmt: audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, Samples: 480000, Default: true}

	t.Run("opus", func(t *testing.T) {
		plan, err := e.PlanSegments(track, waxflow.TranscodeOptions{Format: "opus"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if plan.SegmentSamples != 192000 { // 4 s at 48 kHz, 200 whole frames
			t.Fatalf("SegmentSamples %d, want 192000", plan.SegmentSamples)
		}
		if plan.Codecs != "Opus" || plan.Delay != 312 {
			t.Fatalf("codecs %q delay %d", plan.Codecs, plan.Delay)
		}
		// 480000 samples + 312 delay pads to 501 whole frames = 480960
		// decode samples: three segments of 192000, 192000, 96960.
		if plan.TotalDecodeSamples != 480960 || plan.Segments != 3 {
			t.Fatalf("total %d segments %d, want 480960 and 3", plan.TotalDecodeSamples, plan.Segments)
		}
		if d := plan.SegmentDuration(2); d != 96960 {
			t.Fatalf("last segment %d samples, want 96960", d)
		}
		if d := plan.SegmentDuration(3); d != -1 {
			t.Fatalf("past-end segment duration %d, want -1", d)
		}
		if plan.Bandwidth <= plan.BitRate {
			t.Fatalf("bandwidth %d not above bit rate %d", plan.Bandwidth, plan.BitRate)
		}
		if got := plan.Versions[len(plan.Versions)-1]; got != mp4.SegmenterVersion {
			t.Fatalf("last version %q, want the segmenter's", got)
		}
	})

	t.Run("flac", func(t *testing.T) {
		plan, err := e.PlanSegments(track, waxflow.TranscodeOptions{Format: "flac"}, 4)
		if err != nil {
			t.Fatal(err)
		}
		// 4 s at 48 kHz snaps to 47 blocks of 4096 = 192512 samples.
		if plan.SegmentSamples != 192512 {
			t.Fatalf("SegmentSamples %d, want 192512", plan.SegmentSamples)
		}
		if plan.TotalDecodeSamples != 480000 || plan.Segments != 3 {
			t.Fatalf("total %d segments %d", plan.TotalDecodeSamples, plan.Segments)
		}
		if plan.Codecs != "fLaC" || plan.Delay != 0 {
			t.Fatalf("codecs %q delay %d", plan.Codecs, plan.Delay)
		}
	})

	t.Run("unknown-length", func(t *testing.T) {
		unk := track
		unk.Samples = -1
		plan, err := e.PlanSegments(unk, waxflow.TranscodeOptions{Format: "opus"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if plan.TotalDecodeSamples != -1 || plan.Segments != -1 {
			t.Fatalf("unknown length must plan unknown totals, got %d/%d", plan.TotalDecodeSamples, plan.Segments)
		}
	})

	t.Run("rejections", func(t *testing.T) {
		for name, tc := range map[string]struct {
			opts waxflow.TranscodeOptions
			dur  float64
			code waxerr.Code
		}{
			"no-hls-form":  {waxflow.TranscodeOptions{Format: "mp3"}, 0, waxerr.CodeUnsupportedFormat},
			"wav":          {waxflow.TranscodeOptions{Format: "wav"}, 0, waxerr.CodeUnsupportedFormat},
			"from-sample":  {waxflow.TranscodeOptions{Format: "opus", FromSample: 1}, 0, waxerr.CodeInvalidRequest},
			"negative-dur": {waxflow.TranscodeOptions{Format: "opus"}, -1, waxerr.CodeInvalidRequest},
			"huge-dur":     {waxflow.TranscodeOptions{Format: "opus"}, 61, waxerr.CodeInvalidRequest},
		} {
			if _, err := e.PlanSegments(track, tc.opts, tc.dur); waxerr.CodeOf(err) != tc.code {
				t.Errorf("%s: err %v, want code %s", name, err, tc.code)
			}
		}
	})
}

// TestSegmentedFLACBitExact is the round-trip and boundary proof for the
// lossless path: a continuous run's segments decode back to the source
// bit-for-bit, in order, with every boundary on the planned sample.
func TestSegmentedFLACBitExact(t *testing.T) {
	const frames = 100000 // ~2.08 s at 48 kHz: three 1 s segments, last one short
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 21)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}

	track := container.Track{Codec: codec.PCM, Fmt: src.Fmt, Samples: frames, Default: true}
	plan, err := e.PlanSegments(track, opts, 1)
	if err != nil {
		t.Fatal(err)
	}
	segs, res := collectSegments(t, e, raw, opts, plan.SegmentSamples, 0)
	if int64(len(segs)) != plan.Segments {
		t.Fatalf("%d segments, want %d", len(segs), plan.Segments)
	}
	if res.Samples != frames {
		t.Fatalf("result samples %d, want %d", res.Samples, frames)
	}

	si, err := flac.ParseStreamInfo(mustCodecConfig(t, e, plan, opts))
	if err != nil {
		t.Fatal(err)
	}
	dec, err := flac.NewDecoder(si, plan.Format)
	if err != nil {
		t.Fatal(err)
	}
	got := audio.Get(plan.Format, frames)
	defer audio.Put(got)
	var decodePos int64
	for i, s := range segs {
		p := parseSegment(t, s.Data)
		if p.baseTime != decodePos {
			t.Fatalf("segment %d tfdt %d, want %d", i, p.baseTime, decodePos)
		}
		// One fragment per segment: the sequence number is the index plus
		// one, so restarted workers can reproduce continuous bytes.
		if p.seq != uint32(i)+1 {
			t.Fatalf("segment %d mfhd sequence %d, want %d", i, p.seq, i+1)
		}
		if p.samples() != s.Samples || s.Samples != plan.SegmentDuration(int64(i)) {
			t.Fatalf("segment %d: %d samples in boxes, %d reported, %d planned",
				i, p.samples(), s.Samples, plan.SegmentDuration(int64(i)))
		}
		decodePos += s.Samples
		for _, pkt := range p.packets {
			if err := dec.Decode(pkt, func(b *audio.Buffer) error {
				for c := 0; c < b.Fmt.Channels; c++ {
					copy(got.I[c*got.Stride+got.N:c*got.Stride+got.N+b.N], b.ChanI(c))
				}
				got.N += b.N
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	equalPCM(t, src, got)
}

// TestSegmentedRestartFLAC pins the strongest regeneration guarantee: a
// worker restarted at segment n produces byte-identical segments to the
// continuous run's, because the lossless encoders carry no cross-frame
// state and FLAC numbers its frames absolutely.
func TestSegmentedRestartFLAC(t *testing.T) {
	const frames = 100000
	raw, _ := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 22)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	segSamples := 45056 // 11 blocks of 4096: three segments, last short

	full, _ := collectSegments(t, e, raw, opts, segSamples, 0)
	tail, _ := collectSegments(t, e, raw, opts, segSamples, 1)
	if len(tail) != len(full)-1 {
		t.Fatalf("restart yielded %d segments, want %d", len(tail), len(full)-1)
	}
	for i, s := range tail {
		if !bytes.Equal(s.Data, full[i+1].Data) {
			t.Fatalf("restarted segment %d differs from the continuous run", s.Index)
		}
	}
}

// TestSegmentedRestartOpus verifies the primed restart path for the
// stateful codec: decode positions, packet counts, and per-index sample
// durations match the continuous run exactly, restarts are deterministic,
// and every packet decodes through our Opus decoder.
func TestSegmentedRestartOpus(t *testing.T) {
	const frames = 100000
	raw, _ := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 23)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "opus", OpusBitrate: 64000}
	segSamples := 48000 // 50 frames of 960: 1 s segments

	full, _ := collectSegments(t, e, raw, opts, segSamples, 0)
	track := container.Track{Codec: codec.PCM, Fmt: audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, Samples: frames, Default: true}
	plan, err := e.PlanSegments(track, opts, 1)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(full)) != plan.Segments {
		t.Fatalf("%d segments, want %d", len(full), plan.Segments)
	}

	const restartAt = 2
	tail, _ := collectSegments(t, e, raw, opts, segSamples, restartAt)
	tail2, _ := collectSegments(t, e, raw, opts, segSamples, restartAt)
	if len(tail) != len(full)-restartAt {
		t.Fatalf("restart yielded %d segments, want %d", len(tail), len(full)-restartAt)
	}

	dec, err := opus.NewDecoder(opusHeadConfig(t, e, plan, opts), plan.Format)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range tail {
		cont := full[restartAt+i]
		if !bytes.Equal(s.Data, tail2[i].Data) {
			t.Fatalf("segment %d not deterministic across identical restarts", s.Index)
		}
		p, pc := parseSegment(t, s.Data), parseSegment(t, cont.Data)
		if p.baseTime != pc.baseTime || p.seq != pc.seq || p.samples() != pc.samples() || len(p.packets) != len(pc.packets) {
			t.Fatalf("segment %d framing (%d, seq %d, %d, %d packets) diverges from continuous (%d, seq %d, %d, %d)",
				s.Index, p.baseTime, p.seq, p.samples(), len(p.packets), pc.baseTime, pc.seq, pc.samples(), len(pc.packets))
		}
		for _, pkt := range p.packets {
			if err := dec.Decode(pkt, func(*audio.Buffer) error { return nil }); err != nil {
				t.Fatalf("segment %d packet undecodable: %v", s.Index, err)
			}
		}
	}
}

// TestSegmentedResampledRestart drives the priming path through a
// resampling chain: a 44.1 kHz source to 48 kHz Opus, restarted
// mid-stream, must keep the continuous run's segment timing.
func TestSegmentedResampledRestart(t *testing.T) {
	const frames = 100000
	f := pcm.Config{Bits: 16}.PCMFormat(44100, 2, audio.DefaultLayout(2))
	src := audio.Get(f, frames)
	defer audio.Put(src)
	src.N = frames
	synth(src, 24)
	enc, err := pcm.NewEncoder(pcm.Config{Bits: 16}, f)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: frames, Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(trailer); err != nil {
		t.Fatal(err)
	}

	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "opus"}
	segSamples := 48000
	full, _ := collectSegments(t, e, ws.b, opts, segSamples, 0)
	tail, _ := collectSegments(t, e, ws.b, opts, segSamples, 1)
	if len(tail) != len(full)-1 {
		t.Fatalf("restart yielded %d segments, want %d", len(tail), len(full)-1)
	}
	for i, s := range tail {
		p, pc := parseSegment(t, s.Data), parseSegment(t, full[i+1].Data)
		if p.baseTime != pc.baseTime || p.samples() != pc.samples() {
			t.Fatalf("segment %d timing (%d, %d) diverges from continuous (%d, %d)",
				s.Index, p.baseTime, p.samples(), pc.baseTime, pc.samples())
		}
	}
}

func TestInitSegmentDeterministic(t *testing.T) {
	e := waxflow.New()
	track := container.Track{Codec: codec.PCM, Fmt: audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}, Samples: 480000, Default: true}
	for _, format := range []string{"opus", "flac", "alac", "aac"} {
		opts := waxflow.TranscodeOptions{Format: format}
		plan, err := e.PlanSegments(track, opts, 0)
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatal(err)
		}
		b, err := e.InitSegment(plan, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: init segment not deterministic", format)
		}
		if !bytes.Contains(a, []byte("moov")) || !bytes.Contains(a, []byte("trex")) {
			t.Errorf("%s: init segment missing moov/trex", format)
		}
		if format == "opus" {
			if !bytes.Contains(a, []byte("dOps")) || !bytes.Contains(a, []byte("elst")) {
				t.Errorf("opus init missing dOps or the delay edit list")
			}
		}
		if format == "flac" && !bytes.Contains(a, []byte("dfLa")) {
			t.Errorf("flac init missing dfLa")
		}
		if format == "aac" {
			if !bytes.Contains(a, []byte("mp4a")) || !bytes.Contains(a, []byte("esds")) || !bytes.Contains(a, []byte("elst")) {
				t.Errorf("aac init missing mp4a/esds or the delay edit list")
			}
		}
	}
}

// mustCodecConfig builds the encoder the way the engine does, purely to
// extract its codec config for decoding segments in tests.
func mustCodecConfig(t *testing.T, e *waxflow.Engine, plan *waxflow.SegmentPlan, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	init, err := e.InitSegment(plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	// The STREAMINFO rides in dfLa after the 4-byte metadata block header.
	i := bytes.Index(init, []byte("dfLa"))
	if i < 0 {
		t.Fatal("init has no dfLa")
	}
	return init[i+12 : i+12+flac.StreamInfoLen]
}

// opusHeadConfig extracts the decoder config from the init segment's dOps
// box, the way an fMP4 consumer would.
func opusHeadConfig(t *testing.T, e *waxflow.Engine, plan *waxflow.SegmentPlan, opts waxflow.TranscodeOptions) opus.Config {
	t.Helper()
	init, err := e.InitSegment(plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(init, []byte("dOps"))
	if i < 0 {
		t.Fatal("init has no dOps")
	}
	b := init[i+4:]
	return opus.Config{
		Channels: int(b[1]),
		PreSkip:  int(binary.BigEndian.Uint16(b[2:])),
	}
}

func TestSegmentedRejections(t *testing.T) {
	raw, _ := makeWAV(t, pcm.Config{Bits: 16}, 2, 4800, 25)
	e := waxflow.New()
	ctx := context.Background()
	noop := func(mp4.Segment) error { return nil }
	cases := map[string]struct {
		opts waxflow.TranscodeOptions
		seg  waxflow.SegmentedOptions
		code waxerr.Code
	}{
		"no-hls-form":    {waxflow.TranscodeOptions{Format: "mp3"}, waxflow.SegmentedOptions{SegmentSamples: 1152}, waxerr.CodeUnsupportedFormat},
		"from-sample":    {waxflow.TranscodeOptions{Format: "opus", FromSample: 1}, waxflow.SegmentedOptions{SegmentSamples: 960}, waxerr.CodeInvalidRequest},
		"unaligned":      {waxflow.TranscodeOptions{Format: "opus"}, waxflow.SegmentedOptions{SegmentSamples: 1000}, waxerr.CodeInvalidRequest},
		"zero-samples":   {waxflow.TranscodeOptions{Format: "opus"}, waxflow.SegmentedOptions{}, waxerr.CodeInvalidRequest},
		"negative-start": {waxflow.TranscodeOptions{Format: "opus"}, waxflow.SegmentedOptions{SegmentSamples: 960, StartSegment: -1}, waxerr.CodeInvalidRequest},
	}
	for name, tc := range cases {
		if _, err := e.TranscodeSegments(ctx, container.BytesSource(raw), "wav", tc.opts, tc.seg, noop); waxerr.CodeOf(err) != tc.code {
			t.Errorf("%s: err %v, want code %s", name, err, tc.code)
		}
	}
}

func TestSegmentedCancellation(t *testing.T) {
	raw, _ := makeWAV(t, pcm.Config{Bits: 16}, 2, 48000, 26)
	e := waxflow.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.TranscodeSegments(ctx, container.BytesSource(raw), "wav",
		waxflow.TranscodeOptions{Format: "opus"},
		waxflow.SegmentedOptions{SegmentSamples: 48000},
		func(mp4.Segment) error { return nil })
	if waxerr.CodeOf(err) != waxerr.CodeCanceled {
		t.Fatalf("err %v, want canceled", err)
	}
}

// transientWAV renders a stereo 48 kHz WAV that is quiet everywhere except
// for a loud burst in [loudFrom, loudTo), returned as a WAV and its PCM.
//
// The burst's placement is the whole point of the fixtures below: a gain
// envelope only diverges where it has been driven somewhere, so a restart
// test whose audio is uniform proves nothing about restart state at all.
func transientWAV(t *testing.T, frames, loudFrom, loudTo int) []byte {
	return ampWAV(t, frames, func(i int) float64 {
		if i >= loudFrom && i < loudTo {
			return 0.95 // driven hard
		}
		return 0.03 // a quiet bed: the limiter rests here
	})
}

// ampWAV renders a stereo 48 kHz WAV whose amplitude at each frame comes
// from amp, as a 440 Hz tone.
func ampWAV(t *testing.T, frames int, amp func(i int) float64) []byte {
	t.Helper()
	cfg := pcm.Config{Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	src := audio.Get(f, frames)
	defer audio.Put(src)
	src.N = frames
	for i := range frames {
		v := int32(amp(i) * 32767 * math.Sin(2*math.Pi*440*float64(i)/48000))
		for c := range f.Channels {
			src.I[c*src.Stride+i] = v
		}
	}
	enc, err := pcm.NewEncoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	ws := &memWS{}
	m := riff.NewMuxer(ws, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f, Samples: int64(frames), Default: true}
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return m.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(src, emit); err != nil {
		t.Fatal(err)
	}
	tr, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.End(tr); err != nil {
		t.Fatal(err)
	}
	return ws.b
}

// assertRestartMatches runs the stream continuously and again restarted at
// startAt, and requires byte-identical segments from startAt on.
func assertRestartMatches(t *testing.T, e *waxflow.Engine, raw []byte,
	opts waxflow.TranscodeOptions, segSamples int, startAt int64) {
	t.Helper()
	full, _ := collectSegments(t, e, raw, opts, segSamples, 0)
	tail, _ := collectSegments(t, e, raw, opts, segSamples, startAt)
	if int64(len(tail)) != int64(len(full))-startAt {
		t.Fatalf("restart yielded %d segments, want %d", len(tail), int64(len(full))-startAt)
	}
	for i, s := range tail {
		cont := full[int64(i)+startAt]
		if !bytes.Equal(s.Data, cont.Data) {
			t.Fatalf("restarted segment %d differs from the continuous run (%d bytes vs %d): "+
				"the chain's priming did not settle its state", s.Index, len(s.Data), len(cont.Data))
		}
	}
}

// TestSegmentedRestartWithLimiter is a regression test for a bug that
// shipped, not a test for a new feature.
//
// Segment restart determinism rested on primeSeconds = 0.1 exceeding every
// stateful node's memory. That reasoning holds only for finite memory: the
// resampler is an FIR, so once its window fills with real audio a restarted
// run and a continuous one are exactly equal. The limiter's gain is a
// one-pole IIR with a 50 ms time constant, which converges asymptotically
// and never exactly, so 100 ms of priming left e^-2 (13.5 percent) of the
// gain discrepancy alive: a run restarted after a loud passage reached its
// first kept sample with roughly a 4 percent gain error and produced
// segments that were not the ones a continuous run produces.
//
// It was reachable in a shipped daemon by gain=track on a positively-tagged
// file over HLS, and nothing caught it, because the only restart test
// (TestSegmentedRestartFLAC) runs with no gain: spec.GainDB == 0, so the
// limiter is never in the chain and the path was never exercised. This test
// exists to be the one that would have.
func TestSegmentedRestartWithLimiter(t *testing.T) {
	const frames = 400000
	const segSamples = 49152 // 12 blocks of 4096
	const restartAt = 4
	// The burst ends just before the restart point, so the continuous run
	// arrives there mid-release with the gain far from rest, which is the
	// worst case for a short pre-roll.
	raw := transientWAV(t, frames, restartAt*segSamples-24000, restartAt*segSamples-1000)

	e := waxflow.New()
	// Positive gain is what engages the limiter; the FLAC output keeps the
	// comparison at the segment bytes rather than an encoder's own state.
	opts := waxflow.TranscodeOptions{Format: "flac", GainDB: 6}
	assertRestartMatches(t, e, raw, opts, segSamples, restartAt)
}

// TestSegmentedRestartWithDynamics is the same guarantee for A10's node,
// whose release is 250 ms and whose horizon is therefore 10 s: five times
// the limiter's, and a hundred times the priming window that used to cover
// both.
func TestSegmentedRestartWithDynamics(t *testing.T) {
	const segSamples = 49152
	// The restart must sit beyond the 10 s horizon, or priming clamps at the
	// top of the stream and both runs start from sample 0: the test would
	// pass without exercising convergence at all, which is the trap this
	// comment exists to stop the next person falling into.
	const restartAt = 14 // 14 * 49152 = 688128 samples = 14.3 s at 48 kHz
	const frames = 1200000
	// The chain's horizon is the compressor's 10 s plus the limiter's 2 s,
	// which the preset engages. The restart must sit beyond it, or priming
	// clamps at the top of the stream and both runs start from sample 0: the
	// test would pass without exercising convergence at all, which is the
	// trap this check exists to stop the next person falling into.
	if restartAt*segSamples <= 12*48000 {
		t.Fatalf("restart at %d samples is inside the 12 s horizon; priming would clamp at 0 "+
			"and the runs would start together", restartAt*segSamples)
	}
	raw := transientWAV(t, frames, restartAt*segSamples-24000, restartAt*segSamples-1000)

	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac", Dynamics: gain.PresetVoice}
	assertRestartMatches(t, e, raw, opts, segSamples, restartAt)
}

// TestChainHorizonDrivesPriming pins the wiring rather than the audio: a
// chain with no decaying node keeps the old 100 ms window exactly and pays
// nothing new, and each kernel that declares a horizon raises it to its own.
// Without this, a regression that returned 0 from Chain.Horizon would leave
// the restart tests above as the only signal, and they are slow and
// content-dependent.
func TestChainHorizonDrivesPriming(t *testing.T) {
	f := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	for _, tc := range []struct {
		name string
		spec dsp.ChainSpec
		want time.Duration
	}{
		{"plain passthrough", dsp.ChainSpec{}, 0},
		{"resample only: an FIR window needs no horizon", dsp.ChainSpec{Rate: 44100}, 0},
		{"negative gain: no limiter", dsp.ChainSpec{GainDB: -6}, 0},
		{"positive gain engages the limiter", dsp.ChainSpec{GainDB: 6}, 2 * time.Second},
		// Dynamics engages the limiter too, so this chain already holds two
		// decaying kernels in series and pays for both.
		{"dynamics: compressor plus the limiter it engages", dsp.ChainSpec{Dynamics: gain.PresetVoice}, 12 * time.Second},
		{"gain and dynamics: the same two nodes", dsp.ChainSpec{GainDB: 6, Dynamics: gain.PresetVoice}, 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := audio.Get(f, 64)
			buf.N = 64
			defer audio.Put(buf)
			chain, err := dsp.NewChain(dsp.NewSource(&nullReader{}, f), tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			defer chain.Release()
			if got := chain.Horizon(); got != tc.want {
				t.Errorf("Horizon = %v, want %v", got, tc.want)
			}
		})
	}
}

// nullReader is an immediately-exhausted chain source: the horizon is a
// property of the assembled chain, so no audio need flow to read it.
type nullReader struct{}

func (r *nullReader) ReadChunk(dst *audio.Buffer) error { return io.EOF }

// TestDynamicsReducesRangeEndToEnd drives A10 through the whole engine
// rather than the kernel: transcode a wide-range spoken-word-shaped source
// with the preset off and on, decode both back, and measure them with the
// R128 meter. The loudness range must fall, which is the feature stated as
// a measurement.
//
// The kernel tests prove the curve does arithmetic. This proves the
// arithmetic survives the chain, the encoder, and the decoder, which is
// what a caller actually receives.
//
// The passage levels are chosen against the meter, not for realism: EBU
// Tech 3342 gates the loudness range at 20 LU under the mean, so passages
// further apart than that are excluded from the figure and a wider fixture
// would report a *smaller* range to begin with. 16 LU apart stays inside
// the gate, so the number moves for the reason the test claims.
func TestDynamicsReducesRangeEndToEnd(t *testing.T) {
	// The passages must outlast the meter's own 3 s short-term window, or
	// every window straddles a loud and a quiet one, they all measure the
	// same, and the range collapses to nothing before the preset touches it.
	const seg = 5 * 48000 // 5 s passages
	const frames = 8 * seg
	raw := ampWAV(t, frames, func(i int) float64 {
		if (i/seg)%2 == 0 {
			return 0.4 // about -8 dBFS
		}
		return 0.063 // about -24 dBFS
	})

	e := waxflow.New()
	measure := func(dyn gain.Preset) *waxflow.AnalyzeResult {
		t.Helper()
		var out bytes.Buffer
		if _, err := e.Transcode(context.Background(), container.BytesSource(raw), "wav", &out,
			waxflow.TranscodeOptions{Format: "flac", Dynamics: dyn}); err != nil {
			t.Fatalf("transcode (dynamics=%q): %v", dyn, err)
		}
		res, err := e.Analyze(context.Background(), container.BytesSource(out.Bytes()), "flac",
			waxflow.AnalyzeOptions{})
		if err != nil {
			t.Fatalf("analyze (dynamics=%q): %v", dyn, err)
		}
		return res
	}
	off, on := measure(gain.PresetOff), measure(gain.PresetVoice)

	t.Logf("dynamics=off:   range %.2f LU, integrated %.2f LUFS, true peak %.2f dBTP",
		off.LoudnessRange, off.IntegratedLUFS, off.TruePeakDB)
	t.Logf("dynamics=voice: range %.2f LU, integrated %.2f LUFS, true peak %.2f dBTP",
		on.LoudnessRange, on.IntegratedLUFS, on.TruePeakDB)

	// The fixture must have a range to reduce, or the assertion below
	// passes on a measurement that was never there.
	if off.LoudnessRange < 10 {
		t.Fatalf("the fixture's own range is only %.2f LU; it is not exercising the preset", off.LoudnessRange)
	}
	if on.LoudnessRange >= off.LoudnessRange-3 {
		t.Errorf("loudness range %.2f -> %.2f LU: the preset did not meaningfully reduce it",
			off.LoudnessRange, on.LoudnessRange)
	}
	// The limiter is engaged by the preset and is what makes the raised
	// gain ceiling safe, so the output must sit under its true-peak
	// ceiling. Without the preset this fixture peaks far below it, so this
	// is a real check on the preset's own path.
	if on.TruePeakDB > gain.DefaultCeilingDB+0.1 {
		t.Errorf("true peak %.2f dBTP exceeds the limiter ceiling %.2f: the limiter did not engage",
			on.TruePeakDB, gain.DefaultCeilingDB)
	}
	if off.Samples != on.Samples {
		t.Errorf("dynamics changed the length: %d samples vs %d", on.Samples, off.Samples)
	}
	// Integrated loudness is deliberately not asserted in either
	// direction. It is dominated by whichever passages are loudest, which
	// the preset brings down, so its sign is a property of the content
	// rather than of the preset: a reading with hot passages lands quieter
	// and a uniformly quiet one lands louder. The range is the claim.
}
