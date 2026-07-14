package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// trackID is the single audio track's ID in the produced movie.
const trackID = 1

// defaultFragmentSeconds is the target fragment length. Progressive fMP4
// carries samples in self-contained moof+mdat fragments, so shorter
// fragments lower time-to-first-audio at the cost of a little per-fragment
// box overhead; ~1 second balances the two.
const defaultFragmentSeconds = 1

// MuxerOptions configures the fragmented writer.
type MuxerOptions struct {
	// FragmentSamples is the target sample count per fragment. Zero selects
	// roughly defaultFragmentSeconds of audio at the track rate. A fragment
	// closes once it reaches the target (the final one may be shorter).
	FragmentSamples int
	// Tags embeds canonical metadata fields as iTunes ilst atoms in the
	// moov (the keys ilstText and ilstFreeform map; others are skipped).
	Tags []container.Tag
	// Chapters embeds Nero chpl chapter markers in the moov's udta.
	Chapters []container.Chapter
	// Art embeds cover art as the ilst covr atom. Init headers are written
	// before the first audio byte, so large art delays first audio on a
	// live stream; the caller decides (jobs pass it, live streams do not).
	Art *container.Picture
}

// Muxer writes one audio track as a progressive fragmented MP4 (fMP4): an
// ftyp+moov init header declaring an empty sample table plus a movie-extends
// (mvex) box, then a moof+mdat fragment per bounded run of samples. Nothing
// is back-patched, so NeedsSeek reports false and a plain io.Writer streams
// live. The design is the CMAF/DASH shape the HLS segments reuse.
//
// The muxer carries ALAC and AAC-LC. ALAC is lossless and signals no
// gapless trims, so a nonzero Delay/Padding is rejected rather than
// silently dropped. AAC's encoder priming rides in the init header's
// edit list (delay is known up front; the length joins it when the
// engine projects one), and when the writer can seek, End patches the
// edit's duration with the encoder's exact trailer, upgrading a
// projected or unknown length to the exact one. On a pure stream the
// init bytes stand as written: full gapless when the length was known,
// delay-only otherwise (the capability matrix's live fMP4 cell).
type Muxer struct {
	w    io.Writer
	ws   io.WriteSeeker // nil when w cannot seek
	opts MuxerOptions

	rate     int
	fragTgt  int
	began    bool
	ended    bool
	seq      uint32 // next moof sequence number (1-based)
	baseTime int64  // decode time (in samples) of the current fragment's first sample

	// Edit-list back-patch state (AAC): the file offset of the elst
	// entry's 64-bit duration, -1 when no edit list was written; the
	// delay Begin declared; whether Begin already knew the exact length.
	elstDurOff int64
	delay      int64
	knownLen   int64

	// iTunSMPB back-patch state (AAC on a seekable writer): the file
	// offset of the atom's payload, -1 when none was written. Padding and
	// the exact length are fixed-width hex fields End fills in.
	smpbOff int64

	off int64 // bytes written

	// current fragment accumulator.
	fragSamples int
	fragData    []byte
	durs        []uint32
	sizes       []uint32
}

// NewMuxer returns a fragmented MP4 muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, seq: 1, elstDurOff: -1, smpbOff: -1}
	if ws, ok := w.(io.WriteSeeker); ok {
		m.ws = ws
	}
	if opts != nil {
		m.opts = *opts
		m.fragTgt = opts.FragmentSamples
	}
	return m
}

// NeedsSeek reports false: fragmented MP4 has a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and writes the ftyp and moov init header.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "mp4: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if err := t.Fmt.Valid(); err != nil {
		return err
	}

	var entry, edts []byte
	defaultDur := 0
	switch t.Codec {
	case codec.ALAC:
		if t.Delay != 0 || t.Padding != 0 {
			return waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: ALAC signals no gapless trims (lossless streams have none)")
		}
		cfg, err := alac.ParseMagicCookie(t.CodecConfig)
		if err != nil {
			return err
		}
		// Compare the fields the cookie constrains, not the whole Format: the
		// cookie has no channel-layout field, so cfg.Format always reports the
		// default layout, but the chain may deliver a non-default mask (a WAV
		// EXTENSIBLE stereo file, say) that ALAC codes in bitstream order all the
		// same. Requiring an exact Layout match would reject a track the plan and
		// the encoder both accepted.
		if want := cfg.Format(); t.Fmt.Rate != want.Rate || t.Fmt.Channels != want.Channels ||
			t.Fmt.Type != want.Type || t.Fmt.BitDepth != want.BitDepth {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("mp4: track format %v does not match the ALAC cookie (%v)", t.Fmt, want))
		}
		entry = alacSampleEntry(t.Fmt, cfg.Cookie)
		defaultDur = alac.FrameSize
	case codec.AACLC:
		if t.Delay < 0 || t.Padding < 0 {
			return waxerr.New(waxerr.CodeInvalidRequest, "mp4: negative gapless trims")
		}
		var err error
		entry, err = aacSampleEntry(t) // validates the ASC against t.Fmt
		if err != nil {
			return err
		}
		cfg, _ := aac.ParseASC(t.CodecConfig)
		defaultDur = cfg.FrameLength
		// The edit list carries the encoder priming up front, plus the
		// exact length when the engine projected one; End refines the
		// length from the trailer when the writer can seek.
		if t.Delay > 0 || t.Samples > 0 {
			edts = elstBox(t.Delay, max(t.Samples, 0))
		}
		m.delay = t.Delay
		m.knownLen = t.Samples
	default:
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("mp4: cannot mux codec %q (alac, aac-lc)", t.Codec))
	}

	m.rate = t.Fmt.Rate
	if m.fragTgt <= 0 {
		m.fragTgt = defaultFragmentSeconds * m.rate
	}
	m.began = true

	if err := m.write(ftypBox()); err != nil {
		return err
	}
	// The iTunSMPB gapless atom needs the exact padding and length only
	// End knows, and its fields cannot be corrected on a pure stream, so
	// it is written (with zeros to patch) only when the writer can seek:
	// the job path. Live streams keep the edit list's delay-only (or
	// projected-length) signaling.
	var smpb []byte
	if t.Codec == codec.AACLC && m.ws != nil && t.Delay > 0 {
		smpb = freeformAtom("iTunSMPB", smpbPayload(t.Delay, max(t.Samples, 0)))
	}
	udta := udtaBox(m.opts.Tags, m.opts.Chapters, m.opts.Art, smpb)
	moov := moovBox(t.Fmt.Rate, entry, edts, defaultDur, udta)
	if edts != nil {
		// Remember where the elst entry's 64-bit duration landed for the
		// End back-patch. The whole edts blob is matched, not the 4-byte
		// tag, so a field added ahead of it later whose bytes happen to
		// spell "elst" can never redirect the patch (today everything
		// before the box is fixed constants plus the timescale, but the
		// back-patch must not depend on that staying true).
		if i := bytes.Index(moov, edts); i >= 0 {
			m.elstDurOff = m.off + int64(i) + elstDurOffset
		}
	}
	if smpb != nil {
		// Same idea for the iTunSMPB payload: locate it by content (the
		// template's leading zero field), not by arithmetic over the
		// freeform layout.
		if i := bytes.Index(moov, smpb); i >= 0 {
			if j := bytes.Index(smpb, []byte(" 00000000 ")); j >= 0 {
				m.smpbOff = m.off + int64(i) + int64(j)
			}
		}
	}
	return m.write(moov)
}

// WritePacket appends one ALAC frame to the current fragment, flushing when
// the fragment reaches its target length.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: no track %d", pkt.Track))
	}
	if len(pkt.Data) == 0 || pkt.Dur <= 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet of %d bytes, %d samples", len(pkt.Data), pkt.Dur))
	}
	if len(pkt.Data) > maxSampleBytes || pkt.Dur > maxSampleDur {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet too large (%d bytes, %d samples)", len(pkt.Data), pkt.Dur))
	}

	m.fragData = append(m.fragData, pkt.Data...)
	m.durs = append(m.durs, uint32(pkt.Dur))
	m.sizes = append(m.sizes, uint32(len(pkt.Data)))
	m.fragSamples += int(pkt.Dur)

	// Flush on the sample target, or on the byte cap so a fragment never grows
	// past what a 32-bit box size can hold (the sample target is derived from
	// the rate, which a crafted cookie could inflate; the byte cap bounds
	// memory and box size regardless).
	if m.fragSamples >= m.fragTgt || len(m.fragData) >= maxFragmentBytes {
		return m.flush()
	}
	return nil
}

// End flushes the final fragment. ALAC carries no trailing gapless padding,
// so a nonzero trailer trim is rejected.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != m.delay {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("mp4: trailer delay %d disagrees with the init header's %d", trailer.Delay, m.delay))
	}
	if m.delay == 0 && (trailer.Padding != 0 || trailer.Delay != 0) {
		// The ALAC path: lossless streams have no trims and Begin wrote
		// no edit list to carry any.
		return waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: ALAC signals no gapless trims")
	}
	if len(m.durs) > 0 {
		if err := m.flush(); err != nil {
			return err
		}
	}
	// With a seekable writer, refine the edit list's duration to the
	// encoder's exact sample count (the projected or unknown length
	// Begin wrote becomes exact, completing the gapless signaling).
	if m.ws != nil && m.elstDurOff >= 0 && trailer.Samples > 0 && trailer.Samples != m.knownLen {
		var dur [8]byte
		binary.BigEndian.PutUint64(dur[:], uint64(trailer.Samples))
		if err := m.patch(m.elstDurOff, dur[:], "elst"); err != nil {
			return err
		}
	}
	// Fill the iTunSMPB padding and exact length (fixed-width hex, so the
	// header never moves). Together with the edit list this makes the
	// seekable AAC output a full gapless cell.
	if m.ws != nil && m.smpbOff >= 0 {
		pad := fmt.Sprintf("%08X", uint32(trailer.Padding))
		length := fmt.Sprintf("%016X", uint64(max(trailer.Samples, 0)))
		if err := m.patch(m.smpbOff+smpbPaddingOff, []byte(pad), "iTunSMPB"); err != nil {
			return err
		}
		if err := m.patch(m.smpbOff+smpbLengthOff, []byte(length), "iTunSMPB"); err != nil {
			return err
		}
	}
	return nil
}

// patch overwrites len(b) bytes at off and restores the write position to
// the stream end.
func (m *Muxer) patch(off int64, b []byte, what string) error {
	if _, err := m.ws.Seek(off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: "+what+" seek", err)
	}
	if _, err := m.ws.Write(b); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: "+what+" patch", err)
	}
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: seeking to end", err)
	}
	return nil
}

// flush writes the accumulated fragment as a moof+mdat pair and resets the
// accumulator for the next one.
func (m *Muxer) flush() error {
	frag := fragmentBoxes(m.seq, m.baseTime, m.durs, m.sizes, m.fragData)
	if err := m.write(frag); err != nil {
		return err
	}
	m.seq++
	m.baseTime += int64(m.fragSamples)
	m.fragSamples = 0
	m.fragData = m.fragData[:0]
	m.durs = m.durs[:0]
	m.sizes = m.sizes[:0]
	return nil
}

func (m *Muxer) write(b []byte) error {
	if _, err := m.w.Write(b); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: write", err)
	}
	m.off += int64(len(b))
	return nil
}

// Fragment field caps: a single ALAC frame stays well under these, so a
// value past them is a wiring bug, not a legal stream.
const (
	maxSampleBytes = 1 << 24
	maxSampleDur   = 1 << 20
	// maxFragmentBytes caps one fragment's mdat payload, a backstop that keeps
	// every box size inside the 32-bit box-length field even if the sample
	// target (rate-derived) is absurd. Normal ~1 s fragments stay far under it.
	maxFragmentBytes = 8 << 20
)

// ftypBox declares an ISO base media file compatible with the M4A audio and
// fragmentation (iso5) brands.
func ftypBox() []byte {
	return makeBox("ftyp",
		[]byte("M4A "), u32(0),
		[]byte("M4A "), []byte("mp42"), []byte("isom"), []byte("iso2"), []byte("iso5"))
}

// moovBox builds the movie header for a fragmented file: a track with an
// empty sample table and a movie-extends box declaring the fragment
// defaults. The media timescale is the sample rate, so decode times are
// sample counts. entry is the codec's AudioSampleEntry; a non-nil edts
// (the segmenter's edit list) rides inside the trak; defaultDur is the
// trex fallback sample duration (the trun always overrides it); a
// non-nil udta (tags, chapters, iTunSMPB) closes the box.
func moovBox(rate int, entry, edts []byte, defaultDur int, udta []byte) []byte {
	mvhd := makeFullBox("mvhd", 0, 0,
		u32(0), u32(0), // creation, modification time
		u32(uint32(rate)),                                    // timescale
		u32(0),                                               // duration (0: fragmented, from moof)
		u32(0x00010000), u16(0x0100), u16(0), u32(0), u32(0), // rate, volume, reserved
		identityMatrix(),
		make([]byte, 24), // pre_defined
		u32(trackID+1))   // next_track_ID
	trak := trakBox(rate, entry, edts)
	mvex := makeBox("mvex", trexBox(defaultDur))
	if udta != nil {
		return makeBox("moov", mvhd, trak, mvex, udta)
	}
	return makeBox("moov", mvhd, trak, mvex)
}

func trakBox(rate int, entry, edts []byte) []byte {
	tkhd := makeFullBox("tkhd", 0, 0x000007, // track enabled, in movie, in preview
		u32(0), u32(0), // creation, modification
		u32(trackID),
		u32(0),         // reserved
		u32(0),         // duration (fragmented)
		u32(0), u32(0), // reserved
		u16(0), u16(0), // layer, alternate_group
		u16(0x0100), u16(0), // volume (audio), reserved
		identityMatrix(),
		u32(0), u32(0)) // width, height (audio: 0)
	mdia := mdiaBox(rate, entry)
	if edts != nil {
		return makeBox("trak", tkhd, edts, mdia)
	}
	return makeBox("trak", tkhd, mdia)
}

func mdiaBox(rate int, entry []byte) []byte {
	mdhd := makeFullBox("mdhd", 0, 0,
		u32(0), u32(0), // creation, modification
		u32(uint32(rate)),   // timescale
		u32(0),              // duration (fragmented)
		u16(0x55C4), u16(0)) // language 'und', pre_defined
	hdlr := makeFullBox("hdlr", 0, 0,
		u32(0),                 // pre_defined
		[]byte("soun"),         // handler_type
		u32(0), u32(0), u32(0), // reserved
		append([]byte("SoundHandler"), 0)) // name
	minf := minfBox(entry)
	return makeBox("mdia", mdhd, hdlr, minf)
}

func minfBox(entry []byte) []byte {
	smhd := makeFullBox("smhd", 0, 0, u16(0), u16(0)) // balance, reserved
	dref := makeFullBox("dref", 0, 0, u32(1),
		makeFullBox("url ", 0, 0x000001)) // self-contained
	dinf := makeBox("dinf", dref)
	stbl := stblBox(entry)
	return makeBox("minf", smhd, dinf, stbl)
}

// stblBox holds the sample description plus empty timing/size/offset tables:
// a fragmented file carries no samples in moov.
func stblBox(entry []byte) []byte {
	stsd := makeFullBox("stsd", 0, 0, u32(1), entry)
	stts := makeFullBox("stts", 0, 0, u32(0))
	stsc := makeFullBox("stsc", 0, 0, u32(0))
	stsz := makeFullBox("stsz", 0, 0, u32(0), u32(0)) // sample_size, sample_count
	stco := makeFullBox("stco", 0, 0, u32(0))
	return makeBox("stbl", stsd, stts, stsc, stsz, stco)
}

// alacSampleEntry builds the 'alac' AudioSampleEntry wrapping the magic
// cookie. The cookie is authoritative for rate and depth; the legacy 16.16
// sample-rate field and samplesize are cosmetic (our demuxer and ffmpeg both
// read the cookie), so samplesize stays 16 and the rate is written best
// effort.
func alacSampleEntry(f audio.Format, cookie []byte) []byte {
	inner := makeFullBox("alac", 0, 0, cookie)
	// The 16.16 fixed-point field holds only a 16-bit integer part, so rates
	// at or above 65536 (96/176.4/192 kHz hi-res ALAC) are clamped rather
	// than left to wrap into the fractional bits and read back as garbage.
	// The cookie carries the true rate and is authoritative.
	sampleRate := uint32(f.Rate)
	if sampleRate > 0xFFFF {
		sampleRate = 0xFFFF
	}
	// makeBox concatenates its parts, so the AudioSampleEntry header fields go
	// straight in ahead of the child cookie box, no intermediate slice.
	return makeBox("alac",
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), u32(0), // version, revision, vendor
		u16(uint16(f.Channels)), u16(16), // channelcount, samplesize
		u16(0), u16(0), // compressionID, packetsize
		u32(sampleRate<<16), // samplerate 16.16
		inner)
}

// trexBox declares the fragment defaults: one sample description, the
// codec frame length as the default duration, and sync-sample flags
// (every frame of the audio codecs muxed here is independently
// decodable). trun overrides duration/size per sample, so these are only
// fallbacks.
func trexBox(defaultDur int) []byte {
	return makeFullBox("trex", 0, 0,
		u32(trackID),
		u32(1),                  // default_sample_description_index
		u32(uint32(defaultDur)), // default_sample_duration
		u32(0),                  // default_sample_size
		u32(syncSampleFlags))    // default_sample_flags
}

// syncSampleFlags marks a sample as independently decodable: sample_depends_on
// = 2 (depends on nothing) and sample_is_non_sync_sample = 0.
const syncSampleFlags = 0x02000000

// fragmentBoxes assembles one moof+mdat pair. The trun data offset is
// resolved to point at the mdat payload once the moof size is known.
func fragmentBoxes(seq uint32, baseTime int64, durs, sizes []uint32, data []byte) []byte {
	n := len(durs)

	mfhd := makeFullBox("mfhd", 0, 0, u32(seq))
	tfhd := makeFullBox("tfhd", 0, 0x020000, u32(trackID)) // default-base-is-moof
	tfdt := makeFullBox("tfdt", 1, 0, u64(uint64(baseTime)))

	// trun body: version(1) flags(3) sample_count(4) data_offset(4) then
	// per-sample duration(4)+size(4).
	body := make([]byte, 0, 12+8*n)
	body = append(body, 0)                                // version
	body = append(body, 0x00, 0x03, 0x01)                 // flags: data-offset, sample-duration, sample-size
	body = binary.BigEndian.AppendUint32(body, uint32(n)) // sample_count
	dataOffPos := len(body)
	body = binary.BigEndian.AppendUint32(body, 0) // data_offset placeholder
	for i := 0; i < n; i++ {
		// Append directly into the preallocated body rather than through u32,
		// whose throwaway 4-byte slices would allocate twice per sample.
		body = binary.BigEndian.AppendUint32(body, durs[i])
		body = binary.BigEndian.AppendUint32(body, sizes[i])
	}
	trun := makeBox("trun", body)

	traf := makeBox("traf", tfhd, tfdt, trun)
	moof := makeBox("moof", mfhd, traf)

	// data_offset counts from the moof start (default-base-is-moof) to the
	// first mdat payload byte: the whole moof plus the 8-byte mdat header. The
	// trun is moof's last box, so its data_offset field lands at the moof end
	// minus the trun size, past the trun's 8-byte header and body prefix.
	dataOffset := uint32(len(moof) + 8)
	patchAt := len(moof) - len(trun) + 8 + dataOffPos
	binary.BigEndian.PutUint32(moof[patchAt:], dataOffset)

	mdat := makeBox("mdat", data)
	return append(moof, mdat...)
}

// identityMatrix is the 3x3 unity transform matrix boxes carry.
func identityMatrix() []byte {
	return bytes.Join([][]byte{
		u32(0x00010000), u32(0), u32(0),
		u32(0), u32(0x00010000), u32(0),
		u32(0), u32(0), u32(0x40000000),
	}, nil)
}
