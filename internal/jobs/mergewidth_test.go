package jobs

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/format"
)

// writeWidthWAV renders frames of 48 kHz sine at a channel count into dir/name
// and returns its lib ref. One tone per channel so a fold's weights are
// observable rather than cancelling.
func writeWidthWAV(t *testing.T, dir, name string, channels, frames int) string {
	t.Helper()
	f := audio.Format{Rate: 48000, Channels: channels, Layout: audio.DefaultLayout(channels),
		Type: audio.Int, BitDepth: 16}
	buf := audio.Get(f, frames)
	defer audio.Put(buf)
	buf.N = frames
	for c := range channels {
		ch := buf.ChanI(c)
		freq := 220.0 + 70.0*float64(c)
		for i := range ch[:frames] {
			ch[i] = int32(0.25 * float64(1<<15) * math.Sin(2*math.Pi*freq*float64(i)/48000))
		}
	}
	enc, err := pcm.NewEncoder(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, f)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := riff.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: f,
		Samples: int64(frames), Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return "lib/" + name
}

// runMergeOutput runs a merge to completion and returns the decoded output.
func runMergeOutput(t *testing.T, r *Runner, req Request) *audio.Buffer {
	t.Helper()
	j, err := r.Create(req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := waitJob(t, r, j.ID, StateDone)
	out := soleOutput(t, done)
	raw, err := os.ReadFile(r.OutputPath(done, 0))
	if err != nil {
		t.Fatalf("reading the output: %v", err)
	}
	med, err := format.Open(container.BytesSource(raw), out.Container, nil)
	if err != nil {
		t.Fatalf("opening the output: %v", err)
	}
	defer med.Close()
	return drainAll(t, med)
}

// drainAll reads a Media to end of stream into one buffer.
func drainAll(t *testing.T, med format.Media) *audio.Buffer {
	t.Helper()
	f := med.Info().Default().Fmt
	out := audio.Get(f, 48000*16)
	out.N = 0
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err != nil {
			return out
		}
		if out.N+tmp.N > out.Cap() {
			t.Fatalf("the output is longer than the %d frames the test allows", out.Cap())
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
}

// rmsDB is the RMS of a window across every channel, in dBFS.
func rmsDB(buf *audio.Buffer, from, to int) float64 {
	var sum float64
	var n int
	for c := range buf.Fmt.Channels {
		ch := buf.ChanF(c)[from:to]
		for _, v := range ch {
			sum += float64(v) * float64(v)
		}
		n += len(ch)
	}
	if n == 0 || sum == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(math.Sqrt(sum/float64(n)))
}

// TestMergeFoldsEachMemberAtItsOwnWidth is the in-tree half of the defect the
// width rule closes, and it must be checked on the job's real output rather
// than on an option: a merge of a 5.1 and a stereo member to opus used to
// concatenate at the 5.1 envelope and fold the assembled timeline, which
// delivered the stereo member 3.01 dB under the same member merged alone.
//
// dsp/mix normalizes each output row over every source column, so the stereo
// member's four silent surround columns counted in its divisor. The fix is that
// runMerge resolves the delivered width and concatenates at it, so each member
// is folded by its own matrix before the seam.
func TestMergeFoldsEachMemberAtItsOwnWidth(t *testing.T) {
	const frames = 48000
	root := t.TempDir()
	five := writeWidthWAV(t, root, "five.wav", 6, frames)
	stereo := writeWidthWAV(t, root, "stereo.wav", 2, frames)
	res := openRoots(t, root)
	mk := func() *Runner {
		return openRunner(t, Config{Dir: t.TempDir(), Resolver: res, MeasureTrack: measureTrack()})
	}

	mixed := runMergeOutput(t, mk(), Request{Type: TypeMerge, Srcs: []string{five, stereo}, Format: "opus"})
	defer audio.Put(mixed)
	if mixed.Fmt.Channels != 2 {
		t.Fatalf("an opus merge delivered %d channels, want the row's stereo fold", mixed.Fmt.Channels)
	}
	if mixed.N < 2*frames {
		t.Fatalf("the merge delivered %d frames, want at least %d", mixed.N, 2*frames)
	}

	alone := runMergeOutput(t, mk(), Request{Type: TypeMerge, Srcs: []string{stereo, stereo}, Format: "opus"})
	defer audio.Put(alone)

	// Inner windows, 100 ms in from each edge: opus is lossy and a seam is
	// where its transients live, so the claim is about the member's body.
	const pad = 4800
	got := rmsDB(mixed, frames+pad, 2*frames-pad)
	want := rmsDB(alone, pad, frames-pad)
	if d := math.Abs(got - want); d > 0.1 {
		t.Errorf("the stereo member of a mixed-width merge reads %.4f dB, the same member merged alone reads %.4f "+
			"(%.4f dB apart); a member's fold must be its own, not the envelope's", got, want, d)
	}
}

// TestMergeRefusesNothingItUsedToAccept is the complement: a uniform-width
// merge is byte for byte what it was, because TimelineChannels answers 0 for
// it and the timeline is built exactly as before.
func TestMergeRefusesNothingItUsedToAccept(t *testing.T) {
	const frames = 24000
	root := t.TempDir()
	var refs []string
	for i := range 2 {
		refs = append(refs, writeWidthWAV(t, root, fmt.Sprintf("s%d.wav", i), 6, frames))
	}
	res := openRoots(t, root)
	r := openRunner(t, Config{Dir: t.TempDir(), Resolver: res, MeasureTrack: measureTrack()})
	out := runMergeOutput(t, r, Request{Type: TypeMerge, Srcs: refs, Format: "opus"})
	defer audio.Put(out)
	if out.Fmt.Channels != 2 {
		t.Fatalf("a uniform 5.1 merge to opus delivered %d channels, want the row's stereo fold", out.Fmt.Channels)
	}
	if out.N < 2*frames {
		t.Fatalf("the merge delivered %d frames, want at least %d", out.N, 2*frames)
	}
}
