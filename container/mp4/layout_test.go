package mp4

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// The layout tags the cells below spell out, as Apple's header defines them
// (kAudioChannelLayoutTag_*: an ID in the high 16 bits, the channel count in
// the low 16). Restated here rather than shared with chan.go, so a slip in
// the table there is caught by a second transcription.
const (
	tagUseBitmap       = 1 << 16
	tagMPEG_3_0_A      = 113<<16 | 3 // L R C
	tagQuadraphonic    = 108<<16 | 4 // L R Ls Rs
	tagMPEG_5_1_A      = 121<<16 | 6 // L R C LFE Ls Rs
	tagMPEG_5_1_B      = 122<<16 | 6 // L R Ls Rs C LFE
	tagMPEG_6_1_A      = 125<<16 | 7 // L R C LFE Ls Rs Cs
	tagMPEG_7_1_A      = 126<<16 | 8 // L R C LFE Ls Rs Lc Rc
	tagMPEG_7_1_C      = 128<<16 | 8 // L R C LFE Ls Rs Rls Rrs
	tagSMPTE_DTV       = 130<<16 | 8 // L R C LFE Ls Rs Lt Rt
	tagAAC_6_0         = 141<<16 | 6 // C L R Ls Rs Cs
	tagDiscreteInOrder = 147 << 16
	tagWAVE_6_1        = 188<<16 | 7 // L R C LFE Cs Ls Rs
	tagWAVE_7_1        = 189<<16 | 8 // L R C LFE Rls Rrs Ls Rs
)

const (
	fiveOne  = audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.LowFrequency | audio.BackLeft | audio.BackRight
	sevenOne = fiveOne | audio.SideLeft | audio.SideRight
)

// TestChannelLayoutBoxes reads one hand-built layout box per shape the two
// boxes take, and pins three things for each: the layout the track reports,
// the order the PCM decoder is handed (nil when the file's order is already
// the WAVE one), and what the file is told about it. A layout the box states
// and this build cannot place is a Note and the default order; a box that
// contradicts the entry is damage, which strict refuses.
func TestChannelLayoutBoxes(t *testing.T) {
	sowt := func(channels int, children ...[]byte) []byte {
		return soundEntryWith("sowt", channels, 16, movieTimescale, children...)
	}
	ipcm := func(channels int, children ...[]byte) []byte {
		return soundEntryWith("ipcm", channels, 16, movieTimescale, append([][]byte{pcmCBox(1, 16)}, children...)...)
	}
	for _, tc := range []struct {
		name     string
		entry    []byte
		channels int
		codec    codec.ID // codec.PCM when empty
		layout   audio.ChannelMask
		order    []uint8
		note     string // a Note must carry this; "" means no Note at all
		damage   string // a Damage warning must carry this, and strict refuses
	}{{
		// A bitmap is the WAVE mask bit for bit.
		name: "bitmap", entry: sowt(6, chanBox(tagUseBitmap, 0x3F)), channels: 6, layout: fiveOne,
	}, {
		name: "bitmap-quad-with-centre", entry: sowt(4, chanBox(tagUseBitmap, 0x107)), channels: 4,
		layout: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackCenter,
	}, {
		// The box and the entry state one channel count twice.
		name: "bitmap-count-disagrees", entry: sowt(6, chanBox(tagUseBitmap, 0x7)), channels: 6,
		layout: fiveOne, damage: "bitmap names 3 positions for 6 channels",
	}, {
		// Named tags whose order is already the WAVE one.
		name: "MPEG_3_0_A", entry: sowt(3, chanBox(tagMPEG_3_0_A, 0)), channels: 3,
		layout: audio.FrontLeft | audio.FrontRight | audio.FrontCenter,
	}, {
		name: "Quadraphonic", entry: sowt(4, chanBox(tagQuadraphonic, 0)), channels: 4,
		layout: audio.FrontLeft | audio.FrontRight | audio.BackLeft | audio.BackRight,
	}, {
		name: "MPEG_5_1_A", entry: sowt(6, chanBox(tagMPEG_5_1_A, 0)), channels: 6, layout: fiveOne,
	}, {
		// The pair rule's side clause: a rear centre beside Ls Rs makes
		// them the side pair, which is the tree's 6.1. The file's Cs comes
		// last and the mask's back centre sits below the side bits, so the
		// layout is the default one and the order is not.
		name: "MPEG_6_1_A", entry: sowt(7, chanBox(tagMPEG_6_1_A, 0)), channels: 7,
		layout: audio.DefaultLayout(7), order: []uint8{0, 1, 2, 3, 6, 4, 5},
	}, {
		// The same speakers in the WAVE order.
		name: "WAVE_6_1", entry: sowt(7, chanBox(tagWAVE_6_1, 0)), channels: 7,
		layout: audio.DefaultLayout(7),
	}, {
		name: "MPEG_7_1_A", entry: sowt(8, chanBox(tagMPEG_7_1_A, 0)), channels: 8,
		layout: fiveOne | audio.FrontLeftOfCenter | audio.FrontRightOfCenter,
	}, {
		name: "WAVE_7_1", entry: sowt(8, chanBox(tagWAVE_7_1, 0)), channels: 8, layout: sevenOne,
	}, {
		// Named tags the decoder has to reorder.
		name: "MPEG_5_1_B", entry: sowt(6, chanBox(tagMPEG_5_1_B, 0)), channels: 6,
		layout: fiveOne, order: []uint8{0, 1, 4, 5, 2, 3},
	}, {
		// Ls Rs before Rls Rrs: the side pair is read before the back pair.
		name: "MPEG_7_1_C", entry: sowt(8, chanBox(tagMPEG_7_1_C, 0)), channels: 8,
		layout: sevenOne, order: []uint8{0, 1, 2, 3, 6, 7, 4, 5},
	}, {
		name: "AAC_6_0", entry: sowt(6, chanBox(tagAAC_6_0, 0)), channels: 6,
		layout: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackCenter | audio.SideLeft | audio.SideRight,
		order:  []uint8{1, 2, 0, 5, 3, 4},
	}, {
		// Descriptions: the labels of the 5.1 file ffmpeg's channelmap
		// writes, L R Ls Rs C LFE.
		name: "descriptions-permuted", entry: sowt(6, chanBox(0, 0, 1, 2, 5, 6, 3, 4)), channels: 6,
		layout: fiveOne, order: []uint8{0, 1, 4, 5, 2, 3},
	}, {
		name: "description-unmappable-label", entry: sowt(6, chanBox(0, 0, 1, 2, 3, 4, 42, 6)), channels: 6,
		layout: fiveOne, note: "label 42",
	}, {
		// Apple's bitmap has bits past WAVE's eighteen (LeftTopMiddle here,
		// and bit 31): a speaker no output can name is a Note and the
		// default, not a layout with a hole every encoder refuses.
		name: "bitmap-outside-wave", entry: sowt(6, chanBox(chanTagUseBitmap, 0x20001F)), channels: 6,
		layout: fiveOne, note: "outside the WAVE vocabulary",
	}, {
		name: "bitmap-speaker-all", entry: sowt(6, chanBox(chanTagUseBitmap, 0x8000001F)), channels: 6,
		layout: fiveOne, note: "outside the WAVE vocabulary",
	}, {
		name: "descriptions-share-a-position", entry: sowt(6, chanBox(0, 0, 1, 1, 3, 4, 5, 6)), channels: 6,
		layout: fiveOne, note: "one position",
	}, {
		name: "descriptions-count-disagrees", entry: sowt(6, chanBox(0, 0, 1, 2, 3, 4)), channels: 6,
		layout: fiveOne, damage: "4 descriptions for 6 channels",
	}, {
		// A well-formed box declaring six descriptions and holding two.
		name: "descriptions-truncated",
		entry: sowt(6, makeFullBox("chan", 0, 0, u32(0), u32(0), u32(6),
			u32(1), make([]byte, 16), u32(2), make([]byte, 16))), channels: 6,
		layout: fiveOne, damage: "declares 6 descriptions and holds 2",
	}, {
		name: "DiscreteInOrder", entry: sowt(6, chanBox(tagDiscreteInOrder|6, 0)), channels: 6,
		layout: fiveOne, note: "tag 0x930006",
	}, {
		name: "SMPTE_DTV", entry: sowt(8, chanBox(tagSMPTE_DTV, 0)), channels: 8,
		layout: audio.DefaultLayout(8), note: "no WAVE position",
	}, {
		name: "unknown-tag", entry: sowt(6, chanBox(250<<16|6, 0)), channels: 6,
		layout: fiveOne, note: "tag 0xfa0006",
	}, {
		name: "named-tag-count-disagrees", entry: sowt(8, chanBox(tagMPEG_5_1_A, 0)), channels: 8,
		layout: audio.DefaultLayout(8), damage: "names 6 channels, the entry has 8",
	}, {
		// Stereo has one order; the box is not consulted and says nothing.
		name: "stereo-ignores-the-box", entry: sowt(2, chanBox(tagMPEG_3_0_A, 0)), channels: 2,
		layout: audio.DefaultLayout(2),
	}, {
		// The ISO box, naming a CICP configuration: 5.1 is C L R Ls Rs LFE.
		name: "chnl-defined-6", entry: ipcm(6, chnlDefined(6, 0)), channels: 6,
		layout: fiveOne, order: []uint8{1, 2, 0, 5, 3, 4},
	}, {
		// 7.1 with a 135 degree pair: C L R Ls Rs Lsr Rsr LFE.
		name: "chnl-defined-12", entry: ipcm(8, chnlDefined(12, 0)), channels: 8,
		layout: sevenOne, order: []uint8{1, 2, 0, 7, 5, 6, 3, 4},
	}, {
		name: "chnl-explicit-permuted", entry: ipcm(6, chnlPositions(0, 1, 8, 9, 2, 3)), channels: 6,
		layout: fiveOne, order: []uint8{0, 1, 4, 5, 2, 3},
	}, {
		// A lone 110 degree pair is the back pair, as it is for a named tag.
		name: "chnl-explicit-lone-surround-pair", entry: ipcm(6, chnlPositions(0, 1, 2, 3, 4, 5)), channels: 6,
		layout: fiveOne,
	}, {
		// The same pair beside a 135 degree pair is the side pair.
		name: "chnl-explicit-both-pairs", entry: ipcm(8, chnlPositions(0, 1, 2, 3, 8, 9, 4, 5)), channels: 8,
		layout: sevenOne,
	}, {
		// 5.1 minus its LFE (channel index 5 of configuration 6) is 5.0.
		name: "chnl-omitted-lfe", entry: ipcm(5, chnlDefined(6, 1<<5)), channels: 5,
		layout: audio.DefaultLayout(5), order: []uint8{1, 2, 0, 3, 4},
	}, {
		name: "chnl-omitted-count-disagrees", entry: ipcm(6, chnlDefined(6, 1<<5)), channels: 6,
		layout: fiveOne, damage: "leaves 5 channels, the entry has 6",
	}, {
		name: "chnl-defined-unknown", entry: ipcm(6, chnlDefined(13, 0)), channels: 6,
		layout: fiveOne, note: "configuration 13",
	}, {
		name: "chnl-version-1", entry: ipcm(6, chnlBox(1, 0x10, 6, 6, 0)), channels: 6,
		layout: fiveOne, note: "version 1",
	}, {
		name: "chnl-object-structured", entry: ipcm(6, chnlBox(0, 3, 0, 0, 1, 2, 3, 8, 9, 2)), channels: 6,
		layout: fiveOne, note: "object",
	}, {
		name: "chnl-no-channel-structure", entry: ipcm(6, chnlBox(0, 2, 6)), channels: 6,
		layout: fiveOne, note: "object",
	}, {
		name: "chnl-explicit-angle", entry: ipcm(6, chnlPositions(0, 1, 2, 3, 126, 0, 110, 0, 5)), channels: 6,
		layout: fiveOne, note: "position 126",
	}, {
		name: "chnl-unknown-position", entry: ipcm(6, chnlPositions(0, 1, 2, 3, 15, 16)), channels: 6,
		layout: fiveOne, note: "position 15",
	}, {
		name: "chnl-truncated", entry: ipcm(6, chnlPositions(0, 1, 2)), channels: 6,
		layout: fiveOne, damage: "holds 3 positions for 6 channels",
	}, {
		// Both boxes, disagreeing: the family's own wins and the other is
		// named. An ISO entry owns chnl.
		name:  "ipcm-both-boxes",
		entry: ipcm(6, chnlPositions(0, 1, 2, 3, 8, 9), chanBox(tagUseBitmap, 0x607)), channels: 6,
		layout: fiveOne, note: "chan box disagrees",
	}, {
		// A QuickTime entry owns chan.
		name:  "sowt-both-boxes",
		entry: sowt(6, chanBox(tagUseBitmap, 0x3F), chnlPositions(0, 1, 2, 10, 11, 12)), channels: 6,
		layout: fiveOne, note: "chnl box disagrees",
	}, {
		// The other family's box alone is still the file's statement.
		name: "sowt-chnl-only", entry: sowt(6, chnlPositions(0, 1, 8, 9, 2, 3)), channels: 6,
		layout: fiveOne, order: []uint8{0, 1, 4, 5, 2, 3},
	}, {
		// G.711 adopts a canonical order and keeps the default for a
		// permuted one: its decoder takes no order.
		name: "ulaw-canonical", entry: soundEntryWith("ulaw", 4, 16, movieTimescale, chanBox(tagUseBitmap, 0x107)),
		channels: 4, codec: codec.MuLaw,
		layout: audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackCenter,
	}, {
		name: "ulaw-permuted", entry: soundEntryWith("ulaw", 6, 16, movieTimescale, chanBox(tagMPEG_5_1_B, 0)),
		channels: 6, codec: codec.MuLaw, layout: fiveOne, note: "mu-law",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.codec
			unitBytes := 2 * tc.channels
			if want == "" {
				want = codec.PCM
			} else {
				unitBytes = tc.channels
			}
			raw := buildMovie(tc.entry, unitBytes, 64, 0)
			d := open(t, raw)
			tr := d.Tracks()[0]
			if tr.Codec != want {
				t.Fatalf("codec = %q, want %q", tr.Codec, want)
			}
			if tr.Fmt.Layout != tc.layout {
				t.Errorf("layout = %v, want %v", tr.Fmt.Layout, tc.layout)
			}
			if want == codec.PCM {
				cfg, err := pcm.ParseConfig(tr.CodecConfig)
				if err != nil {
					t.Fatalf("ParseConfig: %v", err)
				}
				if !slices.Equal(cfg.Order, tc.order) {
					t.Errorf("order = %v, want %v", cfg.Order, tc.order)
				}
			}
			var notes, damage []string
			for _, w := range d.Warnings() {
				if w.Kind == container.Note {
					notes = append(notes, w.Msg)
				} else {
					damage = append(damage, w.Msg)
				}
			}
			if tc.note == "" && len(notes) != 0 {
				t.Errorf("notes = %v, want none", notes)
			}
			if tc.note != "" && !strings.Contains(strings.Join(notes, "; "), tc.note) {
				t.Errorf("notes = %v, want one naming %q", notes, tc.note)
			}
			if tc.damage == "" && len(damage) != 0 {
				t.Errorf("damage = %v, want none", damage)
			}
			if tc.damage != "" && !strings.Contains(strings.Join(damage, "; "), tc.damage) {
				t.Errorf("damage = %v, want one naming %q", damage, tc.damage)
			}
			_, err := NewDemuxer(container.BytesSource(raw), &DemuxerOptions{Strict: true})
			switch {
			case tc.damage != "" && err == nil:
				t.Error("strict accepted a layout box that contradicts the entry")
			case tc.damage != "" && waxerr.CodeOf(err) != waxerr.CodeMalformedInput:
				t.Errorf("strict code = %q, want %q (%v)", waxerr.CodeOf(err), waxerr.CodeMalformedInput, err)
			case tc.damage == "" && err != nil:
				t.Errorf("strict refused a well-formed file: %v", err)
			}
		})
	}
}

// TestLayoutBoxReordersThePayload is the end of the chain: a permuted box has
// to move samples, not only rename channels. The mdat pattern is one byte per
// channel per frame, so wire channel w of frame i holds byte 6i+w and output
// channel c must read wire channel order[c].
func TestLayoutBoxReordersThePayload(t *testing.T) {
	order := []uint8{0, 1, 4, 5, 2, 3}
	entry := soundEntryWith("raw ", 6, 8, movieTimescale, chanBox(0, 0, 1, 2, 5, 6, 3, 4))
	raw := buildMovie(entry, 6, 64, 0)
	d := open(t, raw)
	tr := d.Tracks()[0]
	cfg, err := pcm.ParseConfig(tr.CodecConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Order, order) {
		t.Fatalf("order = %v, want %v", cfg.Order, order)
	}
	dec, err := pcm.NewDecoder(cfg, tr.Fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Release()
	pkt := readAllPacketData(t, d)
	frames := 0
	if err := dec.Decode(pkt, func(b *audio.Buffer) error {
		for c := 0; c < 6; c++ {
			for i, v := range b.ChanI(c) {
				if want := int32(int(byte((frames+i)*6+int(order[c]))) - 128); v != want {
					t.Fatalf("output channel %d frame %d = %d, want wire channel %d's %d", c, frames+i, v, order[c], want)
				}
			}
		}
		frames += b.N
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if frames != 64 {
		t.Errorf("decoded %d frames, want 64", frames)
	}
}

// TestCICPConfigurationsAgreeWithAAC cross-checks the chnl reader's copy of
// the CICP ChannelConfiguration table against codec/aac's, for the rows both
// carry: two transcriptions of ISO/IEC 23091-3's rows 1 to 6, read from
// different documents (14496-3 Table 1.19 restates the same layouts), that
// must land on the same WAVE mask.
func TestCICPConfigurationsAgreeWithAAC(t *testing.T) {
	for n := 1; n <= 6; n++ {
		// audioObjectType 2, samplingFrequencyIndex 3, then the configuration.
		cfg, err := aac.ParseASC([]byte{0x11, 0x80 | byte(n)<<3})
		if err != nil {
			t.Fatalf("configuration %d: %v", n, err)
		}
		f, err := cfg.Format()
		if err != nil {
			t.Fatal(err)
		}
		positions, ok := cicpDefined(n, 0)
		if !ok {
			t.Fatalf("configuration %d is not in the chnl table", n)
		}
		l, ok := foldLayout(positions, len(positions))
		if !ok {
			t.Fatalf("configuration %d does not fold", n)
		}
		if l.mask != f.Layout {
			t.Errorf("configuration %d: chnl says %v, codec/aac says %v", n, l.mask, f.Layout)
		}
	}
}
