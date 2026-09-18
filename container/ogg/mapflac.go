package ogg

import (
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
)

// flacMapping decodes the Xiph FLAC-in-Ogg mapping (version 1). FLAC self-times:
// every frame header carries its sample position, so packetTiming reads it
// directly and seeks need no pre-roll.
type flacMapping struct {
	si          flac.StreamInfo
	codecConfig []byte
	num         flac.Numbering
	haveNum     bool
}

func (m *flacMapping) codecID() codec.ID { return codec.FLAC }

// parseID parses the Ogg-FLAC identification packet: the 0x7F FLAC signature,
// mapping version, header count, and an embedded fLaC marker with STREAMINFO.
func (m *flacMapping) parseID(pkt []byte) (int, error) {
	const want = 13 + 4 + flac.StreamInfoLen
	if len(pkt) < want {
		return 0, malformed("FLAC identification packet of %d bytes, want %d", len(pkt), want)
	}
	if pkt[5] != 1 {
		return 0, unsupported("unsupported Ogg-FLAC mapping version %d.%d", pkt[5], pkt[6])
	}
	headerPackets := int(pkt[7])<<8 | int(pkt[8])
	if string(pkt[9:13]) != "fLaC" {
		return 0, malformed("identification packet lacks the fLaC marker")
	}
	if typ := pkt[13] & 0x7F; typ != 0 {
		return 0, malformed("first metadata block is type %d, want STREAMINFO", typ)
	}
	siRaw := append([]byte(nil), pkt[17:17+flac.StreamInfoLen]...)
	si, err := flac.ParseStreamInfo(siRaw)
	if err != nil {
		return 0, err
	}
	m.si = si
	m.codecConfig = siRaw
	if headerPackets == 0 {
		return detectHeaders, nil
	}
	return headerPackets, nil
}

// parseHeader ignores FLAC metadata block packets; STREAMINFO from the
// identification packet is all the decoder needs.
func (m *flacMapping) parseHeader([]byte) error { return nil }

func (m *flacMapping) isAudio(pkt []byte) bool { return flac.SyncOK(pkt) }

// finalizeTrack builds the track and settles its length against the stream's
// final page granule, in both directions, exactly as the native FLAC and
// WavPack opens settle theirs against the payload.
//
// The granule is always read. STREAMINFO's total is the encoder's back-patched
// claim, written once the input ran out, and a file cut short afterwards still
// carries it; taking it on trust hands every caller a duration no read can
// reach, and hands strict mode a damaged file it has nothing to object to. A
// stream whose pages run past it is the mirror image. Five outcomes: a
// shortfall warns (damage) and adopts the granule, an overrun notes and adopts
// it, equality confirms, a declared total of zero is not a claim at all and
// simply takes the granule, and a tail with no page of this serial keeps the
// declared count unflagged, since nothing was found to check it against. A
// stream that declares nothing *and* has no granule to find is unknown (-1),
// not empty.
//
// A length found this way is exact, and that is sound on a tail read alone
// where flacn's is not. The granule is the muxer's own statement of the
// playable end, on a page whose CRC checked out, which is the very basis the
// Opus and Vorbis mappings already promote on; and a cap only ever stops a
// decode early, never extends one. Like flacn's open, a tail read cannot see a
// hole before the last page: a read finds anything earlier, where it is.
func (m *flacMapping) finalizeTrack(lastGranule func() int64, r reporter) (container.Track, error) {
	f := m.si.PCMFormat()
	if err := f.Valid(); err != nil {
		return container.Track{}, container.UnusableFormat("ogg", f, err)
	}
	samples, exact := m.si.Samples, false
	switch granule := lastGranule(); {
	case granule < 0 && samples == 0:
		// Nothing declared and nothing found: unknown, which is what the Opus
		// and Vorbis mappings report in the same position. Reporting the zero
		// through would call the track empty, which is a different claim and a
		// worse one.
		samples = -1
	case granule < 0:
		// No page of this serial in the tail: nothing to check against, so the
		// declared count stands as the claim it always was.
	case samples == 0:
		// Streaming muxers leave STREAMINFO's total at zero, which is not a
		// claim; ffmpeg's Ogg-FLAC output is this shape.
		samples, exact = granule, true
	case granule < samples:
		if err := r.warn(0, "STREAMINFO declares %d samples but the final page granule is %d",
			samples, granule); err != nil {
			return container.Track{}, err
		}
		samples, exact = granule, true
	case granule > samples:
		r.note(0, "STREAMINFO declares %d samples but the final page granule is %d",
			samples, granule)
		samples, exact = granule, true
	default:
		exact = true
	}
	return container.Track{
		Codec:        codec.FLAC,
		CodecConfig:  m.codecConfig,
		Fmt:          f,
		Samples:      samples,
		SamplesExact: exact,
		Default:      true,
	}, nil
}

func (m *flacMapping) packetTiming(pkt []byte, _ int64) (pts, dur int64, sync, ok bool) {
	fi, err := flac.ParseFrameHeader(pkt)
	if err != nil {
		return 0, 0, false, false
	}
	if !m.haveNum {
		m.num = m.si.Numbering(fi)
		m.haveNum = true
	}
	return m.num.Start(fi), int64(fi.BlockSize), true, true
}

func (m *flacMapping) selfTiming() bool { return true }
func (m *flacMapping) preroll() int64   { return 0 }
func (m *flacMapping) resetTiming()     {}
