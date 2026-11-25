package waxflow

import (
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/waxerr"
)

// TestPlanRemuxDeclinesWhatItsMuxerRefuses: a copy plans only where its
// destination muxer's Begin takes the track, so what the muxer would refuse
// declines to the transcode rung instead of failing the response. Each row
// is asked of the plan and of Begin through the form, which must agree, and
// a run asked for a copy the muxer refuses refuses it in Begin's words.
func TestPlanRemuxDeclinesWhatItsMuxerRefuses(t *testing.T) {
	aac := aacTrack(0, 48000)
	withASC := func(asc []byte) container.Track {
		tr := aac
		tr.CodecConfig = asc
		return tr
	}
	pce := withASC([]byte{0x11, 0x80}) // channel configuration 0
	long := withASC(append([]byte{0x11, 0x90}, make([]byte, 103)...))
	longest := withASC(append([]byte{0x11, 0x90}, make([]byte, 102)...))
	frames960 := withASC([]byte{0x11, 0x94})
	escaped := withASC([]byte{0x17, 0x80, 0x61, 0xA8, 0x10}) // 50 kHz, escape-coded
	escaped.Fmt.Rate = 50000
	skewed, unpadded := aac, aac
	skewed.Fmt.Rate = 44100
	unpadded.Padding = -1
	dual := gridLastHEAAC()
	dual.Delay = 0
	single := dual
	single.CodecConfig = []byte{0x29, 0x91, 0x88, 0x00}
	opus, short, fam1, skew := opusTrack(312, 48000), opusTrack(312, 48000), opusTrack(312, 48000), opusTrack(312, 48000)
	short.CodecConfig = short.CodecConfig[:18]
	fam1.CodecConfig = family1Head(312)
	skew.Delay = 0
	fl := muxableTrack(t, "flac")
	minBlock, layout := fl, fl
	minBlock.CodecConfig = append([]byte(nil), fl.CodecConfig...)
	minBlock.CodecConfig[0], minBlock.CodecConfig[1] = 0, 8 // a minimum block under 16
	layout.Fmt.Layout = audio.SideLeft | audio.SideRight
	vorbis := container.Track{Codec: codec.Vorbis, CodecConfig: []byte("not a vorbis config"),
		Fmt: audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Float, BitDepth: 32}}
	pcm16 := audio.Format{Rate: 48000, Channels: 2, Layout: audio.DefaultLayout(2), Type: audio.Int, BitDepth: 16}
	alac := container.Track{Codec: codec.ALAC, CodecConfig: []byte("not a cookie"), Fmt: pcm16}
	wavpack := container.Track{Codec: codec.WavPack, CodecConfig: []byte{0}, Fmt: pcm16, Padding: 1}
	ape := container.Track{Codec: codec.APE, CodecConfig: []byte{0}, Fmt: pcm16, Padding: 1}
	for _, tc := range []struct {
		name   string
		track  container.Track
		opts   TranscodeOptions
		served bool
	}{
		{"a PCE to fMP4", pce, TranscodeOptions{Format: "aac"}, false},
		{"a PCE to progressive MP4", pce, TranscodeOptions{Format: "aac", Container: ContainerProgressive}, false},
		{"a PCE to adts", pce, TranscodeOptions{Format: "aac", Container: "adts"}, false},
		{"a PCE to mka", pce, TranscodeOptions{Format: "aac", Container: "mka"}, true},
		{"a config past the esds", long, TranscodeOptions{Format: "aac"}, false},
		{"the longest config the esds holds", longest, TranscodeOptions{Format: "aac"}, true},
		{"a config past the esds to adts", long, TranscodeOptions{Format: "aac", Container: "adts"}, true},
		{"960-sample frames to adts", frames960, TranscodeOptions{Format: "aac", Container: "adts"}, false},
		{"960-sample frames to fMP4", frames960, TranscodeOptions{Format: "aac"}, true},
		{"an escape-coded rate to adts", escaped, TranscodeOptions{Format: "aac", Container: "adts"}, false},
		{"an escape-coded rate to fMP4", escaped, TranscodeOptions{Format: "aac"}, true},
		{"a format its ASC disagrees with", skewed, TranscodeOptions{Format: "aac"}, false},
		{"a negative padding", unpadded, TranscodeOptions{Format: "aac", Container: ContainerFragmented}, false},
		{"a short ASC", aac, TranscodeOptions{Format: "aac"}, true},
		{"a short ASC to adts", aac, TranscodeOptions{Format: "aac", Container: "adts"}, true},
		{"single-rate SBR to adts", single, TranscodeOptions{Format: "aac", Container: "adts"}, false},
		{"dual-rate SBR to adts", dual, TranscodeOptions{Format: "aac", Container: "adts"}, true},
		{"a truncated OpusHead to Ogg", short, TranscodeOptions{Format: "opus"}, false},
		{"a truncated OpusHead to webm", short, TranscodeOptions{Format: "opus", Container: "webm"}, false},
		{"a family-0 Opus", opus, TranscodeOptions{Format: "opus"}, true},
		{"a family-1 Opus to Ogg", fam1, TranscodeOptions{Format: "opus"}, true},
		{"an Opus delay that disagrees with its pre-skip, to Ogg", skew, TranscodeOptions{Format: "opus"}, true},
		{"a minimum block under 16 to native FLAC", minBlock, TranscodeOptions{Format: "flac"}, false},
		{"a minimum block under 16 to Ogg", minBlock, TranscodeOptions{Format: "flac", Container: ContainerOgg}, true},
		{"a minimum block under 16 to mka", minBlock, TranscodeOptions{Format: "flac", Container: "mka"}, true},
		{"a layout STREAMINFO cannot state, to native FLAC", layout, TranscodeOptions{Format: "flac"}, false},
		{"a layout STREAMINFO cannot state, to Ogg", layout, TranscodeOptions{Format: "flac", Container: ContainerOgg}, true},
		{"a Vorbis config that will not split, to Ogg", vorbis, TranscodeOptions{Format: "vorbis"}, false},
		{"a Vorbis config that will not split, to mka", vorbis, TranscodeOptions{Format: "vorbis", Container: "mka"}, true},
		{"an ALAC cookie that will not parse", alac, TranscodeOptions{Format: "alac"}, false},
		{"a WavPack trim", wavpack, TranscodeOptions{Format: "wavpack"}, false},
		{"an APE trim", ape, TranscodeOptions{Format: "ape"}, false},
	} {
		p, err := New().PlanRemux(tc.track, tc.opts)
		if err != nil || (p != nil) != tc.served {
			t.Errorf("%s: PlanRemux served %v, err %v; want served %v", tc.name, p != nil, err, tc.served)
		}
		_, fm, _, err := remuxForm(tc.track, tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		mt := tc.track
		mt.ID, mt.Default = 0, true
		mux, err := fm.mux(mt, tc.opts, runConfig{}, nil, &testutil.MemWriteSeeker{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		begun := mux.Begin([]container.Track{mt})
		if (begun == nil) != tc.served {
			t.Errorf("%s: Begin = %v; the plan must agree with it", tc.name, begun)
		}
		if tc.served || begun == nil {
			continue
		}
		_, err = New().RemuxDemuxer(t.Context(), &gridDemuxer{}, tc.track, &testutil.MemWriteSeeker{}, tc.opts)
		if err == nil || waxerr.CodeOf(err) != waxerr.CodeOf(begun) || !strings.Contains(err.Error(), begun.Error()) {
			t.Errorf("%s: RemuxDemuxer = %v; want Begin's refusal, %v", tc.name, err, begun)
		}
	}
	// A segmented copy writes fMP4 whatever the form, so the native FLAC
	// muxer's refusal does not decline it.
	if p, err := New().PlanRemuxSegments(minBlock, TranscodeOptions{Format: "flac"}, 2, knownGrid(4096)); p == nil || err != nil {
		t.Errorf("a minimum block under 16 over HLS: %v, %v; want the segmented remux", p, err)
	}
}
