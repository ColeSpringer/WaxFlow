package mpc

import (
	"bytes"
	"fmt"
	"io"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/internal/trailer"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer   = (*Demuxer)(nil)
	_ container.Seeker    = (*Demuxer)(nil)
	_ container.Warner    = (*Demuxer)(nil)
	_ container.Tagger    = (*Demuxer)(nil)
	_ container.Chapterer = (*Demuxer)(nil)
	_ container.Indexer   = (*Demuxer)(nil)
)

// Demuxer reads one Musepack track from a native .mpc source.
type Demuxer struct {
	opts DemuxerOptions

	src container.Source
	w   srcwin.Window
	// base is the byte offset of the magic: nonzero when a tag or padding sits
	// in front of it. SV7 word positions and SV8 seek-table entries are both
	// measured from here.
	base int64

	cfg      musepack.Config
	track    container.Track
	tags     map[string][]string
	chapters []container.Chapter
	warnings []container.Warning
	rg       replayGain

	// total is how many packets the stream delivers (SV7 frames, SV8
	// blocks) and next the one ReadPacket yields next.
	total int64
	next  int64
	pkt   []byte
	// pending is the decoder state a seek landing owes the next packet.
	pending *musepack.State

	sv7 sv7Stream
	sv8 sv8Stream
}

// NewDemuxer parses the header of a Musepack source, verifies its declared
// length against the packets present, and positions on the first frame. The
// returned Demuxer implements container.Seeker, container.Warner,
// container.Tagger, container.Chapterer, and container.Indexer.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "musepack: reading stream data")}
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
	return waxerr.Malformed("musepack: ", format, args...)
}

// unsupported names a well-formed stream this build does not cover. It is a
// different answer from malformed and carries a different code: the file is
// fine and we are not, which is a thing a caller can act on. See
// [waxerr.Malformed] for the rule.
func unsupported(format string, args ...any) error {
	return waxerr.Unsupported("musepack: ", format, args...)
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
	w := container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...), Kind: container.Note}
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
}

func (d *Demuxer) parse() error {
	base, err := d.findMagic()
	if err != nil {
		return err
	}
	d.base = base
	head := d.w.BytesAt(base, 4)
	if len(head) < 4 {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("stream header truncated")
	}
	switch {
	case string(head) == "MPCK":
		err = d.parseSV8()
	case string(head[:3]) == "MP+" && head[3]&0x0F == 7:
		err = d.parseSV7()
	case string(head[:3]) == "MP+":
		// The stream versions before SV7 share the magic and nothing else; the
		// reference refuses them too.
		return unsupported("Musepack SV%d is not supported: only SV7 and SV8 are", head[3]&0x0F)
	default:
		return malformed("not a Musepack stream")
	}
	if err != nil {
		return err
	}
	if err := d.w.Err(); err != nil {
		return err
	}
	f := d.cfg.Format()
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("musepack", f, err)
	}
	blob, err := d.cfg.MarshalBinary()
	if err != nil {
		return err
	}
	d.track.Codec = codec.Musepack
	d.track.CodecConfig = blob
	d.track.Fmt = f
	d.track.Default = true
	return nil
}

// findMagic locates the stream magic: at the end of a leading ID3v2 tag when
// there is one, else within the junk allowance. A "MP+" with a stream version
// this decoder refuses still counts as found, so the refusal can name it.
func (d *Demuxer) findMagic() (int64, error) {
	from := id3.Size(d.w.BytesAt(0, 10))
	limit := min(from+maxJunk, d.w.DataEnd())
	for off := from; off+4 <= limit; {
		b := d.w.BytesAt(off, srcwin.Chunk)
		if len(b) < 4 {
			break
		}
		i := bytes.Index(b, []byte("MP"))
		if i < 0 {
			off += int64(len(b)) - 3
			continue
		}
		cand := off + int64(i)
		if h := d.w.BytesAt(cand, 4); len(h) == 4 && cand < limit &&
			(string(h) == "MPCK" || (string(h[:3]) == "MP+" && h[3]&0x0F >= 4 && h[3]&0x0F <= 7)) {
			return cand, nil
		}
		off = cand + 1
	}
	if d.w.Err() != nil {
		return 0, d.w.Err()
	}
	return 0, malformed("not a Musepack stream")
}

// stripTrailers peels the tags a tagger appended after the audio, shrinking
// the window's data end so the packet walk never runs into them. The APEv2
// block is also where the track's tags come from. No warning: a tag after the
// audio is the normal shape of a .mpc file rather than damage recovered from.
func (d *Demuxer) stripTrailers(floor int64) {
	end, tags := trailer.PeelAll(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2, floor, d.w.DataEnd())
	d.tags = tags
	d.w.SetDataEnd(end)
}

// Tracks returns the single Musepack track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// ReadPacket yields one frame (SV7) or one block (SV8) behind the header the
// decoder reads, with the decoder state a seek landing owes in front of the
// first packet after it. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if d.next >= d.total {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	var err error
	if d.cfg.StreamVersion == 7 {
		err = d.sv7Packet(d.next, pkt)
	} else {
		err = d.sv8Packet(d.next, pkt)
	}
	if err != nil {
		return err
	}
	d.pending = nil
	d.next++
	return nil
}

// packetHeader lays out the packet header and the pending state block into
// d.pkt and returns the payload's start. The state block is what a decoder
// that was Reset needs to continue from anywhere but frame zero.
func (d *Demuxer) packetHeader(frames, bitLen int) int {
	n := musepack.PacketHeaderLen
	if d.pending != nil {
		n += musepack.StateLenSV8
		if d.cfg.StreamVersion == 7 {
			n += musepack.StateLenSV7 - musepack.StateLenSV8
		}
	}
	if cap(d.pkt) < n {
		d.pkt = make([]byte, n, n+4096)
	}
	d.pkt = d.pkt[:n]
	musepack.PutPacketHeader(d.pkt, frames, bitLen, d.pending != nil)
	if d.pending != nil {
		d.pkt = d.pending.AppendBinary(d.pkt[:musepack.PacketHeaderLen], d.cfg.StreamVersion)
	}
	return len(d.pkt)
}

// SeekSample repositions to the packet a seek must resume from: the one
// holding the frame that starts synthWarmup samples before the target, so the
// filter is warm again before the pre-roll ends. It returns that packet's first
// sample; format.Media pre-rolls the remainder. The decoder state at the
// landing is scanned from the stream (cached across seeks) and handed over in
// the first packet.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("musepack: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "musepack: negative seek target")
	}
	if d.total == 0 {
		d.next = 0
		return 0, nil
	}
	frame := max(sample-synthWarmup, 0) / musepack.FrameLength
	if d.cfg.StreamVersion == 7 {
		frame = min(frame, d.total-1)
		st, err := d.sv7StateAt(frame)
		if err != nil {
			return 0, err
		}
		d.setLanding(frame, st)
		return frame * musepack.FrameLength, nil
	}
	fpb := int64(d.cfg.FramesPerBlock())
	block := min(frame/fpb, d.total-1)
	st, err := d.sv8StateAt(block)
	if err != nil {
		return 0, err
	}
	d.setLanding(block, st)
	return block * fpb * musepack.FrameLength, nil
}

// setLanding positions the next read and records the state it must carry. A
// landing at the stream's start carries none: the decoder's Reset state is
// the stream-start state, so the packet reads exactly as a linear read's.
func (d *Demuxer) setLanding(index int64, st musepack.State) {
	d.next = index
	d.pending = nil
	if index > 0 {
		d.pending = &st
	}
}
