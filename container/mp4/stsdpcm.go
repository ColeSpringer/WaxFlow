package mp4

import (
	"math"
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container/internal/codecname"
)

// Uncompressed audio in the MP4 family is spelled two ways, and this file
// reads both.
//
// QuickTime spells it as a fourcc per storage format ('sowt', 'twos', 'in24',
// 'in32', 'fl32', 'fl64', 'raw ', 'NONE'), with the byte order carried either
// by the fourcc itself or by an 'enda' box inside the 'wave' wrapper, and as
// 'lpcm' in a version 2 sound description whose flags word states signedness,
// byte order and packing directly. ISO/IEC 23003-5 spells it as 'ipcm' or
// 'fpcm' with a mandatory 'pcmC' box holding the width and byte order.
//
// What the two have in common is the only thing that matters downstream: the
// samples are raw interleaved frames of a fixed width, so the whole of the
// codec configuration is a pcm.Config, and the sample table is byte-linear.
type pcmEntry struct {
	format string
	body   []byte // the whole sample entry body, for the version 2 struct
	// children is the child box list to search first: the payload of a
	// QuickTime 'wave' wrapper when the entry has one, else the raw list.
	// raw is the list as the entry carries it. Both are searched, because
	// QuickTime puts 'enda' inside the wrapper and ISO puts 'pcmC' and 'srat'
	// beside it, and a file may carry a wrapper and an ISO box both.
	children []byte
	raw      []byte
	// format is the fourcc as the file spells it, which messages quote and
	// which an "ms" entry's WAVEFORMATEX atom is stored under; canon is the
	// canonical spelling of the storage it names, which the arms dispatch on.
	// The two differ when a file writes a fourcc in the wrong case.
	canon    string
	version  uint16
	channels int
	// samplesize is the entry's own depth field, meaningful only for the
	// QuickTime fourccs whose width it states.
	samplesize int
	// rate is the integer part of the 16.16 samplerate field, or the version
	// 2 struct's rate; 0 when the entry states none.
	rate int
}

// childrenParse reports whether both child lists walk to their ends.
//
// It is checked once, up front, because the alternative is silent: walkBoxes
// stops at the first box it cannot size, so a damaged box anywhere in the list
// makes every box after it unreachable, and an unreachable 'enda' or 'srat'
// is indistinguishable from an absent one. Absent is a defined answer for both
// (the fourcc's own byte order, the 16.16 rate field), so the track would
// decode at the wrong byte order or the wrong rate rather than fail.
func (e pcmEntry) childrenParse() error {
	none := func(string, []byte) error { return nil }
	if err := walkBoxes(e.children, none); err != nil {
		return err
	}
	if err := walkBoxes(e.raw, none); err != nil {
		return err
	}
	return nil
}

// child returns the payload of the first child box of the given type, in
// either list, and whether one was found.
//
// The flag is the point, and findChild's nil test is what it replaces: a box
// with no content at all is still a box, and reporting it as absent makes the
// search look past it to the next list. An empty 'enda' shadowing a real one
// is byte-reversed audio and an empty 'srat' is the wrong sample rate, both
// silently, so the first box of the right type wins whatever its length.
func (e pcmEntry) child(typ string) ([]byte, bool) {
	for _, list := range [][]byte{e.children, e.raw} {
		var found []byte
		ok := false
		_ = walkBoxes(list, func(t string, payload []byte) error {
			if !ok && t == typ {
				found, ok = payload, true
			}
			return nil
		})
		if ok {
			return found, true
		}
	}
	return nil, false
}

// setPCM wires an uncompressed audio track from its sample entry.
//
// The width, encoding and byte order come from the entry (the sections below
// per spelling); the rate and channel count come from the header fields the
// caller already read, except where the entry has nowhere to state the rate,
// which defers it to the media timescale. Nothing is read from the sample
// table: a v1 entry's bytesPerFrame and a v2 entry's constBytesPerAudioPacket
// are derivable from the config and the channel count, so a file that
// misstates either is read correctly rather than refused.
//
// The frame width this yields is then geometry rather than a claim to check
// against the table. A constant sample size equal to it is what lets parseStbl
// chunk-index the track; a writer that blocks several frames into one sample,
// or writes a size per sample, simply keeps the flattened table, which costs
// nothing at those sizes. What parseStbl does check is the length the two
// state, since for a byte-linear track they state the same number.
func (d *Demuxer) setPCM(t *track, e pcmEntry) error {
	if err := e.childrenParse(); err != nil {
		return err
	}
	cfg, channels, err := d.pcmConfig(e)
	if err != nil {
		return err
	}
	if channels < 1 {
		return malformed("audio sample entry %q declares %d channels", e.format, channels)
	}
	if channels > audio.MaxChannels {
		return unsupported("%d channels (supported: 1..%d)", channels, audio.MaxChannels)
	}
	if cfg.Bits == 8 {
		// One byte has no ends to put in an order, and the field is part of
		// the codec config that keys the cache (ADR-0004). Left as the
		// spelling happened to set it, 'NONE' and 'raw ' would describe
		// identical storage with two different blobs.
		cfg.BigEndian = false
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	rate, err := d.pcmRate(t, e)
	if err != nil {
		return err
	}
	if _, ok := e.child("chan"); ok && channels > 2 {
		// The QuickTime 'chan' box states a channel layout, and is not read:
		// the pipeline's layouts are the WAVE ones, mapping between the two is
		// a table nothing in the tree needs yet, and a wrong layout is worse
		// than a default one. Mono and stereo have only one order, so this is
		// said only where the file stated an order that could differ from the
		// one it will play in.
		t.addNote("channel layout box not read; %d channels play in the default order", channels)
	}
	t.codec = codec.PCM
	t.codecConfig = blob
	// The pipeline carries floats as float32, so a 64-bit float source decodes
	// at BitDepth 32 and the file's own depth has nowhere in the format to
	// live. Recorded so probe reports what the source holds rather than what
	// the pipeline runs on; riff does the same from the same config. Integer
	// depths need none of it, since ValidBits already puts the source depth in
	// Fmt.BitDepth.
	if cfg.Encoding == pcm.Float && cfg.Bits != cfg.PCMFormat(rate, channels, 0).BitDepth {
		t.sourceBits = cfg.Bits
	}
	t.fmt = cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	// One frame per unit, which is what makes the sample table uniform.
	t.unitBytes = int64(cfg.BytesPerFrame(channels))
	t.unitDur = 1
	return nil
}

// The WAVE format tags an "ms" sample entry spells uncompressed audio with.
// Every other tag names a codec, refused by name in parseAudioSampleEntry's
// default arm.
const (
	waveTagPCM   = 0x0001
	waveTagFloat = 0x0003
)

// isPCMWaveTag reports whether a WAVE format tag names uncompressed audio.
func isPCMWaveTag(tag uint16) bool {
	return tag == waveTagPCM || tag == waveTagFloat
}

// pcmFourcc folds a sample-entry fourcc onto the canonical spelling of the
// uncompressed storage it names, or "" for one that names something else.
//
// Case-insensitively, because the reference reader is: a 'TWOS' entry reads as
// pcm_s16be in ffmpeg 8.0.1, and container/aiff already folds the same way for
// the AIFF-C compression types that spell these same codecs. Matching exactly
// would refuse a file every other reader opens, and refuse it while naming the
// codec this package decodes.
func pcmFourcc(format string) string {
	switch strings.ToUpper(format) {
	case "SOWT":
		return "sowt"
	case "TWOS":
		return "twos"
	case "NONE":
		return "NONE"
	case "RAW ":
		return "raw "
	case "IN24":
		return "in24"
	case "IN32":
		return "in32"
	case "FL32":
		return "fl32"
	case "FL64":
		return "fl64"
	case "LPCM":
		return "lpcm"
	case "IPCM":
		return "ipcm"
	case "FPCM":
		return "fpcm"
	}
	return ""
}

// pcmConfig reads the wire configuration and channel count out of a sample
// entry, dispatching on the spelling. The channel count is the entry header's
// for every spelling but "ms", which carries a WAVEFORMATEX of its own.
func (d *Demuxer) pcmConfig(e pcmEntry) (pcm.Config, int, error) {
	var cfg pcm.Config
	var err error
	switch {
	case e.canon == "lpcm":
		cfg, err = d.lpcmConfig(e)
	case e.canon == "ipcm", e.canon == "fpcm":
		cfg, err = d.isoPCMConfig(e)
	default:
		if tag, ok := codecname.QuickTimeWaveTag(e.format); ok {
			return d.msPCMConfig(e, tag)
		}
		cfg, err = d.fourccPCMConfig(e)
	}
	return cfg, e.channels, err
}

// endaLittle reports whether an 'enda' box says the samples are
// little-endian.
//
// The box only ever forces little-endian: a value of 1 overrides whatever the
// fourcc implies, and anything else leaves the fourcc's own convention
// standing. That asymmetry is the format's rather than a simplification, and
// it is measurable in the reference reader: 'twos' with enda 1 reads as
// pcm_s16le and with enda 0 as pcm_s16be, while 'sowt' with enda 1 stays
// little-endian rather than flipping (ffmpeg 8.0.1).
func endaLittle(e pcmEntry) bool {
	enda, _ := e.child("enda")
	return len(enda) >= 2 && be16(enda) == 1
}

// fourccPCMConfig maps one of QuickTime's storage fourccs onto a wire config.
//
// Two things vary. The width comes from the entry's samplesize for the four
// fourccs that do not state one ('sowt' and 'twos' are 16-bit by convention
// but carry 8, 24 and 32 in the wild; 'NONE' and 'raw ' name an encoding and
// leave the width to the field); the rest state their own width, and their
// samplesize field is conventionally 16 whatever they hold. The byte order is
// the fourcc's own, which an 'enda' box can override in one direction only.
//
// 'raw ' is in the first group and not a fixed 8-bit encoding, which is
// measurable rather than inferred: the reference reader answers pcm_u8 for
// 'raw ' and 'NONE' at 8 bits and pcm_s16be for both at 16 (ffmpeg 8.0.1).
// Refusing the 16-bit form as a fourcc contradicting its own depth, which this
// did, refuses a file that plays elsewhere.
func (d *Demuxer) fourccPCMConfig(e pcmEntry) (pcm.Config, error) {
	var cfg pcm.Config
	bigEndian := true
	switch e.canon {
	case "sowt", "twos", "NONE", "raw ":
		switch e.samplesize {
		case 8, 16, 24, 32:
		default:
			// Not malformed: a sound description states its depth in bits and
			// 20 is a legal thing to write there. It is a storage this build
			// does not unpack, and the reference reader does not either (it
			// ignores the field and reads a 20-bit 'twos' as 16-bit, which is
			// a guess rather than a reading).
			return cfg, unsupported("%d-bit samples in a %q sample entry", e.samplesize, e.format)
		}
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: e.samplesize}
		if cfg.Bits == 8 && e.canon != "sowt" && e.canon != "twos" {
			// 8-bit uncompressed QuickTime audio is offset binary, which is
			// what 'raw ' names outright and what 'NONE' means at that width.
			// The two byte-order fourccs are signed at every width.
			cfg.Encoding = pcm.UnsignedInt
		}
		bigEndian = e.canon != "sowt"
	case "in24":
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: 24}
	case "in32":
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: 32}
	case "fl32":
		cfg = pcm.Config{Encoding: pcm.Float, Bits: 32}
	case "fl64":
		cfg = pcm.Config{Encoding: pcm.Float, Bits: 64}
	default:
		// parseAudioSampleEntry dispatches on the same list, so this is
		// unreachable from a file; it is here so adding a fourcc there and
		// forgetting it here fails loudly instead of yielding zero bits.
		return cfg, unsupported("uncompressed sample entry %q", e.format)
	}
	cfg.BigEndian = bigEndian && !endaLittle(e)
	return cfg, nil
}

// The formatSpecificFlags bits of a version 2 sound description (QuickTime
// File Format, Sound Sample Description version 2). They describe the LPCM
// storage completely, which is why 'lpcm' needs no other box.
const (
	lpcmIsFloat         = 1 << 0
	lpcmIsBigEndian     = 1 << 1
	lpcmIsSignedInteger = 1 << 2
	// IsPacked is bit 3. It is named for the fixtures that set it and is not
	// read: lpcmConfig derives packing from the width and the packet size,
	// which state it without room for the two to disagree.
	lpcmIsPacked         = 1 << 3
	lpcmIsAlignedHigh    = 1 << 4
	lpcmIsNonInterleaved = 1 << 5
)

// lpcmConfig reads a version 2 'lpcm' sound description.
//
// Version 2 is required rather than assumed: the fourcc predates the layout
// and a version 0 or 1 'lpcm' entry carries nothing that says which of the
// several LPCM storages it holds, so it is refused by name rather than
// guessed at. ffmpeg writes this entry for any rate the 16.16 field cannot
// hold, which is every rate above 65535 Hz.
func (d *Demuxer) lpcmConfig(e pcmEntry) (pcm.Config, error) {
	if e.version != 2 {
		return pcm.Config{}, unsupported("PCM (%s) in a version %d sample entry, which does not state its storage",
			e.format, e.version)
	}
	// v2SoundFields already bounded the struct, so these offsets are present,
	// and it is where the 64-bit ceiling on the width was applied.
	flags := be32(e.body[52:56])
	bits := int(be32(e.body[48:52]))
	packetBytes := int64(be32(e.body[56:60]))
	frames := int64(be32(e.body[60:64]))

	if flags&lpcmIsNonInterleaved != 0 {
		return pcm.Config{}, unsupported("non-interleaved PCM (%s), which stores each channel in its own packet", e.format)
	}
	if flags&lpcmIsAlignedHigh != 0 {
		return pcm.Config{}, unsupported("high-aligned PCM (%s), whose samples are left-justified in their words", e.format)
	}
	// Packing is judged from the geometry rather than from the IsPacked flag,
	// which is redundant beside it: a width that is a whole number of bytes,
	// in packets exactly that wide across the channels, leaves nowhere for
	// padding to be. Anything else is a layout pcm.Config cannot describe,
	// since its spare bits are left-justified and LPCM's are not.
	if bits%8 != 0 {
		return pcm.Config{}, unsupported("%d-bit PCM (%s), whose samples do not begin on byte boundaries",
			bits, e.format)
	}
	if want := int64(bits/8) * int64(e.channels); packetBytes != want {
		return pcm.Config{}, unsupported("unpacked PCM (%s): %d-byte packets for %d channels of %d bits",
			e.format, packetBytes, e.channels, bits)
	}
	cfg := pcm.Config{Bits: bits, BigEndian: flags&lpcmIsBigEndian != 0}
	switch {
	case flags&lpcmIsFloat != 0:
		cfg.Encoding = pcm.Float
	case flags&lpcmIsSignedInteger != 0:
		cfg.Encoding = pcm.SignedInt
	case bits == 8:
		cfg.Encoding = pcm.UnsignedInt
	default:
		return pcm.Config{}, unsupported("unsigned %d-bit PCM (%s)", bits, e.format)
	}
	if frames != 1 {
		// Damage rather than a refusal: LPCM is one frame per packet by
		// definition, so a field saying otherwise contradicts the storage it
		// is describing, and the samples are still where the width says.
		if err := d.warn(0, "version 2 sample entry %q declares %d frames per packet, want 1", e.format, frames); err != nil {
			return pcm.Config{}, err
		}
	}
	return cfg, nil
}

// isoPCMConfig reads an ISO/IEC 23003-5 'ipcm' or 'fpcm' entry, whose whole
// configuration is the mandatory 'pcmC' box.
func (d *Demuxer) isoPCMConfig(e pcmEntry) (pcm.Config, error) {
	pcmC, ok := e.child("pcmC")
	if !ok {
		// Mandatory in the spec, and there is nothing to fall back on: the
		// entry's samplesize is 16 whatever the samples are.
		return pcm.Config{}, malformed("%s sample entry has no pcmC box", e.format)
	}
	// Exactly two bytes of content: 23003-5 defines the box as a FullBox with
	// format_flags and PCM_sample_size and nothing else, so a longer one is
	// not that box and its two fields are not where they would be read from.
	_, _, rest, full := fullBox(pcmC)
	if !full || len(rest) != 2 {
		return pcm.Config{}, malformed("pcmC box of %d bytes, want the 6 the spec defines", len(pcmC))
	}
	cfg := pcm.Config{Bits: int(rest[1]), BigEndian: rest[0]&1 == 0}
	if e.canon == "fpcm" {
		cfg.Encoding = pcm.Float
		if cfg.Bits != 32 && cfg.Bits != 64 {
			return pcm.Config{}, malformed("fpcm sample entry declares %d-bit floats", cfg.Bits)
		}
		return cfg, nil
	}
	cfg.Encoding = pcm.SignedInt
	switch cfg.Bits {
	case 16, 24, 32:
	default:
		// 23003-5 defines ipcm at 16, 24 and 32; an 8-bit integer has no byte
		// order to state and is spelled 'raw ' or 'NONE' instead.
		return pcm.Config{}, malformed("ipcm sample entry declares %d-bit integers", cfg.Bits)
	}
	return cfg, nil
}

// msPCMConfig reads a QuickTime "ms" sample entry whose WAVE format tag names
// uncompressed audio: tag 1 (PCM) and tag 3 (IEEE float). Everything about
// the storage then follows WAVE's conventions rather than QuickTime's, which
// is the point of the spelling: little-endian, and 8-bit integers unsigned.
//
// The geometry comes from a WAVEFORMATEX atom inside the 'wave' wrapper when
// the entry carries one, since that structure is the tag's natural home and
// the entry's header is the QuickTime mirror of it. Width AND channel count,
// not one of each: taking the width from the atom and the channels from the
// header is how a file that states them inconsistently yields a frame size
// neither of them states, which then matches no sample in the table.
func (d *Demuxer) msPCMConfig(e pcmEntry, tag uint16) (pcm.Config, int, error) {
	bits, channels := e.samplesize, e.channels
	if w, ok := msWaveFormat(e, tag); ok {
		if w.channels != channels || w.bits != bits {
			// Damage, so --strict refuses: two statements of one geometry
			// that do not agree. The atom wins, being the structure the
			// format tag belongs to.
			if err := d.warn(0, "%s entry says %d channels of %d bits, its WAVEFORMATEX says %d of %d; the latter wins",
				codecname.Label(e.format), channels, bits, w.channels, w.bits); err != nil {
				return pcm.Config{}, 0, err
			}
		}
		if want := w.bits / 8 * w.channels; w.blockAlign != want && w.bits%8 == 0 {
			// riff's rule, restated: the computed frame size is the one the
			// samples are actually laid out at.
			if err := d.warn(0, "%s entry declares a %d-byte block align, computed %d; using computed",
				codecname.Label(e.format), w.blockAlign, want); err != nil {
				return pcm.Config{}, 0, err
			}
		}
		bits, channels = w.bits, w.channels
	}
	switch tag {
	case waveTagPCM:
		switch bits {
		case 8:
			return pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}, channels, nil
		case 16, 24, 32:
			return pcm.Config{Encoding: pcm.SignedInt, Bits: bits}, channels, nil
		}
		return pcm.Config{}, 0, unsupported("%d-bit samples in a %s sample entry",
			bits, codecname.Label(e.format))
	case waveTagFloat:
		if bits != 32 && bits != 64 {
			return pcm.Config{}, 0, malformed("%s sample entry declares %d-bit floats",
				codecname.Label(e.format), bits)
		}
		return pcm.Config{Encoding: pcm.Float, Bits: bits}, channels, nil
	}
	// parseAudioSampleEntry routes only the two tags above here.
	return pcm.Config{}, 0, unsupported("%s", codecname.Label(e.format))
}

// waveFormatEx is the part of a WAVEFORMATEX struct that describes the
// geometry of uncompressed samples.
type waveFormatEx struct {
	channels   int
	blockAlign int
	bits       int
}

// msWaveFormat reads the WAVEFORMATEX atom stored inside an "ms" entry's
// 'wave' wrapper, if it carries one. The atom is a box whose type is the
// entry's own fourcc and whose payload is the little-endian struct:
// wFormatTag(2) nChannels(2) nSamplesPerSec(4) nAvgBytesPerSec(4)
// nBlockAlign(2) wBitsPerSample(2).
func msWaveFormat(e pcmEntry, tag uint16) (waveFormatEx, bool) {
	atom, ok := e.child(e.format)
	if !ok || len(atom) < 16 {
		return waveFormatEx{}, false
	}
	if le16At(atom, 0) != int(tag) {
		return waveFormatEx{}, false // not the struct the wrapper is meant to hold
	}
	return waveFormatEx{
		channels:   le16At(atom, 2),
		blockAlign: le16At(atom, 12),
		bits:       le16At(atom, 14),
	}, true
}

// le16At reads a little-endian 16-bit field, which a WAVEFORMATEX is built of
// and every box field around it is not.
func le16At(b []byte, off int) int { return int(b[off]) | int(b[off+1])<<8 }

// pcmRate resolves the sample rate of an uncompressed track, in the order the
// ISO specification gives: an 'srat' box states the rate outright, the 16.16
// field states it where it fits, and the media timescale is what is left.
//
// The last is not a fallback for damage: ffmpeg's mp4 muxer writes a zero rate
// field and no srat for any rate above 65535 Hz, so for a 96 kHz ipcm track
// the timescale is the file's only statement of it. Reported as a Note, since
// the file is well formed and this says where the answer came from. Reading
// the timescale here is safe whatever order the file writes its boxes in:
// parseMdia reads mdhd before it descends into minf, deliberately.
func (d *Demuxer) pcmRate(t *track, e pcmEntry) (int, error) {
	srat, _ := e.child("srat")
	if _, _, rest, ok := fullBox(srat); ok && len(rest) >= 4 {
		hz := int64(be32(rest))
		// Bounded in int64 so acceptance does not depend on the platform's
		// int width: a rate past MaxInt32 would wrap negative on a 32-bit
		// build, and audio.Format would then refuse it for the wrong reason.
		if hz <= 0 || hz > math.MaxInt32 {
			return 0, malformed("srat box declares a %d Hz rate", hz)
		}
		if e.rate > 0 && int64(e.rate) != hz {
			// Both fields are the file's own statement of one number. srat
			// wins (it is the wider field, and the spec's order), and the
			// disagreement is damage --strict should refuse.
			if werr := d.warn(0, "srat says %d Hz and the sample entry says %d; srat wins", hz, e.rate); werr != nil {
				return 0, werr
			}
		}
		return int(hz), nil
	}
	if e.rate > 0 {
		return e.rate, nil
	}
	// Bounded the same way, and for the same reason: mdhd's timescale is a
	// uint32, so a 32-bit build would wrap a large one negative and the two
	// builds would disagree about one file.
	if t.timescale < 1 || t.timescale > math.MaxInt32 {
		return 0, malformed("sample entry states no sample rate and the media timescale is %d", t.timescale)
	}
	t.addNote("sample entry states no sample rate; taking the media timescale's %d Hz", t.timescale)
	return int(t.timescale), nil
}
