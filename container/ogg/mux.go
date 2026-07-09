package ogg

import (
	"encoding/binary"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// Muxer writes an Ogg-Opus stream (RFC 7845): an OpusHead page, an OpusTags
// page, then audio pages batching multiple Opus packets each. The header
// pages and the first audio page are small so the first audio flushes
// quickly (TTFA); later packets batch until a page nears the streaming
// target size, one second of audio, or the segment-table limit, keeping the
// per-page framing overhead a fraction of a percent instead of the ~11% a
// page per 20 ms packet would cost. Pages carry a 48 kHz granule position;
// the page being batched is held until End can stamp the final page's
// granule with the end-trim and the end-of-stream flag. The muxer needs no
// seeking, so it streams live.
type Muxer struct {
	w      io.Writer
	serial uint32
	seq    uint32
	head   []byte // OpusHead, from the track's CodecConfig
	vendor string

	granule int64 // cumulative decoded samples (48 kHz, includes pre-skip)
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

// MuxerOptions configures the Ogg-Opus muxer.
type MuxerOptions struct {
	// Vendor is the OpusTags vendor string (defaults to "WaxFlow").
	Vendor string
	// Serial overrides the logical bitstream serial number (0 uses the default).
	Serial uint32
}

// oggOpusSerial is the default logical-stream serial ("Opus"), fixed so
// deterministic-mode output is byte-reproducible.
const oggOpusSerial = 0x4F707573

// NewMuxer returns an Ogg-Opus muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, serial: oggOpusSerial, vendor: "WaxFlow"}
	if opts != nil {
		if opts.Vendor != "" {
			m.vendor = opts.Vendor
		}
		if opts.Serial != 0 {
			m.serial = opts.Serial
		}
	}
	return m
}

// NeedsSeek is false: Ogg-Opus writes a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin writes the OpusHead and OpusTags header pages.
func (m *Muxer) Begin(tracks []container.Track) error {
	if len(tracks) != 1 || tracks[0].Codec != codec.Opus {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: muxer needs a single Opus track")
	}
	head := tracks[0].CodecConfig
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: track CodecConfig is not an OpusHead")
	}
	m.head = head
	m.begun = true
	// BOS page: OpusHead alone.
	if err := m.emitPage(head, lacing(len(head)), 0, flagBOS); err != nil {
		return err
	}
	// Second page: the OpusTags comment header.
	tags := make([]byte, 0, 8+4+len(m.vendor)+4)
	tags = append(tags, "OpusTags"...)
	tags = binary.LittleEndian.AppendUint32(tags, uint32(len(m.vendor)))
	tags = append(tags, m.vendor...)
	tags = binary.LittleEndian.AppendUint32(tags, 0) // user comment count
	return m.emitPage(tags, lacing(len(tags)), 0, 0)
}

// WritePacket adds an Opus packet to the page being batched, first flushing
// that page when the packet would not fit (segment table, byte target, or
// duration cap) or when it is the stream's first audio page, which stays a
// single packet so audio reaches the client right after the headers. The
// batch always keeps at least the newest packet, so End can stamp the final
// page's granule with the end-trim.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.begun {
		return waxerr.New(waxerr.CodeInternal, "ogg: WritePacket before Begin")
	}
	segs := len(pkt.Data)/255 + 1
	if len(m.pending) > 0 &&
		(m.audioPages == 0 ||
			len(m.pendingSeg)+segs > maxPageSegEntries ||
			len(m.pending)+len(pkt.Data) > pageTargetBytes ||
			m.pendingDur+pkt.Dur > maxPageGranules) {
		if err := m.flushPending(0); err != nil {
			return err
		}
	}
	m.granule += pkt.Dur
	m.pending = append(m.pending, pkt.Data...)
	m.pendingSeg = appendLacing(m.pendingSeg, len(pkt.Data))
	m.pendingGranule = m.granule
	m.pendingDur += pkt.Dur
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

// End writes the final page with the gapless end granule and the EOS flag.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.begun {
		return waxerr.New(waxerr.CodeInternal, "ogg: End before Begin")
	}
	endGranule := trailer.Delay + trailer.Samples
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
