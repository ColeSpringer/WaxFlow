package mpa

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/muxseek"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// MuxerVersion is this muxer's term in the ADR-0004 cache key: a cached
// output of it regenerates when this bumps. Bump it for a change to what
// gets written around unchanged encoded packets, which is the one kind of
// change no encoder or DSP version can notice.
//
// mpa-mux-3: the encoder string is LAME-prefixed (see encoderTag) and the
// extension sits at its canonical offset with its counts and CRCs filled
// in, where before it was branded WaxFlow01 and ffmpeg, Firefox and
// GStreamer therefore ignored the gapless fields. Encoded output is
// byte-identical; every MP3 written so far plays late and long in those
// decoders and regenerates.
const MuxerVersion = "mpa-mux-3"

// Muxer writes one MP3 track as a bare Layer III elementary stream led by a
// Xing/Info metadata frame with a LAME-format gapless extension. NeedsSeek
// reports false: the leading frame is written on the first packet using the
// engine's projected sample count, so a plain io.Writer already carries exact
// gapless trims for a known-length transcode. A destination that can seek
// refines them at End with the encoder's exact trailer, covering the unknown-length case.
//
// CBR streams carry the "Info" marker (constant rate, no seek table). VBR
// streams carry "Xing" with the frame count, stream byte count, and the
// 100-point TOC: the streaming form writes a linear TOC (the neutral guess a
// player would make anyway), and a seekable destination back-patches the
// measured one at End alongside the exact gapless trailer.
//
// The metadata frame is a self-contained silent frame; the encoder's first
// audio frame references no reservoir before itself, so prepending it never
// disturbs the audio frames' back-references. Decoders (this package's
// demuxer, ffmpeg, browsers) recognize the tag and skip the frame as audio,
// so the gapless delay and padding apply to the audio frames alone, as long
// as the extension passes their acceptance rules (see encoderTag).
type Muxer struct {
	w     io.Writer
	patch muxseek.Patcher
	opts  MuxerOptions

	samples int64 // engine's projected input sample count, -1 unknown
	rate    int
	off     int64 // bytes written
	id3Len  int64 // leading ID3v2 tag length; the MPEG stream starts here
	infoLen int   // metadata frame length, for the End back-patch

	hdr [mp3.HeaderLen]byte // first frame's header, the metadata frame template
	h   mp3.Header          // metadata frame header (bitrate adjusted for VBR)

	// VBR seek-table samples: every strideth audio frame's stream byte
	// offset, compacted by stride doubling so memory stays bounded on
	// arbitrarily long streams.
	frameOff []int64
	stride   int

	audioFrames  int
	minKbps      int    // CBR rate, or the VBR minimum, for the LAME block
	musicCRC     uint16 // running CRC-16/ARC over the audio frames
	began, ended bool
	wroteInfo    bool
}

// MuxerOptions configures writing.
type MuxerOptions struct {
	// Delay is the gapless head trim in samples, used to size the metadata
	// frame's LAME extension before the exact trailer arrives at End.
	// DecodedTrims says which convention it is in. Zero is a valid delay.
	Delay int
	// DecodedTrims says Delay and the End trailer's trims are decoded-sample
	// counts, which is container.Track's and codec.Trailer's convention: the
	// samples a decoder drops from each end. The LAME tag's own fields are
	// not that. They state the encoder's share alone and leave every reader
	// to add the fixed DecoderDelay back, so the muxer converts.
	//
	// An encoder's trims arrive in the tag's convention already
	// (mp3.EncoderDelay is defined as the encoder half, and this package's
	// demuxer adds the 529 back when it reads one), so an encode leaves this
	// false. A remux's trims come from a demuxer, which has already added
	// it, and sets it. Without it a remuxed file's head trim grows by 529
	// samples every generation and a tail trim shorter than that is lost
	// outright, which is silently wrong playback rather than a failure.
	DecodedTrims bool
	// VBR selects the "Xing" metadata form (frame count, byte count, and
	// the 100-point TOC) for a variable-bit-rate stream; the default
	// "Info" form marks constant rate.
	VBR bool
	// Tags are written as a leading ID3v2.4 tag (the descriptive frames
	// id3Text maps; other keys are skipped). The Xing byte counts and TOC
	// stay relative to the first MPEG frame, so the tag never skews seek
	// arithmetic.
	Tags []container.Tag
}

// tocSampleCap bounds the retained frame offsets; reaching it halves the
// samples and doubles the stride. 2048 points resolve a 100-entry TOC to
// well under a percent of the stream at any length.
const tocSampleCap = 2048

// NewMuxer returns an MP3 muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, stride: 1}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek reports false: the elementary stream has a compliant streaming
// form and the gapless tag is written up front from the projected length.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and records the projected length; the metadata
// frame is deferred to the first packet, whose header is its template.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "mp3: Begin called twice")
	}
	m.patch = muxseek.New(m.w, "mp3")
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp3: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.MP3 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("mp3: cannot mux codec %q", t.Codec))
	}
	m.samples = t.Samples
	m.rate = t.Fmt.Rate
	id3, err := id3v2Tag(m.opts.Tags)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "mp3", err)
	}
	m.began = true
	if id3 != nil {
		if err := m.write(id3); err != nil {
			return err
		}
		m.id3Len = int64(len(id3))
	}
	return nil
}

// WritePacket writes the metadata frame (once, from the first packet's
// header) followed by the audio frame.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp3: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp3: no track %d", pkt.Track))
	}
	if len(pkt.Data) < mp3.HeaderLen {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp3: packet of %d bytes", len(pkt.Data)))
	}
	if !m.wroteInfo {
		h, err := mp3.ParseHeader(pkt.Data)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInternal, "mp3: first packet header", err)
		}
		copy(m.hdr[:], pkt.Data[:mp3.HeaderLen])
		m.h = h
		m.minKbps = h.Bitrate / 1000
		if m.opts.VBR {
			// A VBR first frame's rate is whatever its content picked;
			// the metadata frame instead uses the smallest legal rate
			// whose frame holds the whole Xing layout, LAME's approach
			// (deterministic, so the End back-patch matches by length).
			m.h = xingHeader(h)
		}
		delay, padding, frames := m.projectGapless()
		// A nil frame (free format, or too small for even the Xing header)
		// means no metadata frame; the audio frames stream on their own.
		if info := m.buildInfoFrame(infoFields{delay: delay, padding: padding, frames: frames}); info != nil {
			if err := m.write(info); err != nil {
				return err
			}
			m.infoLen = len(info)
		}
		m.wroteInfo = true
	}
	if m.opts.VBR {
		// Frame 0's rate was read above, where the metadata frame's own
		// header came from; the init clause of an if runs whatever the
		// condition says, so the skip has to be outside it.
		if m.audioFrames > 0 {
			if h, err := mp3.ParseHeader(pkt.Data); err == nil && h.Bitrate > 0 {
				m.minKbps = min(m.minKbps, h.Bitrate/1000)
			}
		}
		if m.audioFrames%m.stride == 0 {
			if len(m.frameOff) == tocSampleCap {
				for i := 0; i < tocSampleCap/2; i++ {
					m.frameOff[i] = m.frameOff[2*i]
				}
				m.frameOff = m.frameOff[:tocSampleCap/2]
				m.stride *= 2
			}
			m.frameOff = append(m.frameOff, m.off)
		}
	}
	if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.musicCRC = updateCRC16(m.musicCRC, pkt.Data)
	m.audioFrames++
	return nil
}

// End back-patches the metadata frame with the encoder's exact gapless
// trailer, audio-frame count, and (VBR) the measured byte count and TOC
// when the writer is seekable.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp3: End outside Begin")
	}
	m.ended = true
	if !m.patch.Seekable() || m.infoLen == 0 {
		return nil // no metadata frame to back-patch; the projection stands
	}
	// Rebuild the metadata frame with the exact trailer, audio-frame
	// count, and measured seek table, then patch it in place where the
	// MPEG stream starts (past any leading ID3v2 tag).
	var toc []byte
	if m.opts.VBR {
		toc = m.measureTOC()
	}
	stream := m.off - m.id3Len
	info := m.buildInfoFrame(infoFields{
		delay:    int(trailer.Delay),
		padding:  int(trailer.Padding),
		frames:   m.audioFrames,
		toc:      toc,
		bytes:    stream,
		musicLen: stream - int64(m.infoLen),
		musicCRC: m.musicCRC,
	})
	if info == nil || len(info) != m.infoLen {
		return nil // unbuildable now (should not happen); leave the projection
	}
	if err := m.patch.At(m.id3Len, info); err != nil {
		return err
	}
	return m.patch.Resume(m.off)
}

// measureTOC builds the Xing 100-point seek table from the sampled frame
// offsets: entry i is the stream byte offset at time fraction i/100, as a
// 0..255 fraction of the total byte count. Offsets and the total count
// from the first MPEG frame, so a leading ID3v2 tag never skews the
// fractions.
func (m *Muxer) measureTOC() []byte {
	toc := make([]byte, 100)
	total := m.off - m.id3Len
	if len(m.frameOff) == 0 || total <= 0 || m.audioFrames == 0 {
		for i := range toc {
			toc[i] = byte(min(i*256/100, 255))
		}
		return toc
	}
	for i := range toc {
		frame := m.audioFrames * i / 100
		j := min(frame/m.stride, len(m.frameOff)-1)
		toc[i] = byte(min((m.frameOff[j]-m.id3Len)*256/total, 255))
	}
	return toc
}

// projectGapless computes the metadata frame's gapless fields from the
// engine's projected length before the exact trailer is known. An unknown
// length yields delay-only trims (padding cannot be projected).
func (m *Muxer) projectGapless() (delay, padding, frames int) {
	delay = m.opts.Delay
	if m.samples < 0 {
		return delay, 0, 0
	}
	frames = mp3.FramesFor(m.samples, m.rate)
	total := int64(frames) * int64(m.h.SamplesPerFrame())
	if p := total - m.samples - int64(delay); p > 0 {
		padding = int(p)
	}
	return delay, padding, frames
}

func (m *Muxer) write(b []byte) error {
	n, err := m.w.Write(b)
	m.off += int64(n)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp3: write", err)
	}
	return nil
}

// xingHeader returns h with the smallest legal bit rate whose frame holds
// the full Xing layout (frames + bytes + TOC + the LAME extension) and no
// padding, the deterministic template for a VBR stream's metadata frame.
func xingHeader(h mp3.Header) mp3.Header {
	need := mp3.HeaderLen + h.SideInfoLen() + xingLayoutLen
	for _, kbps := range legalRates(h) {
		h.Bitrate = kbps * 1000
		h.Padding = false
		if h.Size() >= need {
			return h
		}
	}
	return h // largest rate; buildInfoFrame degrades if even that is short
}

// legalRates lists the layer's bit rates in kbit/s, ascending, for the
// header's MPEG version.
func legalRates(h mp3.Header) []int {
	if h.Version == mp3.MPEG1 {
		return []int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	}
	return []int{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
}

// Xing layout: the magic, the flag word, the four optional fields in flag
// order, then the LAME extension (mpegframes.ParseVBRTag reads the same
// shape back). All four fields present puts the extension at magic+120,
// where the readers that use a fixed offset rather than walking the flags
// look for it.
const (
	xingFlagFrames  = 1
	xingFlagBytes   = 2
	xingFlagTOC     = 4
	xingFlagQuality = 8
	// lameBlockLen is the whole extension: the 9-byte encoder string, 12
	// info bytes, the 3-byte delay/padding pack, the misc, gain and preset
	// bytes, the music length and the two CRCs.
	lameBlockLen = 36
	// lamePrefixLen is the extension through the delay/padding pack, what
	// a frame with no room for the rest still carries: the gapless fields
	// are the part playback depends on.
	lamePrefixLen = 24
	xingLayoutLen = 4 + 4 + 4 + 4 + 4 + 100 + lameBlockLen // magic..tag CRC
)

// encoderTag is the extension's 9-byte encoder string. The LAME prefix is
// load-bearing: ffmpeg applies the gapless fields only for a string
// starting LAME, Lavc or Lavf, Firefox only for LAME or Lavc, GStreamer
// only for LAME. Under the WaxFlow01 this muxer used to write, every one
// of them played the file 22 ms late and 57 ms long. Inspectors that read
// a version out of the digits after LAME show none, which is cosmetic.
// The demuxer still takes the old prefix, so files written before this
// still round-trip.
const encoderTag = "LAME WaxF"

// xingFields is the optional fields in flag order, with their lengths.
var xingFields = [...]struct {
	flag uint32
	n    int
}{
	{xingFlagFrames, 4},
	{xingFlagBytes, 4},
	{xingFlagTOC, 100},
	{xingFlagQuality, 4},
}

// pickFields chooses which of want fits in avail bytes with reserve held
// back for the LAME extension, returning the flags and the bytes taken. A
// field that does not fit is skipped and its flag cleared, so a reader
// walking the flags still lands on the extension.
func pickFields(avail, reserve int, want uint32) (flags uint32, used int) {
	for _, f := range xingFields {
		if want&f.flag == 0 || used+f.n+reserve > avail {
			continue
		}
		flags |= f.flag
		used += f.n
	}
	return flags, used
}

// miscByte is the extension's misc byte: the source sample frequency in
// bits 7-6, the encoder's stereo mode in bits 4-2, and zero for the unwise
// flag and the noise shaping this encoder has none of.
//
// The stereo mode is read off the stream's own header rather than left at
// zero, because zero is a VALUE there and it means mono: a tag inspector
// would otherwise report every stereo file this muxer writes as a mono
// encode. ffmpeg's own muxer leaves the whole byte zero and says exactly
// that about its files.
func miscByte(h mp3.Header) byte {
	var stereo byte
	switch h.Mode {
	case mp3.ModeStereo:
		stereo = 1
	case mp3.ModeDual:
		stereo = 2
	case mp3.ModeJoint:
		stereo = 3
	} // ModeMono is 0, the field's own encoding for it
	return srcFreqBits(h.Rate)<<6 | stereo<<2
}

// srcFreqBits is the misc byte's source sample frequency code.
func srcFreqBits(rate int) byte {
	switch {
	case rate <= 32000:
		return 0
	case rate == 44100:
		return 1
	case rate == 48000:
		return 2
	default:
		return 3
	}
}

// infoFields is what the metadata frame states: the gapless trims and the
// counts, plus the measurements only End has.
//
// A destination that cannot be patched leaves those measurements zero, and
// the frame ships with the flags set over them. Zero is the format's own
// "not computed" for all three, and the flags stay set either way: clearing
// them would move the extension off the offset a fixed-offset reader looks
// at, to say nothing more than the zeros already say.
type infoFields struct {
	delay, padding int
	frames         int
	toc            []byte // measured seek table, nil for the linear guess
	bytes          int64  // Xing byte count: the whole MPEG stream, 0 unknown
	musicLen       int64  // LAME music length: the audio frames alone, 0 unknown
	musicCRC       uint16
}

// tagTrims converts the caller's trims into the LAME tag's own fields. See
// MuxerOptions.DecodedTrims.
//
// A head trim under DecoderDelay clamps at zero, which is as close as the
// field gets: it states the encoder's share, and that cannot be negative. So
// a source claiming a head trim of, say, 100 decoded samples is written as 0
// and read back as 529, and the common case of that is real rather than
// hypothetical: an MP3 in an MP4 with no edit list probes with Delay 0
// (container/mp4's gapless), and a remux of it declares 529.
//
// That is the right answer even though it is not a round trip. The 529 is
// Layer III's own decoder latency: decoded sample 529 is the frame's first
// input sample, so a container claiming a zero head trim is claiming its
// decoder emits input sample 0 first, which no Layer III decoder does. The
// clamp writes what the format can say and the reader restores the latency
// the format defines. What it costs is that Track.Samples shrinks by 529
// across such a remux, which the round-trip test states rather than asserts
// away.
func (m *Muxer) tagTrims(delay, padding int) (int, int) {
	if !m.opts.DecodedTrims {
		return delay, padding
	}
	return max(delay-DecoderDelay, 0), max(padding+DecoderDelay, 0)
}

// buildInfoFrame constructs the leading metadata frame: a valid silent
// frame carrying the "Info" (CBR) or "Xing" (VBR) marker, the audio-frame
// count, the stream byte count and TOC, and the LAME extension with the
// gapless delay and padding at the offsets the demuxer reads
// (mpegframes.ParseVBRTag). The header bytes come from m.h (the first
// audio frame's header; VBR swaps in xingHeader's rate). It returns nil
// when no valid frame can hold even the Xing header (free format, whose
// Size is 0, or a frame too small), so the caller skips the metadata frame
// rather than emitting a bogus one.
//
// Both callers must get the same frame length out or the End back-patch is
// refused, so the layout is chosen from the header alone and never from
// the values.
func (m *Muxer) buildInfoFrame(f infoFields) []byte {
	// Both callers hand over the caller's convention, so the conversion is
	// here rather than at each: Begin's projection and End's exact trailer
	// have to produce the same frame length or the back-patch is refused.
	delay, padding := m.tagTrims(f.delay, f.padding)
	h := m.h
	size := h.Size()
	off := mp3.HeaderLen + h.SideInfoLen() // protection forced off: no CRC slot
	if size == 0 || off+12 > size {
		return nil
	}
	avail := size - off - 8 // room past the magic and the flag word

	// Canonical placement first: all four fields ahead of the whole
	// extension puts it at magic+120. A CBR frame cannot be resized (it
	// carries the stream's own bit-rate index), so a small one drops
	// fields in flag order, then the extension's tail, and only then the
	// extension itself, at which point the frame keeps its form's own
	// fields and the gapless trims are lost.
	const canonical = xingFlagFrames | xingFlagBytes | xingFlagTOC | xingFlagQuality
	lame := lameBlockLen
	flags, used := pickFields(avail, lame, canonical)
	if used+lame > avail {
		lame = lamePrefixLen
		flags, used = pickFields(avail, lame, canonical)
	}
	if used+lame > avail {
		// No room for even the gapless fields. The frame keeps the marker and
		// the frame count, which is what it carried before the extension was
		// written at all. A VBR frame never reaches here: xingHeader sizes it
		// to hold the whole canonical layout.
		lame = 0
		flags, used = pickFields(avail, 0, xingFlagFrames)
	}

	frame := make([]byte, size)
	hdr := headerBytesFor(h, m.hdr)
	copy(frame[:mp3.HeaderLen], hdr[:])
	frame[1] |= 1 // protection bit set = no CRC-16

	magic := "Info"
	if m.opts.VBR {
		magic = "Xing"
	}
	copy(frame[off:], magic)
	binary.BigEndian.PutUint32(frame[off+4:], flags)
	p := off + 8
	if flags&xingFlagFrames != 0 {
		binary.BigEndian.PutUint32(frame[p:], uint32(f.frames))
		p += 4
	}
	if flags&xingFlagBytes != 0 {
		if f.bytes > 0 && f.bytes <= int64(^uint32(0)) {
			binary.BigEndian.PutUint32(frame[p:], uint32(f.bytes))
		}
		p += 4
	}
	if flags&xingFlagTOC != 0 {
		if f.toc == nil {
			for i := range 100 {
				frame[p+i] = byte(min(i*256/100, 255))
			}
		} else {
			copy(frame[p:], f.toc)
		}
		p += 100
	}
	if flags&xingFlagQuality != 0 {
		p += 4 // quality indicator: this encoder states none, so it stays 0
	}
	if lame == 0 {
		return frame
	}

	b := frame[p : p+lame]
	copy(b, encoderTag)
	// Info tag revision 0 in the high nibble, VBR method in the low: 1 is
	// CBR, and a VBR encode states 0 (unknown) rather than claim one of
	// LAME's numbered methods, which name its own rate control.
	if !m.opts.VBR {
		b[9] = 1
	}
	// Bytes 10..19 stay zero: no lowpass, no ReplayGain, no encoding
	// flags and no ATH type.
	b[20] = byte(min(m.minKbps, 255)) // the CBR rate, or the VBR minimum
	// The two trims share three bytes but not each other's fate. Each
	// field is 12 bits, and a value past that cannot be written; losing
	// the head trim because the tail did not fit would trade a leading
	// 1105-sample gap for a trailing one that is usually smaller, and
	// DecodedTrims made that cliff easier to reach by adding
	// DecoderDelay to the padding before it is measured. So an
	// unrepresentable padding writes as zero, which leaks the tail, and
	// the delay it has nothing to do with still lands.
	if delay < 0 || delay >= 1<<12 {
		delay = 0
	}
	if padding < 0 || padding >= 1<<12 {
		padding = 0
	}
	b[21] = byte(delay >> 4)
	b[22] = byte((delay&0xF)<<4 | (padding>>8)&0xF)
	b[23] = byte(padding)
	if lame < lameBlockLen {
		return frame
	}
	b[24] = miscByte(m.h)
	// Bytes 25..27 stay zero: no MP3 gain, no preset, no surround info.
	//
	// The music length and CRC cover the audio frames alone. The spec's
	// extent is the file past its tags and this frame, and it has to be:
	// a CRC over this frame would have to include the field it is being
	// written into.
	if f.musicLen > 0 && f.musicLen <= int64(^uint32(0)) {
		binary.BigEndian.PutUint32(b[28:], uint32(f.musicLen))
	}
	binary.BigEndian.PutUint16(b[32:], f.musicCRC)
	binary.BigEndian.PutUint16(b[34:], updateCRC16(0, frame[:p+34]))
	return frame
}

// headerBytesFor renders h's four header bytes, reusing the template's
// non-derived bits (mode, emphasis flags) and replacing the bit rate and
// padding, so a VBR metadata frame's adjusted rate lands on the wire.
func headerBytesFor(h mp3.Header, tmpl [mp3.HeaderLen]byte) [mp3.HeaderLen]byte {
	b := tmpl
	bi := 0
	for i, kbps := range legalRates(h) {
		if kbps*1000 == h.Bitrate {
			bi = i + 1
			break
		}
	}
	pad := byte(0)
	if h.Padding {
		pad = 1
	}
	b[2] = byte(bi)<<4 | b[2]&0x0C | pad<<1
	return b
}
