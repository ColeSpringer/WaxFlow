package mp4

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*ProgressiveMuxer)(nil)

// ProgressiveMuxer writes one audio track as a flat (non-fragmented) MP4: an
// ftyp, an mdat holding every sample, then a moov whose stbl carries the full
// sample tables (stsd/stts/stsc/stsz/stco). This is the .m4a form most players
// and editors expect, the read-side counterpart of the progressive demuxer, and
// the other direction of the fragmented muxer's symmetry.
//
// It needs a seekable destination (NeedsSeek true): the mdat is written first
// with a placeholder size and streamed, then the size is back-patched and the
// moov appended once every sample's size and duration is known. Only the
// per-sample metadata is buffered, not the audio, so memory stays bounded by
// the sample count.
//
// It carries the same codecs as the segmenter (Opus, FLAC, AAC-LC, ALAC),
// reusing seg.go's sample-entry builders so read and write share box
// construction; gapless rides in the moov edit list, which the demuxer's
// parseElst reads back.
type ProgressiveMuxer struct {
	w    io.Writer
	ws   io.WriteSeeker
	opts MuxerOptions

	track container.Track
	rate  int
	entry []byte
	began bool
	ended bool

	off         int64 // bytes written
	mdatSizeOff int64 // file offset of the mdat 64-bit largesize field
	mdatStart   int64 // file offset of the mdat payload (the single chunk offset)

	durs, sizes []uint32
	dataBytes   int64
}

// NewProgressiveMuxer returns a progressive MP4 muxer writing to w, which must
// be an io.WriteSeeker (NeedsSeek is true).
func NewProgressiveMuxer(w io.Writer, opts *MuxerOptions) *ProgressiveMuxer {
	m := &ProgressiveMuxer{w: w, mdatSizeOff: -1}
	if ws, ok := w.(io.WriteSeeker); ok {
		m.ws = ws
	}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek reports true: the mdat size is back-patched and the moov is written
// after the samples.
func (m *ProgressiveMuxer) NeedsSeek() bool { return true }

// Begin validates the track, writes the ftyp, and opens the mdat.
func (m *ProgressiveMuxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "mp4: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: muxers are single-track, got %d", len(tracks)))
	}
	if m.ws == nil {
		return waxerr.New(waxerr.CodeInvalidRequest, "mp4: progressive output requires a seekable destination")
	}
	t := tracks[0]
	if err := t.Fmt.Valid(); err != nil {
		return err
	}
	if t.Delay < 0 || t.Padding < 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "mp4: negative gapless trims")
	}
	entry, err := sampleEntryFor(t) // validates the codec config against t.Fmt
	if err != nil {
		return err
	}
	m.track = t
	m.rate = t.Fmt.Rate
	m.entry = entry
	m.began = true

	if err := m.write(progFtypBox()); err != nil {
		return err
	}
	// Open the mdat with a 64-bit largesize placeholder (size=1 selects the
	// largesize form), back-patched in End. The payload begins after the
	// 16-byte header, and that offset is the single chunk's stco entry.
	mdatHeader := append(u32(1), []byte("mdat")...)
	mdatHeader = append(mdatHeader, u64(0)...) // largesize placeholder
	m.mdatSizeOff = m.off + 8
	m.mdatStart = m.off + 16
	return m.write(mdatHeader)
}

// WritePacket streams one sample into the mdat and records its size and
// duration.
func (m *ProgressiveMuxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: no track %d", pkt.Track))
	}
	if len(pkt.Data) == 0 || pkt.Dur <= 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet of %d bytes, %d samples", len(pkt.Data), pkt.Dur))
	}
	if len(pkt.Data) > maxSampleBytes || pkt.Dur > maxSampleDur {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet too large (%d bytes, %d samples)", len(pkt.Data), pkt.Dur))
	}
	if int64(len(m.durs)) >= maxSamples {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: more than %d samples", int64(maxSamples)))
	}
	if err := m.write(pkt.Data); err != nil {
		return err
	}
	m.durs = append(m.durs, uint32(pkt.Dur))
	m.sizes = append(m.sizes, uint32(len(pkt.Data)))
	m.dataBytes += int64(len(pkt.Data))
	return nil
}

// End back-patches the mdat size and writes the moov with the full sample
// tables and the gapless edit list.
func (m *ProgressiveMuxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: End outside Begin")
	}
	m.ended = true

	// Back-patch the mdat largesize (the 16-byte header plus the payload).
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(16+m.dataBytes))
	if _, err := m.ws.Seek(m.mdatSizeOff, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: mdat size seek", err)
	}
	if _, err := m.ws.Write(size[:]); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: mdat size patch", err)
	}
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: seek to end", err)
	}

	var rawTotal int64
	for _, d := range m.durs {
		rawTotal += int64(d)
	}
	// The edit list carries the gapless trims; the exact length is known now, so
	// no back-patch is needed (unlike the fragmented muxer). It is written only
	// when there is something to trim (an encoder delay or trailing padding).
	var edts []byte
	movieDur := rawTotal
	if trailer.Delay > 0 || trailer.Padding > 0 {
		edts = elstBox(trailer.Delay, max(trailer.Samples, 0))
		movieDur = max(trailer.Samples, 0)
	}
	// Metadata rides in the moov user-data box, built at End with the exact
	// values (the whole file is buffered as metadata anyway). AAC also gets the
	// iTunes iTunSMPB gapless atom, written with the trailer's exact delay and
	// length (no placeholder or back-patch, since moov is written last).
	var smpb []byte
	if m.track.Codec == codec.AACLC && trailer.Delay > 0 {
		smpb = freeformAtom("iTunSMPB", smpbPayload(trailer.Delay, max(trailer.Samples, 0)))
	}
	udta := udtaBox(m.opts.Tags, m.opts.Chapters, m.opts.Art, smpb)
	moov := progMoovBox(m.rate, m.entry, edts, udta, m.durs, m.sizes, m.mdatStart, movieDur, rawTotal)
	return m.write(moov)
}

func (m *ProgressiveMuxer) write(b []byte) error {
	if _, err := m.w.Write(b); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: write", err)
	}
	m.off += int64(len(b))
	return nil
}

// progFtypBox declares a flat (non-fragmented) M4A file: the M4A audio brand
// plus the ISO base brands, without the iso5 fragmentation brand.
func progFtypBox() []byte {
	return makeBox("ftyp",
		[]byte("M4A "), u32(0),
		[]byte("M4A "), []byte("mp42"), []byte("isom"), []byte("iso2"))
}

// progMoovBox builds a flat movie: one audio track whose stbl carries the full
// sample tables, plus an optional user-data box for metadata. Movie and media
// timescales are the sample rate, so decode times are sample counts.
func progMoovBox(rate int, entry, edts, udta []byte, durs, sizes []uint32, chunkOff, movieDur, mediaDur int64) []byte {
	mvhd := makeFullBox("mvhd", 0, 0,
		u32(0), u32(0), // creation, modification
		u32(uint32(rate)),
		u32(uint32(movieDur)),
		u32(0x00010000), u16(0x0100), u16(0), u32(0), u32(0),
		identityMatrix(),
		make([]byte, 24),
		u32(trackID+1))
	trak := progTrakBox(rate, entry, edts, durs, sizes, chunkOff, movieDur, mediaDur)
	if udta != nil {
		return makeBox("moov", mvhd, trak, udta)
	}
	return makeBox("moov", mvhd, trak)
}

func progTrakBox(rate int, entry, edts []byte, durs, sizes []uint32, chunkOff, movieDur, mediaDur int64) []byte {
	tkhd := makeFullBox("tkhd", 0, 0x000007,
		u32(0), u32(0),
		u32(trackID),
		u32(0),
		u32(uint32(movieDur)),
		u32(0), u32(0),
		u16(0), u16(0),
		u16(0x0100), u16(0),
		identityMatrix(),
		u32(0), u32(0))
	mdia := progMdiaBox(rate, entry, durs, sizes, chunkOff, mediaDur)
	if edts != nil {
		return makeBox("trak", tkhd, edts, mdia)
	}
	return makeBox("trak", tkhd, mdia)
}

func progMdiaBox(rate int, entry []byte, durs, sizes []uint32, chunkOff, mediaDur int64) []byte {
	mdhd := makeFullBox("mdhd", 0, 0,
		u32(0), u32(0),
		u32(uint32(rate)),
		u32(uint32(mediaDur)),
		u16(0x55C4), u16(0)) // language 'und'
	hdlr := makeFullBox("hdlr", 0, 0,
		u32(0),
		[]byte("soun"),
		u32(0), u32(0), u32(0),
		append([]byte("SoundHandler"), 0))
	minf := progMinfBox(entry, durs, sizes, chunkOff)
	return makeBox("mdia", mdhd, hdlr, minf)
}

func progMinfBox(entry []byte, durs, sizes []uint32, chunkOff int64) []byte {
	smhd := makeFullBox("smhd", 0, 0, u16(0), u16(0))
	dref := makeFullBox("dref", 0, 0, u32(1), makeFullBox("url ", 0, 0x000001))
	dinf := makeBox("dinf", dref)
	stbl := progStblBox(entry, durs, sizes, chunkOff)
	return makeBox("minf", smhd, dinf, stbl)
}

// progStblBox builds the full sample table: the sample description, a
// run-length-encoded time-to-sample, a single sample-to-chunk run (every
// sample in one chunk), the per-sample sizes, and the chunk offset. Every
// audio sample is a sync point, so no stss is written (its absence means
// all-sync).
func progStblBox(entry []byte, durs, sizes []uint32, chunkOff int64) []byte {
	n := uint32(len(durs))
	stsd := makeFullBox("stsd", 0, 0, u32(1), entry)

	// stts: run-length encode equal durations (constant-rate audio is one run).
	sttsBody := u32(0) // entry_count, patched below
	entries := uint32(0)
	for i := 0; i < len(durs); {
		j := i + 1
		for j < len(durs) && durs[j] == durs[i] {
			j++
		}
		sttsBody = append(sttsBody, u32(uint32(j-i))...)
		sttsBody = append(sttsBody, u32(durs[i])...)
		entries++
		i = j
	}
	binary.BigEndian.PutUint32(sttsBody, entries)
	stts := makeFullBox("stts", 0, 0, sttsBody)

	// stsc: one run, all n samples in chunk 1, sample description 1.
	stsc := makeFullBox("stsc", 0, 0, u32(1), u32(1), u32(n), u32(1))

	// stsz: per-sample sizes (sample_size 0 selects the table form).
	stszBody := append(u32(0), u32(n)...)
	for _, s := range sizes {
		stszBody = append(stszBody, u32(s)...)
	}
	stsz := makeFullBox("stsz", 0, 0, stszBody)

	// stco: the single chunk's offset. It sits near the file head (after ftyp
	// and the mdat header), so it always fits 32 bits.
	stco := makeFullBox("stco", 0, 0, u32(1), u32(uint32(chunkOff)))

	return makeBox("stbl", stsd, stts, stsc, stsz, stco)
}
