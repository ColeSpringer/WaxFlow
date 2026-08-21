package asf

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
	_ container.Tagger  = (*Demuxer)(nil)
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// stream is one Stream Properties Object reduced to what selection needs.
type stream struct {
	number    int
	typ       guid
	wfx       waveFormat
	haveWFX   bool
	encrypted bool
}

// object is one reassembled media object: a codec packet and the output
// position its presentation time names.
type object struct {
	data []byte
	pts  int64
}

// set copies b into the object's own buffer. The copy is what lets the
// demuxer hold an object across the read that finds its successor, since the
// read window rebases underneath it.
func (o *object) set(b []byte, pts int64) {
	o.data = append(o.data[:0], b...)
	o.pts = pts
}

// Demuxer reads one audio stream from an ASF source, emitting one packet per
// media object.
type Demuxer struct {
	opts DemuxerOptions
	// src backs the one-shot header and index reads; w is the streaming
	// window the packet walk runs through. Keeping them apart is what stops a
	// 16 MB header read from becoming the window every later packet read
	// rebases out of.
	src  container.Source
	w    srcwin.Window
	size int64

	track    container.Track
	tags     map[string][]string
	warnings []container.Warning

	sel             stream
	streams         []stream
	prerollMS       int64
	playHNS         uint64
	declaredPackets uint64
	encrypted       bool

	// Data Object geometry. Packets are fixed size, so packet i begins at
	// dataOff + i*packetLen and seeking is arithmetic on that.
	dataOff   int64
	packetLen int64
	packets   int64

	// index holds the Simple Index Object's packet numbers, one per
	// indexInterval of presentation time; empty when the file has none, and
	// seeking then bisects on the packets' own send times instead.
	index         []uint32
	indexInterval int64 // 100-nanosecond units

	// objectPackets is how many data packets one media object spans, and
	// seekPreroll how many a seek backs up past its landing so the decoder
	// gets a whole media object of run-up.
	objectPackets int64
	seekPreroll   int64

	// Reading state: the packet cursor and the current packet's payload list.
	cursor int64
	pay    []payload
	payIdx int
	// probe is the scratch a seek reads packet prologues into, off the
	// streaming window so bisection does not rebase it.
	probe [packetOverhead]byte

	// Fragment reassembly, for a media object split across packets.
	asm     []byte
	asmNum  uint32
	asmSize uint32
	asmMS   int64
	asmHave int
	asmOpen bool
	// resumeUntil is the packet up to which a just-repositioned reader steps
	// over fragments instead of reporting them. A seek lands on a packet
	// boundary, which can sit inside a media object, so the tail of that one
	// object is not damage. It expires: the object it excuses spans at most
	// objectPackets packets, and a flag that never expired would swallow
	// every later fragment error in the file, strict mode included, so
	// damage a linear read refuses would be accepted after a seek.
	resumeUntil int64

	// cur is the object ReadPacket delivers and nxt the one-object lookahead
	// its duration comes from: ASF states presentation times, not durations,
	// so an object's length is the distance to its successor.
	cur, nxt         object
	haveCur, haveNxt bool
	filled, emitted  bool
	lastDur          int64
	lastPTS          int64
	// err is sticky. Without it a strict-mode refusal mid-walk would be
	// followed by the packet whose lookahead raised it, so a caller that
	// logged the error and read on would get data after a failure.
	err error
}

// NewDemuxer parses an ASF header and positions on the first data packet. The
// returned Demuxer implements container.Seeker, container.Warner, and
// container.Tagger.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, size: src.Size()}
	if opts != nil {
		d.opts = *opts
	}
	d.w = srcwin.New(src, d.size, "wma: reading packet data")
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	d.record(container.Warning{Offset: off, Msg: msg})
	return nil
}

// note records a Warning that Strict must not escalate: a limitation of this
// build against a file that is well formed, rather than damage in the file.
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.record(container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...)})
}

func (d *Demuxer) record(w container.Warning) {
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
}

// Tracks returns the selected audio track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns the tolerated oddities seen so far.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// Tags returns the canonical tags parsed from the header's two tag objects.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// ReadPacket delivers the next media object. Data is borrowed until the next
// call, the container.Demuxer contract's own bound: the following call shifts
// the lookahead down and reads into the buffer handed out here.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if d.err != nil {
		return d.err
	}
	if err := d.ensure(); err != nil {
		d.err = err
		return err
	}
	if !d.haveCur {
		return io.EOF
	}
	dur := d.tailDur(d.cur.pts)
	if d.haveNxt {
		dur = max(1, d.nxt.pts-d.cur.pts)
	}
	d.lastDur = dur
	d.emitted = true
	*pkt = container.Packet{
		Track: 0,
		// Every media object is a sync point: ASF's key-frame bit is not set
		// by every writer for audio (ffmpeg leaves it clear), and a WMA
		// super-frame is where a decoder can restart. What the bit reservoir
		// carries across super-frames is what the seek pre-roll covers.
		Packet: codec.Packet{Data: d.cur.data, PTS: d.cur.pts, Dur: dur, Sync: true},
	}
	return nil
}

// ensure makes cur the next undelivered object and refills the lookahead. The
// shift happens here, at the start of the call after the one that handed a
// buffer out, so a consumer's borrowed data survives until then.
func (d *Demuxer) ensure() error {
	switch {
	case d.emitted:
		d.cur, d.nxt = d.nxt, d.cur
		d.haveCur, d.haveNxt = d.haveNxt, false
		d.emitted = false
		if !d.haveCur {
			return nil
		}
		ok, err := d.fetch(&d.nxt)
		if err != nil {
			return err
		}
		d.haveNxt = ok
	case !d.filled:
		d.filled = true
		ok, err := d.fetch(&d.cur)
		if err != nil {
			return err
		}
		if d.haveCur = ok; !ok {
			return nil
		}
		if ok, err = d.fetch(&d.nxt); err != nil {
			return err
		}
		d.haveNxt = ok
	}
	return nil
}

// tailDur is the duration of an object with no successor to measure against.
// A super-frame is constant-length, so the predecessor's duration is the best
// answer there is; the container's own remaining-length arithmetic is the
// fallback for a stream one object long, and it is coarser because the total
// it works from is a rounded play duration.
func (d *Demuxer) tailDur(pts int64) int64 {
	if d.lastDur > 0 {
		return d.lastDur
	}
	if d.track.Samples > pts {
		return min(d.track.Samples-pts, maxObjectSamples)
	}
	return 1
}

// fetch advances the packet cursor until one whole media object is assembled
// into dst, reporting false at the end of the data.
func (d *Demuxer) fetch(dst *object) (bool, error) {
	for {
		if d.payIdx < len(d.pay) {
			p := d.pay[d.payIdx]
			d.payIdx++
			done, err := d.absorb(p, dst)
			if err != nil {
				return false, err
			}
			if !done {
				continue
			}
			if dst.pts < d.lastPTS {
				// No latch on this one: the warning list already drops
				// duplicates and its own cap bounds a file that folds
				// repeatedly, while a latch would have to be cleared on every
				// reposition, and one that was not turned a second walk of a
				// file strict had already refused into a clean read.
				if err := d.warn(d.packetOff(d.cursor-1), "presentation times run backwards"); err != nil {
					return false, err
				}
				dst.pts = d.lastPTS
			}
			d.lastPTS = dst.pts
			return true, nil
		}
		if d.cursor >= d.packets {
			// An object still under assembly here is a file that stops
			// mid-object, which is the same damage a broken fragment run is
			// mid-stream and has to read the same way.
			if err := d.dropOpenAssembly(d.packetOff(d.cursor - 1)); err != nil {
				return false, err
			}
			return false, d.w.Err()
		}
		i := d.cursor
		d.cursor++
		if err := d.loadPacket(i); err != nil {
			return false, err
		}
	}
}

// absorb feeds one payload into the object under assembly, reporting true when
// that completes an object in dst.
func (d *Demuxer) absorb(p payload, dst *object) (bool, error) {
	if p.stream != d.sel.number {
		return false, nil
	}
	off := d.packetOff(d.cursor - 1)
	if p.objSize == 0 || p.objSize > maxObjectBytes {
		d.dropAssembly()
		return false, d.warn(off, "media object of %d bytes", p.objSize)
	}
	if p.objOff == 0 {
		d.resumeUntil = 0
	}
	switch {
	case p.objOff == 0 && int(p.objSize) == len(p.data):
		// The whole object rides in one payload, which is the common case.
		if err := d.dropOpenAssembly(off); err != nil {
			return false, err
		}
		dst.set(p.data, d.samplePos(p.presMS))
		return true, nil
	case p.objOff == 0:
		if err := d.dropOpenAssembly(off); err != nil {
			return false, err
		}
		d.asm = append(d.asm[:0], p.data...)
		d.asmNum, d.asmSize, d.asmMS = p.objNum, p.objSize, p.presMS
		d.asmHave = len(p.data)
		d.asmOpen = true
	default:
		if !d.asmOpen || p.objNum != d.asmNum || p.objSize != d.asmSize || int64(p.objOff) != int64(d.asmHave) {
			d.dropAssembly()
			if d.cursor <= d.resumeUntil {
				// The tail of an object this reader never saw the head of.
				return false, nil
			}
			return false, d.warn(off, "media object %d fragment at offset %d does not continue the one in hand", p.objNum, p.objOff)
		}
		d.asm = append(d.asm, p.data...)
		d.asmHave += len(p.data)
	}
	if d.asmHave > int(d.asmSize) {
		d.dropAssembly()
		return false, d.warn(off, "media object %d overruns its declared %d bytes", p.objNum, p.objSize)
	}
	if d.asmHave < int(d.asmSize) {
		return false, nil
	}
	dst.set(d.asm, d.samplePos(d.asmMS))
	d.dropAssembly()
	return true, nil
}

// dropOpenAssembly abandons a half-built object, which is damage worth a
// warning; dropAssembly is the silent reset the completion path uses.
func (d *Demuxer) dropOpenAssembly(off int64) error {
	if !d.asmOpen {
		return nil
	}
	missing := int(d.asmSize) - d.asmHave
	d.dropAssembly()
	return d.warn(off, "media object %d is missing %d bytes and was dropped", d.asmNum, missing)
}

func (d *Demuxer) dropAssembly() {
	d.asmOpen = false
	d.asmHave = 0
	d.asm = d.asm[:0]
}

// samplePos converts an absolute presentation time to an output sample
// position: ASF measures presentation from the pre-roll, which is buffering
// lead-in rather than audio.
func (d *Demuxer) samplePos(presMS int64) int64 {
	return msToSamples(presMS-d.prerollMS, d.track.Fmt.Rate)
}

// packetOff is the byte offset of data packet i.
func (d *Demuxer) packetOff(i int64) int64 {
	if i < 0 {
		return d.dataOff
	}
	return d.dataOff + i*d.packetLen
}

// SeekSample repositions to a packet boundary at or before the target and
// returns the position reads resume at.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("wma: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "wma: negative seek target")
	}
	if d.packets == 0 {
		return 0, nil
	}
	// A target past the end lands on the last packet, where reads resume and
	// immediately run out, which is what every other demuxer here does.
	target := min(d.packetFor(samplesToMS(sample, d.track.Fmt.Rate)), d.packets-1)
	landing := max(0, target-d.seekPreroll)
	d.rewind(landing)
	if err := d.ensure(); err != nil {
		d.err = err
		return 0, err
	}
	if landing > 0 && (!d.haveCur || d.cur.pts > sample) {
		// The landing is checked against the position it actually produced,
		// and a failed check restarts at the start of the data. Both cases
		// are the same mistrust: a landing past the target breaks the Seeker
		// contract outright, and a landing with nothing behind it means the
		// fetch ran to the end of the data, so no packet will ever follow it
		// even though earlier ones may exist. One retry settles both, since
		// there is nowhere earlier than the start to go.
		d.rewind(0)
		if err := d.ensure(); err != nil {
			d.err = err
			return 0, err
		}
	}
	if d.haveCur {
		return d.cur.pts, nil
	}
	return 0, d.w.Err()
}

// rewind puts the reader at the start of packet i, discarding the lookahead
// and any half-built object.
func (d *Demuxer) rewind(i int64) {
	d.cursor = i
	d.pay = d.pay[:0]
	d.payIdx = 0
	d.dropAssembly()
	d.resumeUntil = 0
	if i > 0 {
		d.resumeUntil = i + d.objectPackets
	}
	d.haveCur, d.haveNxt = false, false
	d.filled, d.emitted = false, false
	d.lastPTS = 0
	d.err = nil
	// lastDur deliberately survives: it is the nominal length of a media
	// object, a property of the stream rather than of the reader's position,
	// and it is the only answer the tail object has. Clearing it left a
	// post-seek walk that ends in one object reporting a duration of 1.
}

// samplesToMS converts a sample position to milliseconds, rounded down so a
// seek target never moves forward past the packet that holds it. It saturates
// rather than wrapping: a caller's target is unbounded (the engine probes for
// the end of a stream with a near-maximum sample position), and at a low
// enough sample rate the product runs past int64.
func samplesToMS(n int64, rate int) int64 {
	if n <= 0 || rate <= 0 {
		return 0
	}
	sec := n / int64(rate)
	rem := n % int64(rate)
	if sec > (math.MaxInt64-1000)/1000 {
		return math.MaxInt64
	}
	return sec*1000 + rem*1000/int64(rate)
}

// parse reads the Header Object's children, locates the Data Object, selects
// the audio stream, and wires the track.
func (d *Demuxer) parse() error {
	head := d.w.BytesAt(0, headerObjectLen)
	if len(head) < headerObjectLen || guidAt(head) != guidHeader {
		if err := d.w.Err(); err != nil {
			return err
		}
		return malformed("not an ASF file")
	}
	size := le.Uint64(head[16:])
	if size < headerObjectLen || size > uint64(d.size) {
		return malformed("header object of %d bytes in a %d-byte file", size, d.size)
	}
	// Reserved1 must be 1 and Reserved2 must be 2. Neither gates anything
	// here, so a writer that got them wrong is noted rather than refused.
	if head[28] != 1 || head[29] != 2 {
		if err := d.warn(28, "header object reserved bytes are %d and %d, not 1 and 2", head[28], head[29]); err != nil {
			return err
		}
	}
	hdrEnd := int64(size)
	body, err := d.readBytes(headerObjectLen, hdrEnd-headerObjectLen, maxHeaderBytes)
	if err != nil {
		return err
	}
	if err := d.walkHeader(body); err != nil {
		return err
	}
	if err := d.selectStream(); err != nil {
		return err
	}
	if err := d.parseData(hdrEnd); err != nil {
		return err
	}
	return d.w.Err()
}

// readBytes reads n bytes at off into a fresh buffer, bounded by a cap so a
// crafted size cannot force a huge allocation.
func (d *Demuxer) readBytes(off, n, limit int64) ([]byte, error) {
	if n < 0 || n > limit {
		return nil, malformed("object of %d bytes exceeds the %d cap", n, limit)
	}
	buf := make([]byte, n)
	if err := container.ReadFull(d.src, buf, off); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "wma: reading the header", err)
	}
	return buf, nil
}

// walkHeader dispatches the Header Object's children. Every object this build
// does not recognize is a leaf and is skipped by its own declared size, the
// Header Extension included: nothing here descends on a depth the file
// chooses.
func (d *Demuxer) walkHeader(body []byte) error {
	base := int64(headerObjectLen)
	for len(body) >= objectHeaderLen {
		id := guidAt(body)
		size := le.Uint64(body[16:])
		if size < objectHeaderLen || size > uint64(len(body)) {
			return d.warn(base, "header object %s declares %d bytes with %d left", id, size, len(body))
		}
		payload := body[objectHeaderLen:size]
		// The parsers index into the body, so they are given the body's own
		// offset: handing them the object start put every warning they raise
		// 24 bytes short of the field it names.
		bodyOff := base + objectHeaderLen
		switch id {
		case guidFileProperties:
			if err := d.parseFileProperties(payload, bodyOff); err != nil {
				return err
			}
		case guidStreamProperties:
			if err := d.parseStreamProperties(payload, bodyOff); err != nil {
				return err
			}
		case guidContentDescription:
			d.parseContentDescription(payload)
		case guidExtendedContentDescription:
			d.parseExtendedContentDescription(payload)
		case guidContentEncryption, guidExtendedContentEncryption:
			d.encrypted = true
		case guidHeaderExtension:
			// The one object worth descending for. Everything else in here is
			// a leaf like every other unrecognized object, but the Advanced
			// Content Encryption Object is a Header Extension child by spec,
			// so leaving the extension opaque would open a WMDRM-protected
			// file and hand stage 10's decoder ciphertext as super-frames.
			d.walkHeaderExtension(payload)
		}
		base += int64(size)
		body = body[size:]
	}
	return nil
}

// fileProperties field offsets within the object body.
const (
	fpDataPackets = 32
	fpPlayDur     = 40
	fpPreroll     = 56
	fpMinPacket   = 68
	fpMaxPacket   = 72
	fpLen         = 80
)

func (d *Demuxer) parseFileProperties(b []byte, off int64) error {
	if len(b) < fpLen {
		return d.warn(off, "File Properties Object is %d bytes, not %d", len(b), fpLen)
	}
	d.playHNS = le.Uint64(b[fpPlayDur:])
	if pre := le.Uint64(b[fpPreroll:]); pre <= uint64(maxPrerollMS) {
		d.prerollMS = int64(pre)
	} else if err := d.warn(off+fpPreroll, "pre-roll of %d ms is implausible and was ignored", pre); err != nil {
		return err
	}
	d.declaredPackets = le.Uint64(b[fpDataPackets:])
	minPkt, maxPkt := le.Uint32(b[fpMinPacket:]), le.Uint32(b[fpMaxPacket:])
	if minPkt != maxPkt {
		// The spec requires the two to agree in a file; they differ only in a
		// live stream, whose packets cannot be addressed by arithmetic and so
		// can be neither walked past damage nor seeked.
		return malformed("variable-size data packets (%d to %d bytes) are not a file this build reads", minPkt, maxPkt)
	}
	if minPkt == 0 || minPkt > maxPacketBytes {
		return malformed("data packet size of %d bytes", minPkt)
	}
	d.packetLen = int64(minPkt)
	return nil
}

// streamProperties field offsets within the object body.
const (
	spTypeDataLen = 40
	spFlags       = 48
	spLen         = 54
)

func (d *Demuxer) parseStreamProperties(b []byte, off int64) error {
	if len(b) < spLen {
		return d.warn(off, "Stream Properties Object is %d bytes, not %d", len(b), spLen)
	}
	if len(d.streams) >= maxStreams {
		return d.warn(off, "more than %d streams", maxStreams)
	}
	flags := le.Uint16(b[spFlags:])
	s := stream{
		number:    int(flags & 0x7f),
		typ:       guidAt(b),
		encrypted: flags&0x8000 != 0,
	}
	tsLen := int64(le.Uint32(b[spTypeDataLen:]))
	avail := int64(len(b) - spLen)
	if tsLen > avail {
		if err := d.warn(off+spTypeDataLen, "stream %d declares %d bytes of type-specific data with %d present", s.number, tsLen, avail); err != nil {
			return err
		}
		tsLen = avail
	}
	if s.typ == guidAudioMedia {
		if w, ok := parseWaveFormat(b[spLen : spLen+tsLen]); ok {
			s.wfx, s.haveWFX = w, true
		}
	}
	d.streams = append(d.streams, s)
	return nil
}

// selectStream picks the audio stream to decode and wires the track. A file
// with no decodable stream is refused by name: the codec table exists so a
// WMA Pro or Voice file says what it is instead of "unsupported format".
func (d *Demuxer) selectStream() error {
	if d.packetLen == 0 {
		return malformed("no File Properties Object")
	}
	if len(d.streams) == 0 {
		return malformed("no Stream Properties Object")
	}
	var audio []stream
	for _, s := range d.streams {
		if s.typ == guidAudioMedia {
			audio = append(audio, s)
		}
	}
	if len(audio) == 0 {
		return malformed("no audio stream (the file carries %s)", d.streamKinds())
	}
	// Encryption is checked ahead of the codec, since it is the more
	// fundamental blocker: a DRM-protected file is unreadable whatever is
	// inside it, and saying so beats naming a codec that is not the problem.
	if d.encrypted {
		return malformed("the file is encrypted (DRM), which this build cannot read")
	}
	var named string
	for _, s := range audio {
		if !s.haveWFX {
			continue
		}
		id, name := asfCodecID(s.wfx.tag)
		if id == "" {
			if name != "" && named == "" {
				named = name
			}
			continue
		}
		if s.encrypted {
			return malformed("%s stream %d is encrypted (DRM), which this build cannot read", name, s.number)
		}
		return d.wireTrack(s, id, name)
	}
	if named != "" {
		return malformed("%s is not a codec this build decodes", named)
	}
	if !audio[0].haveWFX {
		return malformed("audio stream %d carries no WAVEFORMATEX", audio[0].number)
	}
	return malformed("audio format %#04x is not a codec this build decodes", audio[0].wfx.tag)
}

// streamKinds names the stream types present, for the no-audio refusal.
func (d *Demuxer) streamKinds() string {
	seen := make([]string, 0, len(d.streams))
	for _, s := range d.streams {
		if name := streamTypeName(s.typ); !slices.Contains(seen, name) {
			seen = append(seen, name)
		}
	}
	return strings.Join(seen, ", ")
}

// wireTrack builds the container track for the selected stream.
func (d *Demuxer) wireTrack(s stream, id codec.ID, name string) error {
	f := trackFormat(s.wfx)
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "wma: unusable format", err)
	}
	d.sel = s
	d.track = container.Track{
		ID:          0,
		Codec:       id,
		CodecConfig: append([]byte(nil), s.wfx.raw...),
		Fmt:         f,
		Samples:     d.declaredSamples(f.Rate),
		// ASF states positions in milliseconds, so the declared length is a
		// rounded total rather than a hard one the decoder must be trimmed to.
		SamplesExact: false,
		Default:      true,
	}
	// How many packets one media object spans, which sizes both the seek
	// run-up and how long a resumed reader steps over fragments. A packet
	// carries less payload than its own length: the parsing information and
	// one payload header come off the top, so an object exactly as long as a
	// packet already needs two, and the naive size comparison would miss
	// every blockAlign in that gap.
	d.objectPackets = 1
	if room := d.packetLen - packetOverhead; room > 0 {
		d.objectPackets = max(1, (int64(s.wfx.blockAlign)+room-1)/room)
	}
	d.objectPackets = min(d.objectPackets, maxSeekPrerollPackets)
	// A seek lands on a packet boundary, so the run-up it owes the decoder is
	// measured in packets. When a packet holds whole media objects, backing
	// up one already clears at least one of them; when an object spans k
	// packets the landing falls inside the target's own object, so it takes k
	// to clear that and k more for a whole object of history.
	d.seekPreroll = 1
	if d.objectPackets > 1 {
		d.seekPreroll = min(2*d.objectPackets, maxSeekPrerollPackets)
	}
	if len(d.streams) > 1 {
		d.note(0, "the file carries %d streams; %s stream %d was selected", len(d.streams), name, s.number)
	}
	return nil
}

// declaredSamples converts the File Properties play duration to a sample
// count. Play duration counts from the start of the pre-roll, which is
// buffering lead-in rather than audio, so the pre-roll comes back off.
func (d *Demuxer) declaredSamples(rate int) int64 {
	if d.playHNS == 0 || d.playHNS > math.MaxInt64 {
		return -1
	}
	dur := int64(d.playHNS) - d.prerollMS*(hundredNS/1000)
	if dur < 0 {
		return -1
	}
	n, ok := hnsToSamples(dur, rate)
	if !ok {
		return -1
	}
	return n
}

// parseData locates the Data Object, sizes the packet run, and reads the
// Simple Index Object behind it when there is one.
func (d *Demuxer) parseData(off int64) error {
	b := d.w.BytesAt(off, dataObjectLen)
	if len(b) < dataObjectLen {
		if err := d.w.Err(); err != nil {
			return err
		}
		return malformed("no Data Object after the header")
	}
	if id := guidAt(b); id != guidData {
		return malformed("no Data Object after the header (found %s)", id)
	}
	d.dataOff = off + dataObjectLen
	end := d.size
	size := le.Uint64(b[16:])
	switch {
	case size >= dataObjectLen && size <= uint64(d.size-off):
		end = off + int64(size)
	case size != 0:
		// A live capture leaves the size at zero or at a placeholder; either
		// way the packets run to the end of what is there.
		if err := d.warn(off+16, "Data Object declares %d bytes with %d in the file", size, d.size-off); err != nil {
			return err
		}
	}
	if avail := end - d.dataOff; avail > 0 {
		d.packets = avail / d.packetLen
	}
	want := le.Uint64(b[40:])
	if want == 0 {
		// A live capture leaves the Data Object's count at zero; the File
		// Properties Object states the same claim independently.
		want = d.declaredPackets
	}
	if got := uint64(d.packets); want != 0 && want != got {
		// The one cross-check the container offers on its own body: a file
		// truncated after the header still declares its full packet count.
		if err := d.warn(off+40, "Data Object declares %d packets and holds %d", want, got); err != nil {
			return err
		}
	}
	if end < d.size {
		return d.parseIndex(end)
	}
	return nil
}

// payload is one payload of a data packet, normalized: a compressed payload's
// sub-payloads are expanded into an entry each, so reassembly sees one shape.
type payload struct {
	stream  int
	objNum  uint32
	objOff  uint32
	objSize uint32
	presMS  int64
	data    []byte
}

// br is a bounds-checked cursor over one data packet. A read past the end
// leaves ok false and returns zero, so the packet parse checks once at the end
// instead of at every field.
type br struct {
	b  []byte
	p  int
	ok bool
}

func (r *br) take(n int) []byte {
	if n < 0 || !r.ok || r.p+n > len(r.b) {
		r.ok = false
		return nil
	}
	s := r.b[r.p : r.p+n]
	r.p += n
	return s
}

func (r *br) u8() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *br) u16() uint16 {
	if b := r.take(2); b != nil {
		return le.Uint16(b)
	}
	return 0
}

func (r *br) u32() uint32 {
	if b := r.take(4); b != nil {
		return le.Uint32(b)
	}
	return 0
}

// vlen reads a field whose width its length type names: absent, byte, word,
// or dword. The encoding is ASF's throughout the packet header.
func (r *br) vlen(t byte) uint32 {
	switch t {
	case 1:
		return uint32(r.u8())
	case 2:
		return uint32(r.u16())
	case 3:
		return r.u32()
	}
	return 0
}

// packetInfo is a data packet's prologue: everything ahead of the payloads.
type packetInfo struct {
	payStart   int
	payEnd     int
	sendMS     int64
	multi      bool
	nPayloads  int
	payLenType byte
	repType    byte
	offType    byte
	numType    byte
}

// parsePrologue reads the error-correction and payload-parsing headers of one
// packet. It reports false for a packet this build cannot address rather than
// erroring, so the caller decides between a warning and a refusal.
// total is the packet's full length, which is not always len(b): the seek
// path reads only the prologue.
func (d *Demuxer) parsePrologue(b []byte, total int) (packetInfo, bool) {
	if len(b) < 2 {
		return packetInfo{}, false
	}
	p := 0
	if b[0]&0x80 != 0 {
		// Opaque error-correction data replaces the payload area wholesale,
		// and a non-zero length type is reserved; neither is readable here.
		if b[0]&0x10 != 0 || (b[0]>>5)&3 != 0 {
			return packetInfo{}, false
		}
		p = 1 + int(b[0]&0x0f)
	}
	r := br{b: b, p: p, ok: true}
	lenFlags := r.u8()
	propFlags := r.u8()
	pktLen := r.vlen((lenFlags >> 5) & 3)
	r.vlen((lenFlags >> 1) & 3) // sequence, which nothing here needs
	padLen := r.vlen((lenFlags >> 3) & 3)
	sendMS := int64(r.u32())
	r.u16() // packet duration: the span of its payloads, not one object's
	info := packetInfo{
		sendMS:    sendMS,
		multi:     lenFlags&0x01 != 0,
		nPayloads: 1,
		repType:   propFlags & 3,
		offType:   (propFlags >> 2) & 3,
		numType:   (propFlags >> 4) & 3,
	}
	// The stream-number length type must be 01: the field is a byte carrying
	// seven bits of stream number and the key-frame flag.
	if (propFlags>>6)&3 != 1 {
		return packetInfo{}, false
	}
	if info.multi {
		pf := r.u8()
		info.nPayloads = int(pf & 0x3f)
		info.payLenType = (pf >> 6) & 3
	}
	if !r.ok {
		return packetInfo{}, false
	}
	// Both lengths are 32-bit fields and int is 32 bits on some builds, so
	// the arithmetic runs in int64 and the result is bounded by the packet
	// before it is narrowed. A padding length near the top of its range would
	// otherwise wrap the subtraction and slip past the guard below into a
	// slice bound past the end of the packet.
	used := int64(total)
	if pktLen != 0 && int64(pktLen) < used {
		used = int64(pktLen)
	}
	end := used - int64(padLen)
	if end < int64(r.p) {
		return packetInfo{}, false
	}
	info.payStart, info.payEnd = r.p, int(end)
	return info, true
}

// loadPacket parses data packet i into the payload list the fetch loop drains.
// Damage costs the packet, not the stream: the walk moves to the next one.
func (d *Demuxer) loadPacket(i int64) error {
	d.pay = d.pay[:0]
	d.payIdx = 0
	off := d.packetOff(i)
	d.w.Trim(off)
	b := d.w.BytesAt(off, int(d.packetLen))
	if int64(len(b)) < d.packetLen {
		if err := d.w.Err(); err != nil {
			return err
		}
		return d.warn(off, "data packet %d is short", i)
	}
	info, ok := d.parsePrologue(b, len(b))
	if !ok || info.payEnd > len(b) {
		return d.warn(off, "data packet %d has an unreadable header", i)
	}
	r := br{b: b[:info.payEnd], p: info.payStart, ok: true}
	for k := 0; k < info.nPayloads && r.p < info.payEnd && len(d.pay) < maxPayloads; k++ {
		sn := r.u8()
		objNum := r.vlen(info.numType)
		objOff := r.vlen(info.offType)
		rep := r.take(int(r.vlen(info.repType)))
		n := info.payEnd - r.p
		if info.multi {
			n = int(r.vlen(info.payLenType))
		}
		data := r.take(n)
		if !r.ok {
			return d.warn(off, "data packet %d payload %d runs past the packet", i, k)
		}
		if err := d.addPayload(off, int(sn&0x7f), objNum, objOff, rep, data); err != nil {
			return err
		}
	}
	return nil
}

// addPayload normalizes one payload onto the payload list.
func (d *Demuxer) addPayload(off int64, sn int, objNum, objOff uint32, rep, data []byte) error {
	switch {
	case len(rep) == 1:
		// A compressed payload holds a run of length-prefixed sub-payloads,
		// each a whole media object one presentation-time delta after the
		// last; its offset field carries the first one's presentation time
		// rather than an offset.
		delta := int64(rep[0])
		for j := 0; len(data) > 0; j++ {
			if len(d.pay) >= maxPayloads {
				// The one payload form whose entry count grows with the
				// packet rather than with the six-bit count field.
				return d.warn(off, "compressed payload holds more than %d sub-objects", maxPayloads)
			}
			n := int(data[0])
			data = data[1:]
			if n == 0 || n > len(data) {
				return d.warn(off, "compressed payload sub-object of %d bytes with %d left", n, len(data))
			}
			d.pay = append(d.pay, payload{
				stream: sn, objSize: uint32(n), data: data[:n],
				presMS: int64(objOff) + int64(j)*delta,
			})
			data = data[n:]
		}
	case len(rep) >= 8:
		d.pay = append(d.pay, payload{
			stream: sn, objNum: objNum, objOff: objOff,
			objSize: le.Uint32(rep), presMS: int64(le.Uint32(rep[4:])), data: data,
		})
	default:
		return d.warn(off, "payload carries %d bytes of replicated data", len(rep))
	}
	return nil
}

// packetSendTime is packet i's send time in milliseconds, which is its first
// payload's presentation time less the pre-roll. Seeking bisects on it.
func (d *Demuxer) packetSendTime(i int64) (int64, bool) {
	// Read straight from the source rather than through the window. A seek
	// bisects, so its probes are scattered across the file, and each one
	// would pull a fresh 128 KiB chunk in and rebase the window the packet
	// walk is reading through; the prologue is a few dozen bytes.
	n := min(int64(len(d.probe)), d.packetLen)
	if err := container.ReadFull(d.src, d.probe[:n], d.packetOff(i)); err != nil {
		return 0, false
	}
	info, ok := d.parsePrologue(d.probe[:n], int(d.packetLen))
	if !ok {
		return 0, false
	}
	return info.sendMS, true
}

// walkHeaderExtension looks through the Header Extension's children for the
// encryption object that lives there. Everything else is a leaf, so this
// descends exactly one level and never on a depth the file chooses.
func (d *Demuxer) walkHeaderExtension(body []byte) {
	// Reserved GUID (16), reserved word (2), then the data length and data.
	const prologue = 22
	if len(body) < prologue {
		return
	}
	size := int64(le.Uint32(body[18:]))
	if size > int64(len(body)-prologue) {
		size = int64(len(body) - prologue)
	}
	children := body[prologue : prologue+size]
	for len(children) >= objectHeaderLen {
		n := le.Uint64(children[16:])
		if n < objectHeaderLen || n > uint64(len(children)) {
			return
		}
		if guidAt(children) == guidAdvancedContentEncryption {
			d.encrypted = true
			return
		}
		children = children[n:]
	}
}
