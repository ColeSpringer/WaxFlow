package mp4

import (
	"fmt"

	"github.com/colespringer/waxflow/audio"
)

// ISO's 'chnl' box (ISO/IEC 14496-12, ChannelLayout) states a track's layout
// in the vocabulary of the coding-independent code points for audio,
// ISO/IEC 23091-3 (14496-12 names its withdrawn predecessor, ISO/IEC
// 23001-8, whose tables it carries forward): either one speaker position
// per channel, or a ChannelConfiguration index with a bitmap of the
// configuration's channels the track leaves out. Version 0 is read; version
// 1 (which adds a channel ordering field and a base channel count) and an
// object-structured stream are well-formed shapes this build plays in the
// default order and says so.
const (
	chnlChannelStructured = 1
	chnlObjectStructured  = 2
	chnlExplicitPosition  = 126 // an azimuth and elevation follow
)

// cicpPosition is one row of the OutputChannelPosition table: the nominal
// azimuth and elevation in degrees this build places the row's loudspeaker
// at, a positive azimuth being to the left. The names of rows 0 to 31 are
// checked against Table 2 of ISO/IEC 23091-3:2018 as its Amendment 1 (2022)
// replaces it whole; that table lists names, and the angles here are the
// nominal positions its Table 3 gives the same names in the layouts that
// use them. The two LFE rows carry no useful angle and are placed by index.
// The rows map by angle rather than by name, since the table lists both a
// surround-direct pair and a side-surround pair at 90 degrees and the
// angle, not the name, is what places a speaker.
type cicpPosition struct {
	azimuth, elevation float32
}

// cicpPositions is OutputChannelPosition 0 to 31.
var cicpPositions = [32]cicpPosition{
	{30, 0}, {-30, 0}, {0, 0}, {45, -15}, // L R C LFE
	{110, 0}, {-110, 0}, // Ls Rs
	{22.5, 0}, {-22.5, 0}, // Lc Rc
	{135, 0}, {-135, 0}, // Lsr Rsr
	{180, 0},          // Cs
	{90, 0}, {-90, 0}, // Lsd Rsd
	{90, 0}, {-90, 0}, // Lss Rss
	{60, 0}, {-60, 0}, // Lw Rw
	{30, 35}, {-30, 35}, {0, 35}, // Lv Rv Cv
	{135, 35}, {-135, 35}, {180, 35}, // Lvr Rvr Cvr
	{90, 35}, {-90, 35}, // Lvss Rvss
	{0, 90},                         // Ts
	{-45, -15},                      // LFE2
	{45, -15}, {-45, -15}, {0, -15}, // Lb Rb Cb
	{110, 35}, {-110, 35}, // Lvs Rvs
}

// cicpSpeaker places one CICP position by its angle: the 30 degree pair is
// the front pair, 22.5 the front-centre pair, 110 the surround pair the
// pair rule places, 135 the back pair, 90 the side pair, 180 the back
// centre, and the 35 degree row and the zenith are WAVE's top bits.
func cicpSpeaker(pos int) speaker {
	switch pos {
	case 3:
		return spkLFE
	case 26:
		return spkNone // LFE2
	}
	if pos < 0 || pos >= len(cicpPositions) {
		return spkNone
	}
	p := cicpPositions[pos]
	left := p.azimuth > 0
	pair := func(l, r speaker) speaker {
		if left {
			return l
		}
		return r
	}
	az := p.azimuth
	if az < 0 {
		az = -az
	}
	switch p.elevation {
	case 0:
		switch az {
		case 30:
			return pair(spkL, spkR)
		case 0:
			return spkC
		case 22.5:
			return pair(spkLc, spkRc)
		case 110:
			return pair(spkLs, spkRs)
		case 135:
			return pair(spkRls, spkRrs)
		case 180:
			return spkCs
		case 90:
			return pair(spkLsd, spkRsd)
		}
	case 35:
		switch az {
		case 30:
			return pair(spkVhl, spkVhr)
		case 0:
			return spkVhc
		case 135:
			return pair(spkTbl, spkTbr)
		case 180:
			return spkTbc
		}
	case 90:
		if az == 0 {
			return spkTs
		}
	}
	return spkNone
}

// cicpConfigs is the ChannelConfiguration table for the rows of at most
// audio.MaxChannels channels with WAVE positions, each as its ordered list
// of OutputChannelPosition indices, checked row by row and in order against
// Table 3 of ISO/IEC 23091-3:2018 as its Amendment 1 (2022) replaces it.
// Rows 1 to 6 are the AAC configurations codec/aac carries, and a test holds
// the two transcriptions to the same masks. Row 8 is two channels with no
// positions, 13 is 22.2, and 15 onward have more speakers than the pipeline
// carries.
var cicpConfigs = map[int][]uint8{
	1:  {2},
	2:  {0, 1},
	3:  {2, 0, 1},
	4:  {2, 0, 1, 10},
	5:  {2, 0, 1, 4, 5},
	6:  {2, 0, 1, 4, 5, 3},
	7:  {2, 6, 7, 0, 1, 4, 5, 3},
	9:  {0, 1, 10},
	10: {0, 1, 4, 5},
	11: {2, 0, 1, 4, 5, 10, 3},
	12: {2, 0, 1, 4, 5, 8, 9, 3},
	14: {2, 0, 1, 4, 5, 3, 17, 18},
}

// cicpSpeakers lists a ChannelConfiguration's speakers with the channels of
// omitted left out, and whether the configuration is one in the table. The
// map's bits are numbered from the least significant and correspond in that
// order to the configuration's channels, a set bit meaning absent, which is
// the ChannelLayout semantics of ISO/IEC 14496-12.
func cicpSpeakers(layout int, omitted uint64) ([]speaker, bool) {
	cfg, ok := cicpConfigs[layout]
	if !ok {
		return nil, false
	}
	var spk []speaker
	for i, pos := range cfg {
		if omitted&(1<<i) == 0 {
			spk = append(spk, cicpSpeaker(int(pos)))
		}
	}
	return spk, true
}

// cicpDefined is cicpSpeakers placed on WAVE bits.
func cicpDefined(layout int, omitted uint64) ([]audio.ChannelMask, bool) {
	spk, ok := cicpSpeakers(layout, omitted)
	if !ok {
		return nil, false
	}
	return wavePositions(spk), true
}

// readChnl reads a chnl box for a track of channels channels.
func readChnl(payload []byte, channels int) boxLayout {
	version, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 1 {
		return boxLayout{damage: fmt.Sprintf("chnl box of %d bytes, too short for its stream structure", len(payload))}
	}
	if version != 0 {
		return boxLayout{note: fmt.Sprintf("chnl version %d not read", version)}
	}
	structure := rest[0]
	if structure&chnlObjectStructured != 0 {
		return boxLayout{note: "chnl describes an object-structured stream"}
	}
	if structure&chnlChannelStructured == 0 {
		return boxLayout{note: "chnl states no channel structure"}
	}
	if len(rest) < 2 {
		return boxLayout{damage: "chnl box ends before its layout field"}
	}
	defined := int(rest[1])
	rest = rest[2:]
	if defined != 0 {
		if len(rest) < 8 {
			return boxLayout{damage: "chnl box ends before its omitted-channels map"}
		}
		spk, ok := cicpSpeakers(defined, be64(rest))
		if !ok {
			return boxLayout{note: fmt.Sprintf("chnl configuration %d is not one this build maps", defined)}
		}
		if len(spk) != channels {
			return boxLayout{damage: fmt.Sprintf("chnl configuration %d minus its omitted channels leaves %d channels, the entry has %d",
				defined, len(spk), channels)}
		}
		return speakerLayout(spk, fmt.Sprintf("chnl configuration %d", defined))
	}
	spk := make([]speaker, 0, channels)
	for i := 0; i < channels; i++ {
		if len(rest) < 1 {
			return boxLayout{damage: fmt.Sprintf("chnl box holds %d positions for %d channels", i, channels)}
		}
		pos := int(rest[0])
		rest = rest[1:]
		if pos == chnlExplicitPosition {
			// An azimuth and an elevation this build does not place.
			if len(rest) < 3 {
				return boxLayout{damage: fmt.Sprintf("chnl box ends inside the angle of position %d", i)}
			}
			return boxLayout{note: "chnl position 126 states an angle this build does not place"}
		}
		s := cicpSpeaker(pos)
		if s == spkNone {
			return boxLayout{note: fmt.Sprintf("chnl position %d has no WAVE position", pos)}
		}
		spk = append(spk, s)
	}
	return boxLayout{positions: wavePositions(spk)}
}

// audioMask reads a chan bitmap, whose bits are WAVE's bit for bit.
func audioMask(bitmap uint32) audio.ChannelMask { return audio.ChannelMask(bitmap) }

// maskPositions lists a mask's bits in ascending order, one per channel.
func maskPositions(mask audio.ChannelMask) []audio.ChannelMask {
	out := make([]audio.ChannelMask, 0, mask.Count())
	for bit := audio.ChannelMask(1); bit != 0; bit <<= 1 {
		if mask&bit != 0 {
			out = append(out, bit)
		}
	}
	return out
}
