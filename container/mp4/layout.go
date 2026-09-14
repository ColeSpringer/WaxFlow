package mp4

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container/internal/codecname"
)

// A multichannel PCM track states its channel layout in one of two boxes:
// QuickTime's 'chan' (chan.go) or ISO's 'chnl' (chnl.go). Both describe the
// file's channels in their own vocabulary, and both fold here onto the
// pipeline's WAVE mask plus the order the PCM decoder needs when the file's
// channels are not in mask-bit order.

// speaker is a loudspeaker position in the vocabulary the two boxes share
// before the pair rule places it on a WAVE bit. Apple's labels and CICP's
// positions both name one "left surround" that WAVE has two bits for.
type speaker uint8

const (
	spkNone speaker = iota // a position WAVE has no bit for
	spkL
	spkR
	spkC
	spkLFE
	spkLs // left and right surround: Apple's LeftSurround, CICP's 110 degree pair
	spkRs
	spkLc // the front pair beside the centre
	spkRc
	spkCs  // centre surround, WAVE's back centre
	spkLsd // surround direct: the 90 degree pair, always WAVE's side pair
	spkRsd
	spkRls // rear surround: the 135 degree pair, always WAVE's back pair
	spkRrs
	spkTs  // top centre
	spkVhl // vertical height, WAVE's top front row
	spkVhc
	spkVhr
	spkTbl // top back row
	spkTbc
	spkTbr
)

// wavePositions places speakers on WAVE bits, one per file channel, with 0
// for a speaker WAVE has no bit for.
//
// The pair rule: Ls Rs are the back pair unless the layout also has a rear
// element (Rls Rrs or Cs), in which case they are the side pair; Rls Rrs are
// always the back pair and Lsd Rsd always the side pair. The side clause is
// what Apple's own WAVE tags say (WAVE_6_1 is L R C LFE Cs Ls Rs and WAVE_7_1
// is L R C LFE Rls Rrs Ls Rs, both in mask-bit order only with Ls Rs on the
// side bits). The back clause for a lone pair is a choice made for the
// pipeline: every codec in the tree lands 5.1 on the back-pair mask, and so
// does the WAV mask container/riff writes, so reading a lone Ls Rs as the
// side pair would make a 5.1 movie the one input whose layout disagrees
// with every codec's and with its own WAV output.
func wavePositions(spk []speaker) []audio.ChannelMask {
	rear := false
	for _, s := range spk {
		if s == spkRls || s == spkRrs || s == spkCs {
			rear = true
		}
	}
	out := make([]audio.ChannelMask, len(spk))
	for i, s := range spk {
		switch s {
		case spkL:
			out[i] = audio.FrontLeft
		case spkR:
			out[i] = audio.FrontRight
		case spkC:
			out[i] = audio.FrontCenter
		case spkLFE:
			out[i] = audio.LowFrequency
		case spkLs:
			out[i] = audio.BackLeft
			if rear {
				out[i] = audio.SideLeft
			}
		case spkRs:
			out[i] = audio.BackRight
			if rear {
				out[i] = audio.SideRight
			}
		case spkLc:
			out[i] = audio.FrontLeftOfCenter
		case spkRc:
			out[i] = audio.FrontRightOfCenter
		case spkCs:
			out[i] = audio.BackCenter
		case spkLsd:
			out[i] = audio.SideLeft
		case spkRsd:
			out[i] = audio.SideRight
		case spkRls:
			out[i] = audio.BackLeft
		case spkRrs:
			out[i] = audio.BackRight
		case spkTs:
			out[i] = audio.TopCenter
		case spkVhl:
			out[i] = audio.TopFrontLeft
		case spkVhc:
			out[i] = audio.TopFrontCenter
		case spkVhr:
			out[i] = audio.TopFrontRight
		case spkTbl:
			out[i] = audio.TopBackLeft
		case spkTbc:
			out[i] = audio.TopBackCenter
		case spkTbr:
			out[i] = audio.TopBackRight
		}
	}
	return out
}

// waveMask is every bit the pipeline's layout vocabulary defines: the
// eighteen WAVE positions, audio.FrontLeft through audio.TopBackRight. A
// bit above them is a speaker no output here can name.
const waveMask = audio.TopBackRight<<1 - 1

// channelLayout is what a layout box folds to: the mask, and the order the
// PCM decoder applies (nil when the file's channels are already in mask-bit
// order, which is what every existing blob and cache key describes).
type channelLayout struct {
	mask  audio.ChannelMask
	order []uint8
}

// foldLayout folds the file's channels, one WAVE bit each in file order,
// onto a layout. It is adopted only when every channel has a bit, no two
// share one, and there are exactly channels of them; a layout that fails
// any of those is not one the pipeline can name, and the caller keeps the
// default.
func foldLayout(positions []audio.ChannelMask, channels int) (channelLayout, bool) {
	if len(positions) != channels {
		return channelLayout{}, false
	}
	var mask audio.ChannelMask
	for _, p := range positions {
		if p == 0 || p.Count() != 1 || p&^waveMask != 0 || mask&p != 0 {
			return channelLayout{}, false
		}
		mask |= p
	}
	order := make([]uint8, channels)
	identity := true
	c := 0
	for bit := audio.ChannelMask(1); bit != 0 && c < channels; bit <<= 1 {
		if mask&bit == 0 {
			continue
		}
		for i, p := range positions {
			if p == bit {
				order[c] = uint8(i)
			}
		}
		if int(order[c]) != c {
			identity = false
		}
		c++
	}
	if identity {
		order = nil
	}
	return channelLayout{mask: mask, order: order}, true
}

// boxLayout is one box's reading: the positions it states (nil when it
// states none this build can place, with note saying why), or damage when
// the box contradicts the entry it sits in.
type boxLayout struct {
	positions []audio.ChannelMask
	note      string
	damage    string
}

// channelLayout reads the layout box a sample entry carries, for a track of
// more than two channels. It returns the folded layout and whether one was
// adopted; a layout the box states and this build cannot place leaves a
// Note on the track, and a box that contradicts the entry is damage.
//
// An ISO entry (ipcm, fpcm) owns chnl and a QuickTime entry owns chan. Where
// an entry carries both, its own wins and a Note names the other when the
// two disagree; the other family's box alone is still the file's statement
// and is read as such.
func (d *Demuxer) channelLayout(t *track, e pcmEntry, channels int) (channelLayout, bool, error) {
	chnlBox, hasChnl := e.child("chnl")
	chanBox, hasChan := e.child("chan")
	if !hasChnl && !hasChan {
		return channelLayout{}, false, nil
	}
	own, other := "chan", "chnl"
	ownBox, otherBox := chanBox, chnlBox
	hasOther := hasChnl
	if e.canon == "ipcm" || e.canon == "fpcm" {
		own, other = other, own
		ownBox, otherBox = otherBox, ownBox
		hasOther = hasChan
	}
	if own == "chan" && !hasChan || own == "chnl" && !hasChnl {
		own, other = other, own
		ownBox, otherBox = otherBox, ownBox
		hasOther = false
	}
	read := func(name string, box []byte) boxLayout {
		if name == "chan" {
			return readChan(box, channels)
		}
		return readChnl(box, channels)
	}
	bl := read(own, ownBox)
	if bl.damage != "" {
		return channelLayout{}, false, d.warn(0, "%s", bl.damage)
	}
	if bl.note != "" {
		t.addNote("%s; %d channels play in the default order", bl.note, channels)
		return channelLayout{}, false, nil
	}
	l, ok := foldLayout(bl.positions, channels)
	if !ok {
		t.addNote("%s box maps two channels to one position; %d channels play in the default order", own, channels)
		return channelLayout{}, false, nil
	}
	if hasOther {
		ol := read(other, otherBox)
		if o, ook := foldLayout(ol.positions, channels); ol.damage != "" || ol.note != "" || !ook || !sameLayout(o, l) {
			t.addNote("%s box disagrees with the %s box; %s wins", other, own, own)
		}
	}
	return l, true, nil
}

// sameLayout reports whether two folded layouts name the same mask in the
// same file order.
func sameLayout(a, b channelLayout) bool {
	if a.mask != b.mask || len(a.order) != len(b.order) {
		return false
	}
	for i := range a.order {
		if a.order[i] != b.order[i] {
			return false
		}
	}
	return true
}

// codecLayout is channelLayout for the codecs whose decoders take no order:
// a layout in the WAVE order is adopted, and a permuted one keeps the
// default with a Note naming the codec.
func (d *Demuxer) codecLayout(t *track, e pcmEntry, channels int) (audio.ChannelMask, error) {
	layout := audio.DefaultLayout(channels)
	if channels <= 2 {
		return layout, nil
	}
	l, ok, err := d.channelLayout(t, e, channels)
	if err != nil || !ok {
		return layout, err
	}
	if l.order != nil {
		t.addNote("the file's channel order is not the WAVE one and %s is not reordered; %d channels play in the default order",
			codecname.Label(e.format), channels)
		return layout, nil
	}
	return l.mask, nil
}
