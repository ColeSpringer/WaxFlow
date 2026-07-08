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

	// Gapless trims (Track.Delay/Padding): the delivered timeline is the
	// trimmed one, so sample 0 is the first real sample and Track.Samples
	// is the length. delay is the raw samples to cut off the front (skip
	// counts down what is still owed); rawEnd caps the raw decoder
	// timeline where the padding starts, -1 when the length is unknown.
	delay  int64
	skip   int64
	rawEnd int64
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
	m := &media{info: info, demux: demux, track: track, decoder: dec,
		delay: track.Delay, skip: track.Delay, rawEnd: -1}
	// The raw-end cap engages only when the container signaled trims, or when
	// the length is authoritative (SamplesExact): a declared-total mismatch in
	// an untrimmed advisory format (a lying FLAC STREAMINFO, say) stays a
	// tolerated oddity, not a truncation.
	if (track.SamplesExact || track.Delay > 0 || track.Padding > 0) && track.Samples >= 0 {
		m.rawEnd = track.Delay + track.Samples
	}
	m.stashFn = m.stash
	if s, ok := demux.(container.Seeker); ok {
		m.seeker = s
	}
	// Only demuxers that keep a persistable index yield a Media that
	// advertises container.Indexer, so a type assertion is an honest
	// capability gate: consumers skip sidecar work for formats that
	// would only ever answer nil.
	if ix, ok := demux.(container.Indexer); ok {
		return &indexableMedia{media: m, ix: ix}, nil
	}
	return m, nil
}

// indexableMedia adds the demuxer's container.Indexer to the Media.
type indexableMedia struct {
	*media
	ix container.Indexer
}

func (m *indexableMedia) IndexSnapshot() []byte         { return m.ix.IndexSnapshot() }
func (m *indexableMedia) RestoreIndex(blob []byte) bool { return m.ix.RestoreIndex(blob) }

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
	// The front trim still owed (Track.Delay before the first read) is
	// decoded and discarded exactly like seek pre-roll.
	if m.skip > 0 && !m.eof {
		dropped, err := m.discard(m.skip)
		if err != nil {
			return err
		}
		m.skip -= dropped
	}
	dst.N = 0
	m.copyOut(dst)
	for dst.N < dst.Cap() && !m.eof {
		if err := m.fill(dst); err != nil && err != io.EOF {
			return err
		}
	}
	// The back trim (Track.Padding): frames past the raw end are the
	// encoder's flush, not audio.
	if m.rawEnd >= 0 {
		if allowed := m.rawEnd - m.delay - m.pos; int64(dst.N) >= allowed {
			dst.N = int(max(allowed, 0))
			m.eof = true
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
	// The demuxer speaks the raw decoder timeline; the trims map the
	// delivered (trimmed) target onto it.
	rawTarget := target + m.delay
	if m.rawEnd >= 0 {
		rawTarget = min(rawTarget, m.rawEnd)
	}
	landed, err := m.seeker.SeekSample(m.track.ID, rawTarget)
	if err != nil {
		return 0, err
	}
	m.decoder.Reset()
	m.carryOff = 0
	if m.carry != nil {
		m.carry.N = 0
	}
	m.eof = false
	m.skip = 0 // the pre-roll below subsumes any front trim still owed

	// Pre-roll: decode into the carry and discard up to the target,
	// sample-exact. A short discard means the target was past the end:
	// the landing is the stream's end.
	pos := landed
	if rawTarget > landed {
		dropped, err := m.discard(rawTarget - landed)
		if err != nil {
			return 0, err
		}
		pos += dropped
	}
	m.pos = max(pos-m.delay, 0)
	m.discont = true
	return m.pos, nil
}

// discard decodes and drops up to n frames, returning how many were
// dropped: fewer than asked means end of stream (eof latches in fill).
// Both the initial front trim and seek pre-roll ride on this.
func (m *media) discard(n int64) (int64, error) {
	var dropped int64
	for dropped < n {
		if m.carryLen() == 0 {
			err := m.fill(nil)
			if err == io.EOF {
				break
			}
			if err != nil {
				return dropped, err
			}
		}
		drop := int(min(int64(m.carryLen()), n-dropped))
		m.carryOff += drop
		dropped += int64(drop)
	}
	return dropped, nil
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
