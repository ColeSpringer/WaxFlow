package asf

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wma"
	"github.com/colespringer/waxflow/codec/wmalossless"
	"github.com/colespringer/waxflow/codec/wmapro"
	"github.com/colespringer/waxflow/codec/wmavoice"
	"github.com/colespringer/waxflow/waxerr"
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
	tagWMAVoice9   = 0x000A
	tagWMAVoice10  = 0x000B
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
	case tagWMAPro:
		return codec.WMAPro, "Windows Media Audio Pro"
	case tagWMAProSPDIF:
		// Pro over S/PDIF. No reference decoder reads this tag, so nothing
		// could verify a decode of one; it is named apart from the Pro this
		// build decodes so the refusal does not read as a contradiction.
		return "", "Windows Media Audio Pro over S/PDIF"
	case tagWMALossless:
		return codec.WMALossless, "Windows Media Audio Lossless"
	case tagWMAVoice9:
		return codec.WMAVoice, "Windows Media Audio Voice"
	case tagWMAVoice10:
		// The same coded layer under a second registered tag (mmreg.h's
		// WAVE_FORMAT_WMAVOICE10). No reference decoder maps it, so nothing
		// could check a decode of one; it is named apart from the Voice this
		// build decodes so the refusal does not read as a contradiction.
		return "", "Windows Media Audio Voice 10"
	default:
		return "", ""
	}
}

// codecSetup is what a recognized audio stream resolves to: the decoder to
// build and the format it emits.
type codecSetup struct {
	id  codec.ID
	fmt audio.Format
	// syncOK reports whether a decoder can begin at a media object. Nil when
	// every object is one, which is the case for WMA v1/v2: a super-frame is
	// where that decoder restarts and what the bit reservoir carries across
	// one is what the seek run-up covers.
	syncOK func([]byte) bool
	// frameLen is the sample grid a decode resumes on, or 0 when a landing is
	// wherever the object says it is. ASF states presentation times in
	// milliseconds, so an object's own time is up to a millisecond off the
	// sample the decoder will actually produce first; for a codec that resumes
	// on a frame boundary the true landing is the nearest one, and reporting
	// the object's time instead leaves the caller's pre-roll short by the
	// difference. Measured on the corpus: 48 and 58 samples, against frames of
	// 2048 and 4096.
	frameLen int
	// runUp is how far before its target a seek lands, in samples, for a codec
	// whose resumed decode converges rather than lands exact. The container
	// backs the landing up by it and the caller's pre-roll discards the run.
	runUp int
}

// resolveCodec builds the setup for a recognized audio stream, in the shape
// container/mka's resolveCodec has. The codec's own configuration check runs
// here, before a track exists, which is where every other container in this
// tree puts it: without it a header claiming six channels, 96 kHz, or
// nBlockAlign zero came back from Probe as a playable track with a duration,
// and the server's track cache remembered it that way.
//
// The format is taken FROM the parsed config rather than built beside it. That
// is not tidiness: a format built independently is a second reading of the same
// bytes, and the two readings can disagree on a field one of them takes from an
// unobvious place, which for lossless is both the depth and the channel order.
// A decoder is refused a track whose format is not its own, so a disagreement
// there is a file that probes and will not play.
func resolveCodec(id codec.ID, w waveFormat) (codecSetup, error) {
	switch id {
	case codec.WMA:
		cfg, err := wma.ParseConfig(w.raw)
		if err != nil {
			return codecSetup{}, fromCodec(err)
		}
		return codecSetup{id: id, fmt: cfg.Format()}, nil
	case codec.WMAPro:
		cfg, err := wmapro.ParseConfig(w.raw)
		if err != nil {
			return codecSetup{}, fromCodec(err)
		}
		// Every media object is a decode start point here, measured at 65
		// resume points across thirteen files, so there is no syncOK to
		// supply. The frame length is still needed: an object's presentation
		// time is in milliseconds and is not the first sample a decode resumed
		// there produces.
		return codecSetup{id: id, fmt: cfg.Format(), frameLen: cfg.SamplesPerFrame()}, nil
	case codec.WMAVoice:
		cfg, err := wmavoice.ParseConfig(w.raw)
		if err != nil {
			return codecSetup{}, fromCodec(err)
		}
		// Every media object is a decode start point, so there is no syncOK.
		// The frame length is the 480-sample SUPERFRAME rather than the
		// 160-sample frame: a resumed decode begins at the first superframe
		// that starts in the packet, and an object's millisecond presentation
		// time is not that sample. The run-up is the codec's: a resume
		// converges rather than lands exact, so a seek backs up by the
		// measured convergence and the discard covers it.
		return codecSetup{id: id, fmt: cfg.Format(), frameLen: cfg.SamplesPerSuperframe(), runUp: wmavoice.SeekRunUp}, nil
	case codec.WMALossless:
		cfg, err := wmalossless.ParseConfig(w.raw)
		if err != nil {
			return codecSetup{}, fromCodec(err)
		}
		// Lossless is the one codec here whose objects are not all start
		// points: it produces nothing until a seekable tile, which the encoder
		// emits about every four tenths of a second.
		return codecSetup{
			id: id, fmt: cfg.Format(),
			syncOK:   wmalossless.PacketIsSync,
			frameLen: cfg.SamplesPerFrame(),
		}, nil
	default:
		return codecSetup{}, unsupported("codec %q is not one this build decodes", id)
	}
}

// fromCodec gives a codec's own refusal this container's public name, which is
// the one the user sees: the driver row this package lands under is named wma.
// The code and the codec's text stay, so a caller still learns whether the file
// is damaged or merely out of scope, and errors.Is still reaches the original.
func fromCodec(err error) error {
	return waxerr.Wrap(waxerr.CodeOf(err), "wma", err)
}
