package ogg

import (
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
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
		return 0, malformed("unsupported Ogg-FLAC mapping version %d.%d", pkt[5], pkt[6])
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

func (m *flacMapping) finalizeTrack(lastGranule func() int64) (container.Track, error) {
	f := m.si.PCMFormat()
	if err := f.Valid(); err != nil {
		return container.Track{}, waxerr.Wrap(waxerr.CodeUnsupportedFormat, "ogg: unusable format", err)
	}
	samples := m.si.Samples
	if samples == 0 {
		// Streaming muxers leave STREAMINFO's total at zero; only then pay for
		// the tail scan to read the length from the last page's granule.
		samples = lastGranule()
	}
	return container.Track{
		Codec:       codec.FLAC,
		CodecConfig: m.codecConfig,
		Fmt:         f,
		Samples:     samples,
		Default:     true,
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

func (m *flacMapping) selfTiming() bool    { return true }
func (m *flacMapping) preroll() int64      { return 0 }
func (m *flacMapping) granuleShift() int64 { return 0 }
func (m *flacMapping) resetTiming()        {}
