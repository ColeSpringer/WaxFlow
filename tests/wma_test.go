package waxflow_test

// WMA end to end: probe, decode and transcode a .wma through the public API,
// which is what registering the driver and decoder rows actually buys.
//
// The codec's own differential lives in codec/wma; this is the integration
// layer. What it adds is the wiring nothing else covers: that the sniff table
// resolves an ASF header without a filename, that a track built by
// container/asf drives codec/wma without either side knowing about the other,
// that the pipeline reaches a real output format, and that the refusals for
// the codecs sharing the container arrive by name rather than as
// "unsupported".

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// genWMA writes a .wma fixture via ffmpeg and returns its path.
func genWMA(t *testing.T, dir, name, source, acodec, bitrate string, channels int) string {
	t.Helper()
	ffmpeg := testutil.FFmpeg(t)
	path := filepath.Join(dir, name)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source,
		"-ac", strconv.Itoa(channels), "-c:a", acodec, "-b:a", bitrate, path}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", name, err, out)
	}
	return path
}

var wmaCases = []struct {
	name     string
	acodec   string
	source   string
	channels int
	bitrate  string
	rate     int
}{
	{"v2-44-stereo.wma", "wmav2", "sine=frequency=440:sample_rate=44100:duration=1", 2, "128k", 44100},
	{"v1-44-stereo.wma", "wmav1", "sine=frequency=440:sample_rate=44100:duration=1", 2, "128k", 44100},
	{"v2-32-mono.wma", "wmav2", "anoisesrc=sample_rate=32000:duration=1:seed=5", 1, "64k", 32000},
	{"v1-32-mono.wma", "wmav1", "anoisesrc=sample_rate=32000:duration=1:seed=5", 1, "64k", 32000},
	{"v2-8-mono.wma", "wmav2", "sine=frequency=300:sample_rate=8000:duration=1", 1, "32k", 8000},
}

// TestWMAProbeAndDecode: the sniff table resolves an ASF header with no
// filename to work from, and the track it produces decodes.
func TestWMAProbeAndDecode(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range wmaCases {
		t.Run(tc.name, func(t *testing.T) {
			path := genWMA(t, dir, tc.name, tc.source, tc.acodec, tc.bitrate, tc.channels)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// No hint: the 16-byte Header Object GUID is what resolves it.
			info, err := waxflow.New().Probe(container.BytesSource(raw), "", nil)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if info.Container != "wma" {
				t.Errorf("container %q, want wma", info.Container)
			}
			tr := info.Default()
			if tr.Codec != "wma" {
				t.Errorf("codec %q, want wma", tr.Codec)
			}
			if tr.Fmt.Rate != tc.rate || tr.Fmt.Channels != tc.channels {
				t.Errorf("format %v, want %d Hz %d ch", tr.Fmt, tc.rate, tc.channels)
			}
			if tr.Fmt.Type != audio.Float {
				t.Errorf("format type %v, want float", tr.Fmt.Type)
			}
			// ASF states milliseconds, so its length is advisory: a decode
			// must not be trimmed to it and must not be damage for missing it.
			if tr.SamplesExact {
				t.Error("SamplesExact is set; ASF cannot state a sample-exact length")
			}
			if tr.Delay != 0 || tr.Padding != 0 {
				t.Errorf("gapless trims %d/%d; WMA carries none", tr.Delay, tr.Padding)
			}

			got, err := decodeAllDynamic(t, container.BytesSource(raw), "")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			defer audio.Put(got)
			if got.N < tc.rate/2 {
				t.Errorf("decoded %d frames from a one-second source", got.N)
			}
			var peak float32
			for ch := range got.Fmt.Channels {
				for _, v := range got.ChanF(ch) {
					if v > peak {
						peak = v
					} else if -v > peak {
						peak = -v
					}
				}
			}
			if peak == 0 {
				t.Error("the decode is silent")
			}
		})
	}
}

// TestWMATranscodes runs the whole pipeline: a .wma source out to a format
// this tree writes.
func TestWMATranscodes(t *testing.T) {
	dir := t.TempDir()
	path := genWMA(t, dir, "src.wma", "sine=frequency=440:sample_rate=44100:duration=1", "wmav2", "128k", 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"wav", "flac", "opus"} {
		t.Run(format, func(t *testing.T) {
			out := &memWS{}
			res, err := waxflow.New().Transcode(context.Background(), container.BytesSource(raw), "", out,
				waxflow.TranscodeOptions{Format: format})
			if err != nil {
				t.Fatalf("transcode to %s: %v", format, err)
			}
			if res.Samples <= 0 {
				t.Errorf("%d samples written", res.Samples)
			}
			if len(out.Buf) == 0 {
				t.Fatal("no output")
			}
		})
	}
}

// TestWMARefusalsNameTheCodec: WMA Pro, Lossless and Voice are separate codecs
// that share only the container. Registering the driver means a user who feeds
// one now gets a refusal that says which it is, rather than the sniff table
// declining to recognize the file at all.
func TestWMARefusalsNameTheCodec(t *testing.T) {
	dir := t.TempDir()
	path := genWMA(t, dir, "src.wma", "sine=frequency=440:sample_rate=44100:duration=1", "wmav2", "128k", 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tag  uint16
		want string
	}{
		{0x0162, "Windows Media Audio Pro"},
		{0x0163, "Windows Media Audio Lossless"},
		{0x000a, "Windows Media Audio Voice"},
	} {
		t.Run(fmt.Sprintf("%#04x", tc.tag), func(t *testing.T) {
			patched := retagWMA(t, raw, tc.tag)
			_, err := waxflow.New().Probe(container.BytesSource(patched), "", nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if code := waxerr.CodeOf(err); code != waxerr.CodeUnsupportedFormat {
				t.Errorf("code %q, want %q (a 415)", code, waxerr.CodeUnsupportedFormat)
			}
		})
	}
}

// retagWMA rewrites the wFormatTag in the Stream Properties Object's
// WAVEFORMATEX. The header carries exactly one 0x0161, so finding it is a
// search for that value at a two-byte-aligned position followed by a plausible
// channel count and the file's sample rate.
func retagWMA(t *testing.T, raw []byte, tag uint16) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	hits := 0
	for i := 0; i+8 <= len(out); i++ {
		if binary.LittleEndian.Uint16(out[i:]) != 0x0161 {
			continue
		}
		if ch := binary.LittleEndian.Uint16(out[i+2:]); ch != 2 {
			continue
		}
		if rate := binary.LittleEndian.Uint32(out[i+4:]); rate != 44100 {
			continue
		}
		binary.LittleEndian.PutUint16(out[i:], tag)
		hits++
	}
	if hits != 1 {
		t.Fatalf("found %d WAVEFORMATEX candidates in the header, want 1", hits)
	}
	return out
}

// TestWMAExtensionHints: both extensions the driver claims resolve, and the
// media type is the one Windows Media players expect.
//
// The hints have to be tested on bytes that match no magic. format.resolve
// tries every driver's match before it consults an extension, and the Header
// Object GUID is sixteen bytes at offset zero, so on a real .wma file all five
// hints below are the same test five times: the magic resolves it and the hint
// is never read. Deleting "asf" from the driver's extension list left that
// version of this test green.
func TestWMAExtensionHints(t *testing.T) {
	dir := t.TempDir()
	path := genWMA(t, dir, "src.wma", "sine=frequency=440:sample_rate=44100:duration=0.5", "wmav2", "128k", 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The hint is an extension, with or without the dot; a filename is not one
	// (format.Probe's contract), and the old "track.wma" cases here only ever
	// passed because the magic resolved them before the hint was read.
	for _, hint := range []string{"", "wma", ".wma", "asf", ".asf"} {
		t.Run("magic/hint="+hint, func(t *testing.T) {
			info, err := waxflow.New().Probe(container.BytesSource(raw), hint, nil)
			if err != nil {
				t.Fatalf("probe with hint %q: %v", hint, err)
			}
			if info.Container != "wma" {
				t.Errorf("container %q, want wma", info.Container)
			}
		})
	}

	// Bytes no driver's magic claims, so only the hint can route them. The
	// wma driver then refuses its own garbage, and the refusal it stamps names
	// the driver that read it.
	junk := bytes.Repeat([]byte{'z'}, 4096)
	for _, hint := range []string{"wma", ".wma", "asf", ".asf"} {
		t.Run("hint-only/"+hint, func(t *testing.T) {
			_, err := waxflow.New().Probe(container.BytesSource(junk), hint, nil)
			if err == nil {
				t.Fatalf("probe accepted 4096 junk bytes as %q", hint)
			}
			if !strings.HasPrefix(err.Error(), "wma: ") {
				t.Errorf("hint %q routed to %q, not to the wma driver", hint, err)
			}
		})
	}
	// And an extension the driver does not claim must not route here, or the
	// check above would pass for any hint at all.
	if _, err := waxflow.New().Probe(container.BytesSource(junk), "wmv", nil); err == nil ||
		strings.HasPrefix(err.Error(), "wma: ") {
		t.Errorf("an unclaimed extension routed to the wma driver: %v", err)
	}

	if got := format.MediaTypeFor("wma"); got != "audio/x-ms-wma" {
		t.Errorf("media type %q, want audio/x-ms-wma", got)
	}
}
