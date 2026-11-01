package asf

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
)

// frameCounter is a codec whose packets say how many frames they add to the
// stream, so a seek can count its way to a landing instead of trusting a
// stated time. WMA v1/v2 is the one here: its stamps sit off the frame grid a
// resumed decode lands on, by up to two frames on Windows' files.
type frameCounter interface {
	PacketFrames(pkt []byte) (frames int, ok bool)
	// CarryCheck returns the stream's check for a frame counted in prev that
	// the decode completes there, where pkt's header cannot say so: the frame
	// pkt is then counted one high. True only when prev counts frames. The
	// check keeps what it builds, so a count holds one.
	CarryCheck() func(prev, pkt []byte) (bool, error)
	StreamSamples(packets, frames int64) int64
	FrameLen() int
}

// maxObjectFrames is the most frames one object can count: the superframe
// header's frame count is four bits.
const maxObjectFrames = 15

// maxCountEntries caps the count's entries, one per packet an object begins
// in: past it the count keeps its totals without keeping entries, and a
// landing beyond the last entry steps there object by object. It clears days
// of any real WMA and bounds what a crafted file of tiny packets can make the
// index hold. A variable so a test can reach it.
var maxCountEntries = 1 << 20

// frameIndex counts frames from the start of the stream to each packet a
// media object begins in. One entry per such packet rather than per object
// keeps it a few bytes a packet; a seek steps through the landing packet's
// own objects, which are in hand once the packet is. A walk takes the count
// with its own reader, and the demuxer's read adds each object it delivers
// at the count's frontier, so a play from the start counts as it goes.
type frameIndex struct {
	entries []indexEntry
	frames  int64 // frames counted so far
	objects int64 // objects counted so far
	done    bool  // the count reached the end of the data
	// lost records that the count dropped or lost an object to damage. The
	// decoder restarts at such a hole, so the count is no longer what a
	// linear decode delivers.
	lost bool
	grew bool // entries or completion added since the start or a restore
	// capped records that the entries stopped at maxCountEntries while the
	// totals went on.
	capped bool
	err    error // a read failure, sticky: a second walk stops there too
	walk   *reader
	// obj is the object the walk reads into and prev the one before it,
	// which the carry check reads whole.
	obj, prev object
	havePrev  bool
	// carry is the codec's carry check, built on first use.
	carry func(prev, pkt []byte) (bool, error)
	// ahead is the carry check's answer for the object after the last one a
	// read counted, taken while both were in hand.
	ahead carryAnswer
	// pending is what Walk reports: the damage a walk found, since a seek
	// that walks reads ahead of every read the caller asked for, the folds a
	// read found, and a restored sidecar's findings, so a restore reports
	// what a walk would.
	pending []container.Warning
}

// indexEntry is the first object to begin in a packet and what precedes it.
type indexEntry struct {
	packet  int64
	frames  int64
	objects int64
}

// extendTo counts forward until an entry lies past frame n or the data ends.
// The walk reads through its own reader and window, so the demuxer's
// position does not move. A refusal is sticky but fails only a count that
// has to pass it: a landing the count already reaches still lands.
func (x *frameIndex) extendTo(d *Demuxer, n int64) error {
	for !x.done && (len(x.entries) == 0 || x.entries[len(x.entries)-1].frames <= n) {
		if x.err != nil {
			return x.err
		}
		x.err = x.step(d)
	}
	return nil
}

// step counts one more object.
func (x *frameIndex) step(d *Demuxer) error {
	if x.walk == nil {
		// A count with entries, however it took them, resumes by counting
		// again from its last entry's packet; a fresh one starts at the top.
		at := int64(0)
		x.frames, x.objects = 0, 0
		if n := len(x.entries); n > 0 {
			last := x.entries[n-1]
			at, x.frames, x.objects = last.packet, last.frames, last.objects
		}
		x.walk = d.walker(at, x.hold)
		x.havePrev = false
	}
	o := &x.obj
	ok, err := x.walk.fetch(o)
	if err != nil {
		return err
	}
	if !ok {
		x.end(d, x.walk.hole)
		return nil
	}
	var a carryAnswer
	if x.havePrev {
		a = x.uncarried(d, &x.prev, o)
	}
	x.count(d, o, a)
	x.prev, x.obj = x.obj, x.prev
	x.havePrev = true
	return nil
}

// carryAnswer is the carry check's answer for one object (see CarryCheck).
type carryAnswer struct {
	set bool
	ord int64 // the object it answers for, when a read took it ahead
	un  bool
	err error
}

// uncarried asks the codec whether b, the object after a, enters with
// nothing carried. A hole between them breaks the pair, and nothing is asked.
func (x *frameIndex) uncarried(d *Demuxer, a, b *object) carryAnswer {
	if b.hole {
		return carryAnswer{set: true}
	}
	if x.carry == nil {
		x.carry = d.counter.CarryCheck()
	}
	un, err := x.carry(a.data, b.data)
	return carryAnswer{set: true, un: un, err: err}
}

// count adds o, the object after the last one counted, to the count, a being
// the carry check's answer for it. A refusal is the decode stopping at the
// object before, short of this count.
func (x *frameIndex) count(d *Demuxer, o *object, a carryAnswer) {
	f, fok := d.counter.PacketFrames(o.data)
	x.lost = x.lost || !fok || o.hole || a.err != nil
	if a.un && a.err == nil {
		x.frames--
	}
	if n := len(x.entries); n == 0 || o.head > x.entries[n-1].packet {
		if n < maxCountEntries {
			x.entries = append(x.entries, indexEntry{packet: o.head, frames: x.frames, objects: x.objects})
			x.grew = true
		} else {
			x.capped = true
		}
	}
	x.frames += int64(f)
	x.objects++
}

// end completes the count at the end of the data. hole is damage met after
// the last object counted.
func (x *frameIndex) end(d *Demuxer, hole bool) {
	x.lost = x.lost || hole
	x.done, x.grew = true, true
	x.walk = nil
	d.settle()
}

// read counts o, which the demuxer's read delivers as object ord, when the
// count stands exactly there. A walk at the same place is dropped, since the
// read now carries the count on; one needed later resumes at the last entry.
func (x *frameIndex) read(d *Demuxer, o *object, ord int64) {
	if x.done || x.err != nil || ord != x.objects {
		return
	}
	x.walk = nil
	var a carryAnswer
	switch {
	case x.havePrev:
		a = x.uncarried(d, &x.prev, o)
	case x.ahead.set && x.ahead.ord == ord:
		a = x.ahead
	}
	x.count(d, o, a)
	// The next object's answer is taken now, from the lookahead, rather than
	// this object kept for it: playback copies nothing into the count.
	x.havePrev, x.ahead = false, carryAnswer{}
	if d.haveNxt {
		x.ahead = x.uncarried(d, o, &d.nxt)
		x.ahead.ord = ord + 1
	}
}

// readEnd completes the count when the demuxer's read runs out of data with
// every object it delivered counted.
func (x *frameIndex) readEnd(d *Demuxer, ord int64) {
	if !x.done && x.err == nil && ord == x.objects {
		x.end(d, d.rd.hole)
	}
}

// hold keeps a finding of the walk for Walk to report.
func (x *frameIndex) hold(off int64, msg string) error {
	w := container.Warning{Offset: off, Msg: msg, Kind: container.Damage}
	if len(x.pending) < maxWarnings && !slices.Contains(x.pending, w) {
		x.pending = append(x.pending, w)
	}
	return nil
}

// entryAtOrBefore is the last entry whose objects begin at or before frame n,
// or -1 when there is none.
func (x *frameIndex) entryAtOrBefore(n int64) int {
	return sort.Search(len(x.entries), func(i int) bool { return x.entries[i].frames > n }) - 1
}

// walker is a reader at packet at over a window of its own, whose damage
// goes to sink.
func (d *Demuxer) walker(at int64, sink func(int64, string) error) *reader {
	w := srcwin.New(d.src, d.w.DataEnd(), "wma: reading packet data")
	r := &reader{d: d, w: &w, sink: sink}
	r.reset(at)
	return r
}

// seekCounted lands a counted track one media object before the object that
// holds the target, at the position the frame counts give it. A block can
// reuse a channel's exponents from the last block that coded the channel,
// which may lie before the object the target is in, and a decode resumed
// there has no curve to reuse: measured on Windows' stereo encodes, about
// one landing in two hundred, every one of which a landing an object earlier
// decodes exactly. The caller's pre-roll discards the run up to the target,
// and lands earlier still if the decode reports it needs more.
func (d *Demuxer) seekCounted(sample int64) (int64, error) {
	fl := int64(d.counter.FrameLen())
	at, err := d.landCounted(sample / fl)
	if err == nil && at > 0 {
		at, err = d.landCounted(at - 1)
	}
	return at * fl, err
}

// landCounted puts the reader on the last media object whose frames before it
// are at most n, and returns that count.
func (d *Demuxer) landCounted(n int64) (int64, error) {
	if err := d.idx.extendTo(d, n); err != nil {
		return 0, err
	}
	i := d.idx.entryAtOrBefore(n)
	at := int64(0)
	if i >= 0 {
		at = d.idx.entries[i].packet
	}
	d.rewind(at)
	if err := d.ensure(); err != nil {
		return 0, err
	}
	if i < 0 {
		d.ord = 0
		return 0, nil
	}
	e := d.idx.entries[i]
	if !d.haveCur || d.cur.head != e.packet {
		// The count names a packet no object begins in, which only a sidecar
		// that passed its checks can do. The start is a landing that is
		// always right, and the next seek counts afresh.
		d.idx = frameIndex{}
		d.rewind(0)
		if err := d.ensure(); err != nil {
			return 0, err
		}
		d.ord = 0
		return 0, nil
	}
	pos, ord := e.frames, e.objects
	for d.haveNxt {
		next := pos + d.advance(&d.cur, &d.nxt)
		if next > n {
			break
		}
		pos = next
		ord++
		d.emitted = true
		if err := d.ensure(); err != nil {
			return 0, err
		}
	}
	d.ord = ord
	return pos, nil
}

// advance is how far a decode moves from the start of object a to that of b,
// the one after it: a's count, less a frame b's header cannot say a
// completed. A refusal takes nothing back; the count that walked here lost
// the object.
func (d *Demuxer) advance(a, b *object) int64 {
	f, _ := d.counter.PacketFrames(a.data)
	if c := d.idx.uncarried(d, a, b); c.un && c.err == nil {
		f--
	}
	return int64(f)
}

// Walk implements container.Walker. A WMA v1/v2 track counts its frames to the
// end of the data, and an undamaged count is its length: what a linear decode
// delivers, exact, from the headers alone. Damage on the way, whether this
// walk or a seek's met it, is reported here through the warn path, so under
// Strict the walk refuses; a count that lost an object settles nothing,
// since the decoder restarts at the hole. Other codecs defer nothing.
func (d *Demuxer) Walk() error {
	if d.counter == nil {
		return nil
	}
	if err := d.idx.extendTo(d, math.MaxInt64); err != nil {
		return err
	}
	for _, w := range d.idx.pending {
		if err := d.warn(w.Offset, "%s", w.Msg); err != nil {
			return err
		}
	}
	return nil
}

// settle puts a complete, undamaged count on the track, whatever completed it:
// a walk, a seek that counted to the end, or a restored sidecar. Walked is
// true from then on, and the track says what the walk found.
func (d *Demuxer) settle() {
	if x := &d.idx; x.done && !x.lost {
		d.track.Samples = d.counter.StreamSamples(x.objects, x.frames)
		d.track.SamplesExact, d.track.SamplesAdvisory = true, false
	}
}

// Walked implements container.Walker: whether the count has reached the end
// of the data, by a walk, a seek or a restored complete sidecar, and was not
// refused on the way. A track that counts nothing has nothing to walk.
func (d *Demuxer) Walked() bool {
	return d.counter == nil || (d.idx.done && !(d.opts.Strict && len(d.idx.pending) > 0))
}

// Index snapshot format, versioned on its own: magic and version, then as
// uvarints the frame length, packet count and packet length the count was
// taken against, a completeness byte, the entry count, each entry as deltas
// from the one before (packet, frames, objects; the first absolute), for a
// complete count the totals as deltas from the last entry, the findings Walk
// reports (a count, then each as its offset plus one and its message, length
// first), and a CRC-32 of everything before it. The probes cannot check a
// step they do not sample, and a restored count is the exact length a walk
// settles, so a corrupted step would otherwise cap the decode wherever it
// says.
const (
	idxMagic   = "WXASFIDX"
	idxVersion = 3
	// maxIndexMsg bounds a restored finding's message, past any this package
	// writes.
	maxIndexMsg = 256
	// idxMinEntries is the snapshot threshold. A restore reads up to
	// idxProbes+1 packets, so a count much shorter than this costs a restore
	// about what it costs to take again.
	idxMinEntries = 16
	// idxProbes is how many restored entries past the first are checked
	// against the packet they name.
	idxProbes = 8
	// maxIndexTotal is past any real stream's frame or object count, and
	// bounds what a restored blob can make the arithmetic hold.
	maxIndexTotal = 1 << 48
)

// KeepsIndex implements container.Indexer: a WMA v1/v2 track keeps its frame
// count, and Pro, Voice and Lossless land on stated times and keep nothing.
func (d *Demuxer) KeepsIndex() bool { return d.counter != nil }

// IndexSnapshot implements container.Indexer: the count so far, or nil when
// there is none, it is too short to keep, it has not grown since a restore,
// it lost an object to damage, which settles nothing, or it passed its cap,
// which no real file does. What the count found without losing an object,
// stamps that run backwards, rides in the blob, so a restore reports it as a
// walk does.
func (d *Demuxer) IndexSnapshot() []byte {
	x := &d.idx
	if d.counter == nil || x.lost || x.capped || !x.grew || len(x.entries) < idxMinEntries {
		return nil
	}
	buf := make([]byte, 0, len(idxMagic)+16+len(x.entries)*4)
	buf = append(buf, idxMagic...)
	buf = append(buf, idxVersion)
	buf = binary.AppendUvarint(buf, uint64(d.counter.FrameLen()))
	buf = binary.AppendUvarint(buf, uint64(d.packets))
	buf = binary.AppendUvarint(buf, uint64(d.packetLen))
	if x.done {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = binary.AppendUvarint(buf, uint64(len(x.entries)))
	var prev indexEntry
	for _, e := range x.entries {
		buf = binary.AppendUvarint(buf, uint64(e.packet-prev.packet))
		buf = binary.AppendUvarint(buf, uint64(e.frames-prev.frames))
		buf = binary.AppendUvarint(buf, uint64(e.objects-prev.objects))
		prev = e
	}
	if x.done {
		buf = binary.AppendUvarint(buf, uint64(x.frames-prev.frames))
		buf = binary.AppendUvarint(buf, uint64(x.objects-prev.objects))
	}
	buf = binary.AppendUvarint(buf, uint64(len(x.pending)))
	for _, w := range x.pending {
		buf = binary.AppendUvarint(buf, uint64(w.Offset+1))
		buf = binary.AppendUvarint(buf, uint64(len(w.Msg)))
		buf = append(buf, w.Msg...)
	}
	return binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
}

// RestoreIndex implements container.Indexer. The blob is checked, not
// trusted: it must pass its CRC, have been taken against this frame length
// and packet geometry, hold only steps a real stream can take, and in a
// spread of entries (the endpoints included) name packets whose own objects
// count the step to the entry after, less at most a frame per object (see
// Uncarried). Anything else is refused and the count is taken from the data
// instead. The CRC catches damage and the probes a count of another file; a
// blob built to pass both can still misstate a step the probes skip, and is
// trusted there as the cache directory it came from is.
func (d *Demuxer) RestoreIndex(blob []byte) bool {
	x := &d.idx
	if d.counter == nil || len(x.entries) > 0 || x.walk != nil || x.done || x.err != nil {
		return false
	}
	snap, ok := d.decodeIndex(blob)
	if !ok {
		return false
	}
	entries, frames, objects, done := snap.entries, snap.frames, snap.objects, snap.done
	buf := make([]byte, d.packetLen)
	for k, last := 0, -1; k <= idxProbes; k++ {
		i := k * (len(entries) - 1) / idxProbes
		if i == last {
			continue
		}
		last = i
		n, f, ok := d.headsIn(entries[i].packet, buf)
		if !ok {
			return false
		}
		// The objects between two entries all begin in the first one's
		// packet, so that packet alone says what the step is, but for the
		// frame each object's successor can take back (see Uncarried), which
		// the last object of the stream has none to do.
		next, slack := indexEntry{frames: frames, objects: objects}, n-1
		if i+1 < len(entries) {
			next, slack = entries[i+1], n
		} else if !done {
			continue
		}
		step := next.frames - entries[i].frames
		if n != next.objects-entries[i].objects || step > f || step < f-slack {
			return false
		}
	}
	// A partial count stands at its last entry, where a walk resumes and
	// where a read from the start joins it.
	last := entries[len(entries)-1]
	x.entries, x.done, x.grew = entries, done, false
	x.frames, x.objects = last.frames, last.objects
	x.pending = snap.found
	if done {
		x.frames, x.objects = frames, objects
		d.settle()
	}
	return true
}

// snapshot is a decoded index blob.
type snapshot struct {
	entries         []indexEntry
	frames, objects int64
	done            bool
	found           []container.Warning
}

// decodeIndex parses a snapshot taken against this file's geometry.
func (d *Demuxer) decodeIndex(blob []byte) (snapshot, bool) {
	var none snapshot
	if len(blob) < len(idxMagic)+1+4 || string(blob[:len(idxMagic)]) != idxMagic || blob[len(idxMagic)] != idxVersion {
		return none, false
	}
	body := blob[:len(blob)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(blob[len(body):]) {
		return none, false
	}
	rest := body[len(idxMagic)+1:]
	next := func() (uint64, bool) {
		v, n := binary.Uvarint(rest)
		if n <= 0 {
			return 0, false
		}
		rest = rest[n:]
		return v, true
	}
	fl, ok1 := next()
	packets, ok2 := next()
	packetLen, ok3 := next()
	if !ok1 || !ok2 || !ok3 || fl != uint64(d.counter.FrameLen()) ||
		packets != uint64(d.packets) || packetLen != uint64(d.packetLen) || len(rest) == 0 || rest[0] > 1 {
		return none, false
	}
	done := rest[0] == 1
	rest = rest[1:]
	count, okc := next()
	if !okc || count == 0 || count > packets || count > uint64(maxCountEntries) {
		return none, false
	}
	// add reads one entry's frame and object deltas and checks them against
	// what the packets they span can hold: each packet begins at most
	// maxPayloads objects and each object counts at most maxObjectFrames
	// frames. maxIndexTotal bounds the arithmetic.
	add := func(e indexEntry, span uint64, first bool) (indexEntry, bool) {
		df, ok1 := next()
		do, ok2 := next()
		if !ok1 || !ok2 || df > maxIndexTotal || do > maxIndexTotal || (!first && do == 0) ||
			df > do*maxObjectFrames || (span < maxIndexTotal && do > span*maxPayloads) {
			return e, false
		}
		e.frames, e.objects = e.frames+int64(df), e.objects+int64(do)
		return e, e.frames <= maxIndexTotal && e.objects <= maxIndexTotal
	}
	entries := make([]indexEntry, 0, min(count, 4096))
	var e indexEntry
	for i := uint64(0); i < count; i++ {
		dp, okp := next()
		if !okp || dp >= packets-uint64(e.packet) || (i > 0 && dp == 0) {
			return none, false
		}
		ne, oka := add(e, dp, i == 0)
		if !oka || (i == 0 && (ne.frames != 0 || ne.objects != 0)) {
			return none, false
		}
		ne.packet = e.packet + int64(dp)
		e = ne
		entries = append(entries, e)
	}
	frames, objects := e.frames, e.objects
	if done {
		end, oka := add(e, packets-uint64(e.packet), false)
		if !oka {
			return none, false
		}
		frames, objects = end.frames, end.objects
	}
	found, okw := d.decodeFindings(next, &rest)
	if !okw || len(rest) != 0 {
		return none, false
	}
	return snapshot{entries: entries, frames: frames, objects: objects, done: done, found: found}, true
}

// decodeFindings reads a snapshot's findings, each held to a message this
// package could have written about a byte of this file.
func (d *Demuxer) decodeFindings(next func() (uint64, bool), rest *[]byte) ([]container.Warning, bool) {
	n, ok := next()
	if !ok || n > maxWarnings {
		return nil, false
	}
	var found []container.Warning
	for range n {
		off, ok1 := next()
		size, ok2 := next()
		if !ok1 || !ok2 || off > uint64(d.size) || size > maxIndexMsg || size > uint64(len(*rest)) {
			return nil, false
		}
		msg := string((*rest)[:size])
		*rest = (*rest)[size:]
		if !utf8.ValidString(msg) {
			return nil, false
		}
		found = append(found, container.Warning{Offset: int64(off) - 1, Msg: msg, Kind: container.Damage})
	}
	return found, true
}

// refuseProbe refuses a probe at the first damage it meets: what it finds is
// the walk's to report, where the walk reaches it.
func refuseProbe(int64, string) error { return errProbe }

// errProbe is a probe's refusal.
var errProbe = malformed("damage under a restore probe")

// headsIn counts the objects of the selected stream that begin in data packet
// i and the frames they count, reading the packet into buf off the streaming
// window. ok is false when none begins there or the packet will not read.
func (d *Demuxer) headsIn(i int64, buf []byte) (objects, frames int64, ok bool) {
	if i < 0 || i >= d.packets || container.ReadFull(d.src, buf, d.packetOff(i)) != nil {
		return 0, 0, false
	}
	r := reader{d: d, sink: refuseProbe}
	if r.parsePacket(buf, i) != nil || r.hole {
		return 0, 0, false
	}
	for _, p := range r.pay {
		if p.stream != d.sel.number || p.objOff != 0 {
			continue
		}
		f, fok := d.counter.PacketFrames(p.data)
		if p.objSize == 0 || p.objSize > maxObjectBytes || !fok {
			return 0, 0, false
		}
		objects++
		frames += int64(f)
	}
	return objects, frames, objects > 0
}
