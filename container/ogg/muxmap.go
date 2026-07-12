package ogg

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// muxMapping is the codec-specific half of the Ogg muxer, the write-side mirror
// of the demux-side mapping (mapping.go). The Muxer owns page framing, packet
// batching, and the running granule; a mapping writes its header pages, reports
// each audio packet's granule increment, and states the final page's granule.
//
// The per-packet hook is load-bearing, not cosmetic. Every encoder stamps
// pkt.Dur and the muxer accumulated it generically, which is right for Opus and
// FLAC but not Ogg-Vorbis, whose granulepos carries a firstBlock/2 priming
// shift that depends on the runtime first-block size. Routing accumulation
// through writePacket lets each mapping own that accounting.
type muxMapping interface {
	codecID() codec.ID
	// writeHeaders emits the codec's header pages (identification, comment) via
	// emit, whose signature matches Muxer.emitPage. cfg is the track's
	// CodecConfig; the muxer owns the comment header, so it passes vendor and
	// tags for the mapping to build one (the encoder never sees tags).
	writeHeaders(cfg []byte, tags []container.Tag, vendor string,
		emit func(payload, seg []byte, granule int64, headerType byte) error) error
	// writePacket returns the granule increment the muxer adds for pkt. Opus
	// and FLAC return pkt.Dur unchanged.
	writePacket(pkt container.Packet) (granuleIncrement int64, err error)
	// endGranule is the final page's granule position: the gapless end point.
	endGranule(trailer codec.Trailer) int64
}

// muxMappingFor selects the mux mapping for a codec, or nil when the Ogg muxer
// does not carry it.
func muxMappingFor(id codec.ID) muxMapping {
	switch id {
	case codec.Opus:
		return opusMuxMapping{}
	case codec.FLAC:
		return flacMuxMapping{}
	}
	return nil
}

// opusMuxMapping writes the Ogg-Opus mapping (RFC 7845): an OpusHead BOS page
// and an OpusTags comment page, then audio pages whose granule is the running
// 48 kHz sample count including pre-skip.
type opusMuxMapping struct{}

func (opusMuxMapping) codecID() codec.ID { return codec.Opus }

func (opusMuxMapping) writeHeaders(cfg []byte, tags []container.Tag, vendor string,
	emit func(payload, seg []byte, granule int64, headerType byte) error) error {
	if len(cfg) < 19 || string(cfg[:8]) != "OpusHead" {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: track CodecConfig is not an OpusHead")
	}
	// BOS page: OpusHead alone.
	if err := emit(cfg, lacing(len(cfg)), 0, flagBOS); err != nil {
		return err
	}
	// Second page: the OpusTags comment header (the "OpusTags" magic followed
	// by the Vorbis-comment body).
	comment := buildComment("OpusTags", vendor, tags)
	return emit(comment, lacing(len(comment)), 0, 0)
}

func (opusMuxMapping) writePacket(pkt container.Packet) (int64, error) { return pkt.Dur, nil }

// endGranule is the pre-skip plus the true length: the final page granule the
// decoder clamps its output to (RFC 7845 section 4.4).
func (opusMuxMapping) endGranule(trailer codec.Trailer) int64 {
	return trailer.Delay + trailer.Samples
}

// flacMuxMapping writes the Xiph FLAC-in-Ogg mapping (version 1): an
// identification BOS page carrying STREAMINFO, then a VORBIS_COMMENT page, then
// audio pages whose granule is the running sample count (FLAC self-times, so
// granuleShift is 0). It reuses the existing FLAC encoder; no Vorbis encoder is
// involved.
type flacMuxMapping struct{}

func (flacMuxMapping) codecID() codec.ID { return codec.FLAC }

func (flacMuxMapping) writeHeaders(cfg []byte, tags []container.Tag, vendor string,
	emit func(payload, seg []byte, granule int64, headerType byte) error) error {
	if len(cfg) != flac.StreamInfoLen {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: FLAC CodecConfig is not a STREAMINFO block")
	}
	// Identification packet (the inverse of flacMapping.parseID): the mapping
	// header 0x7F"FLAC" version 1.0, the count of following header packets (1,
	// the comment), the native "fLaC" marker, then the STREAMINFO metadata
	// block (type 0, not the last block).
	id := []byte{0x7F, 'F', 'L', 'A', 'C', 1, 0, 0, 1, 'f', 'L', 'a', 'C',
		0x00, 0x00, 0x00, byte(flac.StreamInfoLen)}
	id = append(id, cfg...)
	if err := emit(id, lacing(len(id)), 0, flagBOS); err != nil {
		return err
	}
	// Comment packet: a VORBIS_COMMENT metadata block (type 4) marked as the
	// last block. The body is the plain Vorbis-comment structure, no magic and
	// no framing bit (FLAC metadata blocks carry neither).
	body := buildComment("", vendor, tags)
	comment := append([]byte{0x84, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	return emit(comment, lacing(len(comment)), 0, 0)
}

func (flacMuxMapping) writePacket(pkt container.Packet) (int64, error) { return pkt.Dur, nil }

// endGranule is the true length: FLAC self-times (granuleShift 0), so the final
// page granule is just the sample total, which the demuxer reads back as the
// stream length when STREAMINFO carries none.
func (flacMuxMapping) endGranule(trailer codec.Trailer) int64 { return trailer.Samples }

// buildComment builds a Vorbis-comment structure: an optional magic prefix
// (OpusTags for Opus; empty for a bare FLAC metadata-block body), the vendor
// string, and the KEY=value user comments, dropping invalid keys and any
// comment that would push the header past its single-page budget. This is the
// exact byte layout the old inline OpusTags builder produced, factored out so
// Opus stays byte-identical and FLAC reuses it.
func buildComment(magic, vendor string, tags []container.Tag) []byte {
	out := make([]byte, 0, len(magic)+4+len(vendor)+4)
	out = append(out, magic...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(vendor)))
	out = append(out, vendor...)
	countAt := len(out)
	out = binary.LittleEndian.AppendUint32(out, 0) // user comment count, patched below
	count := uint32(0)
	for _, t := range tags {
		if !container.ValidTagKey(t.Key) {
			continue
		}
		c := t.Key + "=" + t.Value
		if len(out)+4+len(c) > maxTagsPageBytes {
			// Skip just the comment that does not fit: one oversized value
			// (hostile or merely huge lyrics) must not erase the small
			// descriptive tags after it.
			continue
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(len(c)))
		out = append(out, c...)
		count++
	}
	binary.LittleEndian.PutUint32(out[countAt:], count)
	return out
}
