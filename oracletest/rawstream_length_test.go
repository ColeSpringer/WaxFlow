package oracletest

// The length waxlabel reports for a raw stream against the one WaxFlow's own
// demuxer counts. On a mapper-wired route both readers speak for one file,
// and a consumer that takes its duration from the tag library and its
// samples from the engine sees the difference as a gap or a cut. waxlabel
// v1.9.0 trims the delay a LAME or Info header states and counts the frames
// of a mixed-bitrate MP3 with no such header, which is what the engine
// delivers; the rows are the raw shapes where the two have to count alike,
// tagged, cut short and joined ones among them. Where they do not, the row
// names the distance and its cause and fails when either side moves it.

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
)

// repoFile reads a committed fixture by its path from the repository root.
func repoFile(t *testing.T, path ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{".."}, path...)...))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// mp3Frames returns where each frame of an MP3 with no leading tag starts.
func mp3Frames(t *testing.T, raw []byte) []int {
	t.Helper()
	var offs []int
	for off := 0; off+mp3.HeaderLen <= len(raw); {
		h, err := mp3.ParseHeader(raw[off:])
		if err != nil || h.Size() == 0 || off+h.Size() > len(raw) {
			break
		}
		offs = append(offs, off)
		off += h.Size()
	}
	if len(offs) < 4 {
		t.Fatalf("found %d frames", len(offs))
	}
	return offs
}

// wvBlocks returns where each block of a WavPack stream starts, and the
// stream's end.
func wvBlocks(t *testing.T, raw []byte) (offs []int, end int) {
	t.Helper()
	for end+wavpack.BlockHeaderLen <= len(raw) && wavpack.SyncOK(raw[end:]) {
		h, err := wavpack.ParseBlockHeader(raw[end:])
		if err != nil || end+int(h.Size) > len(raw) {
			break
		}
		offs = append(offs, end)
		end += int(h.Size)
	}
	if len(offs) < 3 {
		t.Fatalf("found %d blocks", len(offs))
	}
	return offs, end
}

func TestWaxlabelAgreesRawStreamLength(t *testing.T) {
	stereo := audio.Format{Rate: 44100, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	v1 := make([]byte, 128)
	copy(v1, "TAGtitle")
	// An APEv2 tag of one item, footer only: version 2000, the size of the
	// item and the footer, one item, no flags.
	ape := []byte("\x01\x00\x00\x00\x00\x00\x00\x00Title\x00t")
	ape = append(ape, "APETAGEX"...)
	for _, v := range []uint32{2000, uint32(len(ape) - 8 + 32), 1, 0, 0, 0} {
		ape = binary.LittleEndian.AppendUint32(ape, v)
	}

	vbr := repoFile(t, "testdata", "sine-vbr.mp3")
	adts := repoFile(t, "container", "adts", "testdata", "stereo.aac")
	wv := repoFile(t, "container", "wv", "testdata", "seek-mono.wv")
	blocks, wvEnd := wvBlocks(t, wv)
	// No stated total: the all-ones escape an encoder writing to a pipe
	// leaves in every block.
	noTotal := bytes.Clone(wv[:wvEnd])
	for _, off := range blocks {
		binary.LittleEndian.PutUint32(noTotal[off+12:], 0xFFFFFFFF)
	}
	var piped bytes.Buffer
	wav, _ := synthWAV(t, stereo, 30000)
	if _, err := waxflow.New().Transcode(context.Background(), container.BytesSource(wav), "", &piped, waxflow.TranscodeOptions{Format: "wavpack"}); err != nil {
		t.Fatal(err)
	}

	cbr := repoFile(t, "testdata", "sine-cbr128.mp3")
	untagged := repoFile(t, "testdata", "sine-untagged.mp3")
	cut := untagged[:len(untagged)-100]
	vbrA := transcodeToBytes(t, stereo, 30000, waxflow.TranscodeOptions{Format: "mp3", VBR: true})
	vbrB := transcodeToBytes(t, stereo, 44100, waxflow.TranscodeOptions{Format: "mp3", VBR: true})

	for _, tc := range []struct {
		name, hint string
		raw        []byte
		// damaged marks a file Strict refuses: its count is a tolerant walk's.
		damaged bool
		// deficit is how far waxlabel's length is known to lie from the
		// demuxer's, waxlabel's less the demuxer's, and why. The row fails
		// when the distance moves either way, so a change on either side
		// says so here.
		deficit time.Duration
		why     string
	}{
		{"mp3, Info header, CBR", "mp3", repoFile(t, "testdata", "sine-cbr128.mp3"), false, 0, ""},
		{"mp3, Xing header, VBR", "mp3", vbr, false, 0, ""},
		{"mp3, Info header, 8 kHz MPEG-2.5", "mp3", repoFile(t, "testdata", "sine-8000-cbr16.mp3"), false, 0, ""},
		{"mp3, the engine's CBR", "mp3", transcodeToBytes(t, stereo, 30000, waxflow.TranscodeOptions{Format: "mp3"}), false, 0, ""},
		{"mp3, the engine's VBR", "mp3", transcodeToBytes(t, stereo, 30000, waxflow.TranscodeOptions{Format: "mp3", VBR: true}), false, 0, ""},
		// The Xing frame dropped: frames of several bitrates and no count.
		{"mp3, mixed bitrates, no header", "mp3", vbr[mp3Frames(t, vbr)[1]:], false, 0, ""},
		{"mp3, one VBR file joined to itself", "mp3", slices.Concat(vbr, vbr), false, 0, ""},
		// The deficits. In each the demuxer's count is the samples a decode
		// of the file delivers; the distance is waxlabel's to close.
		//
		// Two different files joined: a run opens with the first stream's
		// delay and ends with the last stream's padding, which is where a
		// decode's audio ends. waxlabel takes both trims from the first
		// header, and these two paddings differ by 276 samples.
		{"mp3, two VBR files joined", "mp3", slices.Concat(vbrA, vbrB), false, -6258504 * time.Nanosecond,
			"waxlabel trims a joined run by the first stream's padding, not the last's"},
		// Joined CBR files: waxlabel drops the first header's frame count and,
		// with one bitrate throughout, divides the bytes by it: both metadata
		// frames counted as audio and no trim taken.
		{"mp3, two CBR files joined", "mp3", slices.Concat(cbr, cbr), false, 74556122 * time.Nanosecond,
			"waxlabel measures a joined CBR run as bytes over bitrate, untrimmed"},
		// A last frame cut short, no header: waxlabel divides the bytes by
		// the bitrate, the 317 left of the cut frame among them. The demuxer
		// drops a frame that is not whole, with tags behind it or without.
		{"mp3, last frame cut", "mp3", cut, true, 19863520 * time.Nanosecond,
			"waxlabel counts the bytes of a frame cut short as audio"},
		{"mp3, last frame cut, tagged", "mp3", slices.Concat(cut, v1), true, 19863520 * time.Nanosecond,
			"waxlabel counts the bytes of a frame cut short as audio"},
		{"wavpack, the engine's to a pipe", "wv", piped.Bytes(), false, 0, ""},
		{"wavpack, total of all ones", "wv", noTotal, false, 0, ""},
		{"wavpack, no total, cut at a block", "wv", noTotal[:blocks[len(blocks)-2]], false, 0, ""},
		{"adts", "aac", adts, false, 0, ""},
		{"adts, tagged", "aac", slices.Concat(adts, ape, v1), false, 0, ""},
		{"adts, last frame cut, tagged", "aac", slices.Concat(adts[:len(adts)-30], v1), true, 0, ""},
		{"musepack SV7", "mpc", repoFile(t, "container", "mpc", "testdata", "seek-sv7.mpc"), false, 0, ""},
		{"musepack SV8", "mpc", repoFile(t, "container", "mpc", "testdata", "seek.mpc"), false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := waxlabelTrack(t, tc.raw)
			native := nativeLength(t, tc.raw, tc.hint, tc.damaged)
			want := time.Duration(math.Round(float64(native.Samples) / float64(native.Fmt.Rate) * float64(time.Second)))
			diff := tr.Duration - want
			if off := diff - tc.deficit; off < -time.Millisecond || off > time.Millisecond {
				t.Errorf("waxlabel reads %v (%d samples at %d Hz), the demuxer counts %d samples at %d Hz, %v: apart by %v, want %v within 1ms %s",
					tr.Duration, tr.TotalSamples, tr.SampleRate, native.Samples, native.Fmt.Rate, want, diff, tc.deficit, tc.why)
			}
		})
	}
}

// nativeLength is the default track as the engine's own demuxer measures it:
// a strict probe, which walks what a tolerant one defers, or for a damaged
// file a tolerant open and the same walk.
func nativeLength(t *testing.T, raw []byte, hint string, damaged bool) container.Track {
	t.Helper()
	if !damaged {
		info, err := format.Probe(t.Context(), container.BytesSource(raw), hint, &format.Options{Strict: true})
		if err != nil {
			t.Fatalf("a strict probe: %v", err)
		}
		return info.Default()
	}
	med, err := format.Open(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	if err := format.WalkMedia(t.Context(), med); err != nil {
		t.Fatal(err)
	}
	if len(med.Info().Warnings) == 0 {
		t.Fatal("the file reads clean: the row pins nothing")
	}
	return med.Info().Default()
}
