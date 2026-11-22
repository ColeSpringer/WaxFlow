package waxflow

import (
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestEncoderFactsMatchTheRows holds what each output row states before any
// encoder exists (the frame size, the format, the segmented priming) to the
// encoder that row builds, over every source shape its plan accepts.
func TestEncoderFactsMatchTheRows(t *testing.T) {
	rates := []int{8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 88200, 96000}
	widths := []int{1, 2, 6}
	samples := []audio.Format{
		{Type: audio.Int, BitDepth: 16},
		{Type: audio.Int, BitDepth: 24},
		{Type: audio.Float, BitDepth: 32},
	}
	for i := range outputs {
		row := &outputs[i]
		if row.remuxOnly || row.encode == nil {
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			type built struct {
				f     audio.Format
				frame int
			}
			seen := map[built]bool{}
			// check reports whether the plan accepted the cell.
			check := func(in audio.Format, opts TranscodeOptions) bool {
				core, err := buildPlanCore(in, opts)
				if err != nil {
					return false
				}
				b := built{core.format, core.frameSize}
				if seen[b] {
					return true
				}
				seen[b] = true
				checkEncoderFacts(t, in, b.f, b.frame, -1, func() (codec.Encoder, error) {
					return row.encode(b.f, opts)
				})
				if row.hls != nil {
					checkEncoderFacts(t, in, b.f, b.frame, row.hls.delay, func() (codec.Encoder, error) {
						return row.hls.encode(b.f, opts, 0)
					})
				}
				return true
			}
			format := func(rate, ch int, s audio.Format) audio.Format {
				return audio.Format{Rate: rate, Channels: ch, Layout: audio.DefaultLayout(ch),
					Type: s.Type, BitDepth: s.BitDepth}
			}
			for _, s := range samples {
				checked := 0
				for _, rate := range rates {
					for _, ch := range widths {
						if check(format(rate, ch, s), TranscodeOptions{Format: row.name}) {
							checked++
						}
					}
				}
				if checked == 0 {
					t.Errorf("the plan accepted no %d-bit %v source, so nothing of it was checked", s.BitDepth, s.Type)
				}
			}
			// An explicit rate converts a source at another rate.
			converted := 0
			for _, rate := range rates {
				src := 44100
				if rate == src {
					src = 48000
				}
				if check(format(src, 2, samples[0]), TranscodeOptions{Format: row.name, Rate: rate}) {
					converted++
				}
			}
			if converted == 0 {
				t.Error("the plan accepted no explicit rate, so nothing of it was checked")
			}
		})
	}
}

// checkEncoderFacts builds one encoder and holds it to the plan's format and
// frame size, then runs one frame through it and holds its trailer's delay to
// its Delay method and, when delay is not negative, to the row's stated one.
func checkEncoderFacts(t *testing.T, in, f audio.Format, frame int, delay int64, build func() (codec.Encoder, error)) {
	t.Helper()
	enc, err := build()
	if err != nil {
		t.Errorf("%v: the plan accepted %v but the encoder refused it: %v", in, f, err)
		return
	}
	if got := enc.InputFormat(); got != f {
		t.Errorf("%v: the plan's format is %v, the encoder's %v", in, f, got)
	}
	if got := enc.FrameSize(); got != frame {
		t.Errorf("%v: the row states a %d-sample frame at %d Hz, the encoder %d", in, frame, f.Rate, got)
	}
	n := enc.FrameSize()
	if n <= 0 {
		n = audio.StandardChunk
	}
	buf := testutil.Sine(f, n, 1000, 0.25)
	defer audio.Put(buf)
	discard := func(codec.Packet) error { return nil }
	if err := enc.Encode(buf, discard); err != nil {
		t.Errorf("%v: encode: %v", in, err)
		return
	}
	tr, err := enc.Finish(discard)
	if err != nil {
		t.Errorf("%v: finish: %v", in, err)
		return
	}
	if d, ok := enc.(interface{ Delay() int }); ok && int64(d.Delay()) != tr.Delay {
		t.Errorf("%v: Delay() is %d, the trailer's %d", in, d.Delay(), tr.Delay)
	}
	if delay >= 0 && delay != tr.Delay {
		t.Errorf("%v: the segmented form states a %d-sample delay, the trailer %d", in, delay, tr.Delay)
	}
}
