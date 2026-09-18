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
	warner  container.Warner // nil when the demuxer records no warnings
	walker  container.Walker // nil when the demuxer defers no walk
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
		delay: track.Delay, skip: track.Delay, rawEnd: rawEndFor(track)}
	m.stashFn = m.stash
	if s, ok := demux.(container.Seeker); ok {
		m.seeker = s
	}
	if w, ok := demux.(container.Warner); ok {
		m.warner = w
	}
	if w, ok := demux.(container.Walker); ok {
		m.walker = w
	}
	// Only demuxers that keep a persistable index yield a Media that
	// advertises container.Indexer, so a type assertion is an honest
	// capability gate: consumers skip sidecar work for formats that
	// would only ever answer nil.
	//
	// Two of them qualify per FILE rather than per format, and Go's method
	// sets cannot say so: a WAV or an AIFF-C carrying MP3 frames has a frame
	// index worth keeping and the same container carrying PCM has none, so
	// container/riff and container/aiff advertise the capability always and
	// answer nil and false for a byte-linear payload. A consumer that needs
	// to know whether the walk is expensive reads Track.SamplesExact beside
	// this (see the daemon's timelineNeedsJob), since a payload that states
	// its own length from its byte count is exactly the one with no index.
	if ix, ok := demux.(container.Indexer); ok {
		return &indexableMedia{media: m, ix: ix}, nil
	}
	return m, nil
}

// rawEndFor is where the raw decoder timeline is capped for a track, -1 for
// one that is not capped at all.
//
// The cap engages only when the container signaled trims, or when the length
// is authoritative (SamplesExact): a declared-total mismatch in an untrimmed
// advisory format (a lying FLAC STREAMINFO, say) stays a tolerated oddity,
// not a truncation.
//
// SamplesAdvisory vetoes it outright, and that is not redundant with the
// clause above: a Matroska track whose Opus CodecDelay sets Delay but whose
// length fell back to the millisecond Info Duration satisfies Delay > 0 while
// carrying a rounded total, and capping the decode there would clip the tail
// by whatever the rounding was worth.
func rawEndFor(track container.Track) int64 {
	if !track.SamplesAdvisory && (track.SamplesExact || track.Delay > 0 || track.Padding > 0) && track.Samples >= 0 {
		return track.Delay + track.Samples
	}
	return -1
}

// indexableMedia adds the demuxer's container.Indexer to the Media.
type indexableMedia struct {
	*media
	ix container.Indexer
}

func (m *indexableMedia) IndexSnapshot() []byte         { return m.ix.IndexSnapshot() }
func (m *indexableMedia) RestoreIndex(blob []byte) bool { return m.ix.RestoreIndex(blob) }

// Walk implements Walker: the demuxer's deferred walk, or nothing, and then
// the track this Media reports and decodes against is refreshed from it.
//
// A finished walk has measured the payload, and the measurement has to be
// observable where format.Walker says it is: without the refresh the media
// that walked keeps the open-time length and the caller has to reopen to see
// its own walk. Only the length fields move (Fmt and Delay are fixed at
// open), and the settled count is what delivery was already producing from
// these bytes, so a Walk taken mid-read cannot shift the position or the
// end: the cap only stops promising samples the packets never held.
func (m *media) Walk() error {
	if m.walker == nil {
		return nil
	}
	if err := m.walker.Walk(); err != nil {
		return err
	}
	for _, t := range m.demux.Tracks() {
		if t.ID != m.track.ID {
			continue
		}
		// The length fields and nothing else, which is a structural
		// restatement of what a walk can settle rather than a comment about
		// it: Delay is already half spent (m.skip counts down against it) and
		// Fmt is what the decoder was built for, so adopting either mid-read
		// would move the delivered timeline under the caller.
		m.track.Samples = t.Samples
		m.track.Padding = t.Padding
		m.track.SamplesExact = t.SamplesExact
		m.track.SamplesAdvisory = t.SamplesAdvisory
		m.rawEnd = rawEndFor(m.track)
		for i := range m.info.Tracks {
			if m.info.Tracks[i].ID == t.ID {
				m.info.Tracks[i] = m.track
			}
		}
		break
	}
	return nil
}

// Walked implements Walker: the demuxer's own answer, and true when it defers
// no walk, which is the honest answer for a length confirmed at open.
func (m *media) Walked() bool {
	return m.walker == nil || m.walker.Walked()
}

// Info returns the same *Info every time, with Warnings and Notes refolded
// from the demuxer on each call: a lazy walk finds damage where the read
// reaches it, so the lists are current as of the last read or seek (see
// Media). No change counter guards the refold. Info is asked a handful of
// times per open and never per chunk, and a length check would have a
// blind spot: flacn truncates and refills its list during its length walk
// and can land on the same length with different content.
func (m *media) Info() *Info {
	if m.warner != nil {
		foldWarnings(m.info, m.warner.Warnings())
	}
	return m.info
}

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
	// A demuxer that owns resources beyond the Source (the HLS client backs its
	// concatenated media with a temp file it must delete) is closed here, so
	// closing the Media releases the whole chain. Most demuxers hold nothing and
	// do not implement io.Closer.
	if c, ok := m.demux.(io.Closer); ok {
		return c.Close()
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
	// A decoder whose output depends on where the decode began is told its
	// landing here. Only codec/wmavoice implements it, and there it is what
	// makes a resumed decode converge to the linear one at all.
	if p, ok := m.decoder.(codec.Positioner); ok {
		p.SetPosition(landed)
	}
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
		if pkt.Discont {
			// The container dropped something between this packet and the one
			// before it. Nothing a decoder carries across packets survives a
			// hole, so it restarts here as it does after a seek, and one whose
			// output depends on where it began is told the landing.
			m.decoder.Reset()
			if p, ok := m.decoder.(codec.Positioner); ok {
				p.SetPosition(pkt.PTS)
			}
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
		// Geometric, not exact-fit: a decoder that hands a long unit out in
		// ordinary chunks (APE emits a frame 4096 frames at a time, and a
		// frame runs to a quarter of a minute) would otherwise regrow and
		// recopy the whole carry once per chunk.
		want := max(m.carryLen()+rest, audio.StandardChunk)
		if m.carry != nil {
			want = max(want, 2*m.carry.Cap())
		}
		grown := audio.Get(m.track.Fmt, want)
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
