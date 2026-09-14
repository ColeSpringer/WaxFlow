package riff

import (
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/adpcm"
	"github.com/colespringer/waxflow/codec/g711"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/codecname"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
	"github.com/colespringer/waxflow/container/internal/waveformat"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants). A legitimate WAV holds a
// handful of chunks; parsing stops long before any pathological count.
const (
	maxChunks     = 1024
	maxFmtPayload = 4096
	// maxWarnings caps the tolerated-damage list. A chunk walk is bounded by
	// maxChunks on its own; a frame-walked payload is not, since it reports
	// one finding per damaged gap and a run of alternating frames and junk
	// has one gap per frame.
	maxWarnings = 64
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	// Conformance tests and `waxflow probe --strict` use it; playback
	// paths stay tolerant because real libraries are messy.
	Strict bool
}

// Demuxer reads one audio track from a WAV source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	track    container.Track
	payload  payload
	warnings []container.Warning
}

// payload is the walk over the data chunk. Two shapes reach it, and the
// difference between them is not a detail of the walk but its whole
// geometry: a flat array of fixed-size units, where position is arithmetic,
// and a run of self-framing MPEG audio frames, where it is an index built by
// walking. That is what earns a seam rather than a flag.
type payload interface {
	// ReadPacket yields the next run of payload, io.EOF past the end.
	ReadPacket(pkt *container.Packet) error
	// SeekSample repositions to the unit or frame holding a sample and
	// reports the sample that unit starts on.
	SeekSample(sample int64) (landed int64, err error)
}

// blockReader walks the data chunk as a run of equal units.
//
// A unit is whatever the codec addresses its payload by: one frame for PCM
// and G.711, one block for the ADPCM families. That is the only difference
// between them at this level, which is why one walk serves all four: the
// payload is a flat array of fixed-size units either way, and the sample
// timeline is the unit index times what a unit decodes to.
type blockReader struct {
	src        container.Source
	dataOff    int64
	unitBytes  int64
	unitFrames int64
	units      int64
	samples    int64 // the track's length, which a fact chunk can trim
	pos        int64 // next unit to read
	buf        []byte
}

// perPacket is how many units one packet carries: the pipeline's working
// size, in units, and never fewer than one. For PCM this is the same
// audio.StandardChunk frames the reader has always delivered.
func (r *blockReader) perPacket() int64 {
	return max(int64(audio.StandardChunk)/r.unitFrames, 1)
}

// NewDemuxer parses the headers of a WAV source. The returned Demuxer
// implements container.Seeker (WAV seeks are trivially sample-exact) and
// container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// malformed reports bytes that deviate from this format: truncated,
// inconsistent, or out of range. See [waxerr.Malformed] for the rule that
// divides it from unsupported.
func malformed(format string, args ...any) error {
	return waxerr.Malformed("wav: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not, which is a thing a caller can act on. See
// [waxerr.Malformed] for the rule.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("wav: ", format, args...)
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	w := container.Warning{Offset: off, Msg: msg, Kind: container.Damage}
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
	return nil
}

// note records a Warning that Strict must not escalate: this build doing
// something with a well-formed file that a caller should know about, rather
// than damage in the file. See [container.Note].
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...), Kind: container.Note})
}

// waveFormat is what a fmt chunk resolves to: the codec, the configuration
// blob its decoder takes, the pipeline format, and the geometry of one
// addressable unit of payload.
type waveFormat struct {
	codec      codec.ID
	config     []byte
	fmt        audio.Format
	unitBytes  int
	unitFrames int
	// sourceBits is Track.SourceBitDepth: the depth the file stores samples
	// at where Fmt.BitDepth does not say it, 0 where it does.
	sourceBits int
	// factIsLength marks a codec whose last unit over-produces, so the fact
	// chunk's count is a hard length rather than a cross-check.
	factIsLength bool
	// codecDelay is MPEGLAYER3WAVEFORMAT's nCodecDelay, reported and never
	// applied; see startMPEG.
	codecDelay int
}

func (d *Demuxer) parse() error {
	size := d.src.Size()
	var head [12]byte
	if err := container.ReadFull(d.src, head[:], 0); err != nil {
		return container.ShortRead("wav: reading header", err)
	}
	if !Match(head[:]) {
		return malformed("not a RIFF/WAVE file")
	}
	rf64 := string(head[:4]) == idRF64 || string(head[:4]) == idBW64

	var (
		ds64DataSize    uint64
		ds64SampleCount uint64
		haveDS64        bool
		fmtSeen         bool
		dataOff         int64 = -1
		dataBytes       int64 = -1
		factSamples     int64 = -1
		factSeen        bool
		wf              waveFormat
		streamingData   bool
	)

	off := int64(12)
	for chunks := 0; off+8 <= size; chunks++ {
		if chunks >= maxChunks {
			return malformed("more than %d chunks", maxChunks)
		}
		var hdr [8]byte
		if err := container.ReadFull(d.src, hdr[:], off); err != nil {
			return container.ShortRead("wav: reading chunk header", err)
		}
		id := string(hdr[:4])
		chunkSize := int64(le.Uint32(hdr[4:]))

		switch id {
		case idDS64:
			if !rf64 {
				if err := d.warn(off, "ds64 chunk in a plain RIFF file ignored"); err != nil {
					return err
				}
				break
			}
			if chunkSize < ds64Payload || off+8+ds64Payload > size {
				return malformed("ds64 chunk truncated")
			}
			var p [ds64Payload]byte
			if err := container.ReadFull(d.src, p[:], off+8); err != nil {
				return container.ShortRead("wav: reading ds64", err)
			}
			ds64DataSize = le.Uint64(p[8:])
			ds64SampleCount = le.Uint64(p[16:])
			haveDS64 = true
			// The chunk table that may follow holds sizes for other big
			// chunks; audio needs none of them, so it is skipped unread.
		case idFmt:
			if fmtSeen {
				if err := d.warn(off, "duplicate fmt chunk ignored"); err != nil {
					return err
				}
				break
			}
			if chunkSize < 16 {
				return malformed("fmt chunk of %d bytes, want at least 16", chunkSize)
			}
			n := chunkSize
			if n > maxFmtPayload {
				if err := d.warn(off, "fmt chunk of %d bytes truncated to %d", chunkSize, maxFmtPayload); err != nil {
					return err
				}
				n = maxFmtPayload
			}
			if off+8+n > size {
				return malformed("fmt chunk extends past end of file")
			}
			payload := make([]byte, n)
			if err := container.ReadFull(d.src, payload, off+8); err != nil {
				return container.ShortRead("wav: reading fmt", err)
			}
			var err error
			if wf, err = d.parseFmt(payload, off); err != nil {
				return err
			}
			fmtSeen = true
		case idFact:
			// The count of samples the stream decodes to. Optional for PCM
			// and required for every other tag, which is what decides below
			// whether it is read at all.
			//
			// A chunk too short or past the end of the file is passed over
			// rather than reported: absent is a defined answer for every
			// codec here, the generic walk below already names a chunk that
			// runs off the end, and warning about the optional form would put
			// "input damage" on PCM files that open everywhere.
			if factSeen || chunkSize < 4 || off+8+4 > size {
				break
			}
			var p [4]byte
			if err := container.ReadFull(d.src, p[:], off+8); err != nil {
				return container.ShortRead("wav: reading fact", err)
			}
			factSeen = true
			if n := int64(le.Uint32(p[:])); n != size32Unknown {
				factSamples = n
			}
		case idData:
			if dataBytes >= 0 {
				if err := d.warn(off, "extra data chunk ignored"); err != nil {
					return err
				}
				break
			}
			dataOff = off + 8
			switch {
			case chunkSize == size32Unknown && haveDS64:
				// ds64 sizes are 64-bit and file-supplied: bound against
				// the file before converting, or a huge value wraps int64
				// negative and dodges the clamp.
				if ds64DataSize > uint64(size-dataOff) {
					if err := d.warn(off, "ds64 data size %d exceeds file, clamped", ds64DataSize); err != nil {
						return err
					}
					dataBytes = size - dataOff
				} else {
					dataBytes = int64(ds64DataSize)
				}
			case chunkSize == size32Unknown:
				// The live-written convention: data runs to end of file.
				if err := d.warn(off, "streaming data size, clamped to end of file"); err != nil {
					return err
				}
				dataBytes = size - dataOff
				streamingData = true
			case dataOff+chunkSize > size:
				if err := d.warn(off, "data chunk size %d exceeds file, clamped", chunkSize); err != nil {
					return err
				}
				dataBytes = size - dataOff
			default:
				dataBytes = chunkSize
			}
			chunkSize = dataBytes
		}

		if streamingData {
			break // data extends to EOF; nothing can follow
		}
		// Chunks are word-aligned: odd sizes carry a pad byte. Advancing
		// by at least the 8-byte header guarantees parse progress.
		next := off + 8 + chunkSize + chunkSize&1
		if next <= off {
			return malformed("chunk size overflow")
		}
		if next > size && id != idData {
			if err := d.warn(off, "%q chunk extends past end of file", id); err != nil {
				return err
			}
			break
		}
		off = next
	}

	if !fmtSeen {
		return malformed("no fmt chunk")
	}
	if dataBytes < 0 {
		return malformed("no data chunk")
	}
	if wf.codec == codec.MP3 {
		return d.startMPEG(wf, dataOff, dataBytes, factSamples)
	}

	unitBytes := int64(wf.unitBytes)
	if rem := dataBytes % unitBytes; rem != 0 {
		if err := d.warn(dataOff, "%d trailing bytes are not a whole %s, ignored", rem, unitName(wf)); err != nil {
			return err
		}
		dataBytes -= rem
	}
	units := dataBytes / unitBytes
	capacity := units * int64(wf.unitFrames)
	samples := capacity
	// Exact in every arm: the count is what a read delivers, a byte-linear
	// payload by its frames and a block codec by unitFrames per unit. A fact
	// inside the last block is a trim the flag makes format/media apply;
	// whether a fact was believed is the warnings' business.
	exact := true
	switch {
	case factSamples < 0:
		// Nothing to compare against; the payload is the only statement of
		// the length.
	case wf.codec == codec.PCM:
		// Ignored, deliberately. The format specification makes the chunk
		// REQUIRED for every tag but PCM and optional for that one, so a PCM
		// file's is a field writers fill in when they feel like it and get
		// wrong when they do; the payload states the same number exactly.
	case wf.factIsLength:
		// The last block decodes whole and the stream ends inside it, so the
		// count is a truncation instruction rather than an observation. It is
		// believed only where it lands inside that last block, which is the
		// only place a real one can land: a count further down than that
		// describes blocks the file went to the trouble of coding and then
		// disclaimed, and a zero or unpatched field would otherwise empty the
		// track silently and call the answer exact.
		switch {
		case factSamples > capacity:
			if err := d.warn(0, "fact declares %d samples, the data holds %d; clamped", factSamples, capacity); err != nil {
				return err
			}
		case capacity-factSamples >= int64(wf.unitFrames):
			if err := d.warn(0, "fact declares %d samples, %d short of the %d the data holds; ignored",
				factSamples, capacity-factSamples, capacity); err != nil {
				return err
			}
		default:
			samples = factSamples
		}
	case factSamples != capacity:
		// A byte-linear payload states its own length exactly, and for a
		// compressed tag the chunk is required rather than optional, so the
		// two disagreeing is one of them being wrong.
		if err := d.warn(0, "fact declares %d samples, the data holds %d", factSamples, capacity); err != nil {
			return err
		}
	}
	if haveDS64 && ds64SampleCount != 0 && ds64SampleCount != uint64(capacity) {
		if err := d.warn(0, "ds64 sample count %d disagrees with data size (%d frames)", ds64SampleCount, capacity); err != nil {
			return err
		}
	}

	d.payload = &blockReader{src: d.src, dataOff: dataOff, unitBytes: unitBytes,
		unitFrames: int64(wf.unitFrames), units: units, samples: samples}
	d.track = container.Track{
		Codec:          wf.codec,
		CodecConfig:    wf.config,
		Fmt:            wf.fmt,
		Samples:        samples,
		SamplesExact:   exact,
		SourceBitDepth: wf.sourceBits,
		Default:        true,
	}
	return nil
}

// unitName names a unit for a message, so a trailing-bytes warning says
// which kind of boundary the file stopped short of.
func unitName(wf waveFormat) string {
	if wf.unitFrames > 1 {
		return "block"
	}
	return "frame"
}

// parseFmt maps a fmt chunk payload onto a codec, its configuration, and the
// geometry of one addressable unit.
//
// The per-tag arms own the fields whose meaning is the tag's. wBitsPerSample
// is PCM's own geometry and reads 4 for ADPCM, 8 for G.711 and 0 for a
// compressed tag that states nothing (ffmpeg writes 0 for MP2 and MP3), so
// judging it ahead of the switch would refuse a well-formed file with the
// wrong code and without naming what it holds. nBlockAlign is the same story:
// for PCM it is a derived number this reader recomputes, and for the block
// codecs it is the block size everything else is measured from.
func (d *Demuxer) parseFmt(b []byte, off int64) (waveFormat, error) {
	var wf waveFormat
	tag := le.Uint16(b)
	channels := int(le.Uint16(b[2:]))
	rate64 := int64(le.Uint32(b[4:]))
	blockAlign := int(le.Uint16(b[12:]))
	bits := int(le.Uint16(b[14:]))

	if channels < 1 {
		return wf, malformed("%d channels", channels)
	}
	if channels > audio.MaxChannels {
		return wf, unsupported("%d channels (supported: 1..%d)", channels, audio.MaxChannels)
	}
	// Bound in int64 so acceptance does not depend on the platform's int
	// width (a rate above MaxInt32 would wrap negative on 32-bit builds).
	if rate64 <= 0 || rate64 > math.MaxInt32 {
		return wf, malformed("sample rate %d", rate64)
	}
	rate := int(rate64)

	validBits := 0
	layout := audio.DefaultLayout(channels)
	extensible := tag == tagExtensible
	if extensible {
		if len(b) < 40 {
			return wf, malformed("extensible fmt chunk of %d bytes, want 40", len(b))
		}
		if bits%8 != 0 {
			return wf, malformed("extensible container of %d bits is not whole bytes", bits)
		}
		validBits = int(le.Uint16(b[18:]))
		if validBits == 0 {
			validBits = bits
		}
		mask := audio.ChannelMask(le.Uint32(b[20:]))
		switch {
		case mask == 0:
			// Common in the wild; keep the guessed default layout.
		case mask.Count() != channels:
			if werr := d.warn(off, "channel mask %v does not cover %d channels, ignored", mask, channels); werr != nil {
				return wf, werr
			}
		default:
			layout = mask
		}
		subTag := le.Uint16(b[24:])
		if [14]byte(b[26:40]) != guidTail {
			return wf, malformed("unknown extensible subformat GUID")
		}
		tag = subTag
	}

	switch tag {
	case tagPCM, tagIEEEFloat:
		cfg, err := d.pcmConfig(tag, bits, validBits)
		if err != nil {
			return wf, err
		}
		blob, err := cfg.MarshalBinary()
		if err != nil {
			return wf, err
		}
		wf = waveFormat{
			codec: codec.PCM, config: blob,
			fmt:       cfg.PCMFormat(rate, channels, layout),
			unitBytes: cfg.BytesPerFrame(channels), unitFrames: 1,
		}
		if cfg.Encoding == pcm.Float && cfg.Bits != wf.fmt.BitDepth {
			// audio.Format carries floats as float32, so a 64-bit float source
			// decodes at BitDepth 32. The file's own depth is not lost, it just
			// has nowhere in the format to live; recorded here so probe reports
			// what the source holds rather than what the pipeline runs on.
			// Integer depths need no such note: ValidBits already puts the
			// source depth in Fmt.BitDepth.
			wf.sourceBits = cfg.Bits
		}
	case tagALaw, tagMuLaw:
		law := g711.ALaw
		id := codec.ALaw
		if tag == tagMuLaw {
			law, id = g711.MuLaw, codec.MuLaw
		}
		// One byte per sample per channel, whatever the field says. A
		// companding law has exactly one width, so a header stating another
		// is describing a stream that does not exist rather than one this
		// build cannot read: damage, and the samples are still where 8 bits
		// put them. (AIFF-C is looser about the same field, and for a reason
		// that does not apply here; see container/aiff.)
		if bits != 8 {
			if werr := d.warn(off, "%v declares %d bits per sample, which is 8 by definition", law, bits); werr != nil {
				return wf, werr
			}
		}
		wf = waveFormat{
			codec: id,
			fmt:   g711.Format(rate, channels, layout),
			// A G.711 sample is one byte, so the payload is byte-linear the
			// way PCM's is and a frame is the unit.
			unitBytes: channels, unitFrames: 1,
			sourceBits: g711.SourceBitDepth,
		}
	case tagIMAADPCM, tagMSADPCM:
		if extensible {
			// The extensible extra IS the extensible struct, so there is
			// nowhere left for the block geometry these tags state.
			return wf, unsupported("%s inside a WAVE_FORMAT_EXTENSIBLE header, which leaves no room for its block geometry",
				codecname.WaveFormat(tag))
		}
		cfg, err := d.adpcmConfig(b, off)
		if err != nil {
			return wf, err
		}
		blob, err := cfg.MarshalBinary()
		if err != nil {
			return wf, err
		}
		id := codec.IMAADPCM
		if tag == tagMSADPCM {
			id = codec.MSADPCM
		}
		wf = waveFormat{
			codec: id, config: blob,
			fmt:       cfg.Format(rate, layout),
			unitBytes: cfg.BlockBytes(), unitFrames: cfg.SamplesPerBlock,
			// A block codec's length is the fact chunk's to state, since the
			// last block over-produces past it.
			factIsLength: true,
			sourceBits:   adpcm.SourceBitDepth,
		}
	case tagMP3:
		// MPEGLAYER3WAVEFORMAT: a WAVEFORMATEX with twelve bytes behind it,
		// of which this reader takes one field and takes it only to say what
		// it is not doing with it. Everything a Layer III decoder needs is in
		// the frames themselves, which is also why extra bytes that are
		// missing or short are a finding about the file rather than a
		// refusal, and why wID and the padding flags are not read: nothing
		// they could say would change how the frames are walked.
		//
		// The format here is the chunk's claim about the stream, which
		// startMPEG then puts against the first frame's own header.
		wf = waveFormat{codec: codec.MP3, fmt: mp3.Header{Rate: rate, Channels: channels}.PCMFormat()}
		if extensible {
			// The extensible extra IS the extensible struct, so there is no
			// MPEGLAYER3 header behind it to read. Nothing is lost: the
			// fields it would hold are the ones not read anyway.
			break
		}
		ex, _ := waveformat.Parse(b) // the 16-byte minimum is checked above
		if len(ex.Extra) < mpegLayer3Extra {
			if werr := d.warn(off, "MP3 fmt chunk carries %d extra bytes, want %d", len(ex.Extra), mpegLayer3Extra); werr != nil {
				return wf, werr
			}
			break
		}
		wf.codecDelay = int(le.Uint16(ex.Extra[10:]))
	default:
		// Named where a name is known, so the refusal says what the file
		// holds rather than only that it is not PCM. Scoped to the container
		// rather than to the build: several of these tags name codecs this
		// same binary decodes elsewhere (MP3 bare and in MP4, WMA in ASF), so
		// "this build has no decoder for it" would be false.
		if name := codecname.WaveFormat(tag); name != "" {
			return wf, unsupported("this build does not read %s (format tag 0x%04X) from a WAV", name, tag)
		}
		return wf, unsupported("this build does not read format tag 0x%04X from a WAV", tag)
	}

	// Only where a unit exists to recompute. A frame-coded payload has none:
	// its frames state their own lengths and nBlockAlign holds whatever the
	// writer thought it meant (ffmpeg writes the frame's SAMPLE count there
	// for MP3), so there is nothing to compare it against.
	if wf.unitBytes != 0 && blockAlign != wf.unitBytes {
		if werr := d.warn(off, "block align %d, computed %d; using computed", blockAlign, wf.unitBytes); werr != nil {
			return wf, werr
		}
	}
	if err := wf.fmt.Valid(); err != nil {
		return wf, container.UnusableFormat("wav", wf.fmt, err)
	}
	return wf, nil
}

// startMPEG builds the track for a data chunk holding MPEG audio frames.
//
// Nothing about this payload is byte-linear: a Layer III frame states its own
// length in its own header and the next one begins where that says, so the
// chunk is walked rather than divided and the fmt chunk's geometry fields
// describe nothing this reader can use. What it does describe is the stream,
// and that claim is worth checking against the stream itself.
func (d *Demuxer) startMPEG(wf waveFormat, dataOff, dataBytes, factSamples int64) error {
	// No trailer hook: the data chunk bounds the frames, so bytes inside it
	// that are not frames are damage rather than the tag baggage a bare
	// stream ends with.
	walk := mpegframes.New(d.src, dataOff+dataBytes, mpegframes.Options{
		Prefix: "wav: ",
		Warn:   func(off int64, msg string) error { return d.warn(off, "%s", msg) },
	})
	tag, _, err := walk.Begin(dataOff)
	if err != nil {
		return err
	}
	f := walk.Header().PCMFormat()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("wav", f, err)
	}
	if f != wf.fmt {
		// A decoder reads the frame, so the frame wins, and a header that
		// disagrees with the stream it describes is the file deviating from
		// its own format rather than something this build is choosing:
		// damage, which --strict refuses and an ordinary read carries on
		// with the truth. container/mp4 treats the same disagreement the
		// same way.
		if werr := d.warn(dataOff, "fmt chunk says %v, the first frame says %v; the frame wins", wf.fmt, f); werr != nil {
			return werr
		}
	}

	samples, delay, padding := tag.Gapless(walk.SamplesPerFrame())
	advisory := false
	if samples < 0 && factSamples > 0 {
		// With no metadata frame the chunk's count is the only statement of
		// the length there is, and it is a rounded one: it counts everything
		// the frames decode to, encoder delay and tail padding included, and
		// nothing in the file says where inside them the audio starts or
		// stops. Advisory keeps it out of arithmetic that has to add up.
		//
		// Bounded both ways first. A zero is the one value the chunk cannot
		// be telling the truth with, since the walk above already found a
		// frame; and the payload's byte length bounds the frames it can hold
		// whatever they are, which is the only cross-check available at O(1)
		// here (the exact count is the walk the lazy index exists to avoid).
		// Outside those the field is the file's to state.
		ceiling := mpegframes.MaxFrames(dataBytes) * walk.SamplesPerFrame()
		if factSamples > ceiling {
			if werr := d.warn(0, "fact declares %d samples, more than %d bytes of frames can hold; ignored",
				factSamples, dataBytes); werr != nil {
				return werr
			}
		} else {
			samples, advisory = factSamples, true
		}
	}
	if delay == 0 && wf.codecDelay != 0 {
		d.note(0, "nCodecDelay declares %d samples; trims come from a Xing or LAME frame, so it is not applied",
			wf.codecDelay)
	}
	d.payload = walk.Reader()
	d.track = container.Track{
		Codec:   codec.MP3,
		Fmt:     f,
		Samples: samples,
		Delay:   delay,
		Padding: padding,
		// SamplesExact stays false whichever of the two stated the length: a
		// metadata frame and a fact chunk are both a writer's claim about
		// frames neither of them counted, and neither is a truncation
		// instruction. The chunk's is rounded on top of that, which is what
		// advisory says; a metadata frame that states one states the trims
		// with it, so the fact chunk is not a second opinion on the same
		// number and is not compared against it.
		SamplesAdvisory: advisory,
		Default:         true,
	}
	return nil
}

// pcmConfig reads the two uncompressed tags' geometry.
func (d *Demuxer) pcmConfig(tag uint16, bits, validBits int) (pcm.Config, error) {
	var cfg pcm.Config
	containerBits := pcm.ContainerBits(bits)
	if tag == tagIEEEFloat {
		if bits != 32 && bits != 64 {
			return cfg, malformed("%d-bit float", bits)
		}
		cfg = pcm.Config{Encoding: pcm.Float, Bits: bits}
		return cfg, cfg.Validate()
	}
	if bits < 1 || bits > 64 {
		return cfg, malformed("%d bits per sample", bits)
	}
	cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: containerBits}
	if containerBits == 8 {
		cfg.Encoding = pcm.UnsignedInt
	}
	switch {
	case validBits != 0 && validBits != containerBits:
		cfg.ValidBits = validBits
	case bits != containerBits:
		cfg.ValidBits = bits // for example 20 valid bits in 24-bit words
	}
	return cfg, cfg.Validate()
}

// adpcmConfig reads a block codec's geometry out of the fmt chunk, which is
// a WAVEFORMATEX. The rules live in container/internal/waveformat, since a
// QuickTime "ms" sample entry states the same geometry the same way; what is
// local is where the findings land, at this chunk's offset and under this
// package's Strict rule.
func (d *Demuxer) adpcmConfig(b []byte, off int64) (adpcm.Config, error) {
	ex, ok := waveformat.Parse(b)
	if !ok {
		return adpcm.Config{}, malformed("fmt chunk of %d bytes is not a WAVEFORMATEX", len(b))
	}
	cfg, found, err := waveformat.ADPCM(ex, "wav: ")
	for _, f := range found {
		if f.Note {
			d.note(off, "%s", f.Msg)
			continue
		}
		if werr := d.warn(off, "%s", f.Msg); werr != nil {
			return cfg, werr
		}
	}
	return cfg, err
}

// Tracks returns the single audio track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// ReadPacket yields the next run of payload. Packet data is reused across
// calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error { return d.payload.ReadPacket(pkt) }

// ReadPacket yields the next run of units: up to audio.StandardChunk frames
// of raw interleaved PCM, or the blocks that decode to about as many.
func (r *blockReader) ReadPacket(pkt *container.Packet) error {
	remaining := r.units - r.pos
	if remaining <= 0 {
		return io.EOF
	}
	n := min(r.perPacket(), remaining)
	need := int(n * r.unitBytes)
	if cap(r.buf) < need {
		r.buf = make([]byte, need)
	}
	r.buf = r.buf[:need]
	if err := container.ReadFull(r.src, r.buf, r.dataOff+r.pos*r.unitBytes); err != nil {
		return container.ShortRead("wav: reading data", err)
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: r.buf,
			PTS:  r.pos * r.unitFrames,
			Dur:  n * r.unitFrames,
			// Every unit is independently decodable: a PCM frame trivially,
			// and an ADPCM block because its header restates the coder state.
			// The one layout that does not restate it, Apple's ima4, is not a
			// thing a WAV can carry.
			Sync: true,
		},
	}
	r.pos += n
	return nil
}

// SeekSample repositions to the unit or frame holding the given sample.
//
// Landing is exact for PCM and G.711, whose unit is one frame; on the block
// boundary at or before the target for the two ADPCM families; and further
// back still for MP3, whose decoder needs the frames ahead of the target to
// converge. format.Media decodes and discards the remainder in every case, so
// the delivered position is sample-exact. Targets past the end land at the end
// and the next ReadPacket returns io.EOF.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("wav: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "wav: negative seek target")
	}
	return d.payload.SeekSample(sample)
}

// SeekSample lands on the unit boundary at or before the target.
func (r *blockReader) SeekSample(sample int64) (int64, error) {
	// Clamped to the track's length and not to the payload's, which for a
	// fact-trimmed block track is strictly larger: container.Seeker's landing
	// is authoritative for its callers, and one past the end of the track is a
	// position the track does not have.
	r.pos = min(min(sample, r.samples)/r.unitFrames, r.units)
	return r.pos * r.unitFrames, nil
}
