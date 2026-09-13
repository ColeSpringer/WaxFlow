package mp4

import (
	"strings"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/codec/g711"
	"github.com/colespringer/waxflow/container/internal/codecname"
	"github.com/colespringer/waxflow/container/internal/waveformat"
)

// The compressed audio the MP4 family spells the way it spells uncompressed
// audio: with a fourcc, or with an "ms" entry naming a WAVE format tag. This
// file reads the four codecs whose payload is a flat run of fixed-size units
// with no framing of its own, which is what makes them sample-table codecs
// rather than bitstream ones.
//
// G.711 is byte-linear like PCM: one unit is one frame, and the law is the
// whole of its configuration. The ADPCM families are not: one unit is a block
// of several samples, so the sample table indexes blocks and the timeline is
// the block index times the block's length. Apple's ima4 states that geometry
// in its fourcc, and the two WAVE-tagged families state it in a WAVEFORMATEX
// inside the 'wave' wrapper, which is the only thing here that is out of band.

// codecFourcc folds a sample-entry fourcc onto the canonical spelling of a
// compressed audio format this file reads, or "" for one that names
// something else. Case-insensitive for the reason pcmFourcc is.
func codecFourcc(format string) string {
	switch strings.ToUpper(format) {
	case "ULAW":
		return "ulaw"
	case "ALAW":
		return "alaw"
	case "IMA4":
		return "ima4"
	}
	return ""
}

// The WAVE format tags an "ms" entry spells these codecs with. The two ADPCM
// tags live in container/internal/waveformat, beside the rules for reading
// the geometry they state.
const (
	waveTagALaw  = 0x0006
	waveTagMuLaw = 0x0007
)

// isCodecWaveTag reports whether a WAVE format tag names one of the codecs
// here. The G.711 pair and the two ADPCM families all have one.
func isCodecWaveTag(tag uint16) bool {
	switch tag {
	case waveTagALaw, waveTagMuLaw, waveformat.TagIMAADPCM, waveformat.TagMSADPCM:
		return true
	}
	return false
}

// setCodec wires a companded or block-coded audio track from its sample
// entry, dispatching on the spelling the way setPCM does.
func (d *Demuxer) setCodec(t *track, e pcmEntry) error {
	if err := e.childrenParse(); err != nil {
		return err
	}
	channels := e.channels
	tag, isMS := codecname.QuickTimeWaveTag(e.format)
	if isMS {
		// An "ms" entry's WAVEFORMATEX is the structure the format tag
		// belongs to, so the channel count is its to state and the entry
		// header mirrors it. The width is neither's: each of these codecs has
		// exactly one, which is why there is nothing here matching
		// msPCMConfig's width reconciliation.
		w, ok := msWaveFormat(e, tag)
		if ok && w.channels != channels {
			if err := d.warn(0, "%s entry says %d channels, its WAVEFORMATEX says %d; the latter wins",
				codecname.Label(e.format), channels, w.channels); err != nil {
				return err
			}
			channels = w.channels
		}
	}
	if channels < 1 {
		return malformed("audio sample entry %q declares %d channels", e.format, channels)
	}
	if channels > audio.MaxChannels {
		return unsupported("%d channels (supported: 1..%d)", channels, audio.MaxChannels)
	}
	rate, err := d.pcmRate(t, e)
	if err != nil {
		return err
	}
	layout := audio.DefaultLayout(channels)

	switch {
	case e.canon == "alaw" || (isMS && tag == waveTagALaw):
		return d.setG711(t, g711.ALaw, codec.ALaw, rate, channels, layout)
	case e.canon == "ulaw" || (isMS && tag == waveTagMuLaw):
		return d.setG711(t, g711.MuLaw, codec.MuLaw, rate, channels, layout)
	case e.canon == "ima4":
		return d.setIMA4(t, e, rate, channels, layout)
	}
	// What is left is an "ms" entry naming one of the two WAVE-tagged ADPCM
	// families, which is the gate parseAudioSampleEntry applied to get here.
	return d.setMSADPCM(t, e, tag, rate, layout)
}

// setG711 wires an A-law or mu-law track. There is nothing to configure: the
// law is the codec ID, and one byte is one sample whatever the entry's
// samplesize field says (it is conventionally 16, the DECODED width).
func (d *Demuxer) setG711(t *track, law g711.Law, id codec.ID, rate, channels int, layout audio.ChannelMask) error {
	t.codec = id
	t.fmt = g711.Format(rate, channels, layout)
	t.sourceBits = g711.SourceBitDepth
	t.unitBytes = int64(channels)
	t.unitDur = 1
	return nil
}

// setIMA4 wires an Apple 'ima4' track, whose geometry is the fourcc's own:
// 34 bytes and 64 samples per channel, always.
//
// A version 1 entry states that geometry again in its samplesPerPacket,
// bytesPerPacket, bytesPerFrame and bytesPerSample fields, and they are NOT
// checked against it. ffmpeg writes 64, 0, 0, 2 there, so three of the four
// are zero on files every player opens; a nonzero field that disagrees is
// worth saying and not worth refusing a file over.
func (d *Demuxer) setIMA4(t *track, e pcmEntry, rate, channels int, layout audio.ChannelMask) error {
	cfg := adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: channels,
		BlockAlign: adpcm.QuickTimeBlockBytes * channels, SamplesPerBlock: adpcm.QuickTimeBlockFrames}
	if err := cfg.Validate(); err != nil {
		return err
	}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	if e.version == 1 && len(e.body) >= 44 {
		frames := int64(be32(e.body[28:32]))
		bytesPerPacket := int64(be32(e.body[32:36]))
		if frames != 0 && frames != adpcm.QuickTimeBlockFrames {
			t.addNote("ima4 entry declares %d samples per packet; the format's own geometry is %d",
				frames, adpcm.QuickTimeBlockFrames)
		}
		if bytesPerPacket != 0 && bytesPerPacket != adpcm.QuickTimeBlockBytes {
			t.addNote("ima4 entry declares %d bytes per packet; the format's own geometry is %d",
				bytesPerPacket, adpcm.QuickTimeBlockBytes)
		}
	}
	t.codec = codec.IMAADPCM
	t.codecConfig = blob
	t.fmt = cfg.Format(rate, layout)
	t.sourceBits = adpcm.SourceBitDepth
	t.unitBytes = int64(cfg.BlockBytes())
	t.unitDur = int64(cfg.SamplesPerBlock)
	t.carryState = true
	return nil
}

// setMSADPCM wires an "ms" entry carrying one of the two WAVE-tagged ADPCM
// families, whose whole geometry is in the WAVEFORMATEX inside the 'wave'
// wrapper.
//
// Refused by name when the atom is absent, rather than guessed at: unlike
// G.711 and ima4, these two state their block size and (for 0x0002) their
// predictor table nowhere else, and a sample entry's own fields say nothing
// about either.
func (d *Demuxer) setMSADPCM(t *track, e pcmEntry, tag uint16, rate int, layout audio.ChannelMask) error {
	atom, ok := e.child(e.format)
	if !ok {
		return malformed("%s sample entry has no WAVEFORMATEX atom, which is where its block geometry lives",
			codecname.Label(e.format))
	}
	ex, ok := waveformat.Parse(atom)
	if !ok {
		return malformed("%s sample entry's WAVEFORMATEX atom is %d bytes, too short to hold one",
			codecname.Label(e.format), len(atom))
	}
	if ex.Tag != tag {
		// A wrapper holding some other tag's struct is not this entry's
		// geometry, and reading it as one would invent a block size.
		return malformed("%s sample entry's WAVEFORMATEX atom declares format tag %#04x",
			codecname.Label(e.format), ex.Tag)
	}
	cfg, found, err := waveformat.ADPCM(ex, "mp4: ")
	for _, f := range found {
		if f.Note {
			t.addNote("%s", f.Msg)
			continue
		}
		if werr := d.warn(0, "%s", f.Msg); werr != nil {
			return werr
		}
	}
	if err != nil {
		return err
	}
	blob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	id := codec.IMAADPCM
	if tag == waveformat.TagMSADPCM {
		id = codec.MSADPCM
	}
	t.codec = id
	t.codecConfig = blob
	t.fmt = cfg.Format(rate, layout)
	t.sourceBits = adpcm.SourceBitDepth
	t.unitBytes = int64(cfg.BlockBytes())
	t.unitDur = int64(cfg.SamplesPerBlock)
	return nil
}
