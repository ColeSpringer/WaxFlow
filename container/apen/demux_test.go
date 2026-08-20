package apen_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/apen"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// fixture loads a committed testdata file, from this package's own testdata or
// from the shared one.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for _, base := range []string{filepath.Join(dir, "testdata"), filepath.Join(dir, "..", "..", "testdata")} {
		if raw, err := os.ReadFile(filepath.Join(base, name)); err == nil {
			return raw
		}
	}
	t.Fatalf("fixture %s not found", name)
	return nil
}

func open(t *testing.T, raw []byte) *apen.Demuxer {
	t.Helper()
	d, err := apen.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// walk reads every packet, asserting the stream is contiguous: each frame
// starts where the previous one ended.
func walk(t *testing.T, d *apen.Demuxer) (frames int, samples int64) {
	t.Helper()
	var pkt container.Packet
	next := int64(0)
	for {
		err := d.ReadPacket(&pkt)
		if err == io.EOF {
			return frames, next
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.PTS != next {
			t.Fatalf("frame %d starts at %d, want %d", frames, pkt.PTS, next)
		}
		if pkt.Dur <= 0 || len(pkt.Data) <= ape.FrameHeaderLen || !pkt.Sync {
			t.Fatalf("bad packet: dur=%d len=%d sync=%v", pkt.Dur, len(pkt.Data), pkt.Sync)
		}
		blocks, skip, _, err := ape.ParseFrameHeader(pkt.Data)
		if err != nil {
			t.Fatalf("frame %d header: %v", frames, err)
		}
		if int64(blocks) != pkt.Dur {
			t.Fatalf("frame %d: header says %d blocks, packet says %d", frames, blocks, pkt.Dur)
		}
		if skip < 0 || skip > 3 {
			t.Fatalf("frame %d: alignment skip %d", frames, skip)
		}
		next += pkt.Dur
		frames++
	}
}

func TestDemuxPackets(t *testing.T) {
	d := open(t, fixture(t, "sine-s16.ape"))
	tr := d.Tracks()[0]
	if tr.Codec != codec.APE || tr.Fmt.Rate != 44100 || tr.Fmt.Channels != 2 || tr.Fmt.BitDepth != 16 {
		t.Fatalf("track = %+v", tr)
	}
	if !tr.Default || !tr.SamplesExact {
		t.Errorf("track flags: default=%v exact=%v", tr.Default, tr.SamplesExact)
	}
	frames, samples := walk(t, d)
	if frames != 1 {
		t.Errorf("frames = %d, want 1 for a fixture shorter than one frame", frames)
	}
	if samples != tr.Samples {
		t.Errorf("packets cover %d samples, track declares %d", samples, tr.Samples)
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("warnings on a clean file: %v", d.Warnings())
	}
}

// TestDemuxMultiFrame walks a file with several frames, which is what proves
// the seek table drives the extents rather than the file's ends.
func TestDemuxMultiFrame(t *testing.T) {
	d := open(t, fixture(t, "seek.ape"))
	tr := d.Tracks()[0]
	frames, samples := walk(t, d)
	if frames < 3 {
		t.Fatalf("frames = %d, want several", frames)
	}
	if samples != tr.Samples {
		t.Errorf("packets cover %d samples, track declares %d", samples, tr.Samples)
	}
}

// TestSeekLandsOnAFrame checks the seek contract: the returned position is the
// start of the frame holding the target, never past it, and reading resumes
// there. A frame is the format's only sync point, so landing early is the
// correct answer and format.Media pre-rolls the rest.
func TestSeekLandsOnAFrame(t *testing.T) {
	raw := fixture(t, "seek.ape")
	d := open(t, raw)
	tr := d.Tracks()[0]
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 1, 73727, 73728, 73729, 100000, tr.Samples - 1, tr.Samples, tr.Samples + 1000} {
		got, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatalf("seek %d: %v", target, err)
		}
		if got%int64(h.BlocksPerFrame) != 0 {
			t.Errorf("seek %d landed at %d, not on a frame boundary", target, got)
		}
		want := min(target, tr.Samples-1) / int64(h.BlocksPerFrame) * int64(h.BlocksPerFrame)
		if got != want {
			t.Errorf("seek %d landed at %d, want %d", target, got, want)
		}
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatalf("read after seek %d: %v", target, err)
		}
		if pkt.PTS != got {
			t.Errorf("seek %d: next packet at %d, seek reported %d", target, pkt.PTS, got)
		}
	}
	if _, err := d.SeekSample(1, 0); err == nil {
		t.Error("seek on a track that does not exist was accepted")
	}
	if _, err := d.SeekSample(0, -1); err == nil {
		t.Error("negative seek target was accepted")
	}
}

// TestSeekDecodesTheRightSamples is the seek check that matters: after a seek
// the samples that come out are the ones at that position. The fixture is a
// ramp, so every sample says where it is.
func TestSeekDecodesTheRightSamples(t *testing.T) {
	raw := fixture(t, "seek.ape")
	d := open(t, raw)
	tr := d.Tracks()[0]
	cfg, err := ape.ParseConfig(tr.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := ape.NewDecoder(cfg, tr.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	for _, target := range []int64{0, 73728, 147456} {
		at, err := d.SeekSample(0, target)
		if err != nil {
			t.Fatal(err)
		}
		dec.Reset()
		var pkt container.Packet
		if err := d.ReadPacket(&pkt); err != nil {
			t.Fatal(err)
		}
		checked := false
		err = dec.Decode(pkt.Data, func(b *audio.Buffer) error {
			if checked {
				return nil
			}
			checked = true
			for i := range min(b.N, 8) {
				want := testutil.RampAtI(tr.Fmt, 0, at+int64(i))
				if got := b.ChanI(0)[i]; got != want {
					t.Errorf("seek %d: sample %d = %d, want %d", target, at+int64(i), got, want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("decode after seek %d: %v", target, err)
		}
	}
}

// TestTags reads the APEv2 block the reference encoder appended, under the
// canonical keys the rest of the pipeline uses.
func TestTags(t *testing.T) {
	d := open(t, fixture(t, "tagged.ape"))
	tags := d.Tags()
	for key, want := range map[string]string{
		"ARTIST":        "Wax Test",
		"ALBUM":         "Fixtures",
		"TITLE":         "Tagged",
		"TRACKNUMBER":   "3",
		"RECORDINGDATE": "2026",
	} {
		if got := tags[key]; len(got) != 1 || got[0] != want {
			t.Errorf("tag %s = %q, want [%q]", key, got, want)
		}
	}
	if len(tags) == 0 {
		t.Fatal("no tags")
	}
	// The tag sits after the audio; the frames must not run into it.
	frames, samples := walk(t, d)
	if frames == 0 || samples != d.Tracks()[0].Samples {
		t.Errorf("tagged file: %d frames covering %d samples, track declares %d",
			frames, samples, d.Tracks()[0].Samples)
	}
	if len(d.Warnings()) != 0 {
		t.Errorf("a tag after the audio is the normal shape, not damage: %v", d.Warnings())
	}
}

// TestTrailingTagIsNotAudio proves the trailer peel is load-bearing rather
// than cosmetic: with the tag left in place the last frame would run past the
// audio, so a demuxer that skipped the peel would hand the decoder bytes that
// are not its own.
func TestTrailingTagIsNotAudio(t *testing.T) {
	raw := fixture(t, "tagged.ape")
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	audioEnd := h.FrameDataOffset + h.FrameDataBytes
	if audioEnd >= int64(len(raw)) {
		t.Fatalf("the tagged fixture has no trailer: audio ends at %d of %d bytes", audioEnd, len(raw))
	}
	// The reference writes a footer and no header, so the block's magic is at
	// the file's end and its declared length reaches back to exactly here.
	if string(raw[len(raw)-32:len(raw)-24]) != "APETAGEX" {
		t.Fatalf("the tagged fixture's trailer is not an APEv2 tag")
	}
	tagBytes := int64(binary.LittleEndian.Uint32(raw[len(raw)-20:]))
	if int64(len(raw))-tagBytes != audioEnd {
		t.Fatalf("the %d-byte tag starts at %d, but the audio ends at %d",
			tagBytes, int64(len(raw))-tagBytes, audioEnd)
	}
}

// TestLeadingID3v2Tag covers a file a tagger put an ID3v2 block in front of:
// the reference scans past one, and so must the header search.
func TestLeadingID3v2Tag(t *testing.T) {
	raw := fixture(t, "sine-s16.ape")
	tag := make([]byte, 10+64)
	copy(tag, "ID3\x04\x00\x00")
	tag[9] = 64
	d := open(t, append(tag, raw...))
	if _, samples := walk(t, d); samples != d.Tracks()[0].Samples {
		t.Errorf("tagged file covers %d samples, track declares %d", samples, d.Tracks()[0].Samples)
	}
}

// TestRefusals pins the files the demuxer turns away rather than half-reads.
func TestRefusals(t *testing.T) {
	raw := fixture(t, "sine-s16.ape")
	h, err := ape.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() []byte{
		"not an APE file at all": func() []byte { return []byte("RIFF....WAVEfmt ") },
		"empty":                  func() []byte { return nil },
		"truncated header": func() []byte {
			return append([]byte(nil), raw[:40]...)
		},
		"seek table disagrees with the header": func() []byte {
			out := append([]byte(nil), raw...)
			binary.LittleEndian.PutUint32(out[h.SeekTableOffset:], uint32(h.FrameDataOffset+4))
			return out
		},
		"frame runs past the file": func() []byte {
			out := append([]byte(nil), raw...)
			binary.LittleEndian.PutUint32(out[h.SeekTableOffset:], uint32(len(raw)+1000))
			return out
		},
		"truncated seek table": func() []byte {
			out := append([]byte(nil), raw[:h.SeekTableOffset+2]...)
			return out
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := apen.NewDemuxer(container.BytesSource(mutate()), nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat && code != waxerr.CodeSourceUnreadable {
				t.Errorf("code = %v", code)
			}
		})
	}
}

// TestTruncatedTailIsReported is the finding a clean parse would otherwise
// hide: a file whose audio was cut short still has a valid header and a valid
// seek table, and the last frame is simply clamped to whatever bytes remain.
// The descriptor's own byte count is the only thing that disagrees, so it is
// what the warning is built on, and strict mode turns it into a refusal rather
// than letting a stream die mid-body.
func TestTruncatedTailIsReported(t *testing.T) {
	full := fixture(t, "seek.ape")
	for _, cut := range []int{8, 1000} {
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			raw := full[:len(full)-cut]
			d := open(t, raw)
			if len(d.Warnings()) == 0 {
				t.Fatal("a truncated file parsed clean")
			}
			if !strings.Contains(d.Warnings()[0].Msg, "bytes of frame data") {
				t.Errorf("warning = %q", d.Warnings()[0].Msg)
			}
			if _, err := apen.NewDemuxer(container.BytesSource(raw), &apen.DemuxerOptions{Strict: true}); err == nil {
				t.Error("strict mode accepted a truncated file")
			}
		})
	}
	// The intact file must stay silent, or the check above proves nothing.
	if w := open(t, full).Warnings(); len(w) != 0 {
		t.Errorf("intact file warns: %v", w)
	}
}

// TestPaddedTailIsReported is the other direction: bytes after the audio that
// no trailer peel recognizes.
func TestPaddedTailIsReported(t *testing.T) {
	raw := append(append([]byte(nil), fixture(t, "sine-s16.ape")...), make([]byte, 64)...)
	d := open(t, raw)
	if len(d.Warnings()) == 0 {
		t.Fatal("trailing padding parsed clean")
	}
	// The frames themselves must still be intact.
	if _, samples := walk(t, d); samples != d.Tracks()[0].Samples {
		t.Errorf("padded file covers %d samples, track declares %d", samples, d.Tracks()[0].Samples)
	}
}

// TestAppendedID3v2IsNotAudio covers the trailer flacn already peels: an
// ID3v2 tag written after the audio, which is findable from behind only
// because it carries a footer.
func TestAppendedID3v2IsNotAudio(t *testing.T) {
	tag := make([]byte, 20+48)
	copy(tag, "ID3")
	tag[3], tag[5] = 4, 0x10 // version 4, footer present
	tag[9] = 48
	copy(tag[len(tag)-10:], "3DI")
	tag[len(tag)-7], tag[len(tag)-5] = 4, 0x10
	tag[len(tag)-1] = 48
	raw := append(append([]byte(nil), fixture(t, "sine-s16.ape")...), tag...)
	d := open(t, raw)
	if w := d.Warnings(); len(w) != 0 {
		t.Errorf("an appended ID3v2 tag is a trailer, not damage: %v", w)
	}
	if _, samples := walk(t, d); samples != d.Tracks()[0].Samples {
		t.Errorf("tagged file covers %d samples, track declares %d", samples, d.Tracks()[0].Samples)
	}
}

// TestHeaderScanCoversTheJunkItClaims pins the search past leading junk. The
// magic's first three bytes are an ordinary word, so the scan looks for all
// four: junk full of "MAC" that is never a magic must cost nothing, and the
// scan must still reach a header most of a megabyte in.
func TestHeaderScanCoversTheJunkItClaims(t *testing.T) {
	body := fixture(t, "sine-s16.ape")
	// "MACARONI" and "MACHINE" start with the magic's three bytes and are not
	// it; a scan that searched three would try every one of them.
	noise := bytes.Repeat([]byte("MACARONI MACHINE MACRO "), 1+(600<<10)/23)
	for _, junk := range []int{1, 4093, 600 << 10} {
		t.Run(fmt.Sprintf("junk-%d", junk), func(t *testing.T) {
			raw := append(append([]byte(nil), noise[:junk]...), body...)
			d := open(t, raw)
			if _, samples := walk(t, d); samples != d.Tracks()[0].Samples {
				t.Errorf("covers %d samples, track declares %d", samples, d.Tracks()[0].Samples)
			}
		})
	}
	// A real "MAC " ahead of the file is a candidate, and a header that fails
	// to parse must not end the search.
	decoys := bytes.Repeat([]byte{'M', 'A', 'C', ' ', 0, 0, 0, 0}, 200)
	d := open(t, append(decoys, body...))
	if _, samples := walk(t, d); samples != d.Tracks()[0].Samples {
		t.Errorf("past decoys: covers %d samples, track declares %d", samples, d.Tracks()[0].Samples)
	}
	// Past the scanned span the file is refused, and the message names what
	// was actually scanned rather than a number nothing reached.
	_, err := apen.NewDemuxer(container.BytesSource(append(make([]byte, 2<<20), body...)), nil)
	if err == nil {
		t.Fatal("a header two megabytes in was found")
	}
	if !strings.Contains(err.Error(), "1048576 bytes") {
		t.Errorf("error %q does not name the megabyte it scanned", err)
	}
}

// TestHeaderScanTerminatesOnAShortFile pins the scan's step: a window shorter
// than the magic must end it, since the step is derived from the window size
// and a window of exactly the magic's length would otherwise advance zero
// bytes forever.
func TestHeaderScanTerminatesOnAShortFile(t *testing.T) {
	for n := range 32 {
		if _, err := apen.NewDemuxer(container.BytesSource(bytes.Repeat([]byte("MAC"), 1+n/3)[:n]), nil); err == nil {
			t.Errorf("%d bytes accepted", n)
		}
	}
}

// TestErrorPrefix pins the message prefix to the registry's container name:
// the package is apen so the registry can import codec/ape beside it, but
// nothing a user sees says so.
func TestErrorPrefix(t *testing.T) {
	_, err := apen.NewDemuxer(container.BytesSource([]byte("MAC not really")), nil)
	if err == nil {
		t.Fatal("accepted")
	}
	if got := err.Error(); len(got) < 5 || got[:5] != "ape: " {
		t.Errorf("error %q does not start with the container name", got)
	}
}

// TestEOFIsTheSentinel pins the demuxer contract: the end of the stream is the
// bare io.EOF, not a wrapped one, because consumers compare with ==.
func TestEOFIsTheSentinel(t *testing.T) {
	d := open(t, fixture(t, "sine-s16.ape"))
	var pkt container.Packet
	for {
		err := d.ReadPacket(&pkt)
		if err == nil {
			continue
		}
		if err != io.EOF {
			t.Fatalf("end of stream = %v, want the bare io.EOF", err)
		}
		if !errors.Is(err, io.EOF) {
			t.Fatal("io.EOF is not io.EOF")
		}
		return
	}
}
