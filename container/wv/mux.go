package wv

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/wavpack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
	"github.com/colespringer/waxflow/internal/muxseek"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// MuxerOptions configures writing.
type MuxerOptions struct {
	// Tags are written as an APEv2 block after the audio, which is where a
	// .wv file conventionally carries them and where the demuxer reads them.
	Tags []container.Tag
}

// Muxer writes one WavPack track as a native .wv stream. NeedsSeek reports
// false: a .wv file is the run of self-delimiting blocks the encoder already
// produced, so a plain io.Writer receives a complete stream.
//
// The one field the blocks cannot fill in for themselves is the stream length,
// which no encoder knows while it is still encoding. Every block carries it,
// but only the first one is ever read (ffmpeg's muxer patches that one too), so
// this writes the engine's projection into the first block on the way past and
// corrects it at End when the destination can seek. A plain writer that
// promised a length it did not deliver is an error rather than a quiet lie.
type Muxer struct {
	w     io.Writer
	patch muxseek.Patcher
	opts  MuxerOptions

	promised int64  // the total written into the first block, -1 for the escape
	first    []byte // the first block as written, for the length patch
	off      int64  // bytes written so far
	samples  int64
	blocks   int

	began, ended bool
}

// NewMuxer returns a WavPack muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, promised: -1}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek reports false: native WavPack has a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track. WavPack has no pre-audio header of its own, so
// nothing is written here: the first block is the start of the file.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "wavpack: Begin called twice")
	}
	m.patch = muxseek.New(m.w, "wavpack")
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("wavpack: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.WavPack {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("wavpack: cannot mux codec %q", t.Codec))
	}
	if t.Delay != 0 || t.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			"wavpack: WavPack signals no gapless trims (lossless streams have none)")
	}
	cfg, err := wavpack.ParseConfig(t.CodecConfig)
	if err != nil {
		return err
	}
	if want := cfg.Format(); t.Fmt != want {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("wavpack: track format %v does not match its codec config (want %v)", t.Fmt, want))
	}
	// The header's field caps what it can state; a longer stream keeps the
	// escape, which every reader must handle anyway.
	if t.Samples >= 0 && t.Samples <= wavpack.MaxSamples {
		m.promised = t.Samples
	}
	m.began = true
	return nil
}

// WritePacket appends one whole block. The first block is rewritten with the
// promised length on the way past, since that is the copy readers consult, and
// kept, because End restates the same field over the whole block again.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "wavpack: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("wavpack: no track %d", pkt.Track))
	}
	h, err := wavpack.ParseBlockHeader(pkt.Data)
	if err != nil {
		return err
	}
	if h.Size != int64(len(pkt.Data)) {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("wavpack: packet of %d bytes declares a %d-byte block", len(pkt.Data), h.Size))
	}
	if pkt.Dur != int64(h.BlockSamples) {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("wavpack: packet of %d samples holds a %d-sample block", pkt.Dur, h.BlockSamples))
	}
	if m.blocks == 0 {
		m.first = append(m.first[:0], pkt.Data...)
		if err := wavpack.SetTotalSamples(m.first, m.promised); err != nil {
			return err
		}
		if err := m.write(m.first); err != nil {
			return err
		}
	} else if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.blocks++
	m.samples += pkt.Dur
	return nil
}

// End appends the tag block and corrects the length the first block promised.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "wavpack: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != 0 || trailer.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "wavpack: WavPack signals no gapless trims")
	}
	if trailer.Samples >= 0 && trailer.Samples != m.samples {
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("wavpack: trailer says %d samples, wrote %d", trailer.Samples, m.samples))
	}
	// A WavPack file is its blocks: there is no header to stand alone, and a
	// stream with no samples has no block to put the format in. Every reader
	// refuses the empty file that would leave behind (the reference encoder
	// refuses to write one at all), so refusing here is the honest answer
	// rather than reporting success over nothing.
	if m.blocks == 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			"wavpack: WavPack cannot hold a stream with no samples")
	}
	if tag := apev2.Build(muxTags(m.opts.Tags)); tag != nil {
		if err := m.write(tag); err != nil {
			return err
		}
	}
	if m.promised == m.samples {
		return nil
	}
	if !m.patch.Seekable() {
		if m.promised < 0 {
			// No length was promised, so the escape the blocks carry is
			// already the truth: a live stream states no length.
			return nil
		}
		return waxerr.New(waxerr.CodeInternal,
			fmt.Sprintf("wavpack: header promised %d samples, wrote %d (unseekable output)", m.promised, m.samples))
	}
	// The whole block goes back out rather than the length field alone: the
	// block checksum covers that field and lands at the far end of the block,
	// so the two rewrites bracket everything between them anyway.
	if err := wavpack.SetTotalSamples(m.first, m.samples); err != nil {
		return err
	}
	if err := m.patch.At(0, m.first); err != nil {
		return err
	}
	return m.patch.Resume(m.off)
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
			return waxerr.Wrap(waxerr.CodeOutputUnwritable, "wavpack: write", err)
		}
	}
	return nil
}
