package waxflow_test

// Ogg streams that start at a granule other than 0 (RFC 7845 section 4.5,
// Vorbis I section A.2, and Ogg FLAC through its mapping's reference to
// Vorbis), end to end. Each is built at test time from a committed WAV by this
// tree's own encoders and muxer, shifted, and held to its twin starting at 0.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// oggStart is a stream and its twin starting at granule 0.
type oggStart struct {
	name, hint    string
	twin, shifted []byte
	// trim is what the shifted stream discards from its twin's front, which
	// only a negative Vorbis start does.
	trim int64
}

// oggStartCases builds each codec's twin from testdata/sine-s16.wav looped to
// seconds, and the same stream starting elsewhere: 480000 samples in for every
// codec, and before time zero for Vorbis and FLAC.
func oggStartCases(t *testing.T, seconds int) []oggStart {
	t.Helper()
	wav := loopedWAV(t, seconds)
	opus := transcodeBytes(t, wav, waxflow.TranscodeOptions{Format: "opus"})
	vorbis, first := vorbisStartLayout(t, transcodeBytes(t, wav, waxflow.TranscodeOptions{Format: "vorbis"}))
	flac := transcodeBytes(t, wav, waxflow.TranscodeOptions{Format: "flac", Container: "ogg"})
	return []oggStart{
		{"opus", "opus", opus, testutil.ShiftOggGranules(t, opus, 2, 480000), 0},
		{"vorbis positive", "ogg", vorbis, testutil.ShiftOggGranules(t, vorbis, 3, 480000), 0},
		{"vorbis negative", "ogg", vorbis, testutil.ShiftOggGranules(t, vorbis, 3, -first/2), first / 2},
		{"flac positive", "oga", flac, testutil.ShiftOggGranules(t, flac, 2, 480000), 0},
		{"flac negative", "oga", flac, testutil.ShiftOggGranules(t, flac, 2, -1000), 0},
	}
}

// loopedWAV is testdata/sine-s16.wav repeated to at least seconds long.
func loopedWAV(t *testing.T, seconds int) []byte {
	t.Helper()
	src := decodeAll(t, fixtureSource(t, "sine-s16.wav"), "wav")
	defer audio.Put(src)
	want := seconds * src.Fmt.Rate
	out := audio.Get(src.Fmt, want+src.N)
	defer audio.Put(out)
	for out.N < want {
		audio.CopyFrames(out, out.N, src, 0, src.N)
		out.N += src.N
	}
	return wavFromBuffer(t, src.Fmt, out)
}

func transcodeBytes(t *testing.T, wav []byte, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(wav), "wav", &out, opts); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// vorbisStartLayout puts a Vorbis stream's first two audio packets alone on
// its first audio page, which Vorbis I section A.2 asks of a stream starting
// anywhere but 0, and returns it with that page's granule.
func vorbisStartLayout(t *testing.T, raw []byte) ([]byte, int64) {
	t.Helper()
	out, first := vorbisFirstPage(t, raw, 2)
	if first < 2 {
		t.Fatalf("the first two packets lap to %d samples, too few to trim", first)
	}
	return out, first
}

// vorbisFirstPage puts a Vorbis stream's first n audio packets alone on its
// first audio page, under the granule they end at, and returns it with that
// granule.
func vorbisFirstPage(t *testing.T, raw []byte, n int) ([]byte, int64) {
	t.Helper()
	d, err := ogg.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	var p container.Packet
	for range n {
		if err := d.ReadPacket(&p); err != nil {
			t.Fatal(err)
		}
	}
	first := p.PTS + p.Dur
	return testutil.RepageOggStart(t, raw, 3, n, first), first
}

// readUntilEnd reads med to its end or its first failure, closes it, and
// returns what it delivered.
func readUntilEnd(med format.Media) (int64, error) {
	defer med.Close()
	return readCount(med)
}

// readCount reads med until it ends or fails and returns what it delivered
// first, with nil for a clean end: the bare io.EOF, since a wrapped one is a
// read that failed.
func readCount(med format.Media) (int64, error) {
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		if err != nil && err != io.EOF {
			return n, err
		}
		n += int64(buf.N)
		if err != nil {
			return n, nil
		}
	}
}

func openBytes(t *testing.T, raw []byte, hint string) format.Media {
	t.Helper()
	med, err := waxflow.New().OpenStream(container.BytesSource(raw), hint)
	if err != nil {
		t.Fatal(err)
	}
	return med
}

// sameFrames reports the first frame where a's n frames from aOff differ from
// b's from bOff, or -1.
func sameFrames(a *audio.Buffer, aOff int, b *audio.Buffer, bOff, n int) int {
	for c := range a.Fmt.Channels {
		for i := range n {
			if a.Fmt.Type == audio.Float {
				if a.ChanF(c)[aOff+i] != b.ChanF(c)[bOff+i] {
					return i
				}
			} else if a.ChanI(c)[aOff+i] != b.ChanI(c)[bOff+i] {
				return i
			}
		}
	}
	return -1
}

// TestOggInitialGranule: a stream starting at a granule other than 0 reports,
// decodes and seeks as its twin does, less the front a negative Vorbis start
// discards, and the open span and packet cut that hold a source to its exact
// length end cleanly over it.
func TestOggInitialGranule(t *testing.T) {
	if raceEnabled {
		// Decoding and seeking with no goroutines, about 90 s under the race
		// detector; the non-race pass runs it in full.
		t.Skip("pure-numeric decode and seek sweep; skipped under -race")
	}
	for _, c := range oggStartCases(t, 48) {
		t.Run(c.name, func(t *testing.T) {
			requireOggTwin(t, c)
			t.Run("ffmpeg", func(t *testing.T) {
				if c.trim > 0 {
					// ffmpeg decodes the samples Vorbis I section A.2 says
					// to discard (libvorbis discards them, as this tree does).
					t.Skip("ffmpeg applies no negative Vorbis start")
				}
				testutil.FFmpeg(t)
				path := filepath.Join(t.TempDir(), "shifted."+c.hint)
				if err := os.WriteFile(path, c.shifted, 0o644); err != nil {
					t.Fatal(err)
				}
				med := openBytes(t, c.shifted, c.hint)
				ch := med.Info().Default().Fmt.Channels
				n, err := readUntilEnd(med)
				if err != nil {
					t.Fatal(err)
				}
				if ref := int64(len(testutil.FFmpegDecodeF32(t, path)) / ch); ref != n {
					t.Errorf("ffmpeg decodes %d samples, this tree %d", ref, n)
				}
			})
		})
	}
}

// TestOggUntrustedFirstGranule: a first page whose granule the next page does
// not back reads as if that granule were right, which is how the stream read
// before an initial granule was honoured, with a note saying so. ffmpeg
// discards a 0 on a first page holding audio (its trac ticket 3710), and a
// granule far past the rest is the same disagreement the other way. An Ogg
// FLAC reads its frames and no granule, so it reads as its twin with nothing
// to note.
func TestOggUntrustedFirstGranule(t *testing.T) {
	if raceEnabled {
		t.Skip("pure-numeric decode and seek sweep; skipped under -race")
	}
	wav := loopedWAV(t, 48)
	vorbis, _ := vorbisFirstPage(t, transcodeBytes(t, wav, waxflow.TranscodeOptions{Format: "vorbis"}), 45)
	for _, tw := range []struct {
		oggStart
		headers int
	}{
		{oggStart{name: "opus", hint: "opus", twin: transcodeBytes(t, wav, waxflow.TranscodeOptions{Format: "opus"})}, 2},
		// libvorbis fills its first page; this tree's muxer starts with one packet.
		{oggStart{name: "vorbis", hint: "ogg", twin: vorbis}, 3},
		{oggStart{name: "flac", hint: "oga", twin: transcodeBytes(t, wav, waxflow.TranscodeOptions{Format: "flac", Container: "ogg"})}, 2},
	} {
		first := testutil.OggFirstAudioPage(t, tw.twin, tw.headers)
		for _, g := range []struct {
			name    string
			granule int64
		}{{"zero", 0}, {"far ahead", 1 << 40}} {
			c := tw.oggStart
			c.name += " " + g.name
			c.shifted = testutil.SetOggGranule(t, tw.twin, first, g.granule)
			t.Run(c.name, func(t *testing.T) {
				med := openBytes(t, c.shifted, c.hint)
				notes := med.Info().Notes
				med.Close()
				// An Ogg FLAC reads its frames and no granule.
				if flacRow := c.hint == "oga"; flacRow && len(notes) != 0 ||
					!flacRow && (len(notes) != 1 || !strings.Contains(notes[0], "disagrees")) {
					t.Errorf("notes %v, want one saying the first page's granule disagrees, or none for FLAC", notes)
				}
				requireOggTwin(t, c)
			})
		}
	}
}

// TestOggFirstGranuleDamageReadsAsFFmpeg: streams ffmpeg made, whose first
// audio pages state a start the rest does not back, read as many samples as
// ffmpeg decodes from them, and say so exactly: an Ogg FLAC with its first
// granule raised or zeroed, or its second zeroed, as encoded and as
// stream-copy cuts, and a stream cut short after its first audio page, as it
// is and with that granule zeroed.
func TestOggFirstGranuleDamageReadsAsFFmpeg(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	dir := t.TempDir()
	readFile := func(t *testing.T, path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	asFFmpeg := func(t *testing.T, name, hint string, raw []byte) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		med := openBytes(t, raw, hint)
		tr := med.Info().Default()
		n, err := readUntilEnd(med)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !tr.SamplesExact || tr.Samples != n {
			t.Errorf("%s: the track says %d samples (exact %v), a decode delivers %d", name, tr.Samples, tr.SamplesExact, n)
		}
		if ref := int64(len(testutil.FFmpegDecodeF32(t, path)) / tr.Fmt.Channels); ref != n {
			t.Errorf("%s: ffmpeg decodes %d samples, this tree %d", name, ref, n)
		}
	}

	t.Run("flac first granule", func(t *testing.T) {
		// Two audio pages, the second ending the stream, made both ways, and
		// a longer cut.
		short, src, cut := filepath.Join(dir, "short.oga"), filepath.Join(dir, "src.flac"), filepath.Join(dir, "cut.oga")
		long, longCut := filepath.Join(dir, "long.flac"), filepath.Join(dir, "longcut.oga")
		testutil.FFmpegGenerateDuration(t, short, 1.5, 44100, 2, "flac")
		testutil.FFmpegGenerateDuration(t, src, 5, 44100, 2, "flac")
		testutil.FFmpegGenerateDuration(t, long, 40, 44100, 2, "flac")
		for _, c := range [][3]string{{"3.5", src, cut}, {"3.5", long, longCut}} {
			out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-ss", c[0], "-i", c[1], "-c", "copy", c[2]).CombinedOutput()
			if err != nil {
				t.Fatalf("ffmpeg: %v\n%s", err, out)
			}
		}
		for _, p := range []string{short, cut, longCut} {
			raw := readFile(t, p)
			first := testutil.OggFirstAudioPage(t, raw, 2)
			g := testutil.OggGranule(t, raw, first)
			name := filepath.Base(p)
			asFFmpeg(t, name, "oga", raw)
			asFFmpeg(t, "raised-"+name, "oga", testutil.SetOggGranule(t, raw, first, g+1000))
			asFFmpeg(t, "zero-"+name, "oga", testutil.SetOggGranule(t, raw, first, 0))
			asFFmpeg(t, "zero-second-"+name, "oga", testutil.SetOggGranule(t, raw, first+1, 0))
		}
	})

	for _, c := range []struct {
		name, hint, acodec, ext string
		rate, headers           int
	}{
		{"opus", "opus", "libopus", "opus", 48000, 2},
		{"vorbis", "ogg", "libvorbis", "ogg", 44100, 3},
		{"flac", "oga", "flac", "oga", 44100, 2},
	} {
		t.Run(c.name+" lone first page", func(t *testing.T) {
			testutil.FFmpegEncoder(t, c.acodec)
			full := filepath.Join(dir, "full."+c.ext)
			testutil.FFmpegGenerateDuration(t, full, 3, c.rate, 2, c.acodec)
			raw := readFile(t, full)
			first := testutil.OggFirstAudioPage(t, raw, c.headers)
			lone := testutil.TruncateOggAfter(t, raw, first)
			asFFmpeg(t, "lone."+c.ext, c.hint, lone)
			asFFmpeg(t, "lone0."+c.ext, c.hint, testutil.SetOggGranule(t, lone, first, 0))
		})
	}
}

// requireOggTwin holds c's shifted stream to its twin through format.Media: an
// exact length a decode delivers, the twin's decode less c.trim bit for bit,
// seeks onto the twin's samples, a clean open span, and a cut that ends
// cleanly (Opus) or declines for the codec (the rest).
func requireOggTwin(t *testing.T, c oggStart) {
	t.Helper()
	// Under two of the demuxer's 128 KiB bisection windows every seek walks
	// from the first page, and a wrong landing is clamped to 0.
	if len(c.twin) < 2*128<<10 {
		t.Fatalf("the stream is %d bytes, too short for a seek to bisect", len(c.twin))
	}
	twin, err := decodeAllDynamic(t, container.BytesSource(c.twin), c.hint)
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(twin)

	t.Run("decode", func(t *testing.T) {
		med := openBytes(t, c.shifted, c.hint)
		tr, ref := med.Info().Default(), openBytes(t, c.twin, c.hint)
		want := ref.Info().Default()
		med.Close()
		ref.Close()
		if tr.Delay != want.Delay+c.trim {
			t.Errorf("a front trim of %d, its twin's %d plus %d", tr.Delay, want.Delay, c.trim)
		}
		got, err := decodeAllDynamic(t, container.BytesSource(c.shifted), c.hint)
		if err != nil {
			t.Fatal(err)
		}
		defer audio.Put(got)
		if !tr.SamplesExact || tr.Samples != int64(got.N) {
			t.Errorf("the track says %d samples (exact %v), a decode delivers %d", tr.Samples, tr.SamplesExact, got.N)
		}
		if want := int64(twin.N) - c.trim; int64(got.N) != want {
			t.Fatalf("decoded %d samples, want its twin's %d less %d", got.N, twin.N, c.trim)
		}
		if i := sameFrames(got, 0, twin, int(c.trim), got.N); i >= 0 {
			t.Errorf("the decode differs from its twin's at frame %d", i)
		}
	})

	t.Run("seek", func(t *testing.T) {
		med, ref := openBytes(t, c.shifted, c.hint), openBytes(t, c.twin, c.hint)
		defer med.Close()
		defer ref.Close()
		total := int64(twin.N) - c.trim
		a, b := audio.Get(twin.Fmt, 512), audio.Get(twin.Fmt, 512)
		defer audio.Put(a)
		defer audio.Put(b)
		state := int64(7)
		for i := range 40 {
			state = state*6364136223846793005 + 1442695040888963407
			target := (state >> 16) % total
			if target < 0 {
				target += total
			}
			if i == 0 {
				target = 0
			}
			landed, err := med.SeekSample(context.Background(), target)
			if err != nil || landed != target {
				t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
			}
			if landed, err := ref.SeekSample(context.Background(), target+c.trim); err != nil || landed != target+c.trim {
				t.Fatalf("twin seek to %d landed at %d (%v)", target+c.trim, landed, err)
			}
			if err := med.ReadChunk(a); err != nil {
				t.Fatalf("read after seek to %d: %v", target, err)
			}
			if err := ref.ReadChunk(b); err != nil {
				t.Fatalf("twin read after seek to %d: %v", target+c.trim, err)
			}
			if a.N != b.N || a.Pos != target {
				t.Fatalf("seek to %d: chunk of %d at %d, twin's %d", target, a.N, a.Pos, b.N)
			}
			if j := sameFrames(a, 0, b, 0, a.N); j >= 0 {
				t.Fatalf("seek to %d: differs from its twin's at frame %d", target, j)
			}
		}
		if landed, err := med.SeekSample(context.Background(), total+100); err != nil || landed != total {
			t.Fatalf("past-end seek landed at %d (%v), want %d", landed, err, total)
		}
		if err := med.ReadChunk(a); !errors.Is(err, io.EOF) {
			t.Fatalf("read past the end = %v, want EOF", err)
		}
	})

	t.Run("open slice", func(t *testing.T) {
		sl, err := waxflow.Slice(openBytes(t, c.shifted, c.hint), 0, waxflow.ToEnd)
		if err != nil {
			t.Fatal(err)
		}
		n, err := readUntilEnd(sl)
		if err != nil {
			t.Fatalf("the open slice failed after %d samples: %v", n, err)
		}
		if want := int64(twin.N) - c.trim; n != want {
			t.Errorf("the open slice delivered %d, want %d", n, want)
		}
	})

	t.Run("open cut", func(t *testing.T) {
		if c.hint != "opus" {
			// The cut moves packets, which only Opus here survives.
			requireCutDeclines(t, c.shifted, c.hint)
			return
		}
		got, ref := openCut(t, c.shifted, c.hint), openCut(t, c.twin, c.hint)
		if a, b := got.Tracks()[0], ref.Tracks()[0]; a.Samples != b.Samples || a.Delay != b.Delay {
			t.Errorf("the cut track is %d samples, delay %d; its twin's %d, delay %d", a.Samples, a.Delay, b.Samples, b.Delay)
		}
		var pkt container.Packet
		var n int
		for {
			err := got.ReadPacket(&pkt)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("the open cut failed after %d packets: %v", n, err)
			}
			n++
		}
	})
}

// requireCutDeclines asserts a cut of all of raw declines for its codec, which
// the cut rung's allowlist leaves out, before any length is weighed.
func requireCutDeclines(t *testing.T, raw []byte, hint string) {
	t.Helper()
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = waxflow.Cut(demux, info.Default(), waxflow.TranscodeOptions{},
		[]waxflow.Span{{From: 0, To: waxflow.ToEnd}}, 4096)
	if waxerr.CodeOf(err) != waxerr.CodeUnsupportedFormat || !strings.Contains(err.Error(), "moved within the stream") {
		t.Errorf("the open cut = %v, want the codec's decline", err)
	}
}

// openCut is an open cut of all of raw: one span from 0 to the end.
func openCut(t *testing.T, raw []byte, hint string) container.Demuxer {
	t.Helper()
	grid, err := waxflow.New().PacketGrid(context.Background(), container.BytesSource(raw), hint)
	if err != nil {
		t.Fatal(err)
	}
	demux, info, err := format.OpenDemuxer(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := waxflow.Cut(demux, info.Default(), waxflow.TranscodeOptions{},
		[]waxflow.Span{{From: 0, To: waxflow.ToEnd}}, grid)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// serveVorbis takes raw, an Ogg Vorbis, down the ladder the server runs for
// opts: the copy rung when PlanRemux plans one, the transcode otherwise. It
// returns the output and whether the copy rung served it.
func serveVorbis(t *testing.T, raw []byte, opts waxflow.TranscodeOptions) ([]byte, bool) {
	t.Helper()
	e := waxflow.New()
	plan, err := e.PlanRemux(probeTrack(t, raw, "ogg"), opts)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if plan != nil {
		_, err = e.Remux(t.Context(), container.BytesSource(raw), "ogg", &out, opts)
	} else {
		_, err = e.Transcode(t.Context(), container.BytesSource(raw), "ogg", &out, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), plan != nil
}

// TestRemuxOfAVorbisFrontTrim: a Vorbis stream starting before time zero
// carries its front trim as Delay. Ogg can state that only as a negative
// start, which this tree's Ogg muxer does not write, so the copy rung declines
// into Ogg and the transcode serves it; WebM states it as CodecDelay and
// copies.
func TestRemuxOfAVorbisFrontTrim(t *testing.T) {
	twin, first := vorbisStartLayout(t, transcodeBytes(t, loopedWAV(t, 2), waxflow.TranscodeOptions{Format: "vorbis"}))
	trimmed := testutil.ShiftOggGranules(t, twin, 3, -first/2)
	src, err := decodeAllDynamic(t, container.BytesSource(trimmed), "ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(src)
	decode := func(raw []byte, hint string) *audio.Buffer {
		b, err := decodeAllDynamic(t, container.BytesSource(raw), hint)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("into ogg", func(t *testing.T) {
		out, copied := serveVorbis(t, trimmed, waxflow.TranscodeOptions{Format: "vorbis"})
		if copied {
			t.Error("the copy rung took a front trim into Ogg")
		}
		got := decode(out, "ogg")
		defer audio.Put(got)
		if got.N != src.N {
			t.Fatalf("the output holds %d samples, the source %d", got.N, src.N)
		}
		// A fresh encode is not the source's samples, but it has to sit on
		// them: the best alignment is no shift at all.
		ref, dec, ch := testutil.InterleaveF(src), testutil.InterleaveF(got), src.Fmt.Channels
		best, bestSNR := 0, math.Inf(-1)
		for lag := -600; lag <= 600; lag++ {
			if s := snrDB(ref, dec, lag, ch); s > bestSNR {
				best, bestSNR = lag, s
			}
		}
		if best != 0 || bestSNR < 10 {
			t.Errorf("the output sits on the source %d samples late (%.1f dB there)", best, bestSNR)
		}
	})
	t.Run("into webm", func(t *testing.T) {
		out, copied := serveVorbis(t, trimmed, waxflow.TranscodeOptions{Format: "vorbis", Container: "webm"})
		if !copied {
			t.Error("the copy rung declined a front trim WebM carries")
		}
		got := decode(out, "webm")
		defer audio.Put(got)
		if got.N != src.N || sameFrames(got, 0, src, 0, got.N) >= 0 {
			t.Errorf("the WebM copy holds %d samples, not the source's %d bit for bit", got.N, src.N)
		}
	})
	t.Run("untrimmed into ogg", func(t *testing.T) {
		// The control: with no front trim the copy rung still serves, and
		// only a copy decodes to the source bit for bit.
		out, copied := serveVorbis(t, twin, waxflow.TranscodeOptions{Format: "vorbis"})
		if !copied {
			t.Fatal("the copy rung declined an untrimmed Vorbis into Ogg")
		}
		want, got := decode(twin, "ogg"), decode(out, "ogg")
		defer audio.Put(want)
		defer audio.Put(got)
		if got.N != want.N || sameFrames(got, 0, want, 0, got.N) >= 0 {
			t.Errorf("the copy holds %d samples, not the source's %d bit for bit", got.N, want.N)
		}
	})
}

// TestOggSeekWithNoGranules: an Opus stream stating no granule at all gives a
// seek nothing to anchor on, so the seek walks from the first audio packet,
// raw 0 by definition, and still lands exactly, on the samples a decode from
// the start delivers.
func TestOggSeekWithNoGranules(t *testing.T) {
	twin := transcodeBytes(t, loopedWAV(t, 10), waxflow.TranscodeOptions{Format: "opus"})
	raw := testutil.StripOggGranules(t, twin, 2)
	full, err := decodeAllDynamic(t, container.BytesSource(twin), "opus")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(full)
	med := openBytes(t, raw, "opus")
	defer med.Close()
	if tr := med.Info().Default(); tr.SamplesExact {
		t.Errorf("%d samples, exact, from a stream stating no granule", tr.Samples)
	}
	buf := audio.Get(full.Fmt, 4096)
	defer audio.Put(buf)
	for _, target := range []int64{0, 1, 48000, 123457, int64(full.N) - 5000} {
		landed, err := med.SeekSample(context.Background(), target)
		if err != nil || landed != target {
			t.Fatalf("seek to %d landed at %d (%v)", target, landed, err)
		}
		if err := med.ReadChunk(buf); err != nil {
			t.Fatalf("read after seek to %d: %v", target, err)
		}
		if buf.Pos != target || sameFrames(buf, 0, full, int(target), buf.N) >= 0 {
			t.Fatalf("seek to %d: a chunk at %d that is not the decode's samples there", target, buf.Pos)
		}
	}
}

// TestOggUnstatedLastGranuleDeliversEveryPacket: a last page stating no granule
// leaves the length at the last granule stated, short of the packets after it,
// so the track calls it advisory: an Opus decode is not capped there, and
// both codecs deliver every packet the file holds.
func TestOggUnstatedLastGranuleDeliversEveryPacket(t *testing.T) {
	for _, codec := range []string{"opus", "vorbis"} {
		t.Run(codec, func(t *testing.T) {
			raw := transcodeBytes(t, loopedWAV(t, 6), waxflow.TranscodeOptions{Format: codec})
			want := decodeAll(t, container.BytesSource(raw), "ogg")
			defer audio.Put(want)
			bad := testutil.SetOggGranule(t, raw, len(testutil.OggPages(t, raw))-1, -1)
			med := openBytes(t, bad, "ogg")
			tr := med.Info().Default()
			med.Close()
			if tr.SamplesExact || !tr.SamplesAdvisory {
				t.Errorf("%d samples, exact %v, advisory %v: want advisory", tr.Samples, tr.SamplesExact, tr.SamplesAdvisory)
			}
			got, err := decodeAllDynamic(t, container.BytesSource(bad), "ogg")
			if err != nil {
				t.Fatal(err)
			}
			defer audio.Put(got)
			if got.N < want.N {
				t.Fatalf("delivered %d samples, the intact file %d", got.N, want.N)
			}
			if j := sameFrames(got, 0, want, 0, want.N); j >= 0 {
				t.Fatalf("differs from the intact file's decode at sample %d", j)
			}
		})
	}
}
