package waxflow_test

// What a run is allowed to promise in a header it cannot go back and fix.
//
// A muxer sizes its headers from the length the run projects. On a destination
// it can seek, a miss is patched at End and nothing is lost; on one it cannot,
// the promise is on the wire before the first sample is encoded. These pin the
// rule that follows from that: a count nothing has checked is confirmed before
// it is committed, and a miss that survives the confirmation is the input's
// fault rather than the library's.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// overDeclaringDemuxer declares more samples than it delivers until it is
// walked, which is the shape a Xing-tagged MP3 cut short has: a header count
// the frames contradict, on a source whose own walk would find that out.
//
// It counts its walks, so a cell can say not only what was projected but what
// was paid to project it.
type overDeclaringDemuxer struct {
	track  container.Track
	raw    int64 // what it really delivers
	pos    int64
	walked bool
	walks  int
}

func newOverDeclaring(declared, raw int64, exact, advisory bool) *overDeclaringDemuxer {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		panic(err)
	}
	return &overDeclaringDemuxer{
		track: container.Track{
			Codec: codec.PCM, CodecConfig: blob,
			Fmt:             cfg.PCMFormat(48000, 1, audio.DefaultLayout(1)),
			Samples:         declared,
			SamplesExact:    exact,
			SamplesAdvisory: advisory,
			Default:         true,
		},
		raw: raw,
	}
}

func (d *overDeclaringDemuxer) Tracks() []container.Track { return []container.Track{d.track} }

func (d *overDeclaringDemuxer) ReadPacket(pkt *container.Packet) error {
	if d.pos >= d.raw {
		return io.EOF
	}
	n := min(int64(1000), d.raw-d.pos)
	*pkt = container.Packet{Track: 0, Packet: codec.Packet{
		Data: make([]byte, n*2), PTS: d.pos, Dur: n, Sync: true,
	}}
	d.pos += n
	return nil
}

// Walk settles the declaration against the payload, the way every deferred
// walk in the tree does, and leaves the packet position alone.
func (d *overDeclaringDemuxer) Walk() error {
	d.walks++
	d.walked = true
	d.track = container.SettleLength(d.track, d.raw)
	return nil
}

func (d *overDeclaringDemuxer) Walked() bool { return d.walked }

// TestUnconfirmedLengthIsWalkedBeforeItIsCommitted is D3 on both destinations
// and both output formats: the pipe pays one walk and writes the delivered
// count, the seekable writer pays none and patches to the same number.
//
// Two formats because the two muxers disagree about a miss and the rule has to
// hold for both: riff.Muxer.End refuses one outright, flacn.Muxer.End refuses a
// STREAMINFO total it cannot patch.
func TestUnconfirmedLengthIsWalkedBeforeItIsCommitted(t *testing.T) {
	const declared, raw = 48000, 47000
	for _, out := range []string{"wav", "flac"} {
		t.Run(out+"/pipe", func(t *testing.T) {
			d := newOverDeclaring(declared, raw, false, false)
			med := mediaOver(t, d)
			defer med.Close()
			var buf bytes.Buffer
			res, err := waxflow.New().TranscodeMedia(context.Background(), med, &buf,
				waxflow.TranscodeOptions{Format: out})
			if err != nil {
				t.Fatalf("to %s on a pipe: %v", out, err)
			}
			if d.walks != 1 {
				t.Errorf("walked %d times, want exactly one: the header is a commitment here", d.walks)
			}
			if res.Samples != raw {
				t.Errorf("result reports %d samples, the source delivered %d", res.Samples, raw)
			}
			if got := probeTrack(t, buf.Bytes(), out).Samples; got != raw {
				t.Errorf("the %s declares %d samples, the source delivered %d", out, got, raw)
			}
		})
		t.Run(out+"/seekable", func(t *testing.T) {
			d := newOverDeclaring(declared, raw, false, false)
			med := mediaOver(t, d)
			defer med.Close()
			dst := &testutil.MemWriteSeeker{}
			if _, err := waxflow.New().TranscodeMedia(context.Background(), med, dst,
				waxflow.TranscodeOptions{Format: out}); err != nil {
				t.Fatalf("to %s on a seekable writer: %v", out, err)
			}
			if d.walks != 0 {
				t.Errorf("walked %d times; a patchable destination needs no confirmation", d.walks)
			}
			if got := probeTrack(t, dst.Buf, out).Samples; got != raw {
				t.Errorf("the %s declares %d samples, the source delivered %d", out, got, raw)
			}
		})
	}
}

// TestAdvisoryLengthIsNotWalked is the D3 boundary. An advisory total is not a
// claim about content, so walking would buy nothing and the projection is
// simply dropped: the WAV carries its streaming placeholder, as it does for a
// source with no length at all.
func TestAdvisoryLengthIsNotWalked(t *testing.T) {
	d := newOverDeclaring(48000, 47000, false, true)
	med := mediaOver(t, d)
	defer med.Close()
	var out bytes.Buffer
	if _, err := waxflow.New().TranscodeMedia(context.Background(), med, &out,
		waxflow.TranscodeOptions{Format: "wav"}); err != nil {
		t.Fatal(err)
	}
	if d.walks != 0 {
		t.Errorf("walked %d times for an advisory total; it is an estimate, not a claim", d.walks)
	}
	// The placeholder, read off the header rather than the probe: a streaming
	// WAV writes 0xFFFFFFFF for its RIFF size, and the demuxer clamps that back
	// to the bytes that are really there, so a probe cannot tell the two apart.
	if got := binary.LittleEndian.Uint32(out.Bytes()[4:8]); got != 0xFFFFFFFF {
		t.Errorf("RIFF size = %#x, want the streaming placeholder: an advisory total projects nothing", got)
	}
	// And the audio is all there whatever the header says.
	if got := probeTrack(t, out.Bytes(), "wav").Samples; got != 47000 {
		t.Errorf("the WAV holds %d samples, the source delivered 47000", got)
	}

	// The contrast, on the same destination: a confirmed count writes its real
	// size up front, so the placeholder above is a decision rather than the
	// only thing this muxer can do on a pipe.
	e := newOverDeclaring(48000, 47000, false, false)
	exact := mediaOver(t, e)
	defer exact.Close()
	var sized bytes.Buffer
	if _, err := waxflow.New().TranscodeMedia(context.Background(), exact, &sized,
		waxflow.TranscodeOptions{Format: "wav"}); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(sized.Bytes()[4:8]); got == 0xFFFFFFFF {
		t.Error("a confirmed count also wrote the placeholder; the walk bought nothing")
	}
}

// TestProjectionMissIsSourceDamage is D4. With the count confirmed, a run that
// still comes up short means the payload contradicted a length something had
// already verified, which is the input's fault: the muxer's internal error
// would tell an operator the library miscounted.
func TestProjectionMissIsSourceDamage(t *testing.T) {
	// Exact and walked, yet short: nothing is left to blame but the file.
	d := newOverDeclaring(48000, 47000, true, false)
	d.walked = true
	med := mediaOver(t, d)
	defer med.Close()
	var out bytes.Buffer
	_, err := waxflow.New().TranscodeMedia(context.Background(), med, &out,
		waxflow.TranscodeOptions{Format: "wav"})
	if err == nil {
		t.Fatal("a confirmed 48000 that delivered 47000 wrote a WAV on a pipe without complaint")
	}
	if code := waxerr.CodeOf(err); code != waxerr.CodeMalformedInput {
		t.Errorf("error code = %q, want %q: %v", code, waxerr.CodeMalformedInput, err)
	}
	for _, want := range []string{"47000", "48000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	// The same source to a writer the muxer can patch simply succeeds: the
	// projection there was provisional, and End rewrites it to what the run
	// produced, so there is no promise left to have missed.
	d2 := newOverDeclaring(48000, 47000, true, false)
	d2.walked = true
	med2 := mediaOver(t, d2)
	defer med2.Close()
	out2 := &testutil.MemWriteSeeker{}
	if _, err := waxflow.New().TranscodeMedia(context.Background(), med2, out2,
		waxflow.TranscodeOptions{Format: "wav"}); err != nil {
		t.Fatalf("a patchable destination must absorb the same miss: %v", err)
	}
}

// TestTruncatedXingMP3ToAPipe is the deferred entry's own shape, end to end on
// a real file: an MP3 whose Xing header counts 22050 samples, cut short so the
// frames hold fewer, transcoded to a destination that cannot be patched.
//
// It failed CodeInternal at riff.Muxer.End, with the WAV's headers already
// written from the projection. The same file to mp3 was worse: the MP3 muxer
// never checks, so it wrote the projected count into the LAME tag and said
// nothing.
func TestTruncatedXingMP3ToAPipe(t *testing.T) {
	raw, err := os.ReadFile(repoPath("testdata", "sine-cbr128.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	cut := raw[:len(raw)-100]
	declared := probeTrack(t, cut, "mp3").Samples
	if declared != 22050 {
		t.Fatalf("the fixture's Xing count is %d; this cell is written to 22050", declared)
	}

	var wav bytes.Buffer
	res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(cut), "mp3", &wav,
		waxflow.TranscodeOptions{Format: "wav"})
	if err != nil {
		t.Fatalf("truncated Xing MP3 to wav on a pipe: %v", err)
	}
	if res.Samples >= declared {
		t.Errorf("the run produced %d samples against a declared %d; this cell needs a source that comes up short",
			res.Samples, declared)
	}
	if got := probeTrack(t, wav.Bytes(), "wav").Samples; got != res.Samples {
		t.Errorf("the WAV declares %d samples, the run produced %d", got, res.Samples)
	}
	if len(res.InputWarnings) == 0 {
		t.Error("no input warning for a file whose frames stop short of its Xing count")
	}

	// And to mp3, where nothing would have caught it: the LAME tag's count is
	// the run's, not the projection's.
	var out bytes.Buffer
	mres, err := waxflow.New().Transcode(context.Background(), container.BytesSource(cut), "mp3", &out,
		waxflow.TranscodeOptions{Format: "mp3"})
	if err != nil {
		t.Fatalf("truncated Xing MP3 to mp3 on a pipe: %v", err)
	}
	if got := probeTrack(t, out.Bytes(), "mp3").Samples; got != mres.Samples {
		t.Errorf("the output's LAME count is %d, the run produced %d", got, mres.Samples)
	}
}

// mediaOver wraps a demuxer as a Media, the way format.FromDemuxer does for
// any caller assembling its own packets.
func mediaOver(t *testing.T, d container.Demuxer) format.Media {
	t.Helper()
	med, err := format.FromDemuxer("synthetic", d)
	if err != nil {
		t.Fatalf("FromDemuxer: %v", err)
	}
	return med
}

// TestRemuxCommitsOnlyAConfirmedLength is the same rule on the packet-copy
// rung. PlanRemux copies the source track's length into the muxer's track
// whatever the flags say, so a FLAC-in-Matroska remuxed to native FLAC on a
// pipe wrote a STREAMINFO total off an Info Duration and died at flacn's End
// with the whole file already written.
//
// The fixture declares seven samples more than its frames hold, so the
// millisecond-rounded Duration the muxer writes is wrong in the direction that
// fails: the estimate promises audio the packets do not have.
func TestRemuxCommitsOnlyAConfirmedLength(t *testing.T) {
	const frames, over = 40 * 4096, 7
	file := flacInMatroska(t, frames, frames+over)
	in := probeTrack(t, file, "mka")
	if !in.SamplesAdvisory {
		t.Fatalf("the fixture's length is not advisory (%d samples, exact %v)", in.Samples, in.SamplesExact)
	}

	e := waxflow.New()
	var out bytes.Buffer
	res, err := e.Remux(context.Background(), container.BytesSource(file), "mka", &out,
		waxflow.TranscodeOptions{Format: "flac"})
	if err != nil {
		t.Fatalf("FLAC-in-Matroska to flac on a pipe: %v", err)
	}
	if res.Samples != frames {
		t.Errorf("the remux reports %d samples, the packets hold %d", res.Samples, frames)
	}
	// The header declares nothing rather than the estimate: STREAMINFO's own
	// total is 0 and this writer cannot be patched, so there is no honest
	// number to put there. Read off the header, not through a probe, because a
	// probe of a FLAC measures the frames and would answer the same either way.
	if got := streamInfoTotal(t, out.Bytes()); got != 0 {
		t.Errorf("STREAMINFO declares %d samples; nothing could confirm a count here", got)
	}
	// And a reader still gets the right length, by measuring: an unknown total
	// is one every FLAC reader handles, where one promising audio the file does
	// not have is not.
	if got := probeTrack(t, out.Bytes(), "flac").Samples; got != frames {
		t.Errorf("a probe of the output reports %d samples, the packets hold %d", got, frames)
	}

	// And onto a writer the muxer can patch, where the trailer's count does
	// reach the header: the estimate is still never committed, it is simply
	// corrected at End from the packets that crossed.
	seek := &testutil.MemWriteSeeker{}
	if _, err := e.Remux(context.Background(), container.BytesSource(file), "mka", seek,
		waxflow.TranscodeOptions{Format: "flac"}); err != nil {
		t.Fatalf("FLAC-in-Matroska to flac on a seekable writer: %v", err)
	}
	if got := probeTrack(t, seek.Buf, "flac").Samples; got != frames {
		t.Errorf("the patched output declares %d samples, the packets hold %d", got, frames)
	}
}

// flacInMatroska builds a Matroska holding `frames` samples of real FLAC
// frames while declaring `declared`, so the Info Duration the muxer writes
// estimates something the packets contradict.
func flacInMatroska(t *testing.T, frames, declared int64) []byte {
	t.Helper()
	// A real FLAC stream first, so the packets that cross are frames a FLAC
	// muxer will accept: a hand-built payload would fail before the length
	// rule was reached.
	f := audio.Format{Rate: 48000, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}
	var enc bytes.Buffer
	if _, err := waxflow.New().Transcode(context.Background(),
		container.BytesSource(synthWAV(t, f, int(frames))), "wav", &enc,
		waxflow.TranscodeOptions{Format: "flac"}); err != nil {
		t.Fatalf("building the FLAC source: %v", err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(enc.Bytes()), "flac", nil)
	if err != nil {
		t.Fatal(err)
	}
	track := info.Default()
	track.Samples, track.SamplesExact, track.SamplesAdvisory = declared, false, false
	// A FLAC encoded onto a stream leaves its STREAMINFO total 0, and that is
	// what makes the container's own number load-bearing: a muxer folds the
	// projection in only where STREAMINFO has none (see flacn's Begin), so a
	// blob carrying the true count would absorb the wrong estimate and this
	// cell would pass on a build that committed it.
	si, err := flac.ParseStreamInfo(track.CodecConfig)
	if err != nil {
		t.Fatalf("the FLAC source's STREAMINFO: %v", err)
	}
	si.Samples = 0
	if track.CodecConfig, err = si.MarshalBinary(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	m := mka.NewMuxer(&out, nil)
	if err := m.Begin([]container.Track{track}); err != nil {
		t.Fatalf("mka Begin: %v", err)
	}
	var pkt container.Packet
	var raw int64
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw += pkt.Dur
		if err := m.WritePacket(container.Packet{Packet: pkt.Packet}); err != nil {
			t.Fatalf("mka WritePacket: %v", err)
		}
	}
	if raw != frames {
		t.Fatalf("the FLAC source holds %d samples, this fixture is written for %d", raw, frames)
	}
	if err := m.End(codec.Trailer{Samples: declared}); err != nil {
		t.Fatalf("mka End: %v", err)
	}
	return out.Bytes()
}

// streamInfoTotal reads a FLAC file's declared total straight out of
// STREAMINFO: the "fLaC" magic, a 4-byte metadata block header, then the block.
func streamInfoTotal(t *testing.T, file []byte) int64 {
	t.Helper()
	if len(file) < 4+4+34 || string(file[:4]) != "fLaC" {
		t.Fatalf("not a FLAC stream (%d bytes)", len(file))
	}
	si, err := flac.ParseStreamInfo(file[8 : 8+34])
	if err != nil {
		t.Fatalf("the output's STREAMINFO: %v", err)
	}
	return si.Samples
}
