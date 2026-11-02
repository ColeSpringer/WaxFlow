package waxflow_test

// The copy rung writes a conforming FLAC, frames numbered from 0 and no MD5
// signature, from a source whose first frame is numbered past 0: into native
// FLAC, Ogg FLAC and Matroska, and into fMP4 segments numbered across their
// boundaries. libFLAC accepts the native and Ogg copies and seeks them.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// flacCopy is a native FLAC for the copy rung and the encode its audio came
// from, which it holds from sample skip on.
type flacCopy struct {
	name      string
	src, twin []byte
	skip      int64
}

// signedTranscode is transcodeBytes into a destination the muxer can seek, so
// a FLAC's STREAMINFO carries the encoder's MD5 signature.
func signedTranscode(t *testing.T, wav []byte, opts waxflow.TranscodeOptions) []byte {
	t.Helper()
	ws := &memWS{}
	if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(wav), "wav", ws, opts); err != nil {
		t.Fatal(err)
	}
	if si := nativeStreamInfo(t, ws.Buf); si.MD5 == ([16]byte{}) {
		t.Fatal("the encode carries no MD5 signature")
	}
	return ws.Buf
}

// noiseFLAC is seconds of full-scale 24-bit noise on six channels at 96 kHz,
// this tree's signed encode of it: frames of over 73000 bytes, more than an
// Ogg page holds.
func noiseFLAC(t *testing.T, seconds int) []byte {
	t.Helper()
	f := audio.Format{Rate: 96000, Channels: 6, Layout: audio.DefaultLayout(6), Type: audio.Int, BitDepth: 24}
	buf := audio.Get(f, seconds*f.Rate)
	defer audio.Put(buf)
	buf.N = seconds * f.Rate
	state := uint64(7)
	for c := range f.Channels {
		for i := range buf.ChanI(c) {
			state = state*6364136223846793005 + 1442695040888963407
			buf.ChanI(c)[i] = int32(uint32(state>>32)) >> 8
		}
	}
	return signedTranscode(t, wavFromBuffer(t, f, buf), waxflow.TranscodeOptions{Format: "flac"})
}

// sineFLAC is this tree's signed encode of testdata/sine-s16.wav looped to
// seconds.
func sineFLAC(t *testing.T, seconds int) []byte {
	t.Helper()
	return signedTranscode(t, loopedWAV(t, seconds), waxflow.TranscodeOptions{Format: "flac"})
}

// copyFLAC runs src, a native FLAC, through the copy rung into the named
// container ("" for native FLAC), asserting the rung was taken, into a
// destination it can seek, so the copy is fully declared.
func copyFLAC(t *testing.T, src []byte, into string) []byte {
	t.Helper()
	ws := &memWS{}
	remuxFLAC(t, src, into, ws)
	return ws.Buf
}

// streamFLAC is copyFLAC into a destination that cannot seek, so the copy's
// headers are what it wrote before its first frame.
func streamFLAC(t *testing.T, src []byte, into string) []byte {
	t.Helper()
	var out bytes.Buffer
	remuxFLAC(t, src, into, &out)
	return out.Bytes()
}

func remuxFLAC(t *testing.T, src []byte, into string, dst io.Writer) {
	t.Helper()
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac", Container: into}
	if plan, err := e.PlanRemux(probeTrack(t, src, "flac"), opts); err != nil || plan == nil {
		t.Fatalf("the copy rung declined (%v)", err)
	}
	if _, err := e.Remux(t.Context(), container.BytesSource(src), "flac", dst, opts); err != nil {
		t.Fatal(err)
	}
}

// trackStreamInfo is the STREAMINFO raw's track carries, in the container hint
// names.
func trackStreamInfo(t *testing.T, raw []byte, hint string) flac.StreamInfo {
	t.Helper()
	med := openBytes(t, raw, hint)
	defer med.Close()
	si, err := flac.ParseStreamInfo(med.Info().Default().CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	return si
}

// nativeStreamInfo is a native FLAC's own STREAMINFO block, read off the file.
func nativeStreamInfo(t *testing.T, raw []byte) flac.StreamInfo {
	t.Helper()
	si, _, _ := nativeLayout(t, raw)
	return si
}

// frameStart is where a frame starts by its own number, under num.
func frameStart(t *testing.T, num flac.Numbering, frame []byte) (flac.FrameInfo, int64) {
	t.Helper()
	fi, err := flac.ParseFrameHeader(frame)
	if err != nil {
		t.Fatalf("a frame header does not parse: %v", err)
	}
	return fi, num.Start(fi)
}

// requireFramesFromZero holds frames, a stream's in order, to RFC 9639's
// numbering: from 0 under the stream's own blocking strategy, each frame
// number one past the last or each sample number where the last frame ended.
func requireFramesFromZero(t *testing.T, si flac.StreamInfo, frames [][]byte) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	first, err := flac.ParseFrameHeader(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	num := si.Numbering(first)
	var want uint64
	for i, frame := range frames {
		fi, _ := frameStart(t, num, frame)
		if fi.Coded != want {
			t.Fatalf("frame %d carries %d, want %d (sample coded %v)", i, fi.Coded, want, num.SampleCoded)
		}
		want++
		if num.SampleCoded {
			want = fi.Coded + uint64(fi.BlockSize)
		}
	}
}

// nativeLayout reads a native FLAC's STREAMINFO, its SEEKTABLE when it has
// one, and where its first frame begins.
func nativeLayout(t *testing.T, raw []byte) (si flac.StreamInfo, table []byte, first int) {
	t.Helper()
	off := 4
	for last := false; !last; {
		last = raw[off]&0x80 != 0
		n := int(raw[off+1])<<16 | int(raw[off+2])<<8 | int(raw[off+3])
		switch raw[off] & 0x7F {
		case 0:
			var err error
			if si, err = flac.ParseStreamInfo(raw[off+4 : off+4+n]); err != nil {
				t.Fatal(err)
			}
		case 3:
			table = raw[off+4 : off+4+n]
		}
		off += 4 + n
	}
	return si, table, off
}

// requireSeekTable holds a native FLAC's SEEKTABLE to its frames: every point
// is the offset of a frame whose own number starts it at the point's sample,
// holding the point's sample count.
func requireSeekTable(t *testing.T, raw []byte) {
	t.Helper()
	si, table, first := nativeLayout(t, raw)
	if len(table) == 0 {
		t.Fatal("the copy carries no SEEKTABLE")
	}
	fi, err := flac.ParseFrameHeader(raw[first:])
	if err != nil {
		t.Fatal(err)
	}
	num := si.Numbering(fi)
	points := 0
	for p := table; len(p) >= 18; p = p[18:] {
		sample := binary.BigEndian.Uint64(p)
		if sample == math.MaxUint64 {
			continue // a placeholder
		}
		at := first + int(binary.BigEndian.Uint64(p[8:]))
		if at >= len(raw) {
			t.Fatalf("the point for sample %d is past the stream", sample)
		}
		fi, start := frameStart(t, num, raw[at:])
		if start != int64(sample) || fi.BlockSize != int(binary.BigEndian.Uint16(p[16:])) {
			t.Errorf("the point for sample %d lands on a frame of %d samples at %d", sample, fi.BlockSize, start)
		}
		points++
	}
	if points == 0 {
		t.Error("the SEEKTABLE holds no point")
	}
}

// requireOggGranules holds an Ogg FLAC's pages to its frames: a page states
// where the last frame completing on it ends, by that frame's own number, and
// a page that completes no frame states -1.
func requireOggGranules(t *testing.T, raw []byte) {
	t.Helper()
	var (
		packets [][]byte
		cur     []byte
	)
	pages := testutil.OggPages(t, raw)
	ends := make([]int, len(pages)) // the packets completed through each page
	for i, p := range pages {
		at := 0
		for _, l := range p.Lacing {
			cur = append(cur, p.Body[at:at+int(l)]...)
			at += int(l)
			if l < 255 {
				packets, cur = append(packets, cur), nil
			}
		}
		ends[i] = len(packets)
	}
	if len(packets) < 3 {
		t.Fatalf("%d packets", len(packets))
	}
	si, err := flac.ParseStreamInfo(packets[0][17 : 17+flac.StreamInfoLen])
	if err != nil {
		t.Fatal(err)
	}
	first, err := flac.ParseFrameHeader(packets[2])
	if err != nil {
		t.Fatal(err)
	}
	num := si.Numbering(first)
	big := 0
	for i, p := range pages {
		if i < 2 {
			continue // the identification and comment headers
		}
		want := int64(-1)
		if ends[i] > ends[i-1] {
			fi, start := frameStart(t, num, packets[ends[i]-1])
			want = start + int64(fi.BlockSize)
		} else {
			big++
		}
		if p.Granule != want {
			t.Fatalf("page %d states granule %d, its frames end at %d", i, p.Granule, want)
		}
	}
	t.Logf("%d of %d pages complete no frame", big, len(pages))
}

// requireSamples holds got, interleaved and left-justified as ffmpeg's s32
// decode is, to want's frames from off on.
func requireSamples(t *testing.T, got []int32, want *audio.Buffer, off int) {
	t.Helper()
	ch, shift := want.Fmt.Channels, 32-want.Fmt.BitDepth
	if len(got)%ch != 0 || off+len(got)/ch > want.N {
		t.Fatalf("%d samples at frame %d do not fit %d frames of %d channels", len(got), off, want.N, ch)
	}
	for i, v := range got {
		if w := want.ChanI(i % ch)[off+i/ch] << shift; v != w {
			t.Fatalf("frame %d channel %d is %d, want %d", off+i/ch, i%ch, v, w)
		}
	}
}

// requireLibFLAC has libFLAC test path and decode spans of it by seeking, each
// held to want, the samples the stream holds.
func requireLibFLAC(t *testing.T, path string, ogg bool, want *audio.Buffer) {
	t.Helper()
	tool := testutil.FlacTool(t)
	args := []string{"-s"}
	if ogg {
		args = append(args, "--ogg")
	}
	if out, err := exec.Command(tool, append(append([]string{"-t"}, args...), path)...).CombinedOutput(); err != nil {
		t.Fatalf("flac -t refused the copy: %v\n%s", err, out)
	}
	total := int64(want.N)
	width := (want.Fmt.BitDepth + 7) / 8
	for _, span := range [][2]int64{{1000, 5000}, {total/2 + 7, total/2 + 20007}, {total - 7000, total - 100}} {
		cmd := exec.Command(tool, append(append([]string{"-d", "-c", "--force-raw-format", "--endian=little",
			"--sign=signed", fmt.Sprintf("--skip=%d", span[0]), fmt.Sprintf("--until=%d", span[1])}, args...), path)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("flac -d --skip=%d --until=%d: %v\n%s", span[0], span[1], err, stderr.Bytes())
		}
		if len(raw)%width != 0 || int64(len(raw)/width) != (span[1]-span[0])*int64(want.Fmt.Channels) {
			t.Fatalf("flac -d --skip=%d --until=%d wrote %d bytes", span[0], span[1], len(raw))
		}
		got := make([]int32, len(raw)/width)
		for i := range got {
			var v uint32
			for b := range width {
				v |= uint32(raw[i*width+b]) << (8 * (4 - width + b))
			}
			got[i] = int32(v)
		}
		requireSamples(t, got, want, int(span[0]))
	}
}

// requireFLACCopies copies c.src into native FLAC, Ogg FLAC and Matroska, and
// into fMP4 segments, and holds each copy to RFC 9639's numbering and to the
// audio c.twin holds from c.skip: its frames run from 0, its STREAMINFO says
// its MD5 is not computed, its SEEKTABLE or its granules agree with its
// frames, this tree reads and seeks it as the twin (requireFLACTwin), ffmpeg
// decodes it, and libFLAC accepts the native and Ogg copies and seeks them.
func requireFLACCopies(t *testing.T, c flacCopy) {
	t.Helper()
	want := twinFrom(t, c)
	defer audio.Put(want)
	for _, into := range []string{"", "ogg", "mka"} {
		hint := map[string]string{"": "flac", "ogg": "oga", "mka": "mka"}[into]
		t.Run(hint, func(t *testing.T) {
			out := copyFLAC(t, c.src, into)
			si := trackStreamInfo(t, out, hint)
			if si.MD5 != ([16]byte{}) {
				t.Errorf("the copy carries the MD5 %x, its source's parent's; want none", si.MD5)
			}
			requireFramesFromZero(t, si, payloads(t, out, hint))
			switch into {
			case "":
				requireSeekTable(t, out)
			case "ogg":
				requireOggGranules(t, out)
			}
			requireFLACTwin(t, out, hint, c.twin, c.skip)
			path := filepath.Join(t.TempDir(), "copy."+hint)
			if err := os.WriteFile(path, out, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Run("ffmpeg", func(t *testing.T) { requireFFmpegDecode(t, path, want) })
			if into != "mka" {
				t.Run("libflac", func(t *testing.T) { requireLibFLAC(t, path, into == "ogg", want) })
			}
		})
	}
	t.Run("segmented", func(t *testing.T) { requireSegmentedFLACCopy(t, c, want) })
}

// twinFrom is this tree's decode of c.twin from c.skip on: the audio a copy
// of c.src holds.
func twinFrom(t *testing.T, c flacCopy) *audio.Buffer {
	t.Helper()
	twin, err := decodeAllDynamic(t, container.BytesSource(c.twin), "flac")
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Put(twin)
	want := audio.Get(twin.Fmt, twin.N-int(c.skip))
	want.N = twin.N - int(c.skip)
	audio.CopyFrames(want, 0, twin, int(c.skip), want.N)
	return want
}

// requireFFmpegDecode holds ffmpeg's decode of path to want.
func requireFFmpegDecode(t *testing.T, path string, want *audio.Buffer) {
	t.Helper()
	dec := testutil.FFmpegDecodeS32(t, path)
	if len(dec) != want.N*want.Fmt.Channels {
		t.Fatalf("ffmpeg decodes %d samples, want %d", len(dec), want.N*want.Fmt.Channels)
	}
	requireSamples(t, dec, want, 0)
}

// requireSegmentedFLACCopy runs c.src through the segmented copy rung, from
// its first segment and again from a later one, and holds the presentation to
// RFC 9639's numbering: its frames run from 0 across segment boundaries, and a
// run from a later segment numbers its first frame where it is, so its
// segments are the continuous run's. This tree and ffmpeg decode the whole as
// want.
func requireSegmentedFLACCopy(t *testing.T, c flacCopy, want *audio.Buffer) {
	t.Helper()
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	grid, err := e.PacketGrid(container.BytesSource(c.src), "flac")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanRemuxSegments(probeTrack(t, c.src, "flac"), opts, 0.5, grid)
	if err != nil || plan == nil {
		t.Fatalf("the segmented copy rung declined (%v)", err)
	}
	init, err := e.RemuxInitSegment(plan)
	if err != nil {
		t.Fatal(err)
	}
	full := collectRemuxSegments(t, e, c.src, "flac", opts, plan.SegmentSamples, 0)
	if len(full) < 3 {
		t.Fatalf("%d segments; need several to restart into", len(full))
	}
	whole := bytes.Clone(init)
	for _, s := range full {
		whole = append(whole, s.Data...)
	}
	si := trackStreamInfo(t, whole, "mp4")
	requireFramesFromZero(t, si, payloads(t, whole, "mp4"))

	k := int64(len(full) / 2)
	tail := collectRemuxSegments(t, e, c.src, "flac", opts, plan.SegmentSamples, k)
	first := payloads(t, append(bytes.Clone(init), tail[0].Data...), "mp4")[0]
	fi, err := flac.ParseFrameHeader(first)
	if err != nil {
		t.Fatal(err)
	}
	at := k * int64(plan.SegmentSamples)
	if num := si.Numbering(fi); num.Start(fi) != at {
		t.Errorf("segment %d's first frame carries %d, which starts it at %d, not %d", k, fi.Coded, num.Start(fi), at)
	}
	for i, s := range tail {
		if !bytes.Equal(s.Data, full[int(k)+i].Data) {
			t.Fatalf("segment %d from a run started there differs from the continuous run's", s.Index)
		}
	}

	requireFLACTwin(t, whole, "mp4", c.twin, c.skip)
	path := filepath.Join(t.TempDir(), "copy.mp4")
	if err := os.WriteFile(path, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("ffmpeg", func(t *testing.T) { requireFFmpegDecode(t, path, want) })
}

// TestFLACCopyNumbersFramesFromZero: a copy of a FLAC numbered from past 0,
// under either blocking strategy, and of one whose frames are larger than an
// Ogg page, is numbered from 0 and carries no MD5.
func TestFLACCopyNumbersFramesFromZero(t *testing.T) {
	twin := sineFLAC(t, 30)
	noise := noiseFLAC(t, 2)
	for _, c := range []flacCopy{
		{"fixed", testutil.RenumberFLACFrames(t, twin, 1000), twin, 0},
		{"variable", testutil.VariableFLACFrames(t, twin, 1000*4096), twin, 0},
		{"frames past a page", testutil.RenumberFLACFrames(t, noise, 1000), noise, 0},
	} {
		t.Run(c.name, func(t *testing.T) { requireFLACCopies(t, c) })
	}
}

// TestFLACCopyOfAStreamCopyCut: ffmpeg's stream-copy cut keeps its source's
// frame numbers and MD5; a copy of it is numbered from 0, holds the source's
// audio from the cut on, and says its MD5 is not computed.
func TestFLACCopyOfAStreamCopyCut(t *testing.T) {
	ffmpeg := testutil.FFmpeg(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		twin []byte
		at   string
	}{
		{"sine", sineFLAC(t, 30), "10"},
		{"frames past a page", noiseFLAC(t, 3), "1.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, cut := filepath.Join(dir, tc.name+".flac"), filepath.Join(dir, tc.name+"-cut.flac")
			if err := os.WriteFile(src, tc.twin, 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-ss", tc.at, "-i", src, "-c", "copy", cut).CombinedOutput()
			if err != nil {
				t.Fatalf("ffmpeg: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(cut)
			if err != nil {
				t.Fatal(err)
			}
			skip := flacFirstFrame(t, raw)
			if skip == 0 {
				t.Fatal("ffmpeg's cut starts at frame 0; there is nothing to test")
			}
			requireFLACCopies(t, flacCopy{tc.name, raw, tc.twin, skip})
		})
	}
}

// TestFLACCopyFromZeroIsUnchanged pins the common case: a stream numbered from
// 0 already copies frame for frame, byte for byte, and keeps its MD5 signature
// in every copy whose header is patched at End; one whose header is final once
// written states none (TestFLACCopySignsOnlyWhatItCopied). An unsigned stream
// copied into a destination that cannot seek is the source itself.
func TestFLACCopyFromZeroIsUnchanged(t *testing.T) {
	for _, src := range [][]byte{
		transcodeBytes(t, loopedWAV(t, 30), waxflow.TranscodeOptions{Format: "flac"}),
		testutil.VariableFLACFrames(t, transcodeBytes(t, loopedWAV(t, 5), waxflow.TranscodeOptions{Format: "flac"}), 0),
		sineFLAC(t, 5),
		noiseFLAC(t, 1),
	} {
		want := payloads(t, src, "flac")
		md5 := nativeStreamInfo(t, src).MD5
		for _, tc := range []struct{ into, hint string }{{"", "flac"}, {"ogg", "oga"}, {"mka", "mka"}} {
			for i, out := range [][]byte{copyFLAC(t, src, tc.into), streamFLAC(t, src, tc.into)} {
				got := payloads(t, out, tc.hint)
				if len(got) != len(want) {
					t.Fatalf("%s: %d frames copied of %d", tc.hint, len(got), len(want))
				}
				for k := range want {
					if !bytes.Equal(got[k], want[k]) {
						t.Fatalf("%s: frame %d changed", tc.hint, k)
					}
				}
				want := md5
				if i == 1 {
					want = [16]byte{}
				}
				if si := trackStreamInfo(t, out, tc.hint); si.MD5 != want {
					t.Errorf("%s: the copy carries the MD5 %x, want %x", tc.hint, si.MD5, want)
				}
			}
		}
		e := waxflow.New()
		grid, err := e.PacketGrid(container.BytesSource(src), "flac")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := e.PlanRemuxSegments(probeTrack(t, src, "flac"), waxflow.TranscodeOptions{Format: "flac"}, 0.5, grid)
		if err != nil || plan == nil {
			t.Fatalf("the segmented copy rung declined (%v)", err)
		}
		init, err := e.RemuxInitSegment(plan)
		if err != nil {
			t.Fatal(err)
		}
		whole := bytes.Clone(init)
		for _, seg := range collectRemuxSegments(t, e, src, "flac", waxflow.TranscodeOptions{Format: "flac"}, plan.SegmentSamples, 0) {
			whole = append(whole, seg.Data...)
		}
		if got := payloads(t, whole, "mp4"); len(got) != len(want) || !bytes.Equal(bytes.Join(got, nil), bytes.Join(want, nil)) {
			t.Errorf("the segmented copy's %d frames are not its source's %d", len(got), len(want))
		}
		if si := trackStreamInfo(t, whole, "mp4"); si.MD5 != ([16]byte{}) {
			t.Errorf("the segmented copy carries the MD5 %x, which no segment can vouch for", si.MD5)
		}
		if bytes.Equal(md5[:], make([]byte, 16)) {
			if out := streamFLAC(t, src, ""); !bytes.Equal(out, src) {
				t.Errorf("the streamed copy is not its source: %d bytes, %d", len(out), len(src))
			}
		}
	}
}

// TestSegmentedFLACRestartNumbersByItsFrames: a fixed-blocksize FLAC whose
// STREAMINFO states 4096-sample blocks over 1152-sample frames is positioned
// by its frames, so a worker restarted at any segment numbers them as the
// continuous run does, at segment lengths that 4096 does and does not divide.
func TestSegmentedFLACRestartNumbersByItsFrames(t *testing.T) {
	raw := transcodeBytes(t, loopedWAV(t, 10), waxflow.TranscodeOptions{Format: "flac", FLACLevel: 1})
	if si := nativeStreamInfo(t, raw); si.MinBlock != 1152 || si.MaxBlock != 1152 {
		t.Fatalf("level 1 encodes %d..%d-sample blocks, want 1152", si.MinBlock, si.MaxBlock)
	}
	binary.BigEndian.PutUint16(raw[8:], 4096) // STREAMINFO's block bounds
	binary.BigEndian.PutUint16(raw[10:], 4096)
	e := waxflow.New()
	opts := waxflow.TranscodeOptions{Format: "flac"}
	track := probeTrack(t, raw, "flac")
	grid, err := e.PacketGrid(container.BytesSource(raw), "flac")
	if err != nil || grid != 1152 {
		t.Fatalf("grid %d (%v), want the frames' 1152", grid, err)
	}
	for _, seconds := range []float64{1.0, 36864.0 / 44100} {
		plan, err := e.PlanRemuxSegments(track, opts, seconds, grid)
		if err != nil || plan == nil {
			t.Fatalf("plan: %v (%v)", plan, err)
		}
		full := collectRemuxSegments(t, e, raw, "flac", opts, plan.SegmentSamples, 0)
		for start := 1; start < len(full); start++ {
			tail := collectRemuxSegments(t, e, raw, "flac", opts, plan.SegmentSamples, int64(start))
			for i, s := range tail {
				if !bytes.Equal(s.Data, full[start+i].Data) {
					t.Fatalf("%d-sample segments restarted at %d: segment %d differs from the continuous run's",
						plan.SegmentSamples, start, s.Index)
				}
			}
		}
	}
}

// TestFLACCopySignsOnlyWhatItCopied: a copy carries its source's MD5 only
// where it can vouch for it, a destination whose header it patches at End: a
// copy that filled a hole with silence, or one into a header written before
// its frames, states none, "not computed", rather than a signature its audio
// does not have.
func TestFLACCopySignsOnlyWhatItCopied(t *testing.T) {
	clean := sineFLAC(t, 6)
	signed := nativeStreamInfo(t, clean).MD5
	frames := payloads(t, clean, "flac")
	at := bytes.Index(clean, frames[len(frames)/2])
	bad := append([]byte(nil), clean...)
	for i := at + 40; i < at+56; i++ {
		bad[i] ^= 0xFF
	}
	hints := map[string]string{"": "flac", "ogg": "oga", "mka": "mka"}
	for _, into := range []string{"", "ogg", "mka"} {
		for _, tc := range []struct {
			name string
			copy func(t *testing.T, src []byte, into string) []byte
			src  []byte
			md5  [16]byte
		}{
			{"clean, patched", copyFLAC, clean, signed},
			{"clean, streamed", streamFLAC, clean, [16]byte{}},
			{"a hole, patched", copyFLAC, bad, [16]byte{}},
			{"a hole, streamed", streamFLAC, bad, [16]byte{}},
		} {
			t.Run(hints[into]+"/"+tc.name, func(t *testing.T) {
				if got := trackStreamInfo(t, tc.copy(t, tc.src, into), hints[into]).MD5; got != tc.md5 {
					t.Errorf("the copy states MD5 % x, want % x", got[:4], tc.md5[:4])
				}
			})
		}
	}
}
