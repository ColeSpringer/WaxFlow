package mp4

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// SegmenterVersion identifies the segment and init-header box layout for
// the ADR-0004 cache key: cached segments regenerate when this bumps, so
// a box-layout fix can never serve stale segments next to fresh ones.
const SegmenterVersion = "mp4-seg-2"

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
	return append(init, moovBox(t.Fmt.Rate, entry, edts, 0)...), nil
}

// initFtypBox and stypBox are constants of the segment layout: an iso6
// init header (64-bit tfdt, default-base-is-moof) with the CMAF brand,
// and the msdh media-segment brand.
var (
	initFtypBox = makeBox("ftyp",
		[]byte("iso6"), u32(0),
		[]byte("iso6"), []byte("iso5"), []byte("cmfc"), []byte("mp41"))
	stypBox = makeBox("styp",
		[]byte("msdh"), u32(0),
		[]byte("msdh"), []byte("msix"))
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
	case codec.AACLC:
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
		return alacSampleEntry(t.Fmt, cfg.Cookie), nil
	}
	return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
		fmt.Sprintf("mp4: cannot segment codec %q (opus, flac, alac, aac-lc)", t.Codec))
}

// opusSampleEntry wraps the OpusHead fields in an 'Opus' entry with a
// 'dOps' box (Opus-in-ISOBMFF). Only channel mapping family 0 (mono and
// stereo single-stream) is produced, matching the encoder; note dOps is
// big-endian where OpusHead is little-endian.
func opusSampleEntry(t container.Track) ([]byte, error) {
	head := t.CodecConfig
	if len(head) != 19 || string(head[:8]) != "OpusHead" || head[8] != 1 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: malformed OpusHead codec config")
	}
	channels := int(head[9])
	preSkip := binary.LittleEndian.Uint16(head[10:])
	inputRate := binary.LittleEndian.Uint32(head[12:])
	outputGain := binary.LittleEndian.Uint16(head[16:])
	if family := head[18]; family != 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: opus channel mapping family %d (only family 0)", family))
	}
	if channels != t.Fmt.Channels {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track has %d channels, OpusHead %d", t.Fmt.Channels, channels))
	}
	if int64(preSkip) != t.Delay {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track delay %d disagrees with OpusHead pre-skip %d", t.Delay, preSkip))
	}
	dops := makeBox("dOps",
		[]byte{0},              // Version
		[]byte{byte(channels)}, // OutputChannelCount
		u16(preSkip),
		u32(inputRate),
		u16(outputGain),
		[]byte{0}) // ChannelMappingFamily
	return audioSampleEntry("Opus", t.Fmt, dops), nil
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
	return audioSampleEntry("fLaC", t.Fmt, dfla), nil
}

// audioSampleEntry assembles a generic AudioSampleEntry of the given type
// around the codec-specific child box. samplesize stays the conventional
// 16 and the legacy 16.16 rate field clamps like alacSampleEntry's: the
// codec config box is authoritative for both.
func audioSampleEntry(typ string, f audio.Format, child []byte) []byte {
	sampleRate := uint32(f.Rate)
	if sampleRate > 0xFFFF {
		sampleRate = 0xFFFF
	}
	return makeBox(typ,
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), u32(0), // version, revision, vendor
		u16(uint16(f.Channels)), u16(16), // channelcount, samplesize
		u16(0), u16(0), // compressionID, packetsize
		u32(sampleRate<<16), // samplerate 16.16
		child)
}
