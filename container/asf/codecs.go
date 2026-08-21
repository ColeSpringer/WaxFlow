package asf

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// waveFormat is the WAVEFORMATEX a Stream Properties Object carries as its
// type-specific data for an audio stream. The parse is local rather than
// riff's: that one exists to resolve a PCM encoding and rejects everything
// compressed, which is the opposite of what is wanted here, and entangling
// the two would make each format's quirks the other's problem.
type waveFormat struct {
	tag        uint16
	channels   int
	rate       int
	blockAlign int
	bits       int
	// extra is the codec-specific tail (cbSize bytes). It is not split out of
	// raw: the whole structure travels as Track.CodecConfig so the decoder
	// reads the fields it needs from one blob.
	extra []byte
	raw   []byte
}

// waveFormatLen is the fixed part of WAVEFORMATEX, up to and including cbSize.
const waveFormatLen = 18

// parseWaveFormat reads a WAVEFORMATEX from a Stream Properties Object's
// type-specific data. A cbSize larger than the bytes present is tolerated by
// clamping: writers have been known to overstate it, and the fixed fields are
// what select the codec.
func parseWaveFormat(b []byte) (waveFormat, bool) {
	if len(b) < waveFormatLen {
		return waveFormat{}, false
	}
	w := waveFormat{
		tag:        le.Uint16(b),
		channels:   int(le.Uint16(b[2:])),
		rate:       int(le.Uint32(b[4:])),
		blockAlign: int(le.Uint16(b[12:])),
		bits:       int(le.Uint16(b[14:])),
	}
	cb := int(le.Uint16(b[16:]))
	if cb > len(b)-waveFormatLen {
		cb = len(b) - waveFormatLen
	}
	// Copied, not sliced. b points into the whole header buffer, which holds
	// whatever cover art the Extended Content Description carried and runs to
	// megabytes; the demuxer keeps this structure for its lifetime, and a
	// server keeps a demuxer per live stream.
	w.raw = append([]byte(nil), b[:waveFormatLen+cb]...)
	w.extra = w.raw[waveFormatLen:]
	return w, true
}

// The wFormatTag values this build recognizes. Everything else is reported by
// its hexadecimal tag so a refusal still names what it found.
const (
	tagWMAV1       = 0x0160
	tagWMAV2       = 0x0161
	tagWMAPro      = 0x0162
	tagWMALossless = 0x0163
	tagWMAProSPDIF = 0x0164
	tagWMAVoice    = 0x000A
	tagWMAVoice9   = 0x000B
)

// asfCodecID maps a wFormatTag onto a waxflow codec ID, in the shape mka's
// mkvCodecID has: a recognized but undecodable codec resolves to "" with a
// name, so track selection reports what it skipped instead of "unsupported".
// WMA 1 and 2 share codec.WMA; the version rides in the WAVEFORMATEX the track
// carries as its config.
func asfCodecID(tag uint16) (codec.ID, string) {
	switch tag {
	case tagWMAV1:
		return codec.WMA, "Windows Media Audio 1"
	case tagWMAV2:
		return codec.WMA, "Windows Media Audio 2"
	case tagWMAPro, tagWMAProSPDIF:
		return "", "Windows Media Audio Pro"
	case tagWMALossless:
		return "", "Windows Media Audio Lossless"
	case tagWMAVoice, tagWMAVoice9:
		return "", "Windows Media Audio Voice"
	default:
		return "", ""
	}
}

// trackFormat builds the pipeline format for a recognized audio stream. WMA
// decodes in the float domain, so the WAVEFORMATEX bit depth (always 16 for
// the tags here, and a fiction the encoder writes rather than a property of
// the coded data) does not reach it.
//
// The layout matters even though WAVEFORMATEX's channel mask does not reach
// here: a format without one is "unknown", and the encoders that assign
// channels by name refuse it. This has to be the same layout codec/wma's
// Config.Format produces, which TestTrackFormatMatchesTheCodec pins.
func trackFormat(w waveFormat) audio.Format {
	return audio.Format{
		Rate:     w.rate,
		Channels: w.channels,
		Layout:   audio.DefaultLayout(w.channels),
		Type:     audio.Float,
		BitDepth: 32,
	}
}
