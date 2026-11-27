package adts

import (
	"context"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/diag"
	"github.com/colespringer/waxflow/container/internal/id3"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/container/internal/trailer"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
	_ container.Tagger  = (*Demuxer)(nil)
)

// DemuxerVersion is this demuxer's term in the ADR-0004 cache key, carried
// by its track. Bump it for a change that makes the demuxer deliver other
// samples or tags for a file that has not changed.
const DemuxerVersion = "adts-demux-2"

// Demuxer reads one AAC track from an ADTS elementary stream. It carries no
// gapless trims (ADTS has none) and seeks early for the decoder's restart
// state: one frame of IMDCT overlap for AAC-LC, and the HE-AAC preroll
// when the first frame's SBR probe upgraded the track.
type Demuxer struct {
	src container.Source

	ref     header
	haveRef bool // ref is set (distinct from firstFrame, which can be 0)
	track   container.Track
	// tags are the fields of an APEv2 tag appended to the frames.
	tags map[string][]string
	log  diag.Log
	// refused is a strict refusal, kept: the walk is not finished while it
	// stands, and Walk and the reads return it again.
	refused error

	// spf is the output samples each frame decodes to (1024, or 2048 for
	// a detected HE-AAC stream), fixed at open: the format is immutable,
	// so a stream that stops carrying SBR conceals rather than reshapes.
	// preFrames is how many frames before the target a seek lands.
	spf       int64
	preFrames int64

	firstFrame int64   // offset of the first audio frame
	idx        []int64 // lazy frame offsets; idx[i] is frame i's byte offset
	done       bool    // the index has reached the end of the stream
	cur        int64   // next frame number ReadPacket delivers

	w srcwin.Window
}

// NewDemuxer parses the stream head (ID3 tags, the first frame), peels the
// tags appended to the frames, and derives the AudioSpecificConfig. The
// returned Demuxer implements container.Seeker, container.Warner,
// container.Tagger and container.Walker.
func NewDemuxer(src container.Source, opts *container.DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, w: srcwin.New(src, src.Size(), "adts: reading frame data"), log: diag.New("adts: ", opts)}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Demuxer) warn(off int64, format string, args ...any) error {
	if err := d.log.Warn(off, format, args...); err != nil {
		d.refused = err
		return err
	}
	return nil
}

// note records a Warning that Strict must not escalate (the mp4
// convention): warn is for framing mess; a probe that cannot decode a
// payload says nothing about framing, and the file still remuxes.
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.log.Note(off, format, args...)
}

func (d *Demuxer) parse() error {
	off := int64(0)
	for range maxID3Tags {
		if n := id3.Size(d.w.Peek(off, 10)); n > 0 && off+n <= d.w.DataEnd() {
			off += n
		} else {
			break
		}
	}

	first, h, ok := d.nextFrame(off, off+maxResync)
	if d.w.Err() == nil {
		// A trailing tag starts no lower than the end of a first frame the
		// header behind it confirms. With no such frame (none found, or one
		// taken only because it runs to the end of the data), a header is all
		// that has to survive, and the look is taken again without the tags:
		// a lone frame in front of them has nothing else to confirm it.
		floor := off + headerLen
		if ok && first+int64(h.frameLen) < d.w.DataEnd() {
			floor = first + int64(h.frameLen)
		}
		if err := d.stripTrailers(floor); err != nil {
			return err
		}
		if floor == off+headerLen {
			first, h, ok = d.nextFrame(off, off+maxResync)
		}
	}
	if !ok {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("no ADTS frames found")
	}
	if h.blocks != 0 {
		// A frame carrying multiple raw_data_blocks would need to be split
		// into one packet per block; we deliver one block per frame, so
		// refuse rather than undercount the timeline.
		return unsupported("%d raw_data_blocks per frame is unsupported", h.blocks+1)
	}
	if first != off {
		if err := d.warn(off, "%d unparsable bytes before the first frame", first-off); err != nil {
			return err
		}
	}
	d.ref = h
	d.haveRef = true
	d.firstFrame = first
	// Only whole frames enter the index: ReadPacket trusts it, and a walk
	// that published a frame the read then drops would let a strict probe
	// pass a file a strict read fails.
	if first+int64(h.frameLen) <= d.w.DataEnd() {
		d.idx = append(d.idx, first)
	} else if err := d.warn(first, "the only frame is truncated, dropped"); err != nil {
		return err
	}
	if first+int64(h.frameLen) >= d.w.DataEnd() {
		d.done = true // one frame: the index is already whole
	}

	cfg, err := h.config()
	if err != nil {
		return err
	}
	asc := h.asc()
	// ADTS signals HE-AAC implicitly: the headers describe the AAC-LC
	// core and the SBR extension rides inside the access units, so the
	// stream head is probed for it. The format is fixed here, once: a
	// probe hit stamps the HE identity, the doubled rate, and (with
	// ps_data over a mono core) the v2 stereo pair; a miss keeps the LC
	// reading and any SBR appearing later stays skipped, which is
	// sanctioned legacy-decoder behavior.
	//
	// One frame settles SBR (the fill rides in every HE access unit), but
	// the v1/v2 split needs a parseable sbr_data, which needs an
	// sbr_header, and encoders repeat those only every ten frames or so.
	// A capture that lost the stream head would otherwise open a v2 file
	// as mono whenever its first frame carries no header, so the probe
	// walks the head's contiguous confirmed frames (no index growth, no
	// resync, no warnings) until DetectSBR is conclusive or the cap ends
	// the scan.
	const probeFrames = 32 // three times fdk's header interval
	probeOff, probeHdr, probed := first, h, 0
	nextAU := func() []byte {
		if probed >= probeFrames {
			return nil
		}
		if probed > 0 {
			off := probeOff + int64(probeHdr.frameLen)
			nh, ok := d.frameAt(off)
			if !ok {
				return nil
			}
			probeOff, probeHdr = off, nh
		}
		payload := d.w.BytesAt(probeOff+int64(probeHdr.hdrLen), probeHdr.frameLen-probeHdr.hdrLen)
		if len(payload) != probeHdr.frameLen-probeHdr.hdrLen {
			return nil
		}
		probed++
		return payload
	}
	sbr, ps, perr := aac.DetectSBR(cfg, nextAU)
	if perr != nil {
		d.note(first, "SBR probe failed on the first frame (%v); reading the stream as AAC-LC", perr)
	} else if sbr {
		heASC, bErr := aac.BuildHEASC(cfg.SampleRate, cfg.ChannelConfig, ps && cfg.Channels == 1)
		if bErr == nil {
			if heCfg, pErr := aac.ParseASC(heASC); pErr == nil {
				cfg, asc = heCfg, heASC
			} else {
				bErr = pErr
			}
		}
		if bErr != nil {
			// Detected but inexpressible (a core rate whose double has no
			// sampling-frequency index): the LC fallback halves the rate and
			// channels, which must not happen silently.
			d.note(first, "implicit SBR detected but not expressible (%v); reading the stream as AAC-LC", bErr)
		}
	}
	f, err := cfg.Format()
	if err != nil {
		return err
	}
	if err := f.Valid(); err != nil {
		return container.UnusableFormat("adts", f, err)
	}
	d.spf = int64(cfg.OutputSamplesPerAU())
	d.preFrames = 1 // one frame of IMDCT overlap history
	if aac.TrackID(cfg) == codec.HEAAC {
		// The SBR/PS restart distance, in this stream's frames.
		d.preFrames = aac.HESeekPreroll / d.spf
	}
	d.track = container.Track{
		Codec:          aac.TrackID(cfg),
		CodecConfig:    asc,
		Fmt:            f,
		DemuxerVersion: DemuxerVersion,
		Samples:        -1, // ADTS declares no length; the stream runs to EOF
		Default:        true,
	}
	return nil
}

// stripTrailers peels the tags a tagger appended to the frames (APEv2, ID3v1,
// a footed ID3v2, a Lyrics3 block), shrinking the window's data end so the
// frame walk ends where they begin: a last frame cut short in front of them
// does not read their bytes as its own. floor is the lowest offset one may
// start at.
// The APEv2 block is where the track's tags come from. No warning: a tagged
// stream is not a damaged one.
func (d *Demuxer) stripTrailers(floor int64) error {
	// Through a window of their own, so the head the probe frames are read
	// from stays where it is. The probes decide where the audio ends, so a
	// read that fails is the open's.
	end := d.w.DataEnd()
	if _, err := d.w.Try(false, func() bool {
		end, d.tags = trailer.PeelAll(&d.w, trailer.APEv2|trailer.ID3v1|trailer.ID3v2|trailer.Lyrics3, floor, end)
		return true
	}); err != nil {
		return err
	}
	d.w.SetDataEnd(end)
	return nil
}

// nextFrame scans [from, limit) for a valid, length-confirmed frame header.
// The frame at from is tried first, through the window a clean stream reads
// its frames from. Only junk pays the scan.
func (d *Demuxer) nextFrame(from, limit int64) (int64, header, bool) {
	limit = min(limit, d.w.DataEnd())
	if from >= limit {
		return 0, header{}, false
	}
	if h, ok := d.frameAt(from); ok {
		return from, h, true
	}
	var h header
	off, ok := d.w.Scan(from, limit, []byte{0xFF}, headerLen, func(off int64, hdr []byte) srcwin.Verdict {
		got, ok := d.confirmed(off, hdr, d.w.Ahead)
		if !ok {
			return srcwin.Skip
		}
		h = got
		return srcwin.Take
	})
	return off, h, ok
}

// frameAt parses and confirms the frame at the exact offset, through the
// packet path's read: a miss loads a window.
func (d *Demuxer) frameAt(off int64) (header, bool) {
	return d.confirmed(off, d.w.BytesAt(off, 9), d.w.BytesAt)
}

// confirmed parses the candidate header hdr found at off and confirms it by
// the header its length points at (the end of data counts), read with the
// frame in one read from off. read is Ahead inside a scan.
func (d *Demuxer) confirmed(off int64, hdr []byte, read func(off int64, n int) []byte) (header, bool) {
	h, ok := parseHeader(hdr)
	if !ok {
		return header{}, false
	}
	if d.haveRef && !d.ref.kin(h) {
		return header{}, false
	}
	next := off + int64(h.frameLen)
	if next >= d.w.DataEnd() {
		return h, true // runs to the end: nothing to confirm against
	}
	b := read(off, h.frameLen+9)
	if len(b) < h.frameLen+9 {
		return header{}, false
	}
	if nh, ok := parseHeader(b[h.frameLen:]); ok && h.kin(nh) {
		return h, true
	}
	return header{}, false
}

// Tracks returns the single AAC track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.log.List() }

// Tags returns the fields of an APEv2 tag appended to the frames, under
// canonical uppercase keys: nil with none. A leading ID3v2 tag is not read
// here.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// DamageCount implements container.Damaged.
func (d *Demuxer) DamageCount() int { return d.log.DamageCount() }

// extend grows the frame index by one and reports whether it did. The
// window is trimmed to the last indexed frame first, and the next header is
// pulled in with that frame: a seek to the end walks the index without
// reading a packet, and this is what keeps that walk at one window, sliding.
func (d *Demuxer) extend() (bool, error) {
	if d.refused != nil {
		return false, d.refused
	}
	if d.done {
		return false, nil
	}
	if d.w.Err() != nil {
		return false, d.w.Err()
	}
	if len(d.idx) == 0 {
		// parse already latches done when the only frame does not fit, so this
		// is unreachable today. It stays because the alternative is an
		// out-of-range read one drifting condition away, and because
		// mpegframes.extend carries the same guard for the same reason.
		d.done = true
		return false, nil
	}
	last := d.idx[len(d.idx)-1]
	d.w.Trim(last)
	h, ok := parseHeader(d.w.BytesAt(last, 9))
	if !ok {
		// A read that failed is no end: the walk is not done.
		if err := d.w.Err(); err != nil {
			return false, err
		}
		d.done = true
		return false, nil
	}
	next := last + int64(h.frameLen)
	if next >= d.w.DataEnd() {
		d.done = true
		return false, nil
	}
	// The sequential step trusts the length the last header states and
	// parses the header it points at, kin to the reference; only the resync
	// scan confirms a candidate by the header behind it. Confirming here
	// too would drop the frame ahead of any junk along with the junk, since
	// that frame's own successor is the junk. The header is read with the
	// last frame so the read extends the window rather than rebasing at a
	// boundary.
	cand := next
	var nh header
	ok = false
	if b := d.w.BytesAt(last, h.frameLen+9); len(b) >= h.frameLen {
		nh, ok = parseHeader(b[h.frameLen:])
	}
	if !ok || !d.ref.kin(nh) {
		if d.w.Err() != nil {
			return false, d.w.Err()
		}
		// Damage or trailing junk: resync within bounds, else end.
		var found bool
		cand, nh, found = d.nextFrame(next, next+maxResync)
		if !found {
			// A scan a read failed in found nothing, which says nothing
			// about the bytes.
			if d.w.Err() != nil {
				return false, d.w.Err()
			}
			d.done = true
			if tail := d.w.DataEnd() - next; tail > 0 {
				return false, d.warn(next, "%d trailing bytes are not frames, dropped", tail)
			}
			return false, nil
		}
		if err := d.warn(next, "%d unparsable bytes skipped", cand-next); err != nil {
			return false, err
		}
	}
	// frameAt accepts a candidate whose length runs to the end of data, since
	// there is no header behind it to confirm against; the fit is decided
	// here, so only whole frames enter the index.
	if cand+int64(nh.frameLen) > d.w.DataEnd() {
		d.done = true
		return false, d.warn(cand, "truncated final frame dropped")
	}
	d.idx = append(d.idx, cand)
	return true, nil
}

// Walk implements container.Walker: the frame index is driven to the end of
// the stream, so junk between frames past the head reaches Warnings, or
// under Strict the error, without a packet being read. A finished walk has
// counted every frame, which is the length the stream itself never states,
// so the track reports it exact from then on: a strict probe, which walks
// before its snapshot, measures the file it refused to guess about. ctx is
// read before every frame; one that has ended pauses the walk there
// (container.Walker). A strict refusal is kept and returned again.
func (d *Demuxer) Walk(ctx context.Context) error {
	if d.refused != nil {
		return d.refused
	}
	for !d.done {
		if err := container.WalkCanceled(ctx, "adts: "); err != nil {
			return err
		}
		grew, err := d.extend()
		if err != nil {
			return err
		}
		if !grew {
			break
		}
	}
	if d.Walked() {
		// Through the shared rule rather than assigned: ADTS carries no
		// trims, so it lands on the same number either way, and one
		// settle keeps every frame-indexed container saying the same
		// thing about what a finished walk means.
		d.track = container.SettleLength(d.track, int64(len(d.idx))*d.spf)
	}
	return nil
}

// Walked implements container.Walker: whether the index reaches the end, and
// nothing was refused on the way.
func (d *Demuxer) Walked() bool { return d.done && d.refused == nil }

// frameNo extends the index up to frame n, returning the highest available.
func (d *Demuxer) frameNo(ctx context.Context, n int64) (int64, error) {
	for int64(len(d.idx)) <= n {
		if d.refused != nil {
			return 0, d.refused
		}
		if d.done {
			break // nothing left to index, whatever ctx says
		}
		if err := container.WalkCanceled(ctx, "adts: "); err != nil {
			return 0, err
		}
		grew, err := d.extend()
		if err != nil {
			return 0, err
		}
		if !grew {
			break
		}
	}
	return int64(len(d.idx)) - 1, nil
}

// ReadPacket yields one raw_data_block. Packet data is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	lastNo, err := d.frameNo(context.Background(), d.cur) // a read carries no context
	if err != nil {
		return err
	}
	if d.cur > lastNo {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	off := d.idx[d.cur]
	h, ok := parseHeader(d.w.BytesAt(off, 9))
	if !ok {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return waxerr.New(waxerr.CodeSourceUnreadable, "adts: indexed frame vanished")
	}
	d.w.Trim(off)
	frame := d.w.BytesAt(off, h.frameLen)
	if len(frame) < h.frameLen {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		// The index only holds frames that fit, so a short read here is the
		// source shrinking under us, which is the read-time rule.
		return waxerr.New(waxerr.CodeSourceUnreadable, "adts: indexed frame shrank")
	}
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: frame[h.hdrLen:],
			PTS:  d.cur * d.spf,
			Dur:  d.spf,
			Sync: true,
		},
	}
	d.cur++
	return nil
}

// SeekSample lands preFrames before the target (the decoder's restart
// state: IMDCT overlap for LC, the SBR/PS preroll for HE-AAC) and returns
// that frame's first sample; format.Media pre-rolls the rest.
func (d *Demuxer) SeekSample(ctx context.Context, track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("adts: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "adts: negative seek target")
	}
	target := sample / d.spf
	lastNo, err := d.frameNo(ctx, target)
	if err != nil {
		return 0, err
	}
	if lastNo < 0 {
		return 0, nil
	}
	target = min(target, lastNo)
	land := max(target-d.preFrames, 0)
	d.cur = land
	return land * d.spf, nil
}
