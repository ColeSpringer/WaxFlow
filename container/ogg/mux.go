package ogg

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/muxseek"
	"github.com/colespringer/waxflow/waxerr"
)

// MuxerVersion is this muxer's term in the ADR-0004 cache key: a cached
// output of it regenerates when this bumps. Bump it for a change to what
// gets written around unchanged encoded packets, which is the one kind of
// change no encoder or DSP version can notice.
//
// ogg-mux-2: the FLAC mapping stamps the run's projected length into
// STREAMINFO instead of passing the source's total through, and a seekable
// destination is patched at End. It is one constant for the whole muxer, so
// every cached Ogg-Opus and Ogg-Vorbis output regenerates too although its
// bytes do not change, which ADR-0004 accepts: over-keying costs a
// regeneration, under-keying serves a wrong file.
const MuxerVersion = "ogg-mux-2"

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
// end-of-stream flag. The muxer needs no seeking, so it streams live: a plain
// io.Writer gets a compliant stream whose length is the final page's granule.
// A destination it can seek gets one more thing, and only for FLAC, which is
// the one mapping whose headers state a sample total: the BOS page's
// STREAMINFO is back-patched at End with the length the run produced and the
// encoder's signature, so the output ends fully declared the way the reference
// `flac --ogg` writes one.
type Muxer struct {
	w       io.Writer
	patch   muxseek.Patcher
	serial  uint32
	seq     uint32
	vendor  string
	tags    []container.Tag
	md5     func() [16]byte
	mapping muxMapping

	off int64 // bytes written so far, the stream offset Resume takes
	// What the mapping's headers declared about the run's length, and the BOS
	// page they went out in. Zero when the mapping states no total (Opus and
	// Vorbis), in which case End has nothing to make good.
	wroteTotal int64
	bosPage    []byte
	// The smallest and largest packet written, which for FLAC are STREAMINFO's
	// frame-size bounds. Zero until a packet lands.
	minFrame, maxFrame int

	granule int64 // cumulative decoded samples (the mapping's granule timeline)
	// The audio page being batched: packet payloads, their segment table,
	// the granule after the last batched packet, and the batch's duration.
	pending        []byte
	pendingSeg     []byte
	pendingGranule int64
	pendingDur     int64
	audioPages     int // audio pages flushed so far (the first stays small)
	begun          bool
	ended          bool
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
	// MD5 is the encoder's signature over the decoded samples, read by the
	// FLAC mapping alone: it is the one mapping here with a field to put one
	// in (STREAMINFO's). The Opus and Vorbis output rows pass none on purpose,
	// since their headers have nowhere for it to go.
	//
	// A remux leaves it nil and the source's own signature stands, which is
	// right because the packets carry the same audio it was computed over.
	MD5 func() [16]byte
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
		m.md5 = opts.MD5
	}
	return m
}

// NeedsSeek is false: Ogg-Opus writes a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin selects the codec's mapping and writes its header pages.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.begun || m.ended {
		return waxerr.New(waxerr.CodeInternal, "ogg: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ogg: muxer needs a single track")
	}
	m.mapping = muxMappingFor(tracks[0].Codec)
	if m.mapping == nil {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ogg: cannot mux codec %q (opus, flac, vorbis)", tracks[0].Codec))
	}
	// Probed here rather than in NewMuxer: the base a Patcher records is where
	// the destination sits now, so probing earlier would fix it before the
	// caller had finished positioning (see muxseek).
	m.patch = muxseek.New(m.w, "ogg")
	m.begun = true
	// The mapping emits its identification and comment pages; comments stay
	// bounded (maxTagsPageBytes) so the comment header always fits one page's
	// segment table (255*255 bytes) and the pre-audio headers stay small.
	total, err := m.mapping.writeHeaders(tracks[0], m.tags, m.vendor, m.emitPage)
	m.wroteTotal = total
	return err
}

// maxTagsPageBytes bounds a comment header so it stays a single page (the
// segment table caps a page at 255*255 payload bytes) and keeps the pre-audio
// headers small for time-to-first-audio.
//
// It is the smallest of the tag caps, which is why meta.EmbeddableTags trims a
// projected source tag set to it: a value that fits here fits every embedding
// output.
const maxTagsPageBytes = 48 << 10

// WritePacket adds a packet to the page being batched, first flushing that page
// when the packet would not fit (segment table, byte target, or duration cap)
// or when it is the stream's first audio page, which stays a single packet so
// audio reaches the client right after the headers. The granule increment comes
// from the mapping (pkt.Dur for Opus and FLAC), so accumulation stays codec
// correct. The batch always keeps at least the newest packet, so End can stamp
// the final page's granule with the end position.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.begun || m.ended {
		return waxerr.New(waxerr.CodeInternal, "ogg: WritePacket outside Begin/End")
	}
	if n := len(pkt.Data); n > 0 && n < 1<<24 {
		// The FLAC mapping's STREAMINFO states the frame-size bounds, which are
		// the packet sizes here; End patches them in beside the total. The 24-bit
		// cap is the field's, and a packet past it simply does not narrow them.
		if m.minFrame == 0 || n < m.minFrame {
			m.minFrame = n
		}
		m.maxFrame = max(m.maxFrame, n)
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

// End writes the final page with the mapping's end granule and the EOS flag,
// then makes good whatever the headers declared about the run's length.
//
// Only the FLAC mapping declares one (STREAMINFO's total); Opus and Vorbis
// state their length in the final page granule alone, which this call has just
// written, so for them there is nothing left to check. For FLAC the rule is
// the native muxer's: a seekable destination is patched, an unseekable one
// refuses, because a header a muxer cannot go back and fix is a commitment and
// a stream that declares a length it does not carry reads back as damage.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.begun || m.ended {
		// The latch is load-bearing now that End patches: a second call would
		// emit another EOS page and then rewrite the BOS page from a total the
		// first call already settled.
		return waxerr.New(waxerr.CodeInternal, "ogg: End outside Begin")
	}
	m.ended = true
	endGranule := m.mapping.endGranule(trailer)
	if len(m.pending) == 0 {
		// No audio: emit an empty EOS page so the stream is well-formed.
		if err := m.emitPage(nil, lacing(0), endGranule, flagEOS); err != nil {
			return err
		}
	} else {
		m.pendingGranule = endGranule
		if err := m.flushPending(flagEOS); err != nil {
			return err
		}
	}
	return m.settleDeclaredTotal(endGranule)
}

// settleDeclaredTotal reconciles what the headers claimed about the run with
// the run that followed. total is what the stream ended up holding, which for
// the one mapping that declares anything is the final page granule.
//
// A seekable destination has its BOS page rewritten, so the output ends fully
// declared and signed, the way the reference `flac --ogg` writes one: the
// total, and the encoder's MD5 where the row supplied one (an encoder's
// signature exists only once it has seen every sample, so Begin could not have
// written it). The page CRC covers both fields, so the whole page is
// re-checksummed and written back.
//
// An unseekable destination refuses a claim the run missed, which the engine
// reclassifies as the projection miss it is (see missedProjection). It cannot
// carry the MD5 at all, which is the same trade the native muxer makes: a
// streamed FLAC goes out with the placeholder signature and `flac -t` accepts
// it with a warning.
func (m *Muxer) settleDeclaredTotal(total int64) error {
	if m.mapping.codecID() != codec.FLAC {
		return nil // no header field here states a length
	}
	// The same clamp Begin stamped through, so the sentinel a trailer may carry
	// (codec.Trailer states -1 for a length its producer does not know) and a
	// run past the 36-bit field both become "unknown" rather than a hard
	// failure after the whole file is written. A seekable destination clears
	// the claim to 0, which every reader handles; an unseekable one keeps what
	// Begin wrote, since there is nothing better to put there and nothing was
	// contradicted.
	declared := declaredTotal(total)
	if !m.patch.Seekable() {
		if m.wroteTotal != 0 && declared != 0 && m.wroteTotal != declared {
			return waxerr.New(waxerr.CodeInternal,
				fmt.Sprintf("ogg: STREAMINFO promised %d samples, wrote %d (unseekable output)", m.wroteTotal, declared))
		}
		return nil
	}
	page, err := patchOggFLACPage(m.bosPage, declared, m.md5, m.minFrame, m.maxFrame)
	if err != nil {
		return err
	}
	if err := m.patch.At(0, page); err != nil {
		return err
	}
	m.wroteTotal = declared
	return m.patch.Resume(m.off)
}

// patchOggFLACPage rewrites what only the finished run knows into a BOS page's
// STREAMINFO -- the sample total, the frame-size bounds, and the MD5 when one
// is supplied -- and recomputes the page checksum.
//
// STREAMINFO sits 17 bytes into the identification packet (the 0x7F"FLAC"
// signature, the version and header count, the fLaC marker, and the metadata
// block header), and the packet begins right after the page's own header and
// segment table. This muxer writes that packet alone on the BOS page, so the
// offsets are its own rather than a parse of anyone's.
func patchOggFLACPage(page []byte, total int64, md5 func() [16]byte, minFrame, maxFrame int) ([]byte, error) {
	if len(page) < headerLen {
		return nil, waxerr.New(waxerr.CodeInternal, "ogg: no BOS page recorded to patch")
	}
	si := headerLen + int(page[26]) + 17
	if si+flacStreamInfoLen > len(page) {
		return nil, waxerr.New(waxerr.CodeInternal, "ogg: the BOS page holds no STREAMINFO")
	}
	out := append([]byte(nil), page...)
	setStreamInfoTotal(out[si:si+flacStreamInfoLen], total)
	if minFrame > 0 && maxFrame > 0 {
		// Bytes 4..9: the 24-bit minimum and maximum frame sizes, which an
		// encoder cannot know until it has written every frame.
		out[si+4], out[si+5], out[si+6] = byte(minFrame>>16), byte(minFrame>>8), byte(minFrame)
		out[si+7], out[si+8], out[si+9] = byte(maxFrame>>16), byte(maxFrame>>8), byte(maxFrame)
	}
	if md5 != nil {
		sum := md5()
		copy(out[si+18:si+34], sum[:])
	}
	for i := 22; i < 26; i++ {
		out[i] = 0
	}
	binary.LittleEndian.PutUint32(out[22:26], crc32(0, out))
	return out, nil
}

// flacStreamInfoLen is the STREAMINFO block body's fixed size, restated here so
// the page arithmetic above does not depend on the codec package.
const flacStreamInfoLen = 34

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
	if headerType&flagBOS != 0 {
		// Kept so End can rewrite the declared total and re-checksum the page.
		m.bosPage = append(append(m.bosPage[:0], header...), payload...)
	}
	m.seq++
	if _, err := m.w.Write(header); err != nil {
		return err
	}
	m.off += int64(len(header))
	if len(payload) > 0 {
		if _, err := m.w.Write(payload); err != nil {
			return err
		}
		m.off += int64(len(payload))
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
