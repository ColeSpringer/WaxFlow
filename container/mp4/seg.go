package mp4

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// SegmenterVersion identifies the segment and init-header box layout for
// the ADR-0004 cache key: cached segments regenerate when this bumps, so
// a box-layout fix can never serve stale segments next to fresh ones.
// mp4-seg-4: the init header's sample entry carries the codec's depth and
// agrees with dOps on rate (both refused by Chromium's MSE parser before).
const SegmenterVersion = "mp4-seg-4"

// maxSegmentPayload bounds one segment's mdat payload. A segment is a
// single moof+mdat pair (see Segmenter), and the most extreme legal
// configuration (8-channel 32-bit 192 kHz lossless at the 60 s segment
// cap) stays around 370 MB, so a gigabyte is a wiring-bug backstop that
// still keeps every box size far inside the 32-bit length field.
const maxSegmentPayload = 1 << 30

// Segment is one emitted media segment: an styp plus one moof+mdat
// pair, self-contained and independently decodable.
type Segment struct {
	// Index is the segment number, counting the whole stream's segments
	// from zero (a segmenter started mid-stream begins at its
	// StartSegment).
	Index int64
	// Data is the segment's bytes. Freshly allocated per segment; the
	// caller owns it.
	Data []byte
	// Samples is the segment's decode duration in track samples.
	Samples int64
}

// SegmenterOptions configures a Segmenter.
type SegmenterOptions struct {
	// SegmentSamples is the decode duration of every segment but the last,
	// in track samples. It must be a positive multiple of the codec frame
	// so segment boundaries land exactly between packets.
	SegmentSamples int
	// StartSegment is the index of the first emitted segment; the base
	// decode time follows as StartSegment * SegmentSamples. Zero is the
	// stream's top.
	StartSegment int64
}

// Segmenter packs codec packets into numbered CMAF media segments for
// HLS: each segment is styp plus exactly one moof+mdat pair whose tfdt
// carries the decode time in track samples (the media timescale is the
// sample rate). The matching init header comes from InitSegment.
// Boundaries are sample-counted, so the packet stream must arrive
// frame-aligned: every packet whole, segment length a frame multiple.
//
// One fragment per segment is load-bearing, not just simple: it makes the
// mfhd sequence_number (index+1) and the tfdt (index*SegmentSamples) pure
// functions of the segment index, so a worker restarted mid-stream
// reproduces a continuous run's bytes exactly. Splitting large segments
// into several fragments would decouple sequence numbers from segment
// indexes and break that guarantee (and buys nothing here: a segment is
// buffered whole before it is emitted either way).
//
// Codecs: Opus, FLAC, and ALAC (the fMP4-capable encoders). Every frame
// of each is independently decodable, so every sample is a sync sample
// and segments can begin anywhere on a frame boundary.
type Segmenter struct {
	segTgt int
	index  int64
	ended  bool

	// current segment accumulator.
	data        []byte
	durs, sizes []uint32
	samples     int64
}

// NewSegmenter validates the track and options and returns a Segmenter.
// The same track must produce the init header (InitSegment); validation
// is shared so a track that plans here cannot fail there.
func NewSegmenter(t container.Track, opts *SegmenterOptions) (*Segmenter, error) {
	if opts == nil || opts.SegmentSamples <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "mp4: segmenter needs a positive SegmentSamples")
	}
	if opts.StartSegment < 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "mp4: negative StartSegment")
	}
	if _, err := sampleEntryFor(t); err != nil {
		return nil, err
	}
	return &Segmenter{
		segTgt: opts.SegmentSamples,
		index:  opts.StartSegment,
	}, nil
}

// WritePacket appends one packet to the current segment, emitting the
// segment once it reaches its target length. Packets must not straddle a
// segment boundary (the caller feeds frame-aligned packets and the target
// is a frame multiple); one that would is a wiring bug and errors.
func (s *Segmenter) WritePacket(pkt codec.Packet, emit func(Segment) error) error {
	if s.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: WritePacket after End")
	}
	if len(pkt.Data) == 0 || pkt.Dur <= 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet of %d bytes, %d samples", len(pkt.Data), pkt.Dur))
	}
	if len(pkt.Data) > maxSampleBytes || pkt.Dur > maxSampleDur {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet too large (%d bytes, %d samples)", len(pkt.Data), pkt.Dur))
	}
	if s.samples+pkt.Dur > int64(s.segTgt) {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("mp4: %d-sample packet straddles the segment boundary (%d of %d samples filled)",
				pkt.Dur, s.samples, s.segTgt))
	}
	if len(s.data)+len(pkt.Data) > maxSegmentPayload {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("mp4: segment payload past %d bytes; no legal configuration reaches this", maxSegmentPayload))
	}

	s.data = append(s.data, pkt.Data...)
	s.durs = append(s.durs, uint32(pkt.Dur))
	s.sizes = append(s.sizes, uint32(len(pkt.Data)))
	s.samples += pkt.Dur

	if s.samples >= int64(s.segTgt) {
		return s.emitSegment(emit)
	}
	return nil
}

// End flushes the final, possibly short, segment.
func (s *Segmenter) End(emit func(Segment) error) error {
	if s.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: End called twice")
	}
	s.ended = true
	if len(s.durs) > 0 {
		return s.emitSegment(emit)
	}
	return nil
}

// emitSegment closes the accumulated packets into the segment's single
// moof+mdat pair, hands it to the caller, and resets for the next one.
// Sequence number and base decode time derive from the index alone.
func (s *Segmenter) emitSegment(emit func(Segment) error) error {
	frag := fragmentBoxes(uint32(s.index)+1, s.index*int64(s.segTgt), s.durs, s.sizes, s.data)
	data := make([]byte, 0, len(stypBox)+len(frag))
	data = append(data, stypBox...)
	data = append(data, frag...)
	seg := Segment{Index: s.index, Data: data, Samples: s.samples}
	s.index++
	s.samples = 0
	s.data = s.data[:0]
	s.durs = s.durs[:0]
	s.sizes = s.sizes[:0]
	return emit(seg)
}

// InitSegment builds the CMAF init header for the track: ftyp plus a moov
// whose track carries the codec's sample entry, an empty sample table,
// the movie-extends defaults, and, when the track declares an encoder
// delay or a known length, an edit list mapping the decode timeline onto
// the presentation one (the fMP4 gapless convention: the delay is known
// up front and rides in the init header; end padding is trimmed by the
// same edit when the length is known). Deterministic: equal tracks yield
// identical bytes.
func InitSegment(t container.Track) ([]byte, error) {
	entry, err := sampleEntryFor(t)
	if err != nil {
		return nil, err
	}
	var edts []byte
	if t.Delay > 0 || t.Samples > 0 {
		edts = elstBox(t.Delay, max(t.Samples, 0))
	}
	init := append([]byte{}, initFtypBox...)
	return append(init, moovBox(t.Fmt.Rate, entry, edts, 0, nil)...), nil
}

// initFtypBox and stypBox are constants of the segment layout: an iso6
// init header (64-bit tfdt, default-base-is-moof) with the CMAF brand,
// and the msdh media-segment brand.
var (
	initFtypBox = makeBox("ftyp",
		[]byte("iso6"), u32(0),
		[]byte("iso6"), []byte("iso5"), []byte("cmfc"), []byte("mp41"))
	// msdh alone: msix is DASH's Indexed Media Segment brand and requires a
	// sidx per segment, which is not written and would be redundant if it
	// were. One segment is served per URL, so there are no byte ranges to
	// index. HLS players ignore brands, so claiming msix was invisible; a
	// DASH conformance validator would not have ignored it.
	stypBox = makeBox("styp",
		[]byte("msdh"), u32(0),
		[]byte("msdh"))
)

// elstBox is a version-1 edit list with one entry: presentation starts
// media_time samples into the decode timeline (the encoder delay) and,
// with a known length, lasts duration samples (trimming the tail padding
// the last packet carries). Movie and media timescales are both the
// sample rate, so both fields are sample counts.
func elstBox(mediaTime, duration int64) []byte {
	elst := makeFullBox("elst", 1, 0,
		u32(1), // entry_count
		u64(uint64(duration)),
		u64(uint64(mediaTime)),
		u16(1), u16(0)) // media_rate_integer, media_rate_fraction
	return makeBox("edts", elst)
}

// elstDurOffset is where the entry's 64-bit duration sits inside the
// blob elstBox returns: the edts and elst headers (8 bytes each), the
// elst version/flags (4), and entry_count (4). The progressive muxer's
// End back-patch depends on it; TestElstDurOffset pins it against the
// builder.
const elstDurOffset = 8 + 8 + 4 + 4

// sampleEntryFor builds the codec's AudioSampleEntry from the track's
// codec config, validating config against format like Muxer.Begin does.
func sampleEntryFor(t container.Track) ([]byte, error) {
	if err := t.Fmt.Valid(); err != nil {
		return nil, err
	}
	switch t.Codec {
	case codec.Opus:
		return opusSampleEntry(t)
	case codec.FLAC:
		return flacSampleEntry(t)
	case codec.AACLC, codec.HEAAC:
		return aacSampleEntry(t)
	case codec.ALAC:
		cfg, err := alac.ParseMagicCookie(t.CodecConfig)
		if err != nil {
			return nil, err
		}
		if want := cfg.Format(); t.Fmt.Rate != want.Rate || t.Fmt.Channels != want.Channels ||
			t.Fmt.Type != want.Type || t.Fmt.BitDepth != want.BitDepth {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("mp4: track format %v does not match the ALAC cookie (%v)", t.Fmt, want))
		}
		if t.Delay != 0 {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: ALAC signals no encoder delay")
		}
		return alacSampleEntry(cfg), nil
	}
	return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("mp4: cannot segment codec %q (opus, flac, alac, aac-lc, he-aac)", t.Codec))
}

// opusSampleEntry wraps the OpusHead fields in an 'Opus' entry with a
// 'dOps' box (Opus-in-ISOBMFF). Only channel mapping family 0 (mono and
// stereo single-stream) is produced, matching the encoder; note dOps is
// big-endian where OpusHead is little-endian.
//
// The entry's samplerate is 48000 as the mapping requires, and dOps's
// InputSampleRate is written as 48000 too rather than copied from the
// header. RFC 7845 makes that field informational (the rate of the PCM
// that went into the encoder, never a playback rate), but Chromium
// refuses the init segment unless it equals the entry's, so a remuxed
// source whose header says 44100 (every CD-sourced opusenc file) would be
// unplayable over MSE. The original does not survive a round trip: this
// package's demuxer rebuilds OpusHead from dOps, so an fMP4 read back
// hands 48000 on to the Ogg and Matroska muxers. Nothing interprets the
// value, and the only Opus fMP4 written here is HLS output, never a source.
func opusSampleEntry(t container.Track) ([]byte, error) {
	head := t.CodecConfig
	if len(head) != 19 || string(head[:8]) != "OpusHead" || head[8] != 1 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: malformed OpusHead codec config")
	}
	channels := int(head[9])
	preSkip := binary.LittleEndian.Uint16(head[10:])
	outputGain := binary.LittleEndian.Uint16(head[16:])
	if family := head[18]; family != 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: opus channel mapping family %d (only family 0)", family))
	}
	if channels != t.Fmt.Channels {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track has %d channels, OpusHead %d", t.Fmt.Channels, channels))
	}
	if t.Fmt.Rate != opus.SampleRate {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: opus track at %d Hz; Opus decodes at %d", t.Fmt.Rate, opus.SampleRate))
	}
	if int64(preSkip) != t.Delay {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track delay %d disagrees with OpusHead pre-skip %d", t.Delay, preSkip))
	}
	dops := makeBox("dOps",
		[]byte{0},              // Version
		[]byte{byte(channels)}, // OutputChannelCount
		u16(preSkip),
		u32(opus.SampleRate), // InputSampleRate, normalized (see above)
		u16(outputGain),
		[]byte{0}) // ChannelMappingFamily
	h := sampleEntryHeader{channels: channels, depth: 16, rate16: clampRate16(opus.SampleRate)}
	return audioSampleEntry("Opus", h, dops), nil
}

// flacSampleEntry wraps the STREAMINFO block in an 'fLaC' entry with a
// 'dfLa' box (FLAC-in-ISOBMFF): version+flags, then the STREAMINFO as a
// complete metadata block, header included and marked last.
func flacSampleEntry(t container.Track) ([]byte, error) {
	si, err := flac.ParseStreamInfo(t.CodecConfig)
	if err != nil {
		return nil, err
	}
	if want := si.PCMFormat(); t.Fmt.Rate != want.Rate || t.Fmt.Channels != want.Channels ||
		t.Fmt.Type != want.Type || t.Fmt.BitDepth != want.BitDepth {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track format %v does not match STREAMINFO (%v)", t.Fmt, want))
	}
	if t.Delay != 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: FLAC signals no encoder delay")
	}
	dfla := makeFullBox("dfLa", 0, 0,
		[]byte{0x80, 0, 0, flac.StreamInfoLen}, // last-block flag, type STREAMINFO, 24-bit length
		t.CodecConfig)
	h := sampleEntryHeader{channels: si.Channels, depth: si.Bits, rate16: flacRate16(si.Rate)}
	return audioSampleEntry("fLaC", h, dfla), nil
}

// sampleEntryHeader is the AudioSampleEntry's own fields, the ones beside
// the codec config box. Keyed, so a call cannot transpose two counts.
type sampleEntryHeader struct {
	channels int
	depth    int    // samplesize, in bits
	rate16   uint32 // samplerate as 16.16 fixed point
}

// audioSampleEntry assembles an AudioSampleEntry of the given type around
// the codec-specific child box. Strict fMP4 consumers cross-check the
// header against that box: FLAC-in-ISOBMFF says channelcount and
// samplesize shall equal STREAMINFO's, and Chromium's MSE parser refuses
// an init segment whose FLAC entry disagrees with its dfLa, so the
// lossless codecs pass their config's depth. Opus and AAC pass 16: the
// Opus mapping fixes it, AAC carries the conventional value (it has no
// PCM depth), and Chromium accepts only 8, 16, 24, or 32 there whatever
// the codec.
func audioSampleEntry(typ string, h sampleEntryHeader, child []byte) []byte {
	return makeBox(typ,
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), u32(0), // version, revision, vendor
		u16(uint16(h.channels)), u16(uint16(h.depth)), // channelcount, samplesize
		u16(0), u16(0), // compressionID, packetsize
		u32(h.rate16), // samplerate 16.16
		child)
}

// clampRate16 is the 16.16 samplerate for the entries whose config box is
// where every decoder reads the rate (ALAC's cookie, AAC's ASC): the rate
// itself when it fits, else saturated at 65535. No spec prescribes a
// substitute for those entries, and a metadata reader that takes the field
// verbatim (waxlabel does for ALAC) is better handed an obviously saturated
// value than a plausible wrong one.
func clampRate16(rate int) uint32 {
	if rate > 0xFFFF {
		rate = 0xFFFF
	}
	return uint32(rate) << 16
}

// flacRate16 is the fLaC entry's 16.16 samplerate per FLAC-in-ISOBMFF: the
// rate itself when it fits, otherwise the greatest whole halving that does
// (48000.0 for 96 and 192 kHz, 44100.0 for 88.2 and 176.4), and the spec's
// 65535.0 fallback for a rate with no such halving. Readers take the true
// rate from STREAMINFO, as the spec requires of them.
func flacRate16(rate int) uint32 {
	for rate > 0xFFFF {
		if rate%2 != 0 {
			return clampRate16(rate)
		}
		rate /= 2
	}
	return uint32(rate) << 16
}
