package wv

import (
	"bytes"
	"fmt"
	"io"
	"slices"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer = (*Demuxer)(nil)
	_ container.Seeker  = (*Demuxer)(nil)
	_ container.Warner  = (*Demuxer)(nil)
	_ container.Tagger  = (*Demuxer)(nil)
)

// Hostile-input caps (ADR-0005 invariants).
const (
	// maxResync bounds the scan for a block header after damage.
	maxResync = 1 << 20
	// seekWindow is the bisection cutoff: below this span, walk blocks.
	seekWindow = 128 << 10
	// maxTrailers bounds trailing-tag peeling at end of stream; tags stack,
	// APEv2 then ID3v1 being the classic pair.
	maxTrailers = 8
	// maxMetaBlocks bounds the run of sample-free blocks the header walk will
	// step over on its way to the first audio block. Real files hold at most
	// one; the reference decoder gives up after sixteen.
	maxMetaBlocks = 16
	// maxTailScan bounds the backward scan that recovers the length of a
	// stream whose header never learned it.
	maxTailScan = 1 << 22
	// id3v1Len is the fixed size of a trailing ID3v1 tag.
	id3v1Len = 128
	// maxProbe bounds one bisection probe's scan for a block header. A block
	// is under a megabyte (SyncOK), so a probe landing just past one block's
	// start finds the next inside this; without the bound a probe over a
	// damaged tail would pull the rest of the file into the read window.
	maxProbe = 2 << 20
	// maxWarnings caps the tolerated-damage list. Seeking re-walks spans a
	// linear read already crossed, so a damaged file would otherwise
	// accumulate the same warning without bound.
	maxWarnings = 64
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Demuxer reads one WavPack track from a native .wv source.
type Demuxer struct {
	opts DemuxerOptions

	cfg      wavpack.Config
	track    container.Track
	tags     map[string][]string
	warnings []container.Warning

	firstBlock int64 // byte offset of the first audio block
	// initialIndex is the first audio block's own sample index. A .wv file
	// carved out of a longer encode starts partway up the numbering, so
	// packet positions are measured from here rather than from zero.
	initialIndex int64

	off   int64 // offset of the current undelivered block
	cur   wavpack.BlockHeader
	valid bool

	// w is the shared read-ahead window; its data end is the end of block
	// data (trailing tags stripped during the header parse) and its sticky
	// error surfaces on the packet and seek paths.
	w srcwin.Window
}

// NewDemuxer parses the header of a native WavPack source and positions on
// the first audio block. The returned Demuxer implements container.Seeker,
// container.Warner, and container.Tagger.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{w: srcwin.New(src, src.Size(), "wavpack: reading block data")}
	if opts != nil {
		d.opts = *opts
	}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

func malformed(format string, args ...any) error {
	return waxerr.New(waxerr.CodeUnsupportedFormat, "wavpack: "+fmt.Sprintf(format, args...))
}

// warn records tolerated damage, or fails in strict mode.
func (d *Demuxer) warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if d.opts.Strict {
		return malformed("%s (at offset %d)", msg, off)
	}
	w := container.Warning{Offset: off, Msg: msg}
	if len(d.warnings) < maxWarnings && !slices.Contains(d.warnings, w) {
		d.warnings = append(d.warnings, w)
	}
	return nil
}

func (d *Demuxer) parse() error {
	if !Match(d.w.BytesAt(0, 4)) {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("not a WavPack file")
	}
	if err := d.stripTrailers(); err != nil {
		return err
	}

	// Step over any leading sample-free blocks: an encoder can put the source
	// file's RIFF wrapper in one of its own before the audio starts.
	off := int64(0)
	var h wavpack.BlockHeader
	for i := 0; ; i++ {
		var ok bool
		h, ok = d.blockAt(off)
		if !ok {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return malformed("no parsable block at offset %d", off)
		}
		if h.Audio() {
			break
		}
		if i >= maxMetaBlocks {
			return malformed("more than %d blocks before the first audio block", maxMetaBlocks)
		}
		off += h.Size
	}

	block := d.w.BytesAt(off, int(h.Size))
	if int64(len(block)) != h.Size {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return malformed("first block truncated")
	}
	cfg, err := wavpack.ProbeBlock(block)
	if err != nil {
		return err
	}
	d.cfg = cfg

	f := cfg.Format()
	if err := f.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "wavpack: unusable format", err)
	}
	cfgBlob, err := cfg.MarshalBinary()
	if err != nil {
		return err
	}
	d.firstBlock, d.initialIndex = off, h.BlockIndex
	d.off, d.cur, d.valid = off, h, true

	// The declared total is the encoder's back-patched length and counts from
	// sample zero, so it only applies to a stream that starts there; anything
	// else (a carved-out .wv, an interrupted encode with no total at all) has
	// its length read off the last block instead.
	//
	// SamplesExact stays false either way. It exists for formats whose decoder
	// over-produces past a length the container states exactly (an Ogg
	// granule); WavPack's does not, since every block declares its own sample
	// count and yields exactly that. Setting it would buy nothing and would
	// turn a declared total that disagrees with the blocks into a silent
	// truncation instead of the tolerated oddity flacn treats it as.
	samples := int64(-1)
	if h.BlockIndex == 0 && h.TotalSamples >= 0 {
		samples = h.TotalSamples
	} else if end, ok := d.scanTail(); ok {
		samples = end - d.initialIndex
	}
	d.track = container.Track{
		Codec:       codec.WavPack,
		CodecConfig: cfgBlob,
		Fmt:         f,
		Samples:     samples,
		Default:     true,
	}
	// The stream's own depth, when the container width it decodes at is wider
	// (a 20-bit source in 24-bit words). Probe reports this rather than the
	// width, the same way a 64-bit float WAV reports 64.
	if cfg.ValidBits != f.BitDepth {
		d.track.SourceBitDepth = cfg.ValidBits
	}
	return d.w.Err()
}

// stripTrailers peels recognized non-audio structures off the end of the
// file (an APEv2 tag, an ID3v1 tag, and the two stacked), shrinking the
// window's data end so the block walk never runs into them. The APEv2 block
// is also where the track's tags come from.
func (d *Demuxer) stripTrailers() error {
	end := d.w.DataEnd()
	for range maxTrailers {
		if e := end - id3v1Len; e >= 0 && string(d.w.BytesAt(e, 3)) == "TAG" {
			end = e
			continue
		}
		if e := end - apev2.FooterLen; e >= 0 {
			n, hasHeader := apev2.Size(d.w.BytesAt(e, apev2.FooterLen))
			// The tag has to leave a block behind it, and when its footer
			// claims a header, the header has to be there: a declared length
			// is otherwise an unverified instruction to drop audio.
			if n > 0 && end-n >= wavpack.BlockHeaderLen {
				tag := d.w.BytesAt(end-n, int(n))
				if int64(len(tag)) == n && (!hasHeader || apev2.StartsTag(tag)) {
					if d.tags == nil {
						d.tags = apev2.Parse(tag)
					}
					end -= n
					continue
				}
			}
		}
		break
	}
	// No warning here, unlike the FLAC demuxer: a tag after the audio is the
	// normal shape of a .wv file rather than damage recovered from, and
	// warning would make strict mode reject every tagged WavPack file.
	d.w.SetDataEnd(end)
	return d.w.Err()
}

// scanTail looks back from the end of the audio for the last block, and
// returns the sample index one past its last frame. It is how a stream whose
// header carries no total gets a length.
//
// The scan is backward only in where it starts looking; a candidate counts
// only once its declared length lands exactly on the end of the audio, which
// is what tells the real last block from a "wvpk" the payload happened to
// spell.
func (d *Demuxer) scanTail() (int64, bool) {
	audioEnd := d.w.DataEnd()
	lo := max(d.firstBlock, audioEnd-maxTailScan)
	for hi := audioEnd; hi > lo; {
		from := max(lo, hi-srcwin.Chunk)
		buf := d.w.BytesAt(from, int(hi-from))
		if len(buf) == 0 {
			return 0, false
		}
		for rel := len(buf); ; {
			i := bytes.LastIndex(buf[:rel], magic)
			if i < 0 {
				break
			}
			rel = i + len(magic) - 1
			cand := from + int64(i)
			h, ok := d.blockAt(cand)
			if !ok || !h.Audio() || !d.tilesToEnd(cand+h.Size, audioEnd) {
				continue
			}
			return h.BlockIndex + int64(h.BlockSamples), true
		}
		if from == lo {
			break // the whole window is scanned
		}
		// Overlap by the magic length so a candidate straddling the chunk
		// boundary is not lost between passes.
		hi = from + int64(len(magic)) - 1
	}
	return 0, false
}

// tilesToEnd reports whether the blocks from off tile exactly to end. It is
// the tail scan's confirmation: a real last audio block is followed only by
// the sample-free blocks a stream signs off with, and they land on the end of
// the audio exactly. A "wvpk" the payload happened to spell does not.
func (d *Demuxer) tilesToEnd(off, end int64) bool {
	for range maxMetaBlocks + 1 {
		switch {
		case off == end:
			return true
		case off > end:
			return false
		}
		h, ok := d.blockAt(off)
		if !ok {
			return false
		}
		off += h.Size
	}
	return false
}

// blockAt parses the block header at off and reports whether a whole block
// starts there: the header parses and its declared length fits inside the
// audio data. That is all a sequential step needs, because a WavPack block
// carries its own length and the position it steps from was already trusted.
func (d *Demuxer) blockAt(off int64) (wavpack.BlockHeader, bool) {
	buf := d.w.BytesAt(off, wavpack.BlockHeaderLen)
	if len(buf) < wavpack.BlockHeaderLen || !wavpack.SyncOK(buf) {
		return wavpack.BlockHeader{}, false
	}
	h, err := wavpack.ParseBlockHeader(buf)
	if err != nil || off+h.Size > d.w.DataEnd() {
		return wavpack.BlockHeader{}, false
	}
	return h, true
}

// confirm is blockAt plus the checks a scan needs: what follows the block must
// be either the end of the audio or another block header, and the block must
// agree with the track. Compressed payload can spell "wvpk" and pass the
// header sniff; chaining to a second header is what tells a real boundary from
// that coincidence. Only the paths that guess an offset (resync, bisection
// probes) pay for it.
func (d *Demuxer) confirm(off int64) (wavpack.BlockHeader, bool) {
	h, ok := d.blockAt(off)
	if !ok {
		return wavpack.BlockHeader{}, false
	}
	// A candidate that disagrees with the track is not this stream's block, so
	// the scan keeps looking. Only the sequential walk treats a shape change
	// as the hard error it is; a guess that lands on one is just a bad guess.
	if d.cfg.Channels != 0 && h.Audio() &&
		(h.Channels() != d.cfg.Channels || h.BytesPerSample()*8 != d.cfg.BitDepth) {
		return wavpack.BlockHeader{}, false
	}
	end := off + h.Size
	if end == d.w.DataEnd() {
		return h, true
	}
	if _, ok := d.blockAt(end); !ok {
		return wavpack.BlockHeader{}, false
	}
	return h, true
}

// magic is the block marker the scans look for.
var magic = []byte("wvpk")

// nextCandidate scans [from, limit) for a confirmable block header. It is the
// resync path after damage and the bisection probe.
func (d *Demuxer) nextCandidate(from, limit int64) (int64, wavpack.BlockHeader, bool) {
	limit = min(limit, d.w.DataEnd())
	for off := from; off < limit; {
		buf := d.w.BytesAt(off, srcwin.Chunk)
		if len(buf) < wavpack.BlockHeaderLen {
			return 0, wavpack.BlockHeader{}, false
		}
		i := bytes.Index(buf, magic)
		if i < 0 {
			// Step back by the magic length so one straddling the chunk
			// boundary is not lost between passes.
			off += int64(len(buf) - len(magic) + 1)
			continue
		}
		cand := off + int64(i)
		if cand >= limit {
			return 0, wavpack.BlockHeader{}, false
		}
		if h, ok := d.confirm(cand); ok {
			return cand, h, true
		}
		off = cand + 1
	}
	return 0, wavpack.BlockHeader{}, false
}

// advance moves to the block after the current one, resyncing over damage.
func (d *Demuxer) advance() error {
	end := d.off + d.cur.Size
	if end >= d.w.DataEnd() {
		d.valid = false
		return nil
	}
	h, ok := d.blockAt(end)
	if !ok {
		off, resynced, found := d.nextCandidate(end, end+maxResync)
		if !found {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			if err := d.warn(end, "%d trailing bytes are not a block, dropped", d.w.DataEnd()-end); err != nil {
				return err
			}
			d.valid = false
			return nil
		}
		if err := d.warn(end, "%d unparsable bytes between blocks", off-end); err != nil {
			return err
		}
		end, h = off, resynced
	}
	d.off, d.cur = end, h
	return nil
}

// Tracks returns the single WavPack track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// Tags returns the APEv2 tag's fields under canonical uppercase keys.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// ReadPacket yields one whole block, header included. Blocks that carry no
// samples are stream metadata, not packets, and are stepped over. Packet data
// is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	for d.valid {
		d.w.Trim(d.off)
		h := d.cur
		if !h.Audio() {
			// Stream metadata (a RIFF trailer, an MD5 signature), not a packet.
			if err := d.advance(); err != nil {
				return err
			}
			continue
		}
		if err := d.checkShape(h); err != nil {
			return err
		}
		data := d.w.BytesAt(d.off, int(h.Size))
		if int64(len(data)) != h.Size {
			if d.w.Err() != nil {
				return d.w.Err()
			}
			return waxerr.New(waxerr.CodeSourceUnreadable, "wavpack: reading block data")
		}
		if err := d.advance(); err != nil {
			return err
		}
		*pkt = container.Packet{
			Track: 0,
			Packet: codec.Packet{
				Data: data,
				PTS:  h.BlockIndex - d.initialIndex,
				Dur:  int64(h.BlockSamples),
				Sync: true,
			},
		}
		return nil
	}
	if d.w.Err() != nil {
		return d.w.Err()
	}
	return io.EOF
}

// checkShape rejects a block whose channel count or width disagrees with the
// track. WavPack permits the change; the fixed-format pipeline does not, so
// it is a hard error rather than tolerated damage.
func (d *Demuxer) checkShape(h wavpack.BlockHeader) error {
	if h.Channels() != d.cfg.Channels || h.BytesPerSample()*8 != d.cfg.BitDepth {
		return malformed("mid-stream format change at offset %d", d.off)
	}
	return nil
}

// SeekSample repositions to the block containing the target sample and
// returns that block's first sample; format.Media pre-rolls the remainder.
// Bisection on the blocks' own sample indexes narrows the range, then the
// remaining span is walked.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("wavpack: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "wavpack: negative seek target")
	}
	off, h, ok := d.bisect(sample)
	if !ok {
		if d.w.Err() != nil {
			return 0, d.w.Err()
		}
		return 0, malformed("cannot relocate any block for seeking")
	}
	d.off, d.cur, d.valid = off, h, true
	// Walk forward to the block holding the target, stepping over the
	// sample-free blocks a stream ends with (an MD5 signature, a RIFF
	// trailer): their block index is not a position, so landing on one would
	// report whatever the encoder happened to write there. A past-the-end
	// target runs out of blocks and the walk stands on the last one that
	// carried samples, which is where the seek lands and where reads resume.
	lastOff, lastH := d.off, d.cur
	for d.valid {
		if d.cur.Audio() {
			if d.pos(d.cur)+int64(d.cur.BlockSamples) > sample {
				return d.pos(d.cur), nil
			}
			lastOff, lastH = d.off, d.cur
		}
		d.w.Trim(d.off)
		if err := d.advance(); err != nil {
			return 0, err
		}
	}
	d.off, d.cur, d.valid = lastOff, lastH, true
	return d.pos(d.cur), nil
}

// pos is a block's position in the track timeline.
func (d *Demuxer) pos(h wavpack.BlockHeader) int64 { return h.BlockIndex - d.initialIndex }

// bisect narrows the byte range holding the target sample using block headers
// found from probe offsets, then hands back the earliest block of the final
// window.
func (d *Demuxer) bisect(sample int64) (int64, wavpack.BlockHeader, bool) {
	lo, loH, ok := d.firstBlock, wavpack.BlockHeader{}, false
	if loH, ok = d.blockAt(lo); !ok || !loH.Audio() {
		lo, loH, ok = d.nextAudio(lo, lo+maxResync)
		if !ok {
			return 0, wavpack.BlockHeader{}, false
		}
	}
	if d.pos(loH) > sample {
		return lo, loH, true
	}
	hi := d.w.DataEnd()
	for hi-lo > seekWindow {
		mid := lo + (hi-lo)/2
		d.w.Trim(mid)
		// One block's worth of scan is all a probe can need; past that the
		// span is damaged and halving is the right answer anyway.
		off, h, found := d.nextAudio(mid, min(hi, mid+maxProbe))
		if !found || d.pos(h) > sample {
			hi = mid
			continue
		}
		lo, loH = off, h
	}
	return lo, loH, true
}

// nextAudio is nextCandidate restricted to blocks that carry samples, which
// are the only ones a seek can land on.
func (d *Demuxer) nextAudio(from, limit int64) (int64, wavpack.BlockHeader, bool) {
	for off := from; off < limit; {
		cand, h, ok := d.nextCandidate(off, limit)
		if !ok {
			return 0, wavpack.BlockHeader{}, false
		}
		if h.Audio() {
			return cand, h, true
		}
		off = cand + 1
	}
	return 0, wavpack.BlockHeader{}, false
}
