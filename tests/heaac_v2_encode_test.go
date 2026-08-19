package waxflow_test

// HE-AAC v2 encoder, engine surface: the he-aac row under
// TranscodeOptions.HEAACv2. The codec-level pins (AOT-29 ASC bytes, the
// shared 3010-sample delay, ps_data round-trip, the stereo-image gates)
// live in codec/aac; these cover the container story, the option
// surface, and the ffmpeg legs.

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestHEAACv2TranscodeFMP4 drives the he-aac row with HEAACv2 into its
// default fMP4 container: the AOT-29 identity read back stereo at the
// doubled rate, the edit list carrying the shared delay, exact gapless
// trims, and ffmpeg's decode agreeing with ours on our own bitstream.
func TestHEAACv2TranscodeFMP4(t *testing.T) {
	const frames = 48000 + 777
	wav := brightWAV(t, 48000, frames, 2)

	var out bytes.Buffer
	res := transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{HEAACv2: true})
	if res.Samples != frames {
		t.Fatalf("samples = %d, want %d", res.Samples, frames)
	}

	i := bytes.Index(out.Bytes(), []byte("elst"))
	if i < 0 {
		t.Fatal("no elst in fMP4 output")
	}
	dur := int64(binary.BigEndian.Uint64(out.Bytes()[i+12:]))
	mt := int64(binary.BigEndian.Uint64(out.Bytes()[i+20:]))
	if dur != frames || mt != aac.HEEncoderDelay {
		t.Fatalf("elst (dur %d, mediaTime %d), want (%d, %d)", dur, mt, frames, aac.HEEncoderDelay)
	}

	demux, info, err := format.OpenDemuxer(container.BytesSource(out.Bytes()), "m4a", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = demux
	tr := info.Default()
	if tr.Codec != codec.HEAAC || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
		t.Errorf("readback track = codec %q %v", tr.Codec, tr.Fmt)
	}
	if tr.Delay != aac.HEEncoderDelay || tr.Samples != frames {
		t.Errorf("readback gapless = delay %d samples %d, want %d/%d", tr.Delay, tr.Samples, aac.HEEncoderDelay, frames)
	}
	cfg, err := aac.ParseASC(tr.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PS || cfg.SampleRate != 24000 || cfg.ExtensionRate != 48000 {
		t.Errorf("stored ASC parses to %+v, want explicit AOT-29 over a 24 kHz mono core", cfg)
	}
	ours, err := decodeAllDynamic(t, container.BytesSource(out.Bytes()), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ours)
	if ours.N != frames || ours.Fmt.Channels != 2 {
		t.Errorf("our decode = %d samples %d ch, want the gapless %d stereo", ours.N, ours.Fmt.Channels, frames)
	}

	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "out.m4a")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := testutil.FFprobeFile(t, path)
	if ref.CodecName != "aac" || ref.SampleRate != 48000 || ref.Channels != 2 {
		t.Errorf("ffprobe on our output = %+v", ref)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	if len(got) < frames*2 {
		t.Fatalf("ffmpeg decoded %d interleaved samples, want at least %d", len(got), frames*2)
	}
	rms, off := alignedRMS(testutil.InterleaveF(ours), got, 2)
	t.Logf("cross-decoder differential on our own v2 stream: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("our decode and ffmpeg's disagree on our own bitstream: RMS %g exceeds %g (offset %d)", rms, aacRMSGate, off)
	}
}

// TestHEAACv2TranscodeADTS covers container=adts under HEAACv2: implicit
// signalling, so our demuxer's head probe must see the ps_data and
// recover the stereo HE identity, and ffmpeg must decode a stereo pair
// at the doubled rate.
func TestHEAACv2TranscodeADTS(t *testing.T) {
	const frames = 48000
	wav := brightWAV(t, 48000, frames, 2)

	var out bytes.Buffer
	res := transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{HEAACv2: true, Container: "adts"})
	if res.Samples != frames || res.Container != "adts" {
		t.Fatalf("result = %+v, want %d samples in adts", res, frames)
	}

	med, err := format.Open(container.BytesSource(out.Bytes()), "aac", nil)
	if err != nil {
		t.Fatalf("open our adts: %v", err)
	}
	defer med.Close()
	tr := med.Info().Default()
	if tr.Codec != codec.HEAAC || tr.Fmt.Rate != 48000 || tr.Fmt.Channels != 2 {
		t.Errorf("adts readback = codec %q %v, want stereo he-aac at 48000", tr.Codec, tr.Fmt)
	}

	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "out.aac")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	want := heAUCount(frames) * 2048
	got := testutil.FFmpegDecodeF32(t, path)
	if int64(len(got)) != want*2 {
		t.Errorf("ffmpeg decoded %d interleaved samples, want %d (stereo 2048 per AU at the doubled rate)", len(got), want*2)
	}
}

// TestHEAACv2DelayPinFFmpeg is the ffmpeg leg of the v2 delay pin: the
// PS front end is delay-compensated, so the toneburst train through the
// v2 encoder must land at exactly the shared HEEncoderDelay under
// ffmpeg's decoder too (our own decoder's leg lives in codec/aac).
func TestHEAACv2DelayPinFFmpeg(t *testing.T) {
	testutil.FFmpeg(t)
	const rate, frames = 48000, 48000 * 2
	const burstLen = 480
	burst := make([]float32, burstLen)
	for i := range burst {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/burstLen)
		burst[i] = float32(0.7 * w * math.Sin(2*math.Pi*3000*float64(i)/48000))
	}
	f := audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	samples := make([]float32, 2*frames)
	var starts []int
	for p := 5000; p < frames-8000; p += 4800 {
		for i, w := range burst {
			samples[2*(p+i)] = w
			samples[2*(p+i)+1] = w
		}
		starts = append(starts, p)
	}
	wav := synthWAVFromSamples(t, f, samples)

	var out bytes.Buffer
	transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{HEAACv2: true, Container: "adts"})
	path := filepath.Join(t.TempDir(), "bursts.aac")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got := testutil.FFmpegDecodeF32(t, path)

	bestLag, bestCorr := -1, 0.0
	for lag := 0; lag < 6000; lag++ {
		var corr float64
		for _, p := range starts {
			for i, w := range burst {
				if j := 2 * (p + lag + i); j < len(got) {
					corr += float64(w) * float64(got[j])
				}
			}
		}
		if corr > bestCorr {
			bestLag, bestCorr = lag, corr
		}
	}
	t.Logf("ffmpeg matched-filter v2 delay %d (declared %d)", bestLag, aac.HEEncoderDelay)
	if bestLag != aac.HEEncoderDelay {
		t.Errorf("ffmpeg-measured v2 delay %d, declared %d", bestLag, aac.HEEncoderDelay)
	}
}

// TestHEAACv2PlanDefaults pins the option surface: the flag drops the
// zero-bitrate default to 32 kb/s, an explicit bitrate still wins, the
// plan's version key splits from v1's, and a mono source is refused at
// plan time by name.
func TestHEAACv2PlanDefaults(t *testing.T) {
	e := waxflow.New()
	stereo := container.Track{Fmt: audio.Format{Rate: 48000, Channels: 2,
		Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}, Samples: -1}

	plan, err := e.PlanTranscode(stereo, waxflow.TranscodeOptions{Format: "he-aac", HEAACv2: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BitRate != aac.HEV2DefaultBitrate {
		t.Errorf("v2 default bitrate = %d, want %d", plan.BitRate, aac.HEV2DefaultBitrate)
	}
	hasVersion := func(p *waxflow.TranscodePlan, v string) bool {
		for _, s := range p.Versions {
			if s == v {
				return true
			}
		}
		return false
	}
	if !hasVersion(plan, aac.HEV2EncoderVersion) {
		t.Errorf("v2 plan versions %v carry no %s", plan.Versions, aac.HEV2EncoderVersion)
	}

	v1, err := e.PlanTranscode(stereo, waxflow.TranscodeOptions{Format: "he-aac"})
	if err != nil {
		t.Fatal(err)
	}
	if v1.BitRate != aac.HEDefaultBitrate || hasVersion(v1, aac.HEV2EncoderVersion) {
		t.Errorf("v1 plan = bitrate %d versions %v; the flag must not leak", v1.BitRate, v1.Versions)
	}

	plan, err = e.PlanTranscode(stereo, waxflow.TranscodeOptions{Format: "he-aac", HEAACv2: true, AACBitrate: 64000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BitRate != 64000 {
		t.Errorf("explicit bitrate = %d, want 64000", plan.BitRate)
	}

	mono := container.Track{Fmt: audio.Format{Rate: 48000, Channels: 1,
		Layout: audio.DefaultLayout(1), Type: audio.Float, BitDepth: 32}, Samples: -1}
	if _, err := e.PlanTranscode(mono, waxflow.TranscodeOptions{Format: "he-aac", HEAACv2: true}); err == nil {
		t.Error("mono source planned under HEAACv2, want a named refusal")
	}
}

// TestHEAACv2Segmented pins the segmented column under the flag: the
// plan's CODECS resolves to mp4a.40.29 (the codecsFor seam), the delay
// stays the shared pin, and the segments carry whole 2048-sample AUs.
func TestHEAACv2Segmented(t *testing.T) {
	const rate, frames = 48000, 48000*3 + 555
	wav := brightWAV(t, rate, frames, 2)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "he-aac", HEAACv2: true}

	track := container.Track{Codec: codec.PCM,
		Fmt:     audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: frames, Default: true}
	plan, err := e.PlanSegments(track, opts, 2)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Codecs != "mp4a.40.29" || plan.Delay != aac.HEEncoderDelay {
		t.Fatalf("plan codecs %q delay %d, want mp4a.40.29/%d", plan.Codecs, plan.Delay, aac.HEEncoderDelay)
	}
	if plan.BitRate != aac.HEV2DefaultBitrate {
		t.Fatalf("segmented v2 bitrate %d, want the %d default", plan.BitRate, aac.HEV2DefaultBitrate)
	}

	// The v1 plan on the same track keeps its CODECS: the resolver splits
	// on the option, not the row.
	v1, err := e.PlanSegments(track, waxflow.TranscodeOptions{Format: "he-aac"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Codecs != "mp4a.40.5" {
		t.Fatalf("v1 plan codecs %q, want mp4a.40.5", v1.Codecs)
	}

	segs, res := collectSegments(t, e, wav, opts, plan.SegmentSamples, 0)
	if int64(len(segs)) != plan.Segments {
		t.Fatalf("produced %d segments, plan says %d", len(segs), plan.Segments)
	}
	if res.Samples != frames {
		t.Fatalf("segmented result samples = %d, want %d", res.Samples, frames)
	}
	var total int64
	for i, s := range segs {
		p := parseSegment(t, s.Data)
		for _, d := range p.durs {
			if d != 2048 {
				t.Fatalf("segment %d has an AU of %d samples, want 2048", i, d)
			}
		}
		total += p.samples()
	}
	if total != plan.TotalDecodeSamples {
		t.Fatalf("segments carry %d decode samples, plan says %d", total, plan.TotalDecodeSamples)
	}
}
