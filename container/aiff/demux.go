package aiff

import (
	"fmt"
	"io"
	"math"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
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
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one PCM track from an AIFF or AIFF-C source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	track      container.Track
	frameBytes int
	dataOff    int64
	pos        int64
	warnings   []container.Warning
	readBuf    []byte
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

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "aiff: "+fmt.Sprintf(format, args...))
}

func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: msg})
	return nil
}

func (d *Demuxer) parse() error {
	size := d.src.Size()
	var head [12]byte
	if err := container.ReadFull(d.src, head[:], 0); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "aiff: reading header", err)
	}
	if !Match(head[:]) {
		return malformed("not an AIFF/AIFF-C file")
	}
	aifc := string(head[8:12]) == idAIFC

	var (
		commSeen   bool
		cfg        pcm.Config
		rate       int
		channels   int
		commFrames int64
		dataBytes  int64 = -1
	)

	off := int64(12)
	for chunks := 0; off+8 <= size; chunks++ {
		if chunks >= maxChunks {
			return malformed("more than %d chunks", maxChunks)
		}
		var hdr [8]byte
		if err := container.ReadFull(d.src, hdr[:], off); err != nil {
			return waxerr.Wrap(waxerr.CodeSourceUnreadable, "aiff: reading chunk header", err)
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
				return waxerr.Wrap(waxerr.CodeSourceUnreadable, "aiff: reading COMM", err)
			}
			var err error
			cfg, rate, channels, commFrames, err = d.parseCOMM(payload, aifc, off)
			if err != nil {
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
				return waxerr.Wrap(waxerr.CodeSourceUnreadable, "aiff: reading SSND header", err)
			}
			dataStart := int64(be.Uint32(ssnd[:])) // alignment offset
			d.dataOff = off + 8 + 8 + dataStart
			dataBytes = chunkSize - 8 - dataStart
			if dataBytes < 0 {
				return malformed("SSND offset %d exceeds chunk", dataStart)
			}
			if d.dataOff+dataBytes > size {
				if err := d.warn(off, "SSND data of %d bytes exceeds file, clamped", dataBytes); err != nil {
					return err
				}
				dataBytes = size - d.dataOff
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

	d.frameBytes = cfg.BytesPerFrame(channels)
	if rem := dataBytes % int64(d.frameBytes); rem != 0 {
		if err := d.warn(d.dataOff, "%d trailing bytes are not a whole frame, ignored", rem); err != nil {
			return err
		}
		dataBytes -= rem
	}
	available := dataBytes / int64(d.frameBytes)
	samples := commFrames
	switch {
	case available < commFrames:
		if err := d.warn(d.dataOff, "COMM declares %d frames, SSND holds %d; clamped", commFrames, available); err != nil {
			return err
		}
		samples = available
	case available > commFrames:
		if err := d.warn(d.dataOff, "SSND holds %d frames beyond the %d COMM declares; extra ignored", available-commFrames, commFrames); err != nil {
			return err
		}
	}

	cfgBytes, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	f := cfg.PCMFormat(rate, channels, audio.DefaultLayout(channels))
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "aiff: unusable format", err)
	}
	d.track = container.Track{
		Codec:       codec.PCM,
		CodecConfig: cfgBytes,
		Fmt:         f,
		Samples:     samples,
		Default:     true,
	}
	return nil
}

// parseCOMM maps a COMM payload onto a wire config and stream parameters.
func (d *Demuxer) parseCOMM(b []byte, aifc bool, off int64) (cfg pcm.Config, rate, channels int, frames int64, err error) {
	channels = int(int16(be.Uint16(b)))
	frames = int64(be.Uint32(b[2:]))
	bits := int(int16(be.Uint16(b[6:])))
	rateF := fromExt80(b[8:18])

	if channels < 1 || channels > audio.MaxChannels {
		return cfg, 0, 0, 0, malformed("%d channels (supported: 1..%d)", channels, audio.MaxChannels)
	}
	if math.IsNaN(rateF) || rateF <= 0 || rateF > math.MaxInt32 {
		return cfg, 0, 0, 0, malformed("sample rate %v", rateF)
	}
	rate = int(math.Round(rateF))
	if float64(rate) != rateF {
		if werr := d.warn(off, "non-integer sample rate %v rounded to %d", rateF, rate); werr != nil {
			return cfg, 0, 0, 0, werr
		}
	}

	comp := compNONE
	if aifc {
		comp = string(b[18:22])
	}
	switch comp {
	case compNONE, compTwos:
		if bits < 1 || bits > 32 {
			return cfg, 0, 0, 0, malformed("%d bits per sample", bits)
		}
		containerBits := pcm.ContainerBits(bits)
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: containerBits, BigEndian: containerBits > 8}
		if bits != containerBits {
			cfg.ValidBits = bits
		}
	case compSowt:
		if bits < 1 || bits > 32 {
			return cfg, 0, 0, 0, malformed("%d bits per sample", bits)
		}
		containerBits := pcm.ContainerBits(bits)
		cfg = pcm.Config{Encoding: pcm.SignedInt, Bits: containerBits}
		if bits != containerBits {
			cfg.ValidBits = bits
		}
	case compRaw:
		if bits != 8 {
			return cfg, 0, 0, 0, malformed("raw compression with %d bits", bits)
		}
		cfg = pcm.Config{Encoding: pcm.UnsignedInt, Bits: 8}
	case compFl32, compFL32:
		cfg = pcm.Config{Encoding: pcm.Float, Bits: 32, BigEndian: true}
	case compFl64, compFL64:
		cfg = pcm.Config{Encoding: pcm.Float, Bits: 64, BigEndian: true}
	default:
		return cfg, 0, 0, 0, malformed("compression type %q (only PCM types are supported)", comp)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, 0, 0, 0, err
	}
	return cfg, rate, channels, frames, nil
}

// Tracks returns the single PCM track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// ReadPacket yields up to audio.StandardChunk frames of raw interleaved
// PCM. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	remaining := d.track.Samples - d.pos
	if remaining <= 0 {
		return io.EOF
	}
	frames := int64(audio.StandardChunk)
	if frames > remaining {
		frames = remaining
	}
	need := int(frames) * d.frameBytes
	if cap(d.readBuf) < need {
		d.readBuf = make([]byte, need)
	}
	d.readBuf = d.readBuf[:need]
	if err := container.ReadFull(d.src, d.readBuf, d.dataOff+d.pos*int64(d.frameBytes)); err != nil {
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "aiff: reading SSND data", err)
	}
	*pkt = container.Packet{
		Track:  0,
		Packet: codec.Packet{Data: d.readBuf, PTS: d.pos, Dur: frames, Sync: true},
	}
	d.pos += frames
	return nil
}

// SeekSample repositions to the given sample; landing is exact.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("aiff: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "aiff: negative seek target")
	}
	if sample > d.track.Samples {
		sample = d.track.Samples
	}
	d.pos = sample
	return sample, nil
}
