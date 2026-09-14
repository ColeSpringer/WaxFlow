package mp4

import "fmt"

// QuickTime's 'chan' box is Core Audio's AudioChannelLayout struct in a
// FullBox: a layout tag, a bitmap, a description count, and that many
// 20-byte AudioChannelDescriptions (a label, flags, three coordinates). The
// tag says which of the three carries the layout: kAudioChannelLayoutTag_
// UseChannelBitmap names the bitmap, whose bits are the WAVE mask's bit for
// bit; kAudioChannelLayoutTag_UseChannelDescriptions names the descriptions,
// whose labels each name one speaker; any other tag names a fixed layout by
// itself, with the channel count in its low 16 bits.
//
// The values are transcribed from Apple's CoreAudioTypes header (the
// AudioChannelLayoutTag, AudioChannelLabel and AudioChannelBitmap
// enumerations; MAINTENANCE.md names the copy read).
const (
	chanTagUseDescriptions = 0
	chanTagUseBitmap       = 1 << 16
	chanDescriptionLen     = 20
)

// chanLabels maps an AudioChannelLabel onto a speaker. Labels absent here
// (Mono, the wide pair, LFE2, the matrix-encoded Lt Rt, the ambisonic,
// discrete and mid/side sets, and every value above 34) have no WAVE
// position.
var chanLabels = map[uint32]speaker{
	1: spkL, 2: spkR, 3: spkC, 4: spkLFE,
	5: spkLs, 6: spkRs, 7: spkLc, 8: spkRc, 9: spkCs,
	10: spkLsd, 11: spkRsd, 12: spkTs,
	13: spkVhl, 14: spkVhc, 15: spkVhr,
	16: spkTbl, 17: spkTbc, 18: spkTbr,
	33: spkRls, 34: spkRrs,
}

// chanTags is every AudioChannelLayoutTag of at most audio.MaxChannels
// channels that names a fixed layout, as the header's own comment spells
// its channel order. A speaker WAVE has no bit for is spkNone, which the
// fold reports as a layout this build cannot place. Tags of more channels,
// the two ambisonic tags, DiscreteInOrder and Unknown are absent, and read
// as a tag this build does not map.
var chanTags = func() map[uint32][]speaker {
	const (
		L, R, C, LFE = spkL, spkR, spkC, spkLFE
		Ls, Rs       = spkLs, spkRs
		Lc, Rc, Cs   = spkLc, spkRc, spkCs
		Lsd, Rsd     = spkLsd, spkRsd
		Rls, Rrs     = spkRls, spkRrs
		Ts           = spkTs
		Vhl, Vhc     = spkVhl, spkVhc
		Vhr          = spkVhr
		X            = spkNone // Lt Rt, Lw Rw, Ltm Rtm, the binaural and matrix pairs
	)
	tag := func(id uint32, spk ...speaker) (uint32, []speaker) {
		return id<<16 | uint32(len(spk)), spk
	}
	m := map[uint32][]speaker{}
	add := func(id uint32, spk ...speaker) {
		k, v := tag(id, spk...)
		m[k] = v
	}
	add(100, C)                              // Mono
	add(101, L, R)                           // Stereo
	add(102, L, R)                           // StereoHeadphones
	add(103, X, X)                           // MatrixStereo: Lt Rt
	add(104, X, X)                           // MidSide
	add(105, X, X)                           // XY
	add(106, X, X)                           // Binaural
	add(107, X, X, X, X)                     // Ambisonic_B_Format
	add(108, L, R, Ls, Rs)                   // Quadraphonic
	add(109, L, R, Ls, Rs, C)                // Pentagonal
	add(110, L, R, Ls, Rs, C, Cs)            // Hexagonal
	add(111, L, R, Ls, Rs, C, Cs, X, X)      // Octagonal: Lw Rw
	add(112, X, X, X, X, X, X, X, X)         // Cube: its tops have no labels
	add(113, L, R, C)                        // MPEG_3_0_A
	add(114, C, L, R)                        // MPEG_3_0_B
	add(115, L, R, C, Cs)                    // MPEG_4_0_A
	add(116, C, L, R, Cs)                    // MPEG_4_0_B
	add(117, L, R, C, Ls, Rs)                // MPEG_5_0_A
	add(118, L, R, Ls, Rs, C)                // MPEG_5_0_B
	add(119, L, C, R, Ls, Rs)                // MPEG_5_0_C
	add(120, C, L, R, Ls, Rs)                // MPEG_5_0_D
	add(121, L, R, C, LFE, Ls, Rs)           // MPEG_5_1_A
	add(122, L, R, Ls, Rs, C, LFE)           // MPEG_5_1_B
	add(123, L, C, R, Ls, Rs, LFE)           // MPEG_5_1_C
	add(124, C, L, R, Ls, Rs, LFE)           // MPEG_5_1_D
	add(125, L, R, C, LFE, Ls, Rs, Cs)       // MPEG_6_1_A
	add(126, L, R, C, LFE, Ls, Rs, Lc, Rc)   // MPEG_7_1_A
	add(127, C, Lc, Rc, L, R, Ls, Rs, LFE)   // MPEG_7_1_B
	add(128, L, R, C, LFE, Ls, Rs, Rls, Rrs) // MPEG_7_1_C
	add(129, L, R, Ls, Rs, C, LFE, Lc, Rc)   // Emagic_Default_7_1
	add(130, L, R, C, LFE, Ls, Rs, X, X)     // SMPTE_DTV: Lt Rt
	add(131, L, R, Cs)                       // ITU_2_1
	add(132, L, R, Ls, Rs)                   // ITU_2_2
	add(133, L, R, LFE)                      // DVD_4
	add(134, L, R, LFE, Cs)                  // DVD_5
	add(135, L, R, LFE, Ls, Rs)              // DVD_6
	add(136, L, R, C, LFE)                   // DVD_10
	add(137, L, R, C, LFE, Cs)               // DVD_11
	add(138, L, R, Ls, Rs, LFE)              // DVD_18
	add(139, L, R, Ls, Rs, C, Cs)            // AudioUnit_6_0
	add(140, L, R, Ls, Rs, C, Rls, Rrs)      // AudioUnit_7_0
	add(141, C, L, R, Ls, Rs, Cs)            // AAC_6_0
	add(142, C, L, R, Ls, Rs, Cs, LFE)       // AAC_6_1
	add(143, C, L, R, Ls, Rs, Rls, Rrs)      // AAC_7_0
	add(144, C, L, R, Ls, Rs, Rls, Rrs, Cs)  // AAC_Octagonal
	add(148, L, R, Ls, Rs, C, Lc, Rc)        // AudioUnit_7_0_Front
	add(149, C, LFE)                         // AC3_1_0_1
	add(150, L, C, R)                        // AC3_3_0
	add(151, L, C, R, Cs)                    // AC3_3_1
	add(152, L, C, R, LFE)                   // AC3_3_0_1
	add(153, L, R, Cs, LFE)                  // AC3_2_1_1
	add(154, L, C, R, Cs, LFE)               // AC3_3_1_1
	add(155, L, C, R, Ls, Rs, Cs)            // EAC_6_0_A
	add(156, L, C, R, Ls, Rs, Rls, Rrs)      // EAC_7_0_A
	add(157, L, C, R, Ls, Rs, LFE, Cs)       // EAC3_6_1_A
	add(158, L, C, R, Ls, Rs, LFE, Ts)       // EAC3_6_1_B
	add(159, L, C, R, Ls, Rs, LFE, Vhc)      // EAC3_6_1_C
	add(160, L, C, R, Ls, Rs, LFE, Rls, Rrs) // EAC3_7_1_A
	add(161, L, C, R, Ls, Rs, LFE, Lc, Rc)   // EAC3_7_1_B
	add(162, L, C, R, Ls, Rs, LFE, Lsd, Rsd) // EAC3_7_1_C
	add(163, L, C, R, Ls, Rs, LFE, X, X)     // EAC3_7_1_D: Lw Rw
	add(164, L, C, R, Ls, Rs, LFE, Vhl, Vhr) // EAC3_7_1_E
	add(165, L, C, R, Ls, Rs, LFE, Cs, Ts)   // EAC3_7_1_F
	add(166, L, C, R, Ls, Rs, LFE, Cs, Vhc)  // EAC3_7_1_G
	add(167, L, C, R, Ls, Rs, LFE, Ts, Vhc)  // EAC3_7_1_H
	add(168, C, L, R, LFE)                   // DTS_3_1
	add(169, C, L, R, Cs, LFE)               // DTS_4_1
	add(170, Lc, Rc, L, R, Ls, Rs)           // DTS_6_0_A
	add(171, C, L, R, Rls, Rrs, Ts)          // DTS_6_0_B
	add(172, C, Cs, L, R, Rls, Rrs)          // DTS_6_0_C
	add(173, Lc, Rc, L, R, Ls, Rs, LFE)      // DTS_6_1_A
	add(174, C, L, R, Rls, Rrs, Ts, LFE)     // DTS_6_1_B
	add(175, C, Cs, L, R, Rls, Rrs, LFE)     // DTS_6_1_C
	add(176, Lc, C, Rc, L, R, Ls, Rs)        // DTS_7_0
	add(177, Lc, C, Rc, L, R, Ls, Rs, LFE)   // DTS_7_1
	add(178, Lc, Rc, L, R, Ls, Rs, Rls, Rrs) // DTS_8_0_A
	add(179, Lc, C, Rc, L, R, Ls, Cs, Rs)    // DTS_8_0_B
	add(182, C, L, R, Ls, Rs, LFE, Cs)       // DTS_6_1_D
	add(183, C, L, R, Ls, Rs, Rls, Rrs, LFE) // AAC_7_1_B
	add(184, C, L, R, Ls, Rs, LFE, Vhl, Vhr) // AAC_7_1_C
	add(185, L, R, Rls, Rrs)                 // WAVE_4_0_B
	add(186, L, R, C, Rls, Rrs)              // WAVE_5_0_B
	add(187, L, R, C, LFE, Rls, Rrs)         // WAVE_5_1_B
	add(188, L, R, C, LFE, Cs, Ls, Rs)       // WAVE_6_1
	add(189, L, R, C, LFE, Rls, Rrs, Ls, Rs) // WAVE_7_1
	add(194, L, R, C, LFE, Ls, Rs, X, X)     // Atmos_5_1_2: Ltm Rtm
	return m
}()

// readChan reads a chan box for a track of channels channels.
func readChan(payload []byte, channels int) boxLayout {
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 12 {
		return boxLayout{damage: fmt.Sprintf("chan box of %d bytes, too short for its three fields", len(payload))}
	}
	tag := be32(rest)
	bitmap := be32(rest[4:])
	n := int(be32(rest[8:]))
	switch tag {
	case chanTagUseBitmap:
		mask := audioMask(bitmap)
		if mask.Count() != channels {
			return boxLayout{damage: fmt.Sprintf("chan bitmap names %d positions for %d channels", mask.Count(), channels)}
		}
		// Apple's bitmap goes on past WAVE's eighteen positions (its top
		// pairs, and bit 31); a bit there names a speaker this pipeline
		// cannot place, so the file is well formed and plays in the default
		// order, like a description whose label is not mapped.
		if outside := mask &^ waveMask; outside != 0 {
			return boxLayout{note: fmt.Sprintf("chan bitmap 0x%X names positions outside the WAVE vocabulary", bitmap)}
		}
		return boxLayout{positions: maskPositions(mask)}
	case chanTagUseDescriptions:
		if n != channels {
			return boxLayout{damage: fmt.Sprintf("chan box lists %d descriptions for %d channels", n, channels)}
		}
		if have := (len(rest) - 12) / chanDescriptionLen; have < n {
			return boxLayout{damage: fmt.Sprintf("chan box declares %d descriptions and holds %d", n, have)}
		}
		spk := make([]speaker, n)
		for i := range spk {
			label := be32(rest[12+i*chanDescriptionLen:])
			s, ok := chanLabels[label]
			if !ok {
				return boxLayout{note: fmt.Sprintf("chan description label %d has no WAVE position", label)}
			}
			spk[i] = s
		}
		return speakerLayout(spk, "chan layout")
	}
	spk, ok := chanTags[tag]
	if !ok {
		return boxLayout{note: fmt.Sprintf("chan layout tag %#x is not one this build maps", tag)}
	}
	if len(spk) != channels {
		return boxLayout{damage: fmt.Sprintf("chan layout tag %#x names %d channels, the entry has %d", tag, len(spk), channels)}
	}
	return speakerLayout(spk, fmt.Sprintf("chan layout tag %#x", tag))
}

// speakerLayout places a speaker list, reporting a speaker WAVE cannot name.
func speakerLayout(spk []speaker, what string) boxLayout {
	for _, s := range spk {
		if s == spkNone {
			return boxLayout{note: what + " has a channel with no WAVE position"}
		}
	}
	return boxLayout{positions: wavePositions(spk)}
}
