package aiff

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
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	maxChunks      = 1024
	maxCommPayload = 512
	// maxWarnings caps the tolerated-damage list. A chunk walk is bounded by
	// maxChunks on its own; a frame-walked payload is not, since it reports
	// one finding per damaged gap and a run of alternating frames and junk
	// has one gap per frame.
	maxWarnings = 64
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one audio track from an AIFF or AIFF-C source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	track   container.Track
	payload payload
	// carryState marks a compression type whose decoder does not restart at a
	// unit boundary, so a decode begun at one never converges to the linear
	// decode. ima4 is the only one; see SeekSample.
	carryState bool
	warnings   []container.Warning
}

// payload is the walk over the SSND chunk. Two shapes reach it, and the
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

// blockReader walks the SSND payload as a run of equal units.
//
// A unit is whatever the compression type addresses its payload by: one
// frame for the PCM types and for G.711, one 34-byte-per-channel packet for
// ima4. COMM's numSampleFrames counts units either way, which is the field's
// real meaning and why one walk serves all of them.
type blockReader struct {
	src        container.Source
	dataOff    int64
	unitBytes  int64
	unitFrames int64
	units      int64
	samples    int64 // the track's length; equal to units times unitFrames here
	pos        int64 // next unit to read
	buf        []byte
}

// perPacket is how many units one packet carries: the pipeline's working
// size, in units, and never fewer than one.
func (r *blockReader) perPacket() int64 {
	return max(int64(audio.StandardChunk)/r.unitFrames, 1)
}

// commFormat is what a COMM chunk resolves to: the codec, the configuration
// blob its decoder takes, the pipeline format, and the geometry of one
// addressable unit of payload.
type commFormat struct {
	codec      codec.ID
	config     []byte
	fmt        audio.Format
	unitBytes  int
	unitFrames int
	// sourceBits is Track.SourceBitDepth: the depth the file stores samples
	// at where Fmt.BitDepth does not say it, 0 where it does.
	sourceBits int
	// carryState marks a decoder whose state crosses unit boundaries.
	carryState bool
}

// NewDemuxer parses the headers of an AIFF source. The returned Demuxer
// implements container.Seeker (PCM seeks are trivially sample-exact) and
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
	return waxerr.Malformed("aiff: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not, which is a thing a caller can act on. See
// [waxerr.Malformed] for the rule.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("aiff: ", format, args...)
}

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

func (d *Demuxer) parse() error {
	size := d.src.Size()
	var head [12]byte
	if err := container.ReadFull(d.src, head[:], 0); err != nil {
		return container.ShortRead("aiff: reading header", err)
	}
	if !Match(head[:]) {
		return malformed("not an AIFF/AIFF-C file")
	}
	aifc := string(head[8:12]) == idAIFC

	var (
		commSeen  bool
		cf        commFormat
		commUnits int64
		dataOff   int64 = -1
		dataBytes int64 = -1
	)

	off := int64(12)
	for chunks := 0; off+8 <= size; chunks++ {
		if chunks >= maxChunks {
			return malformed("more than %d chunks", maxChunks)
		}
		var hdr [8]byte
		if err := container.ReadFull(d.src, hdr[:], off); err != nil {
			return container.ShortRead("aiff: reading chunk header", err)
		}
		id := string(hdr[:4])
		chunkSize := int64(be.Uint32(hdr[4:]))

		switch id {
		case idCOMM:
			if commSeen {
				if err := d.warn(off, "duplicate COMM chunk ignored"); err != nil {
					return err
				}
				break
			}
			want := int64(18)
			if aifc {
				want = 22
			}
			if chunkSize < want {
				return malformed("COMM chunk of %d bytes, want at least %d", chunkSize, want)
			}
			n := chunkSize
			if n > maxCommPayload {
				if err := d.warn(off, "COMM chunk of %d bytes truncated to %d", chunkSize, maxCommPayload); err != nil {
					return err
				}
				n = maxCommPayload
			}
			if off+8+n > size {
				return malformed("COMM chunk extends past end of file")
			}
			payload := make([]byte, n)
			if err := container.ReadFull(d.src, payload, off+8); err != nil {
				return container.ShortRead("aiff: reading COMM", err)
			}
			var err error
			if cf, commUnits, err = d.parseCOMM(payload, aifc, off); err != nil {
				return err
			}
			commSeen = true
		case idSSND:
			if dataBytes >= 0 {
				if err := d.warn(off, "extra SSND chunk ignored"); err != nil {
					return err
				}
				break
			}
			if chunkSize < 8 {
				return malformed("SSND chunk of %d bytes, want at least 8", chunkSize)
			}
			var ssnd [8]byte
			if err := container.ReadFull(d.src, ssnd[:], off+8); err != nil {
				return container.ShortRead("aiff: reading SSND header", err)
			}
			dataStart := int64(be.Uint32(ssnd[:])) // alignment offset
			dataOff = off + 8 + 8 + dataStart
			dataBytes = chunkSize - 8 - dataStart
			if dataBytes < 0 {
				return malformed("SSND offset %d exceeds chunk", dataStart)
			}
			if dataOff+dataBytes > size {
				if err := d.warn(off, "SSND data of %d bytes exceeds file, clamped", dataBytes); err != nil {
					return err
				}
				dataBytes = size - dataOff
				if dataBytes < 0 {
					dataBytes = 0
				}
			}
		}

		next := off + 8 + chunkSize + chunkSize&1
		if next <= off {
			return malformed("chunk size overflow")
		}
		if next > size && id != idSSND {
			if err := d.warn(off, "%q chunk extends past end of file", id); err != nil {
				return err
			}
			break
		}
		off = next
	}

	if !commSeen {
		return malformed("no COMM chunk")
	}
	if dataBytes < 0 {
		return malformed("no SSND chunk")
	}
	if cf.codec == codec.MP3 {
		return d.startMPEG(cf, dataOff, dataBytes, commUnits)
	}

	unitBytes := int64(cf.unitBytes)
	if rem := dataBytes % unitBytes; rem != 0 {
		if err := d.warn(dataOff, "%d trailing bytes are not a whole %s, ignored", rem, unitName(cf)); err != nil {
			return err
		}
		dataBytes -= rem
	}
	// Both sides of this comparison are in UNITS, which is what makes one
	// clamp serve every compression type: COMM counts frames for the PCM
	// types and packets for ima4, and that is the same field counting the
	// same thing the payload is laid out in.
	available := dataBytes / unitBytes
	units := commUnits
	switch {
	case available < commUnits:
		if err := d.warn(dataOff, "COMM declares %d %ss, SSND holds %d; clamped", commUnits, unitName(cf), available); err != nil {
			return err
		}
		units = available
	case available > commUnits:
		if err := d.warn(dataOff, "SSND holds %d %ss beyond the %d COMM declares; extra ignored", available-commUnits, unitName(cf), commUnits); err != nil {
			return err
		}
	}

	d.payload = &blockReader{src: d.src, dataOff: dataOff, unitBytes: unitBytes,
		unitFrames: int64(cf.unitFrames), units: units, samples: units * int64(cf.unitFrames)}
	d.carryState = cf.carryState
	d.track = container.Track{
		Codec:       cf.codec,
		CodecConfig: cf.config,
		Fmt:         cf.fmt,
		Samples:     units * int64(cf.unitFrames),
		// Exact by construction, and saying so is what stops a caller that
		// needs an authoritative length from decoding the file to find one.
		// The count is units the payload actually holds, clamped to what COMM
		// declares, and the walk stops at exactly that many, so there is no
		// room between the number and the decode for the two to differ. The
		// trim it authorizes is a no-op here; what it buys is the measure pass
		// an HLS mint would otherwise run, which for an ima4 source is a
		// full linear decode.
		SamplesExact:   true,
		SourceBitDepth: cf.sourceBits,
		Default:        true,
	}
	return nil
}

// unitName names a unit for a message, so a length warning says which kind
// of boundary the file stopped short of.
func unitName(cf commFormat) string {
	if cf.unitFrames > 1 {
		return "packet"
	}
	return "frame"
}

// parseCOMM maps a COMM payload onto a codec, its configuration, and the
// geometry of one addressable unit, and returns the unit count the chunk
// declares.
func (d *Demuxer) parseCOMM(b []byte, aifc bool, off int64) (commFormat, int64, error) {
	var cf commFormat
	channels := int(int16(be.Uint16(b)))
	units := int64(be.Uint32(b[2:]))
	bits := int(int16(be.Uint16(b[6:])))
	rateF := fromExt80(b[8:18])

	if channels < 1 {
		return cf, 0, malformed("%d channels", channels)
	}
	if channels > audio.MaxChannels {
		return cf, 0, unsupported("%d channels (supported: 1..%d)", channels, audio.MaxChannels)
	}
	if math.IsNaN(rateF) || rateF <= 0 || rateF > math.MaxInt32 {
		return cf, 0, malformed("sample rate %v", rateF)
	}
	rate := int(math.Round(rateF))
	if float64(rate) != rateF {
		// AIFF stores the rate as an 80-bit extended float, so a fractional
		// one is a value the format allows and this pipeline rounds.
		d.note(off, "non-integer sample rate %v rounded to %d", rateF, rate)
	}

	raw := compNONE
	if aifc {
		raw = string(b[18:22])
	}
	comp := foldComp(raw)
	layout := audio.DefaultLayout(channels)
	switch comp {
	case compALaw, compULaw:
		law, id := g711.ALaw, codec.ALaw
		if comp == compULaw {
			law, id = g711.MuLaw, codec.MuLaw
		}
		// One byte per sample per channel, whatever COMM says. The field is
		// the DECODED width for a compressed type and the writers disagree
		// about which one to put there: ffmpeg writes 8 and QuickTime 16 for
		// the same bytes, so both are left alone and anything else is damage.
		if bits != 8 && bits != 16 {
			if err := d.warn(off, "%v declares %d bits per sample, want 8 or 16", law, bits); err != nil {
				return cf, 0, err
			}
		}
		cf = commFormat{codec: id, fmt: g711.Format(rate, channels, layout),
			unitBytes: channels, unitFrames: 1, sourceBits: g711.SourceBitDepth}
	case compMP3:
		// Nothing here describes the stream: an MPEG audio frame states its
		// own rate, channel mode and length in its own four-byte header, so
		// COMM's sampleSize is not read and its numSampleFrames is not
		// divided by anything. What the chunk does state is the container's
		// claim, which startMPEG puts against the first frame's own header.
		cf = commFormat{codec: codec.MP3, fmt: mp3.Header{Rate: rate, Channels: channels}.PCMFormat()}
	case compIMA4:
		// Apple's ima4 states no geometry anywhere: the type name fixes it at
		// 34 bytes and 64 samples per channel, which is why the sample entry
		// and the COMM chunk have no fields for it.
		cfg := adpcm.Config{Layout: adpcm.IMAQuickTime, Channels: channels,
			BlockAlign: adpcm.QuickTimeBlockBytes * channels, SamplesPerBlock: adpcm.QuickTimeBlockFrames}
		if err := cfg.Validate(); err != nil {
			return cf, 0, err
		}
		blob, err := cfg.MarshalBinary()
		if err != nil {
			return cf, 0, err
		}
		if bits != 4 && bits != 16 {
			if err := d.warn(off, "ima4 declares %d bits per sample, want 4 or 16", bits); err != nil {
				return cf, 0, err
			}
		}
		cf = commFormat{codec: codec.IMAADPCM, config: blob, fmt: cfg.Format(rate, layout),
			unitBytes: cfg.BlockBytes(), unitFrames: cfg.SamplesPerBlock,
			sourceBits: adpcm.SourceBitDepth, carryState: true}
	default:
		cfg, err := d.pcmConfig(comp, raw, bits)
		if err != nil {
			return cf, 0, err
		}
		blob, err := cfg.MarshalBinary()
		if err != nil {
			return cf, 0, err
		}
		cf = commFormat{codec: codec.PCM, config: blob, fmt: cfg.PCMFormat(rate, channels, layout),
			unitBytes: cfg.BytesPerFrame(channels), unitFrames: 1}
		// FL64 decodes to float32 like every other float source, so the
		// file's own depth has nowhere in audio.Format to live; see the riff
		// demuxer.
		if cfg.Encoding == pcm.Float && cfg.Bits != cf.fmt.BitDepth {
			cf.sourceBits = cfg.Bits
		}
	}
	if err := cf.fmt.Valid(); err != nil {
		return cf, 0, container.UnusableFormat("aiff", cf.fmt, err)
	}
	return cf, units, nil
}

// startMPEG builds the track for an SSND payload holding MPEG audio frames.
//
// The length is the one thing the container cannot supply. COMM's
// numSampleFrames counts frames for a byte-linear type and packets for a
// packetized one, nothing defines which it counts here, and no writer exists
// to measure: unknown is the honest answer, and a leading Xing or LAME frame
// is the only thing in reach that improves on it.
func (d *Demuxer) startMPEG(cf commFormat, dataOff, dataBytes, commUnits int64) error {
	// No trailer hook: the SSND chunk bounds the frames, so bytes inside it
	// that are not frames are damage rather than the tag baggage a bare
	// stream ends with.
	walk := mpegframes.New(d.src, dataOff+dataBytes, mpegframes.Options{
		Prefix: "aiff: ",
		Warn:   func(off int64, msg string) error { return d.warn(off, "%s", msg) },
	})
	tag, _, err := walk.Begin(dataOff)
	if err != nil {
		return err
	}
	f := walk.Header().PCMFormat()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("aiff", f, err)
	}
	if f != cf.fmt {
		// A decoder reads the frame, so the frame wins, and a chunk that
		// disagrees with the stream it describes is the file deviating from
		// its own format: damage, which --strict refuses and an ordinary
		// read carries on with the truth.
		if werr := d.warn(dataOff, "COMM says %v, the first frame says %v; the frame wins", cf.fmt, f); werr != nil {
			return werr
		}
	}

	samples, delay, padding := tag.Gapless(walk.SamplesPerFrame())
	if samples < 0 && commUnits > 0 {
		d.note(0, "COMM declares %d sample frames; nothing defines whether that counts frames or packets "+
			"for this compression type, so the length is unknown", commUnits)
	}
	d.payload = walk.Reader()
	d.track = container.Track{
		Codec:   codec.MP3,
		Fmt:     f,
		Samples: samples,
		Delay:   delay,
		Padding: padding,
		// SamplesExact stays false, unlike every other compression type
		// here. Those state their length in units this walk counts, so the
		// number and the decode cannot differ; this one has a writer's
		// metadata frame or nothing at all.
		Default: true,
	}
	return nil
}

// pcmConfig maps an uncompressed compression type onto a wire config. raw is
// the file's own spelling, which the refusal quotes.
func (d *Demuxer) pcmConfig(comp, raw string, bits int) (pcm.Config, error) {
	var cfg pcm.Config
	switch comp {
	case compNONE, compTwos, compSowt:
		if bits < 1 || bits > 32 {
			return cfg, malformed("%d bits per sample", bits)
		}
		containerBits := pcm.ContainerBits(bits)
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: containerBits, BigEndian: comp != compSowt && containerBits > 8}
		if bits != containerBits {
			cfg.ValidBits = bits
		}
	case compRaw:
		if bits != 8 {
			return cfg, malformed("raw compression with %d bits", bits)
		}
		cfg = pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}
	case compIn24, compIn32:
		// These two name their own width, and their sampleSize field is
		// conventionally 16 whatever they hold, so it is not read.
		width := 24
		if comp == compIn32 {
			width = 32
		}
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: width, BigEndian: true}
	case compFl32:
		cfg = pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}
	case compFl64:
		cfg = pcm.Config{Encoding: pcm.Float, Bits: 64, BigEndian: true}
	default:
		// Named where a name is known, so the refusal says what the file
		// holds rather than only that it is not one of the above. Scoped to
		// the container: MACE and the video types name codecs nothing in this
		// build decodes, but an MP3 in an AIFF-C names one it does elsewhere,
		// so "this build has no decoder for it" would be false.
		if name := codecname.Fourcc(raw); name != "" {
			return cfg, unsupported("this build does not read %s (compression type %q) from an AIFF-C", name, raw)
		}
		return cfg, unsupported("this build does not read compression type %q from an AIFF-C", raw)
	}
	return cfg, cfg.Validate()
}

// Tracks returns the single audio track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// ReadPacket yields the next run of payload. Packet data is reused across
// calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error { return d.payload.ReadPacket(pkt) }

// ReadPacket yields the next run of units: up to audio.StandardChunk frames
// of raw interleaved PCM, or the packets that decode to about as many.
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
		return container.ShortRead("aiff: reading SSND data", err)
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: r.buf,
			PTS:  r.pos * r.unitFrames,
			Dur:  n * r.unitFrames,
			// Every PCM frame is independently decodable. An ima4 packet is
			// not, strictly: its header restates only nine bits of the
			// predictor, which is why SeekSample below refuses to start at
			// one. What Sync marks is which packets the walk may BEGIN at,
			// and after a seek to zero that is all of them.
			Sync: true,
		},
	}
	r.pos += n
	return nil
}

// SeekSample repositions to the unit or frame holding the given sample.
//
// Landing is exact for every type but ima4, whose unit is 64 samples, and MP3,
// whose landing backs off far enough for the bit reservoir and the filterbank
// to converge. format.Media decodes and discards the remainder, so the
// delivered position is sample-exact in every case.
//
// ima4 lands at the start of the file whatever the target, which is not a
// pessimisation but the only correct landing: Apple's layout carries the
// predictor across packets and a header restates only its top nine bits, so a
// decode begun at any other packet sits up to 127 LSB from the linear one and
// never rejoins it. The engine's discard then runs the linear decode up to the
// target, which costs about a second per hour of stereo 44.1 kHz audio and is
// exact. Fixing that cost means checkpointing decoder state, never starting
// fresh at a packet.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("aiff: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "aiff: negative seek target")
	}
	if d.carryState {
		// Apple's ima4 layout carries the predictor across packets, so the
		// only landing a decode converges from is the head of the stream.
		sample = 0
	}
	return d.payload.SeekSample(sample)
}

// SeekSample lands on the unit boundary at or before the target.
func (r *blockReader) SeekSample(sample int64) (int64, error) {
	// Clamped to the track's length rather than the payload's; see the riff
	// sibling, where the two can differ.
	r.pos = min(min(sample, r.samples)/r.unitFrames, r.units)
	return r.pos * r.unitFrames, nil
}
