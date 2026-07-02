package riff

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

// Hostile-input caps (ADR-0005 invariants). A legitimate WAV holds a
// handful of chunks; parsing stops long before any pathological count.
const (
	maxChunks     = 1024
	maxFmtPayload = 4096
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	// Conformance tests and `waxflow probe --strict` use it; playback
	// paths stay tolerant because real libraries are messy.
	Strict bool
}

// Demuxer reads one PCM track from a WAV source.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions

	track      container.Track
	frameBytes int
	dataOff    int64
	pos        int64 // next frame to read
	warnings   []container.Warning
	readBuf    []byte
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

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "riff: "+fmt.Sprintf(format, args...))
}

// warn records tolerated damage, or fails in strict mode.
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
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "riff: reading header", err)
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
		cfg             pcm.Config
		rate, channels  int
		layout          audio.ChannelMask
		dataBytes       int64 = -1
		streamingData   bool
	)

	off := int64(12)
	for chunks := 0; off+8 <= size; chunks++ {
		if chunks >= maxChunks {
			return malformed("more than %d chunks", maxChunks)
		}
		var hdr [8]byte
		if err := container.ReadFull(d.src, hdr[:], off); err != nil {
			return waxerr.Wrap(waxerr.CodeSourceUnreadable, "riff: reading chunk header", err)
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
				return waxerr.Wrap(waxerr.CodeSourceUnreadable, "riff: reading ds64", err)
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
				return waxerr.Wrap(waxerr.CodeSourceUnreadable, "riff: reading fmt", err)
			}
			var err error
			cfg, rate, channels, layout, err = d.parseFmt(payload, off)
			if err != nil {
				return err
			}
			fmtSeen = true
		case idData:
			if dataBytes >= 0 {
				if err := d.warn(off, "extra data chunk ignored"); err != nil {
					return err
				}
				break
			}
			d.dataOff = off + 8
			switch {
			case chunkSize == size32Unknown && haveDS64:
				// ds64 sizes are 64-bit and file-supplied: bound against
				// the file before converting, or a huge value wraps int64
				// negative and dodges the clamp.
				if ds64DataSize > uint64(size-d.dataOff) {
					if err := d.warn(off, "ds64 data size %d exceeds file, clamped", ds64DataSize); err != nil {
						return err
					}
					dataBytes = size - d.dataOff
				} else {
					dataBytes = int64(ds64DataSize)
				}
			case chunkSize == size32Unknown:
				// The live-written convention: data runs to end of file.
				if err := d.warn(off, "streaming data size, clamped to end of file"); err != nil {
					return err
				}
				dataBytes = size - d.dataOff
				streamingData = true
			case d.dataOff+chunkSize > size:
				if err := d.warn(off, "data chunk size %d exceeds file, clamped", chunkSize); err != nil {
					return err
				}
				dataBytes = size - d.dataOff
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

	d.frameBytes = cfg.BytesPerFrame(channels)
	if rem := dataBytes % int64(d.frameBytes); rem != 0 {
		if err := d.warn(d.dataOff, "%d trailing bytes are not a whole frame, ignored", rem); err != nil {
			return err
		}
		dataBytes -= rem
	}
	samples := dataBytes / int64(d.frameBytes)
	if haveDS64 && ds64SampleCount != 0 && ds64SampleCount != uint64(samples) {
		if err := d.warn(0, "ds64 sample count %d disagrees with data size (%d frames)", ds64SampleCount, samples); err != nil {
			return err
		}
	}

	cfgBytes, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	f := cfg.PCMFormat(rate, channels, layout)
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "riff: unusable format", err)
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

// parseFmt maps a fmt chunk payload onto a wire config and stream
// parameters.
func (d *Demuxer) parseFmt(b []byte, off int64) (cfg pcm.Config, rate, channels int, layout audio.ChannelMask, err error) {
	tag := le.Uint16(b)
	channels = int(le.Uint16(b[2:]))
	rate64 := int64(le.Uint32(b[4:]))
	blockAlign := int(le.Uint16(b[12:]))
	bits := int(le.Uint16(b[14:]))

	if channels < 1 || channels > audio.MaxChannels {
		return cfg, 0, 0, 0, malformed("%d channels (supported: 1..%d)", channels, audio.MaxChannels)
	}
	// Bound in int64 so acceptance does not depend on the platform's int
	// width (a rate above MaxInt32 would wrap negative on 32-bit builds).
	if rate64 <= 0 || rate64 > math.MaxInt32 {
		return cfg, 0, 0, 0, malformed("sample rate %d", rate64)
	}
	rate = int(rate64)
	if bits < 1 || bits > 64 {
		return cfg, 0, 0, 0, malformed("%d bits per sample", bits)
	}

	validBits := 0
	layout = audio.DefaultLayout(channels)
	if tag == tagExtensible {
		if len(b) < 40 {
			return cfg, 0, 0, 0, malformed("extensible fmt chunk of %d bytes, want 40", len(b))
		}
		if bits%8 != 0 {
			return cfg, 0, 0, 0, malformed("extensible container of %d bits is not whole bytes", bits)
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
				return cfg, 0, 0, 0, werr
			}
		default:
			layout = mask
		}
		subTag := le.Uint16(b[24:])
		if [14]byte(b[26:40]) != guidTail {
			return cfg, 0, 0, 0, malformed("unknown extensible subformat GUID")
		}
		tag = subTag
	}

	containerBits := pcm.ContainerBits(bits)
	switch tag {
	case tagPCM:
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
	case tagIEEEFloat:
		if bits != 32 && bits != 64 {
			return cfg, 0, 0, 0, malformed("%d-bit float", bits)
		}
		cfg = pcm.Config{Encoding: pcm.Float, Bits: bits}
	default:
		return cfg, 0, 0, 0, malformed("format tag 0x%04X (only integer and float PCM are supported)", tag)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, 0, 0, 0, err
	}

	if want := cfg.BytesPerFrame(channels); blockAlign != want {
		if werr := d.warn(off, "block align %d, computed %d; using computed", blockAlign, want); werr != nil {
			return cfg, 0, 0, 0, werr
		}
	}
	return cfg, rate, channels, layout, nil
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
		return waxerr.Wrap(waxerr.CodeSourceUnreadable, "riff: reading data", err)
	}
	*pkt = container.Packet{
		Track:  0,
		Packet: codec.Packet{Data: d.readBuf, PTS: d.pos, Dur: frames, Sync: true},
	}
	d.pos += frames
	return nil
}

// SeekSample repositions to the given sample. Every PCM frame is a sync
// point, so landing is exact; targets past the end land at the end and
// the next ReadPacket returns io.EOF.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("riff: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "riff: negative seek target")
	}
	if sample > d.track.Samples {
		sample = d.track.Samples
	}
	d.pos = sample
	return sample, nil
}
