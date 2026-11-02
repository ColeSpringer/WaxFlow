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
	// origin is the first frame's sample position. Positions run from 0 at
	// that frame, whatever number it carries, as a native FLAC's do.
	origin int64
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

// finalizeTrack builds the track and settles its length against its frames
// (flac.Settle): the span from the first frame's own position to the end of
// the last whole frame in the file's tail, read off its header. Page granules
// play no part, so neither a lost frame nor a broken granule moves it.
//
// That length is exact on a tail read alone: the last frame's header is on a
// page whose CRC checked, and a cap only stops a decode early. A hole before
// the last page is a read's to find. No FLAC decoder trims on a granule, so a
// stream starting at another granule moves the timeline its pages state, not
// the audio.
func (m *flacMapping) finalizeTrack(first firstPage, lastEnd func() int64, r reporter) (container.Track, error) {
	f := m.si.PCMFormat()
	if err := f.Valid(); err != nil {
		return container.Track{}, container.UnusableFormat("ogg", f, err)
	}
	st, err := flac.Settle(m.codecConfig, m.origin, lastEnd())
	if err != nil {
		return container.Track{}, err
	}
	for _, n := range st.Notes {
		r.note(first.off, "%s", n)
	}
	if st.Damage != "" {
		if err := r.warn(first.off, "%s", st.Damage); err != nil {
			return container.Track{}, err
		}
	}
	return container.Track{
		Codec:        codec.FLAC,
		CodecConfig:  st.StreamInfo,
		Fmt:          f,
		Samples:      st.Samples,
		SamplesExact: st.Measured,
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
		m.origin = m.num.Start(fi)
		m.haveNum = true
	}
	if m.num.Start(fi) < m.origin {
		return 0, 0, false, false // numbered before the first frame: not this stream's
	}
	return m.num.Start(fi) - m.origin, int64(fi.BlockSize), true, true
}

func (m *flacMapping) selfTiming() bool { return true }
func (m *flacMapping) preroll() int64   { return 0 }
func (m *flacMapping) resetTiming()     {}
