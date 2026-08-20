package apen

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/ape"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/internal/muxseek"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// Fixed sizes of the sections written ahead of the audio.
const (
	descriptorLen = 52
	headerLen     = 24
)

// maxSeekEntries caps the table this muxer will reserve, which is the same
// bound the demuxer puts on one it will read. It is what keeps a stream
// declaring a very short frame length from asking for a table nothing could
// hold.
const maxSeekEntries = maxSeekTableBytes / 4

// maxTagBytes bounds the APEv2 block, matching the demuxer's own trailer cap.
const maxTagBytes = 16 << 20

// packChunk is how many bytes the word packer buffers before writing. Frames
// run to megabytes, and the packer walks them a byte at a time.
const packChunk = 32 << 10

// MuxerOptions configures writing.
type MuxerOptions struct {
	// Tags are written as an APEv2 block after the audio, which is where a
	// .ape file conventionally carries them and where the demuxer reads them.
	Tags []container.Tag
}

// Muxer writes one Monkey's Audio track as a native .ape stream.
//
// NeedsSeek reports true: the file opens with a descriptor whose fields (the
// frame count, the final frame's length, the file's MD5) are only known once
// the audio has gone out, and with a seek table that is the format's only
// index -- a frame states neither its length nor its position, so a .ape
// without a filled table is not a degraded stream but an unreadable one. There
// is no streaming form to fall back to.
type Muxer struct {
	w     io.Writer
	patch muxseek.Patcher
	opts  MuxerOptions

	cfg ape.Config

	// seek is the frame index being built: one absolute stream offset per
	// frame. entries is what was reserved for it at Begin, which caps the
	// stream.
	seek    []uint32
	entries int

	// md5 covers the file the way the reference's quick verify recomputes it:
	// the stored source header (none here), then the frame data, then the
	// format header and the seek table.
	md5 hash.Hash

	off          int64 // bytes written so far
	frameDataOff int64
	logical      int64 // coded bytes laid down, which is where the next frame starts
	blocks       int64
	final        int
	began, ended bool

	// word is the 32-bit word being filled and wordN how many of its logical
	// bytes are in it. The frame data is a run of little-endian words read
	// most significant byte first, so a frame starts wherever in a word the
	// one before it ended and the boundary has to be carried across packets.
	word    [4]byte
	wordN   int
	scratch []byte
}

// NewMuxer returns a Monkey's Audio muxer writing to w. Begin fails unless w
// can actually seek; callers should check NeedsSeek and provide a file.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, md5: md5.New()}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek reports true: Monkey's Audio cannot be written to a plain stream.
func (m *Muxer) NeedsSeek() bool { return true }

// Begin validates the track and writes the descriptor, the format header, and
// the seek table's reservation.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "ape: Begin called twice")
	}
	m.patch = muxseek.New(m.w, "ape")
	if !m.patch.Seekable() {
		return waxerr.New(waxerr.CodeInvalidRequest,
			"ape: output requires a seekable destination (the seek table and the file's own totals are written last)")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ape: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.APE {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("ape: cannot mux codec %q", t.Codec))
	}
	if t.Delay != 0 || t.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			"ape: Monkey's Audio signals no gapless trims (lossless streams have none)")
	}
	cfg, err := ape.ParseConfig(t.CodecConfig)
	if err != nil {
		return err
	}
	if want := cfg.Format(); t.Fmt != want {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ape: track format %v does not match its codec config (want %v)", t.Fmt, want))
	}
	m.cfg = cfg
	m.entries = reserveEntries(t.Samples, cfg.BlocksPerFrame)
	m.seek = make([]uint32, 0, m.entries)
	m.began = true
	return m.writeHeaders()
}

// reserveEntries sizes the seek table from the engine's projected length, with
// one frame of slack because the projection is a rounded resample estimate
// rather than a count. The table sits between the header and the first frame,
// so its size is fixed before a single sample is coded and a stream that
// outgrew it could not be finished.
//
// A source that will not say how long it is gets the largest table the stream
// could ever need: every block the header's 32-bit count can describe, at THIS
// stream's frame length. That is the reference encoder's own answer, and
// deriving it from the frame length in hand rather than from a constant is
// what keeps it true on the remux path, where the frame length is the source
// file's and not this encoder's. A length past what the format can count is
// treated the same way, which also keeps the arithmetic below from
// overflowing.
func reserveEntries(samples int64, blocksPerFrame int) int {
	bpf := int64(blocksPerFrame)
	most := min(int64(maxSeekEntries), (1<<32-1)/bpf+1)
	if samples < 0 || samples > (most-1)*bpf {
		return int(most)
	}
	return int(min((samples+bpf-1)/bpf+1, most))
}

func (m *Muxer) writeHeaders() error {
	// The placeholder is the finished descriptor with nothing behind it yet:
	// rendering both through one function is what keeps the reservation and
	// the patch the same size and the same shape.
	if err := m.write(m.descriptor(0)); err != nil {
		return err
	}
	if err := m.write(m.formatHeader(0, 0)); err != nil {
		return err
	}
	// The table's reservation is zeros; End writes the real offsets over it.
	zeros := make([]byte, min(m.entries, 1<<12)*4)
	for left := m.entries * 4; left > 0; {
		n := min(left, len(zeros))
		if err := m.write(zeros[:n]); err != nil {
			return err
		}
		left -= n
	}
	m.frameDataOff = m.off
	return nil
}

// formatHeader renders the 24-byte APE_HEADER. It is rendered twice, once as
// the reservation and once with the totals, because it is also hashed into the
// file MD5 and the two copies must be the same bytes.
func (m *Muxer) formatHeader(totalFrames, finalFrameBlocks int) []byte {
	var h [headerLen]byte
	binary.LittleEndian.PutUint16(h[0:], uint16(m.cfg.CompressionLevel))
	// The only flag we set says no source-file header is stored.
	binary.LittleEndian.PutUint16(h[2:], flagCreateWAVHeader)
	binary.LittleEndian.PutUint32(h[4:], uint32(m.cfg.BlocksPerFrame))
	binary.LittleEndian.PutUint32(h[8:], uint32(finalFrameBlocks))
	binary.LittleEndian.PutUint32(h[12:], uint32(totalFrames))
	binary.LittleEndian.PutUint16(h[16:], uint16(m.cfg.BitsPerSample))
	binary.LittleEndian.PutUint16(h[18:], uint16(m.cfg.Channels))
	binary.LittleEndian.PutUint32(h[20:], uint32(m.cfg.Rate))
	return h[:]
}

// flagCreateWAVHeader is the format flag saying the source file's header was
// not stored and a decompressor should synthesize one.
const flagCreateWAVHeader = 1 << 5

// WritePacket appends one frame and indexes it.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "ape: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("ape: no track %d", pkt.Track))
	}
	blocks, skip, n, data, err := ape.ParseFrameHeader(pkt.Data)
	if err != nil {
		return err
	}
	if pkt.Dur != int64(blocks) {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("ape: packet of %d blocks holds a %d-block frame", pkt.Dur, blocks))
	}
	// Every frame but the last is the header's own length, because that is the
	// only place a reader can learn it. A short frame arriving mid-stream
	// would produce a file whose block count and seek table disagree.
	if m.final != 0 && m.final != m.cfg.BlocksPerFrame {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("ape: a %d-block frame was followed by another; only the last frame may be short", m.final))
	}
	if blocks > m.cfg.BlocksPerFrame {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("ape: frame of %d blocks exceeds the stream's %d", blocks, m.cfg.BlocksPerFrame))
	}
	if len(m.seek) >= m.entries {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("ape: stream outgrew its %d-frame seek table (the projected length was short)", m.entries))
	}
	// The table's entries are 32-bit and a long file wraps them; the reader
	// recovers the high bits by watching for an entry that went backwards,
	// which frames written in order never do. The entry is where the frame's
	// first coded byte falls in the run, which is not where its word starts.
	m.seek = append(m.seek, uint32(m.frameDataOff+m.logical))
	if err := m.pack(data, skip, n); err != nil {
		return err
	}
	m.logical += int64(n)
	m.blocks += int64(blocks)
	m.final = blocks
	return nil
}

// pack lays a frame's coded bytes into the run, continuing whatever word the
// frame before it left half full.
//
// The bytes arrive in the file's own order (little-endian words taken most
// significant byte first) and leave in it, but not at the same offsets: a
// packet's frame sits wherever its source put it and this one goes wherever
// the run has reached. Reading a packet as logical bytes and writing it back
// as physical ones is what lets a frame from any source land here, which is
// what a remux of somebody else's .ape needs.
func (m *Muxer) pack(data []byte, skip, n int) error {
	buf := m.scratch[:0]
	i := 0
	if m.wordN == 0 && skip == 0 {
		// The common case, and the one the encoder always hits: the frame's
		// words already sit where they would go, so the whole words move
		// across untouched and only the tail goes through the packer.
		if whole := n &^ 3; whole > 0 {
			if err := m.writeHashed(data[:whole]); err != nil {
				return err
			}
			i = whole
		}
	}
	for ; i < n; i++ {
		// Logical byte j sits at physical j&^3+3-j&3, which reaches up to
		// three bytes past j: a frame whose last word is not all there runs
		// off the end of the packet. Reading zero there is what the decoder's
		// own reader does, so the frame is laid down as the frame that would
		// decode, rather than refused or read out of bounds.
		j := skip + i
		var b byte
		if p := j&^3 + 3 - j&3; p < len(data) {
			b = data[p]
		}
		m.word[3-m.wordN] = b
		m.wordN++
		if m.wordN == 4 {
			buf = append(buf, m.word[0], m.word[1], m.word[2], m.word[3])
			m.word, m.wordN = [4]byte{}, 0
			if len(buf) >= packChunk {
				if err := m.writeHashed(buf); err != nil {
					return err
				}
				buf = buf[:0]
			}
		}
	}
	m.scratch = buf[:0]
	if len(buf) == 0 {
		return nil
	}
	return m.writeHashed(buf)
}

// writeHashed writes frame-data bytes and folds them into the file MD5, which
// covers exactly the bytes the frame region holds.
func (m *Muxer) writeHashed(b []byte) error {
	if err := m.write(b); err != nil {
		return err
	}
	m.md5.Write(b)
	return nil
}

// End writes the trailing word, the tag block, and the totals: the seek table,
// the format header's frame counts, and the descriptor's byte counts and MD5.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "ape: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != 0 || trailer.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "ape: Monkey's Audio signals no gapless trims")
	}
	if trailer.Samples >= 0 && trailer.Samples != m.blocks {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("ape: trailer says %d samples, wrote %d", trailer.Samples, m.blocks))
	}
	// A .ape file states its length as (frames-1)*blocksPerFrame plus the last
	// frame's blocks, so a file with no frames can only claim a length it does
	// not have. Every reader refuses one, this package's own included.
	if len(m.seek) == 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			"ape: Monkey's Audio cannot hold a stream with no samples")
	}
	// One word behind the audio: the last frame's leftover bytes if it left
	// any, zeros if it did not. The reference writes the same word for the
	// same reason, which is the read-ahead its range coder wants on the last
	// frame.
	if err := m.writeHashed(m.word[:]); err != nil {
		return err
	}
	m.word, m.wordN = [4]byte{}, 0
	frameDataBytes := m.off - m.frameDataOff

	if tag := apev2.Build(muxTags(m.opts.Tags)); tag != nil {
		if len(tag) > maxTagBytes {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("ape: tag block of %d bytes exceeds the %d-byte bound", len(tag), maxTagBytes))
		}
		if err := m.write(tag); err != nil {
			return err
		}
	}

	header := m.formatHeader(len(m.seek), m.final)
	table := make([]byte, m.entries*4)
	for i, off := range m.seek {
		binary.LittleEndian.PutUint32(table[i*4:], off)
	}
	// The MD5's remaining halves, in the order the reference's verify
	// recomputes them: the format header, then the whole seek table including
	// the entries no frame claimed.
	m.md5.Write(header)
	m.md5.Write(table)

	if err := m.patch.At(0, m.descriptor(frameDataBytes), header, table); err != nil {
		return err
	}
	return m.patch.Resume(m.off)
}

// descriptor renders the 52-byte descriptor: the section lengths, the frame
// data's byte count, and the file MD5 as it stands. Begin writes it with
// nothing coded yet and End writes it again over the top.
func (m *Muxer) descriptor(frameDataBytes int64) []byte {
	var desc [descriptorLen]byte
	copy(desc[:4], "MAC ")
	binary.LittleEndian.PutUint16(desc[4:], uint16(m.cfg.FileVersion))
	// The program-version field names the Monkey's Audio build that wrote the
	// file. Zero is what the format's own older headers carry for "not
	// stated", which is the honest answer from an encoder that is not one.
	binary.LittleEndian.PutUint16(desc[6:], 0)
	binary.LittleEndian.PutUint32(desc[8:], descriptorLen)
	binary.LittleEndian.PutUint32(desc[12:], headerLen)
	binary.LittleEndian.PutUint32(desc[16:], uint32(m.entries*4))
	// No stored source header: the format flag says a decompressor should
	// synthesize one, which is the truth here (there was no WAV file). The
	// trailing-data count stays zero for the same reason.
	binary.LittleEndian.PutUint32(desc[20:], 0)
	binary.LittleEndian.PutUint32(desc[24:], uint32(frameDataBytes))
	binary.LittleEndian.PutUint32(desc[28:], uint32(frameDataBytes>>32))
	binary.LittleEndian.PutUint32(desc[32:], 0)
	// Sum appends without disturbing the hash, so this is the digest so far
	// rather than a finalization.
	m.md5.Sum(desc[36:36])
	return desc[:]
}

// muxTags converts the engine's tags to the writer's, dropping keys no reader
// could ask for.
func muxTags(tags []container.Tag) []apev2.Tag {
	out := make([]apev2.Tag, 0, len(tags))
	for _, t := range tags {
		if container.ValidTagKey(t.Key) {
			out = append(out, apev2.Tag{Key: t.Key, Value: t.Value})
		}
	}
	return out
}

func (m *Muxer) write(parts ...[]byte) error {
	for _, p := range parts {
		n, err := m.w.Write(p)
		m.off += int64(n)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "ape: write", err)
		}
	}
	return nil
}
