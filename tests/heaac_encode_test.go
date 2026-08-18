package waxflow_test

// HE-AAC v1 encoder, engine surface: the he-aac output row end to end.
// The codec-level pins (ASC bytes, the 3010-sample delay against our own
// decoder, QMF band energies) live in codec/aac; these tests cover the
// container story (fMP4 gapless signaling, the ADTS implicit form), the
// ffmpeg legs (our streams must decode in libavcodec at the doubled rate,
// and its decode must agree with ours), and the option surface.

import (
	"bytes"
	"context"
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

// transcodeHEAAC transcodes a source to HE-AAC via the engine.
func transcodeHEAAC(t *testing.T, src []byte, dst interface {
	Write([]byte) (int, error)
}, opts waxflow.TranscodeOptions) waxflow.TranscodeResult {
	t.Helper()
	opts.Format = "he-aac"
	e := waxflow.New()
	res, err := e.Transcode(context.Background(), container.BytesSource(src), "", dst, opts)
	if err != nil {
		t.Fatalf("Transcode to he-aac: %v", err)
	}
	return *res
}

// brightWAV synthesizes a float WAV with real energy above 12 kHz, so the
// SBR band has something to carry.
func brightWAV(t *testing.T, rate, frames, ch int) []byte {
	t.Helper()
	f := audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch), Type: audio.Float, BitDepth: 32}
	samples := make([]float32, frames*ch)
	seed := uint64(99)
	for i := 0; i < frames; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		noise := 0.02 * (2*float64(seed>>11)/float64(1<<53) - 1)
		x := float64(i)
		v := 0.28*math.Sin(2*math.Pi*500*x/float64(rate)) +
			0.18*math.Sin(2*math.Pi*3200*x/float64(rate)) +
			0.12*math.Sin(2*math.Pi*9500*x/float64(rate)) +
			0.08*math.Sin(2*math.Pi*13800*x/float64(rate)) + noise
		for c := 0; c < ch; c++ {
			samples[i*ch+c] = float32(v * (1 - 0.15*float64(c)))
		}
	}
	return synthWAVFromSamples(t, f, samples)
}

// heAUCount is the AU count the encoder emits for a source length: the
// padded core blocks, the core's flush block, and one more when the
// declared delay outruns the tail (see HEEncoder.Finish).
func heAUCount(frames int) int64 {
	nCore := int64((frames + 1) / 2)
	aus := (nCore+1023)/1024 + 1
	if aus*2048-int64(frames)-aac.HEEncoderDelay < 0 {
		aus++
	}
	return aus
}

// TestHEAACTranscodeFMP4 drives the he-aac row into its default fMP4
// container: exact trailer, the edit list carrying the 3010-sample delay,
// ffmpeg probing the doubled rate, and ffmpeg's decode agreeing with our
// own decode of the same file within the AAC RMS gate (the cross-decoder
// conformance check: two independent decoders, one bitstream).
func TestHEAACTranscodeFMP4(t *testing.T) {
	const frames = 48000 + 777
	wav := brightWAV(t, 48000, frames, 2)

	var out bytes.Buffer
	res := transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{})
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

	// Our own readback: the HE identity, the doubled rate, exact trims.
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
	ours, err := decodeAllDynamic(t, container.BytesSource(out.Bytes()), "m4a")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(ours)
	if ours.N != frames {
		t.Errorf("our decode = %d samples, want the gapless %d", ours.N, frames)
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
	t.Logf("cross-decoder differential on our own stream: rms=%g offset=%d", rms, off)
	if rms > aacRMSGate {
		t.Errorf("our decode and ffmpeg's disagree on our own bitstream: RMS %g exceeds %g (offset %d)", rms, aacRMSGate, off)
	}
}

// TestHEAACTranscodeADTS covers container=adts: implicit signalling, so
// our demuxer's first-frame probe must recover the HE identity, and
// ffmpeg must decode at the doubled rate (each AU is 2048 samples there;
// a decoder that missed the SBR layer would emit half as many).
func TestHEAACTranscodeADTS(t *testing.T) {
	const frames = 48000
	wav := brightWAV(t, 48000, frames, 2)

	var out bytes.Buffer
	res := transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{Container: "adts"})
	if res.Samples != frames || res.Container != "adts" {
		t.Fatalf("result = %+v, want %d samples in adts", res, frames)
	}

	med, err := format.Open(container.BytesSource(out.Bytes()), "aac", nil)
	if err != nil {
		t.Fatalf("open our adts: %v", err)
	}
	defer med.Close()
	tr := med.Info().Default()
	if tr.Codec != codec.HEAAC || tr.Fmt.Rate != 48000 {
		t.Errorf("adts readback = codec %q rate %d, want he-aac at 48000", tr.Codec, tr.Fmt.Rate)
	}
	buf := audio.Get(tr.Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		if err := med.ReadChunk(buf); err != nil {
			break
		}
		n += int64(buf.N)
	}
	want := heAUCount(frames) * 2048
	if n != want {
		t.Errorf("our adts decode = %d samples, want %d (untrimmed whole AUs)", n, want)
	}

	testutil.FFmpeg(t)
	path := filepath.Join(t.TempDir(), "out.aac")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got := testutil.FFmpegDecodeF32(t, path)
	if int64(len(got)) != want*2 {
		t.Errorf("ffmpeg decoded %d interleaved samples, want %d (2048 per AU at the doubled rate)", len(got), want*2)
	}
}

// TestHEAACDelayPinFFmpeg is the ffmpeg leg of the delay measurement: a
// train of Hann-windowed 3 kHz tonebursts (band-limited under the SBR
// crossover, so the matched filter rides the exact linear-phase core
// path rather than the envelope-gated high band) through the encoder,
// decoded by ffmpeg from the untrimmed ADTS form, must land at exactly
// the declared HEEncoderDelay. The same measurement against our own
// decoder lives in codec/aac.
func TestHEAACDelayPinFFmpeg(t *testing.T) {
	testutil.FFmpeg(t)
	// Stereo: ffmpeg widens mono ADTS HE-AAC to an implicit-PS-ready
	// stereo pair, which would double the interleaving under the search.
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
	transcodeHEAAC(t, wav, &out, waxflow.TranscodeOptions{Container: "adts"})
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
	t.Logf("ffmpeg matched-filter delay %d (declared %d)", bestLag, aac.HEEncoderDelay)
	if bestLag != aac.HEEncoderDelay {
		t.Errorf("ffmpeg-measured delay %d, declared %d", bestLag, aac.HEEncoderDelay)
	}
}

// TestHEAACPlanRates pins the rate story: the three dual-rate pairs pass
// through, every other source rate snaps onto them family-first (a
// hi-res or low-rate library degrades to a resample instead of a 415;
// caps advertises he-aac with no rate qualifier, so it must serve), and
// an explicit conflicting rate= is still refused by name.
func TestHEAACPlanRates(t *testing.T) {
	e := waxflow.New()
	for _, tc := range []struct{ src, want int }{
		{32000, 32000}, {44100, 44100}, {48000, 48000},
		{22050, 44100}, {24000, 48000}, {16000, 32000}, {11025, 44100},
		{88200, 44100}, {96000, 48000}, {64000, 48000}, {8000, 32000},
	} {
		f := audio.Format{Rate: tc.src, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
		plan, err := e.PlanTranscode(container.Track{Fmt: f, Samples: -1}, waxflow.TranscodeOptions{Format: "he-aac"})
		if err != nil {
			t.Errorf("plan for a %d Hz source: %v", tc.src, err)
			continue
		}
		if plan.Format.Rate != tc.want {
			t.Errorf("source %d planned at %d, want the %d snap", tc.src, plan.Format.Rate, tc.want)
		}
	}
	f := audio.Format{Rate: 24000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}
	if _, err := e.PlanTranscode(container.Track{Fmt: f, Samples: -1}, waxflow.TranscodeOptions{Format: "he-aac", Rate: 24000}); err == nil {
		t.Error("explicit rate=24000 accepted, want a named refusal")
	}
	plan, err := e.PlanTranscode(container.Track{Fmt: f, Samples: -1}, waxflow.TranscodeOptions{Format: "he-aac", Rate: 48000})
	if err != nil {
		t.Fatalf("plan with rate=48000 override: %v", err)
	}
	if plan.BitRate != aac.HEDefaultBitrate {
		t.Errorf("plan bitrate = %d, want the %d default", plan.BitRate, aac.HEDefaultBitrate)
	}
}

// TestHEAACSegmented is the segmented (HLS) smoke: the he-aac row's
// segmenter column produces CMAF segments whose AUs are whole 2048-sample
// frames, with the plan's delay riding the init segment's edit list and
// the segment count matching the plan.
func TestHEAACSegmented(t *testing.T) {
	const rate, frames = 48000, 48000*4 + 1234
	wav := brightWAV(t, rate, frames, 2)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "he-aac"}

	track := container.Track{Codec: codec.PCM,
		Fmt:     audio.Format{Rate: rate, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32},
		Samples: frames, Default: true}
	plan, err := e.PlanSegments(track, opts, 2)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Codecs != "mp4a.40.5" || plan.Delay != aac.HEEncoderDelay {
		t.Fatalf("plan codecs %q delay %d, want mp4a.40.5/%d", plan.Codecs, plan.Delay, aac.HEEncoderDelay)
	}
	if plan.SegmentSamples%2048 != 0 {
		t.Fatalf("segment samples %d not whole 2048-sample AUs", plan.SegmentSamples)
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

// TestHEAACBitrateScales checks AACBitrate reaches the HE encoder.
func TestHEAACBitrateScales(t *testing.T) {
	const frames = 48000 * 2
	wav := brightWAV(t, 48000, frames, 2)
	var lo, hi bytes.Buffer
	transcodeHEAAC(t, wav, &lo, waxflow.TranscodeOptions{AACBitrate: 32000, Container: "adts"})
	transcodeHEAAC(t, wav, &hi, waxflow.TranscodeOptions{AACBitrate: 96000, Container: "adts"})
	if hi.Len() < lo.Len()*2 {
		t.Fatalf("96k output (%d bytes) not clearly larger than 32k (%d bytes)", hi.Len(), lo.Len())
	}
}
