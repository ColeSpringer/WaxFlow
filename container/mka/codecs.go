package mka

import (
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/waxerr"
)

// trackEntry is the subset of a Matroska TrackEntry the demuxer keeps: enough
// to select the audio track and wire its decoder.
type trackEntry struct {
	number      uint64
	trackType   uint64
	codecID     string
	codecName   string // trimmed codecID prefix, for diagnostics
	codecPriv   []byte
	rate        int
	channels    int
	bitDepth    int
	codecDelay  int64 // nanoseconds
	seekPreRoll int64 // nanoseconds
	def         bool
}

// codecSetup is the result of resolving a TrackEntry to a wired codec: the
// container.Track fields plus the state per-frame durations need.
type codecSetup struct {
	id     codec.ID
	config []byte
	fmt    audio.Format

	pcmBytesPerFrame int
	aacFrameLength   int
	vorbisCfg        vorbis.Config
	vorbisModeBits   int
	haveVorbis       bool

	// warning is a note the resolving demuxer records once the track is
	// selected, for a limitation knowable from the codec config alone (an
	// explicitly signalled HE-AAC band limit). Empty for a clean track.
	warning string
}

// mkvCodecs maps a Matroska CodecID to a waxflow codec.ID. A recognized but
// unwired codec (ALAC, MP3, ...) resolves to its name so track selection can
// report why it was skipped; an unknown one stays "".
func mkvCodecID(codecID string) codec.ID {
	switch {
	case codecID == "A_OPUS":
		return codec.Opus
	case codecID == "A_VORBIS":
		return codec.Vorbis
	case codecID == "A_FLAC":
		return codec.FLAC
	case codecID == "A_AAC" || hasPrefix(codecID, "A_AAC/"):
		return codec.AACLC
	case hasPrefix(codecID, "A_PCM/"):
		return codec.PCM
	default:
		return ""
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// resolveCodec builds the codec setup for an audio TrackEntry with one of the
// five wired codecs. The container's CodecPrivate is translated to the
// Track.CodecConfig the matching decoder parses.
func resolveCodec(t *trackEntry) (codecSetup, error) {
	switch mkvCodecID(t.codecID) {
	case codec.Opus:
		return setupOpus(t)
	case codec.Vorbis:
		return setupVorbis(t)
	case codec.FLAC:
		return setupFLAC(t)
	case codec.AACLC:
		return setupAAC(t)
	case codec.PCM:
		return setupPCM(t)
	default:
		return codecSetup{}, malformed("codec %q is not one this build decodes", t.codecID)
	}
}

// setupOpus wires an Opus track: CodecPrivate is the OpusHead the decoder and
// the Ogg mapping both parse, passed through unchanged.
func setupOpus(t *trackEntry) (codecSetup, error) {
	cfg, err := opus.ParseOpusHead(t.codecPriv)
	if err != nil {
		return codecSetup{}, err
	}
	return codecSetup{id: codec.Opus, config: t.codecPriv, fmt: cfg.Format()}, nil
}

// setupVorbis wires a Vorbis track: CodecPrivate is the Xiph-laced trio of
// header packets, the exact blob PackHeaders produces, so it is the codec
// config directly. The parsed config also drives per-packet durations.
func setupVorbis(t *trackEntry) (codecSetup, error) {
	cfg, err := vorbis.ParseConfig(t.codecPriv)
	if err != nil {
		return codecSetup{}, err
	}
	return codecSetup{
		id:             codec.Vorbis,
		config:         t.codecPriv,
		fmt:            cfg.Format(),
		vorbisCfg:      cfg,
		vorbisModeBits: vorbis.ModeBits(cfg),
		haveVorbis:     true,
	}, nil
}

// setupFLAC wires a FLAC track: CodecPrivate is the native fLaC header, from
// which the 34-byte STREAMINFO the decoder needs is extracted.
func setupFLAC(t *trackEntry) (codecSetup, error) {
	si, err := flacStreamInfo(t.codecPriv)
	if err != nil {
		return codecSetup{}, err
	}
	parsed, err := flac.ParseStreamInfo(si)
	if err != nil {
		return codecSetup{}, err
	}
	return codecSetup{id: codec.FLAC, config: si, fmt: parsed.PCMFormat()}, nil
}

// setupAAC wires an AAC-LC track: CodecPrivate is the AudioSpecificConfig the
// decoder parses. The ASC is authoritative for rate and channels; the Audio
// element's channel count fills in only when the ASC left it implicit.
func setupAAC(t *trackEntry) (codecSetup, error) {
	if len(t.codecPriv) == 0 {
		return codecSetup{}, malformed("AAC track has no CodecPrivate (AudioSpecificConfig)")
	}
	cfg, err := aac.ParseASC(t.codecPriv)
	if err != nil {
		return codecSetup{}, err
	}
	// ParseASC leaves Channels 0 for an in-band PCE and refuses more than two
	// otherwise, because the decoder emits channels in bitstream order with no
	// WAV remap. Fill from the Audio element only within that mono/stereo
	// support, so a PCE track the container claims is multichannel is refused
	// (Format().Valid fails on 0 channels) rather than decoded out of order.
	if cfg.Channels == 0 && t.channels >= 1 && t.channels <= 2 {
		cfg.Channels = t.channels
	}
	return codecSetup{
		id:             codec.AACLC,
		config:         t.codecPriv,
		fmt:            cfg.Format(),
		aacFrameLength: cfg.FrameLength,
		warning:        cfg.SBRWarning(),
	}, nil
}

// preRoll is the sample count a seek should land before its target so an
// overlap-add decoder has rebuilt its inter-frame state by the first delivered
// sample. FLAC and PCM carry no such state and need none.
func (s codecSetup) preRoll() int64 {
	switch s.id {
	case codec.Opus:
		return 3840 // 80 ms at 48 kHz (RFC 7845)
	case codec.Vorbis:
		if s.haveVorbis {
			return int64(s.vorbisCfg.LongBlock())
		}
	case codec.AACLC:
		return 1024 // one IMDCT frame of overlap history
	}
	return 0
}

// setupPCM wires a raw-PCM track from the CodecID (endianness and encoding)
// and the Audio element (rate, channels, bit depth).
func setupPCM(t *trackEntry) (codecSetup, error) {
	var enc pcm.Encoding
	switch t.codecID {
	case "A_PCM/INT/LIT":
		enc = pcm.SignedInt
	case "A_PCM/INT/BIG":
		enc = pcm.SignedInt
	case "A_PCM/FLOAT/IEEE":
		enc = pcm.Float
	default:
		return codecSetup{}, malformed("unsupported PCM flavor %q", t.codecID)
	}
	if t.bitDepth == 0 {
		return codecSetup{}, malformed("PCM track has no BitDepth")
	}
	c := pcm.Config{Encoding: enc, Bits: t.bitDepth, BigEndian: t.codecID == "A_PCM/INT/BIG"}
	cfgBytes, err := c.MarshalBinary()
	if err != nil {
		return codecSetup{}, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "mka: unusable PCM config", err)
	}
	if t.channels < 1 || t.channels > audio.MaxChannels {
		return codecSetup{}, malformed("PCM track with %d channels", t.channels)
	}
	f := c.PCMFormat(t.rate, t.channels, audio.DefaultLayout(t.channels))
	return codecSetup{
		id:               codec.PCM,
		config:           cfgBytes,
		fmt:              f,
		pcmBytesPerFrame: c.BytesPerFrame(t.channels),
	}, nil
}

// flacStreamInfo extracts the 34-byte STREAMINFO body from a Matroska A_FLAC
// CodecPrivate. The canonical form is the native "fLaC" magic followed by the
// STREAMINFO metadata block; a bare 34-byte body from a terse muxer is
// tolerated.
func flacStreamInfo(priv []byte) ([]byte, error) {
	b := priv
	if len(b) >= 4 && string(b[:4]) == "fLaC" {
		b = b[4:]
	}
	if len(b) == flac.StreamInfoLen {
		return append([]byte(nil), b...), nil // bare STREAMINFO body
	}
	// Metadata block: a 1-byte type/flags header, a 24-bit length, then body.
	if len(b) < 4 {
		return nil, malformed("FLAC CodecPrivate of %d bytes too short", len(priv))
	}
	if typ := b[0] & 0x7F; typ != 0 {
		return nil, malformed("FLAC CodecPrivate first block is type %d, want STREAMINFO", typ)
	}
	length := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if length != flac.StreamInfoLen || 4+length > len(b) {
		return nil, malformed("FLAC CodecPrivate STREAMINFO length %d", length)
	}
	return append([]byte(nil), b[4:4+length]...), nil
}

// frameSamples returns one codec frame's output length in samples, in the
// codec's decode timeline. It drives per-packet PTS/Dur and, for gapless
// tracks, the raw-total pass; Vorbis carries state across the call, so a seek
// restart resets it (resetTiming). A frame it cannot measure returns 0 rather
// than failing: only Opus (the gapless codec) needs an exact answer, and the
// others' durations are informational.
func (d *Demuxer) frameSamples(data []byte) int64 {
	switch d.setup.id {
	case codec.Opus:
		n, err := opus.PacketSamples(data)
		if err != nil {
			return 0
		}
		return int64(n)
	case codec.AACLC:
		// The ASC's frame length (1024, or 960 with the short-frame flag), so
		// the count tracks whatever the track declares rather than assuming.
		if d.setup.aacFrameLength > 0 {
			return int64(d.setup.aacFrameLength)
		}
		return 1024
	case codec.PCM:
		if d.setup.pcmBytesPerFrame <= 0 {
			return 0
		}
		return int64(len(data) / d.setup.pcmBytesPerFrame)
	case codec.FLAC:
		fi, err := flac.ParseFrameHeader(data)
		if err != nil {
			return 0
		}
		return int64(fi.BlockSize)
	case codec.Vorbis:
		if !d.setup.haveVorbis {
			return 0
		}
		block, ok := vorbis.PacketBlockSize(d.setup.vorbisCfg, d.setup.vorbisModeBits, data)
		if !ok {
			return 0
		}
		dur := int64(0)
		if d.vorbisPrevBlock != 0 {
			dur = int64(d.vorbisPrevBlock+block) / 4
		}
		d.vorbisPrevBlock = block
		return dur
	default:
		return 0
	}
}
