package aac

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/audio"
)

// asc builds a plain AOT 2 AudioSpecificConfig: audioObjectType(5)=2,
// samplingFrequencyIndex(4)=3 (48000), channelConfiguration(4), then the
// GASpecificConfig's frameLengthFlag and two more zero flags.
func asc(chanConfig int) []byte {
	bits := uint32(2)<<11 | uint32(3)<<7 | uint32(chanConfig)<<3
	return []byte{byte(bits >> 8), byte(bits)}
}

// TestASCChannelConfigs pins the count and the speaker layout each
// configuration resolves to. Configurations 4 and 7 are the reason this
// is a table and not audio.DefaultLayout of the count.
func TestASCChannelConfigs(t *testing.T) {
	for _, tc := range []struct {
		config   int
		channels int
		layout   audio.ChannelMask
	}{
		{1, 1, audio.FrontCenter},
		{2, 2, audio.FrontLeft | audio.FrontRight},
		{3, 3, audio.FrontLeft | audio.FrontRight | audio.FrontCenter},
		{4, 4, audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackCenter},
		{5, 5, audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.BackLeft | audio.BackRight},
		{6, 6, audio.FrontLeft | audio.FrontRight | audio.FrontCenter | audio.LowFrequency |
			audio.BackLeft | audio.BackRight},
	} {
		cfg, err := ParseASC(asc(tc.config))
		if err != nil {
			t.Errorf("configuration %d: %v", tc.config, err)
			continue
		}
		if cfg.Channels != tc.channels || cfg.ChannelConfig != tc.config {
			t.Errorf("configuration %d parsed as config %d with %d channels",
				tc.config, cfg.ChannelConfig, cfg.Channels)
		}
		f, err := cfg.Format()
		if err != nil {
			t.Errorf("configuration %d Format: %v", tc.config, err)
			continue
		}
		if f.Channels != tc.channels || f.Layout != tc.layout {
			t.Errorf("configuration %d format = %d ch %v, want %d ch %v",
				tc.config, f.Channels, f.Layout, tc.channels, tc.layout)
		}
		if f.Layout.Count() != f.Channels {
			t.Errorf("configuration %d layout %v names %d positions for %d channels",
				tc.config, f.Layout, f.Layout.Count(), f.Channels)
		}
	}
}

// TestASCConfig4IsNotQuad is the row worth its own test: taking
// audio.DefaultLayout of the channel count would call this file quad and
// route its centre and its back centre to a back pair.
func TestASCConfig4IsNotQuad(t *testing.T) {
	cfg, err := ParseASC(asc(4))
	if err != nil {
		t.Fatal(err)
	}
	f, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	if f.Layout == audio.DefaultLayout(4) {
		t.Fatalf("configuration 4 reports %v, the conventional quad layout; "+
			"AAC's 4.0 is a centre and a back centre", f.Layout)
	}
}

// TestASCConfig7Refused pins the refusal and, more importantly, that it
// says why: a caller reading "not supported" with no reason will file it
// as an oversight rather than the standards disagreement it is.
func TestASCConfig7Refused(t *testing.T) {
	_, err := ParseASC(asc(7))
	if err == nil {
		t.Fatal("channel configuration 7 was accepted")
	}
	if !strings.Contains(err.Error(), "channel configuration 7") {
		t.Errorf("error = %q, want it to name channel configuration 7", err)
	}
}

// TestASCReservedConfigs pins configurations 8-15, which no path here
// decodes: 8-10 and 15 are reserved, and 11-14 were given layouts (6.1,
// 7.1 rear surround, 22.2, 7.1 top) by later amendments. The failure has
// to name the configuration. Left to fall through they resolve to a zero
// channel count, and the caller ends up reporting a count where the
// configuration is the cause.
func TestASCReservedConfigs(t *testing.T) {
	for config := 8; config <= 15; config++ {
		_, err := ParseASC(asc(config))
		if err == nil {
			t.Errorf("channel configuration %d was accepted", config)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("channel configuration %d", config)) {
			t.Errorf("configuration %d: error = %q, want it to name the configuration", config, err)
		}
	}
}

// TestFormatRefusesUnlayoutedConfig covers the same ground one level up.
// ParseASC rejects these, so only a hand-built Config reaches Format, but
// it must still refuse by name rather than hand back a format with no
// channels for the caller to reject generically.
func TestFormatRefusesUnlayoutedConfig(t *testing.T) {
	for _, config := range []int{7, 8, 12, 15} {
		c := Config{ObjectType: aotAACLC, SampleRate: 48000, ChannelConfig: config, FrameLength: 1024}
		f, err := c.Format()
		if err == nil {
			t.Errorf("configuration %d produced %v instead of an error", config, f)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("channel configuration %d", config)) {
			t.Errorf("configuration %d: error = %q, want it to name the configuration", config, err)
		}
	}
}

// TestASCConfig0 pins the in-band PCE contract. ParseASC leaves the count
// to the container, Format takes a filled-in mono or stereo count, and
// refuses anything else rather than guessing an element order it cannot
// read. The old code defaulted an unfilled count to stereo, which turned
// an unreadable layout into audio that was silently the wrong channels.
func TestASCConfig0(t *testing.T) {
	cfg, err := ParseASC(asc(0))
	if err != nil {
		t.Fatalf("configuration 0 must parse (the count is the container's job): %v", err)
	}
	if cfg.Channels != 0 || cfg.ChannelConfig != 0 {
		t.Fatalf("configuration 0 parsed as config %d with %d channels",
			cfg.ChannelConfig, cfg.Channels)
	}
	if _, err := cfg.Format(); err == nil {
		t.Error("an unfilled configuration 0 produced a format instead of an error")
	} else if !strings.Contains(err.Error(), "program config element") {
		t.Errorf("error = %q, want it to name the in-band program config element", err)
	}

	for _, ch := range []int{1, 2} {
		filled := cfg
		filled.Channels = ch
		f, err := filled.Format()
		if err != nil {
			t.Errorf("configuration 0 filled to %d channels: %v", ch, err)
			continue
		}
		if f.Channels != ch || f.Layout != audio.DefaultLayout(ch) {
			t.Errorf("configuration 0 filled to %d = %d ch %v", ch, f.Channels, f.Layout)
		}
	}
	for _, ch := range []int{3, 6, 8} {
		filled := cfg
		filled.Channels = ch
		if _, err := filled.Format(); err == nil {
			t.Errorf("configuration 0 filled to %d channels produced a format; "+
				"the element order is still unknown", ch)
		}
	}
}

// TestWaveSlotsPermutation pins the invariant a typo in the slot table
// would break without any test noticing: every configuration must route
// each element to a distinct output channel, covering all of them. A
// duplicate would leave one channel silent and overwrite another.
func TestWaveSlotsPermutation(t *testing.T) {
	for config := 1; config <= 6; config++ {
		cfg, err := ParseASC(asc(config))
		if err != nil {
			t.Errorf("configuration %d: %v", config, err)
			continue
		}
		slots := waveSlots(config, cfg.Channels)
		if len(slots) != cfg.Channels {
			t.Errorf("configuration %d has %d slots for %d channels", config, len(slots), cfg.Channels)
			continue
		}
		sorted := slices.Clone(slots)
		slices.Sort(sorted)
		for i, v := range sorted {
			if v != i {
				t.Errorf("configuration %d slots %v are not a permutation of 0..%d",
					config, slots, cfg.Channels-1)
				break
			}
		}
	}
	if got := waveSlots(7, 8); got != nil {
		t.Errorf("waveSlots(7, 8) = %v, want nil (the configuration is refused)", got)
	}
	if got := waveSlots(0, 6); got != nil {
		t.Errorf("waveSlots(0, 6) = %v, want nil (a PCE order is unknown)", got)
	}
}

// TestNewDecoderRefusesUnknownOrder pins the decoder's own guard. Format
// is the gate a container passes through, but a caller building a Config
// by hand reaches NewDecoder directly, and a nil slot table there would
// panic on the first element rather than refuse.
func TestNewDecoderRefusesUnknownOrder(t *testing.T) {
	cfg, err := ParseASC(asc(0))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Channels = 6
	f := audio.Format{Rate: 48000, Channels: 6, Layout: audio.DefaultLayout(6),
		Type: audio.Float, BitDepth: 32}
	if _, err := NewDecoder(cfg, f); err == nil {
		t.Fatal("NewDecoder accepted a configuration 0 stream with 6 channels")
	}
}

// TestElementSequenceMatchesSlots pins the invariant that keeps the two
// halves of the channel map from drifting: channelElements describes the
// same Table 1.19 sequence as waveSlots, so they must agree on its
// length, and a pair must occupy two positions rather than one.
func TestElementSequenceMatchesSlots(t *testing.T) {
	for config := 1; config <= 6; config++ {
		cfg, err := ParseASC(asc(config))
		if err != nil {
			t.Errorf("configuration %d: %v", config, err)
			continue
		}
		elems := channelElements(config)
		if elems == nil {
			if config > 2 {
				t.Errorf("configuration %d has a permutation slot map but no element sequence", config)
			}
			continue
		}
		if len(elems) != len(waveSlots(config, cfg.Channels)) {
			t.Errorf("configuration %d: %d elements against %d slots",
				config, len(elems), len(waveSlots(config, cfg.Channels)))
			continue
		}
		for i := 0; i < len(elems); i++ {
			if elems[i] != elCPE {
				continue
			}
			if i+1 >= len(elems) || elems[i+1] != elCPE {
				t.Errorf("configuration %d: channel pair at %d does not occupy two positions", config, i)
				break
			}
			i++ // the pair's second position, already accounted for
		}
	}
}

// TestDecodeRefusesDeviantElementOrder is the guard against the failure
// waveSlots cannot see. Routing is by position, so a stream that sent its
// channel pair where Table 1.19 puts the centre would write every channel
// somewhere plausible and wrong; only the element tag says so. The packet
// is three bits of tag and filler, since the check runs before any of the
// element's own data is read.
func TestDecodeRefusesDeviantElementOrder(t *testing.T) {
	cfg, err := ParseASC(asc(6))
	if err != nil {
		t.Fatal(err)
	}
	f, err := cfg.Format()
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDecoder(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	// 001 in the top three bits: a channel pair element, where
	// configuration 6 opens with the centre's single channel element.
	pkt := make([]byte, 16)
	pkt[0] = elCPE << 5
	err = d.Decode(pkt, func(*audio.Buffer) error { return nil })
	if err == nil {
		t.Fatal("a channel pair where the centre belongs was accepted")
	}
	for _, want := range []string{"single channel element", "channel pair element"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}

	// The control: the same packet opening with the element the
	// configuration does specify gets past this check, so the refusal
	// above is the order and not the filler.
	pkt[0] = elSCE << 5
	if err := d.Decode(pkt, func(*audio.Buffer) error { return nil }); err != nil &&
		strings.Contains(err.Error(), "codes a single channel element") {
		t.Errorf("the conforming element order was refused as deviant: %v", err)
	}
}
