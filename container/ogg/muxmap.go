package ogg

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/vorbis"
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
	case codec.Vorbis:
		return &vorbisMuxMapping{}
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

// vorbisMuxMapping writes the Ogg-Vorbis mapping (Vorbis I spec section A.2):
// an identification BOS page, then a page carrying the comment and setup headers
// together (the standard libvorbis layout), then audio pages. Vorbis timing
// accumulates and its granulepos carries a firstBlock/2 priming shift, so this
// mapping owns the per-packet granule accounting rather than trusting pkt.Dur:
// the first (priming) audio packet advances the granule by firstBlock/2 (the
// half-block the decoder consumes but never emits), and each later packet by the
// (prevBlock+block)/4 telescoping increment. The muxer never sees tags on the
// encoder side; this mapping rebuilds the comment header from vendor+tags exactly
// as the Opus mapping rebuilds OpusTags.
type vorbisMuxMapping struct {
	cfg        vorbis.Config
	modeBits   int
	haveCfg    bool
	firstBlock int // first audio packet's block size, for the priming shift
	prevBlock  int // previous audio packet's block size, 0 before the first
}

func (m *vorbisMuxMapping) codecID() codec.ID { return codec.Vorbis }

func (m *vorbisMuxMapping) writeHeaders(cfg []byte, tags []container.Tag, vendor string,
	emit func(payload, seg []byte, granule int64, headerType byte) error) error {
	id, _, setup, err := vorbis.SplitConfig(cfg)
	if err != nil {
		return err
	}
	if len(id) < 7 || id[0] != 0x01 || string(id[1:7]) != "vorbis" {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: track CodecConfig is not a Vorbis identification header")
	}
	// Parse the config now so writePacket can read each packet's block size
	// (ModeBits + PacketBlockSize) without a full decode.
	c, err := vorbis.ParseConfig(cfg)
	if err != nil {
		return err
	}
	m.cfg = c
	m.modeBits = vorbis.ModeBits(c)
	m.haveCfg = true

	// BOS page: the identification packet alone (the demuxer sniffs it as the
	// whole page body, so it must not share a page).
	if err := emit(id, lacing(len(id)), 0, flagBOS); err != nil {
		return err
	}
	// Rebuild the comment header from vendor+tags: type byte 0x03, the "vorbis"
	// signature, the Vorbis-comment structure, then the framing bit (a
	// byte-aligned 0x01), matching the encoder's commentHeader byte-for-byte.
	comment := append([]byte{0x03, 'v', 'o', 'r', 'b', 'i', 's'}, buildComment("", vendor, tags)...)
	comment = append(comment, 0x01)
	// The comment and setup headers share the next page(s), spilling only if
	// setup is large enough to overflow one page's 255x255-byte segment table.
	return emitHeaderPages([][]byte{comment, setup}, emit)
}

// writePacket returns pkt's granule increment. The first audio packet primes the
// decoder overlap and emits nothing, but advances the granule by firstBlock/2
// (the demuxer's granuleShift), so the granule timeline stays firstBlock/2 ahead
// of the decoded output the way real Ogg-Vorbis granulepos does; from the second
// packet on the increment is the (prevBlock+block)/4 the decoder actually emits.
func (m *vorbisMuxMapping) writePacket(pkt container.Packet) (int64, error) {
	if !m.haveCfg {
		return 0, waxerr.New(waxerr.CodeInternal, "ogg: vorbis writePacket before writeHeaders")
	}
	block, ok := vorbis.PacketBlockSize(m.cfg, m.modeBits, pkt.Data)
	if !ok {
		return 0, waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: not a valid Vorbis audio packet")
	}
	var inc int64
	if m.prevBlock == 0 {
		m.firstBlock = block
		inc = int64(block / 2) // the priming half-block shift
	} else {
		inc = int64(m.prevBlock+block) / 4
	}
	m.prevBlock = block
	return inc, nil
}

// endGranule is the true length plus the priming shift: the final page granule
// the decoder end-trims to, so the demuxer recovers Samples exactly as
// lastGranule - firstBlock/2 (finalizeTrack in mapvorbis.go).
func (m *vorbisMuxMapping) endGranule(trailer codec.Trailer) int64 {
	return trailer.Samples + int64(m.firstBlock/2)
}

// emitHeaderPages frames whole header packets into Ogg pages via emit, packing as
// many packets per page as the 255-entry segment table allows and continuing an
// oversized packet onto a flagContinued page. Header pages carry granule 0 and no
// BOS/EOS flag (the caller emits the BOS identification page itself). It mirrors
// the generic reassembly the demuxer performs, so comment+setup ride one page in
// the common case yet a huge setup still frames legally.
func emitHeaderPages(pkts [][]byte,
	emit func(payload, seg []byte, granule int64, headerType byte) error) error {
	var body, laces []byte
	for _, pkt := range pkts {
		body = append(body, pkt...)
		laces = appendLacing(laces, len(pkt))
	}
	byteOff := 0
	for i := 0; i < len(laces); {
		end := min(i+maxPageSegEntries, len(laces))
		seg := laces[i:end]
		n := 0
		for _, l := range seg {
			n += int(l)
		}
		flags := byte(0)
		if i > 0 && laces[i-1] == 255 {
			flags = flagContinued // the previous page ended mid-packet
		}
		if err := emit(body[byteOff:byteOff+n], seg, 0, flags); err != nil {
			return err
		}
		byteOff += n
		i = end
	}
	return nil
}

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
