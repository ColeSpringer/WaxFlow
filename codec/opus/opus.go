// Package opus decodes Opus audio (RFC 6716, with the RFC 8251 errata) carried
// in Ogg (RFC 7845). Opus always decodes to 48 kHz; the decoder emits planar
// float32 and the container applies pre-skip and end-trim.
//
// Opus has three internal modes selected per packet by the TOC byte: SILK
// (speech, LPC), CELT (music, MDCT), and a hybrid that stacks SILK on the low
// band and CELT on the high band. The range decoder (rangedec.go) is shared;
// each mode has its own decode path. The implementation is clean-room from the
// RFC, cross-checked structurally against libopus.
package opus

import (
	"encoding/binary"
	"fmt"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/waxerr"
)

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "opus: "+fmt.Sprintf(format, args...))
}

// SampleRate is Opus's fixed decode rate; every stream decodes to 48 kHz
// regardless of the OpusHead input rate (RFC 7845).
const SampleRate = 48000

// SeekPreroll is the decoder convergence time RFC 7845 section 4.2
// recommends: 80 ms at Opus's fixed 48 kHz. A decoder started cold at an
// arbitrary packet has neither its filter state nor its overlap window, so
// audio before this much has converged is not the audio the encoder wrote.
//
// It is the amount a cut backs its head off by, which is what makes an
// exact head mean exact *audio* rather than an exact sample index. It is
// also what opusenc writes as its pre-skip, and that coincidence is worth
// naming because it is the one that bites: 3840 is a whole multiple of the
// 960-sample grid, so a snap that did not back off by it would drop every
// priming packet of such a stream and declare Delay 0, leaving a cold
// decoder at output sample 0.
const SeekPreroll = 3840

// SetPreSkip returns a copy of an OpusHead header with its pre-skip set to
// n samples.
//
// The header is copied rather than patched in place because a
// container.Track's CodecConfig is shared with the demuxer that produced
// it: patching would reach back into a stream still being read.
//
// This is the rewrite a cut cannot skip. The pre-skip in OpusHead is the
// authority every muxer reads for Opus priming (mka overwrites Track.Delay
// from it outright, and mp4 and ogg source it independently of Track.Delay
// too), so setting Track.Delay without also rewriting the config does
// nothing at all: the trim silently never happens.
func SetPreSkip(head []byte, n int) ([]byte, error) {
	if _, err := ParseOpusHead(head); err != nil {
		return nil, err
	}
	if n < 0 || n > 65535 {
		return nil, malformed("pre-skip %d does not fit OpusHead's 16-bit field", n)
	}
	out := make([]byte, len(head))
	copy(out, head)
	binary.LittleEndian.PutUint16(out[10:], uint16(n))
	return out, nil
}

// Config is the stream configuration from the OpusHead identification header
// (RFC 7845 section 5.1).
type Config struct {
	Channels int
	PreSkip  int
	Gain     int16 // Q7.8 dB, output gain
	Family   int
}

// ParseOpusHead parses an OpusHead header into a Config.
func ParseOpusHead(head []byte) (Config, error) {
	var c Config
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return c, malformed("not an OpusHead header")
	}
	if head[8]&0xF0 != 0 {
		return c, malformed("unsupported OpusHead version %d", head[8])
	}
	c.Channels = int(head[9])
	if c.Channels < 1 || c.Channels > audio.MaxChannels {
		return c, malformed("OpusHead channel count %d", c.Channels)
	}
	c.PreSkip = int(binary.LittleEndian.Uint16(head[10:]))
	c.Gain = int16(binary.LittleEndian.Uint16(head[16:]))
	c.Family = int(head[18])
	if c.Family == 0 && c.Channels > 2 {
		return c, malformed("OpusHead family 0 with %d channels", c.Channels)
	}
	return c, nil
}

// Format returns the PCM format the decoder produces.
func (c Config) Format() audio.Format {
	return audio.Format{
		Rate:     SampleRate,
		Channels: c.Channels,
		Layout:   audio.DefaultLayout(c.Channels),
		Type:     audio.Float,
		BitDepth: 32,
	}
}

// Opus internal modes (RFC 6716 section 3.1).
const (
	modeSILK = iota
	modeHybrid
	modeCELT
)

// Bandwidths (RFC 6716 Table 1).
const (
	bandNB  = iota // 4 kHz
	bandMB         // 6 kHz
	bandWB         // 8 kHz
	bandSWB        // 12 kHz
	bandFB         // 20 kHz
)

// tocConfig describes one of the 32 TOC configurations.
type tocConfig struct {
	mode      int
	bandwidth int
	frameSize int // in 48 kHz samples
}

// tocTable maps the 5-bit TOC config to its mode, bandwidth, and frame size
// (RFC 6716 Table 2).
var tocTable = func() [32]tocConfig {
	var t [32]tocConfig
	// SILK NB/MB/WB: 10/20/40/60 ms.
	silkMs := []int{480, 960, 1920, 2880}
	for i, bw := range []int{bandNB, bandMB, bandWB} {
		for j, fs := range silkMs {
			t[i*4+j] = tocConfig{modeSILK, bw, fs}
		}
	}
	// Hybrid SWB/FB: 10/20 ms.
	for i, bw := range []int{bandSWB, bandFB} {
		for j, fs := range []int{480, 960} {
			t[12+i*2+j] = tocConfig{modeHybrid, bw, fs}
		}
	}
	// CELT NB/WB/SWB/FB: 2.5/5/10/20 ms.
	celtMs := []int{120, 240, 480, 960}
	for i, bw := range []int{bandNB, bandWB, bandSWB, bandFB} {
		for j, fs := range celtMs {
			t[16+i*4+j] = tocConfig{modeCELT, bw, fs}
		}
	}
	return t
}()

// PacketSamples returns the number of 48 kHz samples a packet decodes to,
// derived from its TOC byte and frame count alone (RFC 6716 section 3.1). It
// reads only the first one or two bytes, so a truncated leading slice still
// times correctly, which is what lets containers time Opus packets from a
// prefix without decoding or materializing the whole packet.
func PacketSamples(pkt []byte) (int, error) {
	if len(pkt) == 0 {
		return 0, malformed("empty packet")
	}
	frameSize := tocTable[pkt[0]>>3].frameSize
	var frames int
	switch pkt[0] & 0x3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(pkt) < 2 {
			return 0, malformed("code 3 packet missing frame count byte")
		}
		frames = int(pkt[1] & 0x3F)
	}
	total := frameSize * frames
	// RFC 6716: a packet spans at most 120 ms (5760 samples at 48 kHz).
	if frames <= 0 || total <= 0 || total > 5760 {
		return 0, malformed("packet duration %d samples out of range", total)
	}
	return total, nil
}

// frame is one Opus frame's compressed bytes plus its shared configuration.
type frame struct {
	data   []byte
	cfg    tocConfig
	stereo bool
}

// splitPacket parses a packet's TOC and framing into individual frames
// (RFC 6716 section 3.2).
func splitPacket(pkt []byte) ([]frame, error) {
	if len(pkt) < 1 {
		return nil, malformed("empty packet")
	}
	toc := pkt[0]
	cfg := tocTable[toc>>3]
	stereo := toc&0x4 != 0
	code := toc & 0x3
	body := pkt[1:]

	mkFrames := func(sizes []int, data []byte) ([]frame, error) {
		frames := make([]frame, 0, len(sizes))
		off := 0
		for _, s := range sizes {
			if off+s > len(data) {
				return nil, malformed("frame length %d overruns packet", s)
			}
			frames = append(frames, frame{data: data[off : off+s], cfg: cfg, stereo: stereo})
			off += s
		}
		return frames, nil
	}

	switch code {
	case 0:
		return []frame{{data: body, cfg: cfg, stereo: stereo}}, nil
	case 1:
		if len(body)%2 != 0 {
			return nil, malformed("code 1 packet with odd length")
		}
		h := len(body) / 2
		return mkFrames([]int{h, h}, body)
	case 2:
		n, rest, err := frameLength(body)
		if err != nil {
			return nil, err
		}
		if n > len(rest) {
			return nil, malformed("code 2 first frame overruns packet")
		}
		return []frame{
			{data: rest[:n], cfg: cfg, stereo: stereo},
			{data: rest[n:], cfg: cfg, stereo: stereo},
		}, nil
	default: // code 3
		return splitCode3(body, cfg, stereo)
	}
}

// splitCode3 parses a code-3 (arbitrary frame count) packet (RFC 6716 3.2.5).
func splitCode3(body []byte, cfg tocConfig, stereo bool) ([]frame, error) {
	if len(body) < 1 {
		return nil, malformed("code 3 packet missing frame count byte")
	}
	fc := body[0]
	vbr := fc&0x80 != 0
	padded := fc&0x40 != 0
	count := int(fc & 0x3F)
	body = body[1:]
	if count < 1 || count > 48 {
		return nil, malformed("code 3 frame count %d", count)
	}
	// Padding: one or more length bytes; 255 means 254 padding plus continue.
	padding := 0
	if padded {
		for {
			if len(body) < 1 {
				return nil, malformed("code 3 padding overruns packet")
			}
			p := int(body[0])
			body = body[1:]
			if p == 255 {
				padding += 254
				continue
			}
			padding += p
			break
		}
	}
	if padding > len(body) {
		return nil, malformed("code 3 padding %d overruns packet", padding)
	}
	body = body[:len(body)-padding]

	sizes := make([]int, count)
	if vbr {
		total := 0
		for i := 0; i < count-1; i++ {
			n, rest, err := frameLength(body)
			if err != nil {
				return nil, err
			}
			sizes[i] = n
			total += n
			body = rest
		}
		if total > len(body) {
			return nil, malformed("code 3 VBR frames overrun packet")
		}
		sizes[count-1] = len(body) - total
	} else {
		if len(body)%count != 0 {
			return nil, malformed("code 3 CBR length not divisible by frame count")
		}
		each := len(body) / count
		for i := range sizes {
			sizes[i] = each
		}
	}
	frames := make([]frame, count)
	off := 0
	for i, s := range sizes {
		if off+s > len(body) {
			return nil, malformed("code 3 frame overruns packet")
		}
		frames[i] = frame{data: body[off : off+s], cfg: cfg, stereo: stereo}
		off += s
	}
	return frames, nil
}

// frameLength decodes a 1- or 2-byte frame length prefix (RFC 6716 3.2.1).
func frameLength(b []byte) (n int, rest []byte, err error) {
	if len(b) < 1 {
		return 0, nil, malformed("missing frame length")
	}
	first := int(b[0])
	if first < 252 {
		return first, b[1:], nil
	}
	if len(b) < 2 {
		return 0, nil, malformed("truncated 2-byte frame length")
	}
	return first + int(b[1])*4, b[2:], nil
}
