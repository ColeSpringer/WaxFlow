package mpa_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// decodeFrom reads raw through the whole stack from sample to the end and
// returns the first channel.
func decodeFrom(t *testing.T, raw []byte, sample int64) []float32 {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), "mp3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if sample > 0 {
		if landed, err := med.SeekSample(context.Background(), sample); err != nil || landed != sample {
			t.Fatalf("a seek to %d landed at %d: %v", sample, landed, err)
		}
	}
	dst := audio.Get(med.Info().Default().Fmt, 4096)
	defer audio.Put(dst)
	var out []float32
	for {
		err := med.ReadChunk(dst)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadChunk: %v", err)
		}
		out = append(out, dst.ChanF(0)[:dst.N]...)
	}
}

func id3v2Tag(n int) []byte {
	return append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, byte(n >> 7), byte(n & 0x7f)}, make([]byte, n)...)
}

func id3v1Tag() []byte {
	tag := make([]byte, 128)
	copy(tag, "TAGtitle")
	return tag
}

func apeTag(t *testing.T) []byte {
	t.Helper()
	b, err := apev2.Build([]apev2.Tag{{Key: "TITLE", Value: "a title long enough to reach past a frame header"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestJoinedFilesPlayThrough: two files with metadata frames, one after the
// other, are one run. The first header's counts end where the second file
// begins, so the length is both streams' frames and the second's metadata
// frame, less the first stream's delay at the front and the last stream's
// padding at the end. The open states it, a walk confirms it under Strict,
// and a read and a seek deliver it: the first stream's samples, then the
// second's from the frame behind its metadata frame, each as it decodes
// alone.
func TestJoinedFilesPlayThrough(t *testing.T) {
	for _, name := range []string{"sine-cbr128.mp3", "sine-vbr.mp3"} {
		raw := mp3Fixture(t, name)
		alone := decodeFrom(t, raw, 0)
		// 21 frames a stream, 1152 samples a frame, the trims the tag states.
		const frames, spf, delay, padding = 21, 1152, 1105, 1037
		if len(alone) != frames*spf-delay-padding {
			t.Fatalf("%s decodes to %d samples alone", name, len(alone))
		}
		want := int64(2*frames+1)*spf - delay - padding
		second := (frames + 1) * spf // where the second stream's audio starts
		for _, tc := range []struct {
			name  string
			raw   []byte
			notes int
		}{
			{"end to end", slices.Concat(raw, raw), 0},
			{"a tag between", slices.Concat(raw, id3v2Tag(37), raw), 1},
			{"tags behind", slices.Concat(raw, raw, apeTag(t), id3v1Tag()), 0},
		} {
			t.Run(name+" "+tc.name, func(t *testing.T) {
				d, err := mpaOpen(tc.raw, false)
				if err != nil {
					t.Fatal(err)
				}
				if tr := d.Tracks()[0]; tr.Samples != want || tr.Delay != delay || tr.Padding != padding {
					t.Errorf("the open states %d samples, trims %d/%d; want %d, %d/%d", tr.Samples, tr.Delay, tr.Padding, want, delay, padding)
				}
				track, ws, err := walked(t, tc.raw, true)
				if err != nil {
					t.Fatalf("a strict walk refused a joined file: %v", err)
				}
				if track.Samples != want || !track.SamplesExact {
					t.Errorf("a walk settled %d samples (exact %v), want %d", track.Samples, track.SamplesExact, want)
				}
				if got := findings(ws, container.Damage); len(got) != 0 {
					t.Errorf("damage = %v", got)
				}
				if got := findings(ws, container.Note); len(got) != tc.notes {
					t.Errorf("notes = %v, want %d", got, tc.notes)
				}
				if err := readToEnd(t, tc.raw, true); err != nil {
					t.Errorf("a strict read = %v", err)
				}
				got := decodeFrom(t, tc.raw, 0)
				if int64(len(got)) != want {
					t.Fatalf("a read delivered %d samples, want %d", len(got), want)
				}
				if !slices.Equal(got[:len(alone)], alone) {
					t.Error("the first stream's samples are not what it decodes to alone")
				}
				if !slices.Equal(got[second:], alone) {
					t.Error("the second stream's samples are not what it decodes to alone")
				}
				for _, from := range []int64{int64(len(alone)) - 100, int64(second) + 5000} {
					if !slices.Equal(decodeFrom(t, tc.raw, from), got[from:]) {
						t.Errorf("a read behind a seek to %d differs from the read from the start", from)
					}
				}
			})
		}
	}
}

// TestJoinedOntoFramesWithNoCount: frames joined on with no metadata frame
// of their own leave the end of the run unstated. The first stream's count
// and padding stop applying; its delay still opens the run, and every frame
// behind it plays, on a read as on a walk.
func TestJoinedOntoFramesWithNoCount(t *testing.T) {
	raw := mp3Fixture(t, "sine-cbr128.mp3")
	offs := frameOffsets(t, raw)
	joined := slices.Concat(raw, raw[offs[1]:])
	const frames, spf, delay = 21, 1152, 1105
	want := int64(2*frames*spf - delay)
	d, err := mpaOpen(joined, false)
	if err != nil {
		t.Fatal(err)
	}
	if tr := d.Tracks()[0]; tr.Samples != -1 || tr.Delay != delay || tr.Padding != 0 {
		t.Errorf("the open states %d samples, trims %d/%d; want no length and the front trim alone", tr.Samples, tr.Delay, tr.Padding)
	}
	track, ws, err := walked(t, joined, true)
	if err != nil || track.Samples != want || !track.SamplesExact || len(ws) != 0 {
		t.Errorf("a strict walk = %v, %d samples (exact %v), findings %v; want %d and none", err, track.Samples, track.SamplesExact, ws, want)
	}
	got := decodeFrom(t, joined, 0)
	if int64(len(got)) != want {
		t.Fatalf("a read delivered %d samples, want %d", len(got), want)
	}
	alone := decodeFrom(t, raw, 0)
	if !slices.Equal(got[:len(alone)], alone) {
		t.Error("the first stream's samples are not what it decodes to alone")
	}
	if from := int64(len(alone)) + 3000; !slices.Equal(decodeFrom(t, joined, from), got[from:]) {
		t.Error("a read behind a seek differs from the read from the start")
	}
}

// TestAByteCountInsideAFrameJoinsNothing: a byte count that ends on no frame
// is merely wrong, and the tag's frame count and trims bound the run as they
// do with a right one.
func TestAByteCountInsideAFrameJoinsNothing(t *testing.T) {
	raw := slices.Clone(mp3Fixture(t, "sine-cbr128.mp3"))
	offs := frameOffsets(t, raw)
	setByteCount(t, raw, int(offs[9])+17)
	track, ws, err := walked(t, raw, true)
	if err != nil || track.Samples != 22050 || track.Padding != 1037 || len(ws) != 0 {
		t.Errorf("a strict walk = %v, %d samples, padding %d, findings %v; want the tag's own 22050", err, track.Samples, track.Padding, ws)
	}
	if n := decodeLength(t, raw); n != 22050 {
		t.Errorf("a decode delivered %d, want 22050", n)
	}
}

// TestTruncatedFinalFrameBehindTags: a last frame cut short ends at the tags
// behind it and is dropped, as it is with nothing behind it: the same
// samples on a read and behind a seek, the same length and findings from a
// walk, and the same refusal under Strict. Bounded by the end of the file
// the frame fit again and decoded the tags' bytes as audio.
func TestTruncatedFinalFrameBehindTags(t *testing.T) {
	for _, name := range []string{"sine-untagged.mp3", "sine-cbr128.mp3"} {
		raw := mp3Fixture(t, name)
		cut := raw[:len(raw)-100]
		want := decodeFrom(t, cut, 0)
		wantTrack, wantFindings, err := walked(t, cut, false)
		if err != nil {
			t.Fatal(err)
		}
		for tail, tags := range map[string][]byte{
			"id3v1":        id3v1Tag(),
			"apev2, id3v1": slices.Concat(apeTag(t), id3v1Tag()),
		} {
			t.Run(name+" "+tail, func(t *testing.T) {
				tagged := slices.Concat(cut, tags)
				if !slices.Equal(decodeFrom(t, tagged, 0), want) {
					t.Error("a read delivers other samples than the cut stream alone")
				}
				if from := int64(len(want)) - 3000; !slices.Equal(decodeFrom(t, tagged, from), want[from:]) {
					t.Error("a read behind a seek delivers other samples than the cut stream alone")
				}
				track, ws, err := walked(t, tagged, false)
				if err != nil {
					t.Fatal(err)
				}
				if track.Samples != wantTrack.Samples || !track.SamplesExact || !slices.Equal(ws, wantFindings) {
					t.Errorf("a walk settled %d samples with %v, want %d with %v", track.Samples, ws, wantTrack.Samples, wantFindings)
				}
				if _, _, err := walked(t, tagged, true); !errors.Is(err, waxerr.ErrMalformedInput) {
					t.Errorf("a strict walk = %v, want the cut frame refused", err)
				}
				if err := readToEnd(t, tagged, true); !errors.Is(err, waxerr.ErrMalformedInput) {
					t.Errorf("a strict read = %v, want the cut frame refused", err)
				}
			})
		}
	}
}
