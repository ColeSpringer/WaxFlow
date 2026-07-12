package ogg

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Muxer writes an Ogg stream: header pages (identification, comment), then
// audio pages batching multiple packets each. The codec-specific part (which
// header pages, how a packet advances the granule, the final granule) lives in
// a muxMapping selected from the track codec at Begin; Opus and FLAC are wired.
// The header pages and the first audio page are small so the first audio
// flushes quickly (TTFA); later packets batch until a page nears the streaming
// target size, one second of audio, or the segment-table limit, keeping the
// per-page framing overhead a fraction of a percent instead of the ~11% a page
// per 20 ms packet would cost. The page being batched is held until End can
// stamp the final page's granule with the mapping's end position and the
// end-of-stream flag. The muxer needs no seeking, so it streams live.
type Muxer struct {
	w       io.Writer
	serial  uint32
	seq     uint32
	vendor  string
	tags    []container.Tag
	mapping muxMapping

	granule int64 // cumulative decoded samples (the mapping's granule timeline)
	// The audio page being batched: packet payloads, their segment table,
	// the granule after the last batched packet, and the batch's duration.
	pending        []byte
	pendingSeg     []byte
	pendingGranule int64
	pendingDur     int64
	audioPages     int // audio pages flushed so far (the first stays small)
	begun          bool
}

// Page batching bounds. The byte target keeps pages streaming-friendly (the
// size reference Ogg muxers aim for); the duration cap bounds a page's worth
// of very small packets (silence) to one second, matching opusenc's default
// maximum page delay. The segment table itself allows at most 255 entries.
const (
	pageTargetBytes   = 4096
	maxPageGranules   = 48000
	maxPageSegEntries = 255
)

// MuxerOptions configures the Ogg muxer.
type MuxerOptions struct {
	// Vendor is the comment-header vendor string (defaults to "WaxFlow").
	Vendor string
	// Serial overrides the logical bitstream serial number (0 uses the default).
	Serial uint32
	// Tags are written as comment-header user comments in order. Vorbis
	// comments are the canonical vocabulary already, so every key passes
	// through as KEY=value.
	Tags []container.Tag
}

// oggOpusSerial is the default logical-stream serial ("Opus"), fixed so
// deterministic-mode output is byte-reproducible. The value is the same for
// every mapping; the serial only needs to be stable within one stream.
const oggOpusSerial = 0x4F707573

// NewMuxer returns an Ogg muxer writing to w. The codec (and so the mapping) is
// selected from the track at Begin.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, serial: oggOpusSerial, vendor: "WaxFlow"}
	if opts != nil {
		if opts.Vendor != "" {
			m.vendor = opts.Vendor
		}
		if opts.Serial != 0 {
			m.serial = opts.Serial
		}
		m.tags = opts.Tags
	}
	return m
}

// NeedsSeek is false: Ogg-Opus writes a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin selects the codec's mapping and writes its header pages.
func (m *Muxer) Begin(tracks []container.Track) error {
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: muxer needs a single track")
	}
	m.mapping = muxMappingFor(tracks[0].Codec)
	if m.mapping == nil {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ogg: cannot mux codec %q (opus, flac)", tracks[0].Codec))
	}
	m.begun = true
	// The mapping emits its identification and comment pages; comments stay
	// bounded (maxTagsPageBytes) so the comment header always fits one page's
	// segment table (255*255 bytes) and the pre-audio headers stay small.
	return m.mapping.writeHeaders(tracks[0].CodecConfig, m.tags, m.vendor, m.emitPage)
}

// maxTagsPageBytes bounds a comment header so it stays a single page (the
// segment table caps a page at 255*255 payload bytes) and keeps the pre-audio
// headers small for time-to-first-audio.
const maxTagsPageBytes = 48 << 10

// WritePacket adds a packet to the page being batched, first flushing that page
// when the packet would not fit (segment table, byte target, or duration cap)
// or when it is the stream's first audio page, which stays a single packet so
// audio reaches the client right after the headers. The granule increment comes
// from the mapping (pkt.Dur for Opus and FLAC), so accumulation stays codec
// correct. The batch always keeps at least the newest packet, so End can stamp
// the final page's granule with the end position.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.begun {
		return waxerr.New(waxerr.CodeInternal, "ogg: WritePacket before Begin")
	}
	inc, err := m.mapping.writePacket(pkt)
	if err != nil {
		return err
	}
	segs := len(pkt.Data)/255 + 1
	if len(m.pending) > 0 &&
		(m.audioPages == 0 ||
			len(m.pendingSeg)+segs > maxPageSegEntries ||
			len(m.pending)+len(pkt.Data) > pageTargetBytes ||
			m.pendingDur+inc > maxPageGranules) {
		if err := m.flushPending(0); err != nil {
			return err
		}
	}
	m.granule += inc
	m.pending = append(m.pending, pkt.Data...)
	m.pendingSeg = appendLacing(m.pendingSeg, len(pkt.Data))
	m.pendingGranule = m.granule
	m.pendingDur += inc
	return nil
}

// flushPending writes the batched page and resets the batch.
func (m *Muxer) flushPending(headerType byte) error {
	err := m.emitPage(m.pending, m.pendingSeg, m.pendingGranule, headerType)
	m.pending = m.pending[:0]
	m.pendingSeg = m.pendingSeg[:0]
	m.pendingDur = 0
	m.audioPages++
	return err
}

// End writes the final page with the mapping's end granule and the EOS flag.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.begun {
		return waxerr.New(waxerr.CodeInternal, "ogg: End before Begin")
	}
	endGranule := m.mapping.endGranule(trailer)
	if len(m.pending) == 0 {
		// No audio: emit an empty EOS page so the stream is well-formed.
		return m.emitPage(nil, lacing(0), endGranule, flagEOS)
	}
	m.pendingGranule = endGranule
	return m.flushPending(flagEOS)
}

// emitPage writes one Ogg page carrying the payload described by the segment
// table (one or more whole packets; the batching bounds keep every packet
// completed within its page).
func (m *Muxer) emitPage(payload, seg []byte, granule int64, headerType byte) error {
	header := make([]byte, headerLen+len(seg))
	copy(header, "OggS")
	header[4] = 0 // stream structure version
	header[5] = headerType
	binary.LittleEndian.PutUint64(header[6:], uint64(granule))
	binary.LittleEndian.PutUint32(header[14:], m.serial)
	binary.LittleEndian.PutUint32(header[18:], m.seq)
	// header[22:26] checksum stays zero for the CRC pass.
	header[26] = byte(len(seg))
	copy(header[headerLen:], seg)
	crc := crc32(0, header)
	crc = crc32(crc, payload)
	binary.LittleEndian.PutUint32(header[22:], crc)
	m.seq++
	if _, err := m.w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := m.w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// lacing builds the Ogg segment table for a single packet of n bytes: as many
// 255-byte segments as needed plus a shorter terminator (a 0 when n is an exact
// multiple of 255).
func lacing(n int) []byte {
	return appendLacing(make([]byte, 0, n/255+1), n)
}

// appendLacing appends one packet's segment values to a page's table.
func appendLacing(seg []byte, n int) []byte {
	for n >= 255 {
		seg = append(seg, 255)
		n -= 255
	}
	return append(seg, byte(n))
}
