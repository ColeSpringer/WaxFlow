package format

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// media wires a demuxer and decoder behind the Media interface. It owns
// position authority (ADR-0006): every delivered chunk is stamped with its
// source-timeline position, and seeks pre-roll from the demuxer's sync
// point so landing is sample-exact.
//
// Decoded frames flow straight into the active ReadChunk destination;
// only overflow past its capacity (and pre-roll output) is staged in the
// carry buffer, so the common aligned path copies each sample once.
//
// End-of-stream is compared with == against the bare io.EOF sentinel,
// per the Demuxer contract: an errors.Is match would also accept a
// wrapped I/O failure whose chain happens to contain io.EOF, silently
// truncating the stream.
type media struct {
	info    *Info
	demux   container.Demuxer
	seeker  container.Seeker // nil when the demuxer cannot seek
	track   container.Track
	decoder codec.Decoder

	stashFn  func(*audio.Buffer) error // m.stash bound once, decoder emit target
	sink     *audio.Buffer             // active ReadChunk destination; nil during pre-roll
	carry    *audio.Buffer             // overflow frames not yet delivered
	carryOff int                       // frames of carry already consumed
	pos      int64                     // source-timeline position of the next frame out
	discont  bool                      // stamp the next chunk as a discontinuity
	eof      bool
	closed   bool
}

func newMedia(info *Info, demux container.Demuxer) (Media, error) {
	if len(info.Tracks) == 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "format: no audio tracks")
	}
	track := info.Default()
	dec, err := newDecoder(track)
	if err != nil {
		return nil, err
	}
	m := &media{info: info, demux: demux, track: track, decoder: dec}
	m.stashFn = m.stash
	if s, ok := demux.(container.Seeker); ok {
		m.seeker = s
	}
	return m, nil
}

func (m *media) Info() *Info { return m.info }

func (m *media) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	audio.Put(m.carry)
	m.carry = nil
	if r, ok := m.decoder.(codec.Releaser); ok {
		r.Release()
	}
	return nil
}

// ReadChunk fills dst to capacity from the decoded stream.
func (m *media) ReadChunk(dst *audio.Buffer) error {
	if m.closed {
		return waxerr.New(waxerr.CodeInternal, "format: ReadChunk on closed media")
	}
	if dst.Fmt != m.track.Fmt {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("format: chunk buffer is %v, track is %v", dst.Fmt, m.track.Fmt))
	}
	if dst.Cap() == 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "format: zero-capacity chunk buffer")
	}
	dst.N = 0
	m.copyOut(dst)
	for dst.N < dst.Cap() && !m.eof {
		if err := m.fill(dst); err != nil && err != io.EOF {
			return err
		}
	}
	if dst.N == 0 {
		return io.EOF
	}
	dst.Pos = m.pos
	dst.Discont = m.discont
	m.discont = false
	m.pos += int64(dst.N)
	return nil
}

// SeekSample repositions to target. The demuxer lands on a sync point at
// or before it; the remainder is decoded and discarded so the next chunk
// starts exactly at target (or at end of stream for past-the-end
// targets). When the stream's first sync point lies beyond the target
// (container.Seeker allows that landing), the returned position exceeds
// the target and is where the next chunk really starts.
func (m *media) SeekSample(target int64) (int64, error) {
	if m.closed {
		return 0, waxerr.New(waxerr.CodeInternal, "format: SeekSample on closed media")
	}
	if m.seeker == nil {
		return 0, waxerr.New(waxerr.CodeUnsupportedFormat, "format: source is not seekable")
	}
	if target < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "format: negative seek target")
	}
	landed, err := m.seeker.SeekSample(m.track.ID, target)
	if err != nil {
		return 0, err
	}
	m.decoder.Reset()
	m.carryOff = 0
	if m.carry != nil {
		m.carry.N = 0
	}
	m.eof = false

	// Pre-roll: decode into the carry and discard up to the target,
	// sample-exact.
	pos := landed
	for pos < target {
		if m.carryLen() == 0 {
			err := m.fill(nil)
			if err == io.EOF {
				break // target past end: land at the stream's end
			}
			if err != nil {
				return 0, err
			}
		}
		drop := int(min(int64(m.carryLen()), target-pos))
		m.carryOff += drop
		pos += int64(drop)
	}
	m.pos = pos
	m.discont = true
	return pos, nil
}

// carryLen is the number of undelivered frames in the carry buffer.
func (m *media) carryLen() int {
	if m.carry == nil {
		return 0
	}
	return m.carry.N - m.carryOff
}

// copyOut moves frames from carry into dst.
func (m *media) copyOut(dst *audio.Buffer) {
	n := min(dst.Cap()-dst.N, m.carryLen())
	if n == 0 {
		return
	}
	audio.CopyFrames(dst, dst.N, m.carry, m.carryOff, n)
	dst.N += n
	m.carryOff += n
}

// fill decodes the next packet (or drains the decoder at end of stream)
// with dst as the primary emit target; overflow goes to the carry. A nil
// dst stages everything in the carry (the pre-roll path). Returns io.EOF
// once the stream is exhausted.
func (m *media) fill(dst *audio.Buffer) error {
	if m.eof {
		return io.EOF
	}
	m.sink = dst
	defer func() { m.sink = nil }()
	var pkt container.Packet
	for {
		err := m.demux.ReadPacket(&pkt)
		if err == io.EOF {
			m.eof = true
			return m.decoder.Drain(m.stashFn)
		}
		if err != nil {
			return err
		}
		if pkt.Track != m.track.ID {
			continue
		}
		return m.decoder.Decode(pkt.Data, m.stashFn)
	}
}

// stash receives borrowed decoder buffers (valid only during the call):
// as much as fits goes straight into the active sink, the rest is copied
// into the carry.
func (m *media) stash(b *audio.Buffer) error {
	if b.N == 0 {
		return nil
	}
	if b.Fmt != m.track.Fmt {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("format: decoder emitted %v for a %v track", b.Fmt, m.track.Fmt))
	}
	off := 0
	if m.sink != nil {
		take := min(m.sink.Cap()-m.sink.N, b.N)
		if take > 0 {
			audio.CopyFrames(m.sink, m.sink.N, b, 0, take)
			m.sink.N += take
			off = take
		}
	}
	rest := b.N - off
	if rest == 0 {
		return nil
	}
	m.compact()
	if m.carry == nil || m.carry.Cap()-m.carry.N < rest {
		grown := audio.Get(m.track.Fmt, max(m.carryLen()+rest, audio.StandardChunk))
		if old := m.carry; old != nil {
			grown.N = m.carryLen()
			audio.CopyFrames(grown, 0, old, m.carryOff, grown.N)
			audio.Put(old)
		}
		m.carry = grown
		m.carryOff = 0
	}
	audio.CopyFrames(m.carry, m.carry.N, b, off, rest)
	m.carry.N += rest
	return nil
}

// compact drops consumed frames so stash can append.
func (m *media) compact() {
	if m.carry == nil || m.carryOff == 0 {
		return
	}
	n := m.carryLen()
	audio.CopyFrames(m.carry, 0, m.carry, m.carryOff, n)
	m.carry.N = n
	m.carryOff = 0
}
