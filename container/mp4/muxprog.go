package mp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

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
//
// Chapters, when the options carry any, are written twice: as a QuickTime text
// track beside the audio (unbounded, the form readers prefer) and as the Nero
// chpl list in the udta (capped at 255, for the readers that know only that).
// The text track is the muxer's own, synthesized here rather than accepted as
// an input track, which is why the single-track contract still holds.
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

	var rawTotal int64
	for _, d := range m.durs {
		rawTotal += int64(d)
	}
	// The edit list carries the gapless trims; the exact length is known now, so
	// no back-patch is needed (unlike the fragmented muxer). It is written only
	// when there is something to trim (an encoder delay or trailing padding).
	// It is resolved here, ahead of the mdat, because the chapter track below is
	// measured against the presentation it defines.
	var edts []byte
	movieDur := rawTotal
	if trailer.Delay > 0 || trailer.Padding > 0 {
		edts = elstBox(trailer.Delay, max(trailer.Samples, 0))
		movieDur = max(trailer.Samples, 0)
	}
	// The chapter text track's samples share the audio mdat, appended after the
	// last audio sample and ahead of the size patch below, so the movie keeps
	// one mdat and one back-patch. Its chunk offset is then simply where the
	// audio payload ended. The write position is still the end of the audio
	// here, since every write so far has been sequential.
	//
	// It is built against movieDur rather than rawTotal because the audio edit
	// list trims the presentation to it: the chapter track's own edit list does
	// not trim its tail, so its media time is presentation time, and measuring
	// it against the raw sample total would run the last chapter past the end of
	// the movie by exactly the delay plus the padding.
	chap := buildChapterTrack(m.opts.Chapters, movieDur, m.rate, m.mdatStart+m.dataBytes)
	var textBytes int64
	if chap != nil {
		if err := m.write(chap.data); err != nil {
			return err
		}
		textBytes = int64(len(chap.data))
	}

	// Back-patch the mdat largesize (the 16-byte header plus the payload).
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(16+m.dataBytes+textBytes))
	if _, err := m.ws.Seek(m.mdatSizeOff, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: mdat size seek", err)
	}
	if _, err := m.ws.Write(size[:]); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: mdat size patch", err)
	}
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: seek to end", err)
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
	moov := progMoovBox(m.rate, m.entry, edts, udta, m.durs, m.sizes, m.mdatStart, movieDur, rawTotal, chap)
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
// sample tables, an optional text chapter track beside it, plus an optional
// user-data box for metadata. Movie and audio media timescales are the sample
// rate, so decode times are sample counts; the chapter track keeps its own.
func progMoovBox(rate int, entry, edts, udta []byte, durs, sizes []uint32, chunkOff, movieDur, mediaDur int64, chap *chapterTrack) []byte {
	// next_track_ID must clear every track_ID in the movie, so it moves up
	// with the chapter track when there is one.
	nextTrack := uint32(trackID + 1)
	if chap != nil {
		nextTrack = chapterTrackID + 1
	}
	mvhdVer := durVersion(movieDur)
	mvhd := makeFullBox("mvhd", mvhdVer, 0,
		zeroTimes(mvhdVer), // creation, modification
		u32(uint32(rate)),
		durField(mvhdVer, movieDur),
		u32(0x00010000), u16(0x0100), u16(0), u32(0), u32(0),
		identityMatrix(),
		make([]byte, 24),
		u32(nextTrack))
	parts := [][]byte{mvhd, progTrakBox(rate, entry, edts, durs, sizes, chunkOff, movieDur, mediaDur, chap != nil)}
	if chap != nil {
		parts = append(parts, chap.trakBox(rate, movieDur))
	}
	if udta != nil {
		parts = append(parts, udta)
	}
	return makeBox("moov", parts...)
}

func progTrakBox(rate int, entry, edts []byte, durs, sizes []uint32, chunkOff, movieDur, mediaDur int64, hasChapters bool) []byte {
	tkhdVer := durVersion(movieDur)
	tkhd := makeFullBox("tkhd", tkhdVer, 0x000007,
		zeroTimes(tkhdVer),
		u32(trackID),
		u32(0),
		durField(tkhdVer, movieDur),
		u32(0), u32(0),
		u16(0), u16(0),
		u16(0x0100), u16(0),
		identityMatrix(),
		u32(0), u32(0))
	mdia := progMdiaBox(rate, entry, durs, sizes, chunkOff, mediaDur)
	parts := [][]byte{tkhd}
	if edts != nil {
		parts = append(parts, edts)
	}
	if hasChapters {
		// The 'chap' reference is what binds the text track to this one, and is
		// how chapterTrack finds it again on the way back in.
		parts = append(parts, makeBox("tref", makeBox("chap", u32(chapterTrackID))))
	}
	return makeBox("trak", append(parts, mdia)...)
}

func progMdiaBox(rate int, entry []byte, durs, sizes []uint32, chunkOff, mediaDur int64) []byte {
	mdhdVer := durVersion(mediaDur)
	mdhd := makeFullBox("mdhd", mdhdVer, 0,
		zeroTimes(mdhdVer),
		u32(uint32(rate)),
		durField(mdhdVer, mediaDur),
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

	// The single chunk's offset, 32-bit stco where it fits and co64 otherwise.
	// The audio chunk sits near the file head (after ftyp and the mdat header)
	// and always fits, but the chapter chunk trails the whole audio payload,
	// which a long audiobook pushes past 4 GiB. parseStco reads both widths.
	stco := makeFullBox("stco", 0, 0, u32(1), u32(uint32(chunkOff)))
	if chunkOff > math.MaxUint32 {
		stco = makeFullBox("co64", 0, 0, u32(1), u64(uint64(chunkOff)))
	}

	return makeBox("stbl", stsd, stts, stsc, stsz, stco)
}

// The QuickTime text chapter track. Only the progressive muxer synthesizes
// one: the fragmented muxer writes its moov at Begin, before a single chapter
// sample exists to point at, and demux.go resolves chapters for progressive
// files only. The track is built from MuxerOptions.Chapters rather than from
// an input track, since container.Muxer is single-track by design and a
// chapter track is not audio anyone handed us.

const (
	// chapterTrackID is the text track's track_ID. The audio track is
	// trackID, so the chapter track takes the next one.
	chapterTrackID = trackID + 1

	// chapterTimescale is the text track's own media timescale. A chapter
	// track is not audio and carries no sample rate, so it picks its own time
	// base; 1000 ticks per second is the conventional choice and resolves
	// chapters far finer than anyone marks them.
	chapterTimescale = 1000

	// maxChapterTitleBytes caps a title at what the sample format spells: a
	// 16-bit length prefix, less the prefix's own two bytes, so the whole
	// sample still fits the 1<<16 bound readTextChapters enforces.
	maxChapterTitleBytes = 1<<16 - 2
)

// chapterTrack is a synthesized text chapter track: the sample bytes bound for
// the mdat plus the tables that time them.
type chapterTrack struct {
	data     []byte   // every sample, concatenated
	durs     []uint32 // per-sample duration in chapterTimescale ticks
	sizes    []uint32 // per-sample byte size
	chunkOff int64    // where data lands in the file
	mediaDur int64    // sum of durs

	// startTicks is the first chapter's start, in chapterTimescale ticks. It
	// is the one time in the track that no sample duration can express, since
	// stts spells deltas from zero; the empty edit in edtsBox carries it.
	startTicks int64
}

// buildChapterTrack renders chapters as QuickTime text samples: a 16-bit
// big-endian title length then that many UTF-8 bytes, the exact inverse of
// readTextChapters. Returns nil when there are no chapters.
//
// Every chapter is written, however many there are, which is the whole reason
// resolveChapters prefers this track: the Nero chpl list beside it in the udta
// stops at the 255 its one-byte count can spell. A caller that wants that
// truncation surfaced must report it itself; a muxer has no warning channel
// (container.Warner is demuxer-side only).
//
// Timing chains: each sample runs until the next chapter starts, and the last
// runs to Chapter.End, or to the end of the presentation when End is zero. A
// reader recovers each start by accumulating those durations from zero, so the
// deltas hold every start but the first: that one is the track's anchor, and
// edtsBox writes it as an empty edit. Dropping it shifts every chapter in the
// file earlier by it.
//
// movieSamples and rate are the presentation length (what the audio track's
// edit list plays, not its raw sample total) and its sample rate, together the
// end of the movie. Nothing here may present past that, so both a chapter's
// start and the caller's Chapter.End clamp to it. chunkOff is where the samples
// land in the file.
func buildChapterTrack(chapters []container.Chapter, movieSamples int64, rate int, chunkOff int64) *chapterTrack {
	if len(chapters) == 0 {
		return nil
	}
	movieEnd := mulDivSat(movieSamples, chapterTimescale, int64(rate))
	starts := make([]int64, len(chapters))
	for i, ch := range chapters {
		starts[i] = min(mulDivSat(int64(ch.Start), chapterTimescale, int64(time.Second)), movieEnd)
	}
	// Chapter.End is the caller's, and a caller has no reason to know the
	// length the encoder settled on, so it is a request rather than a bound.
	end := movieEnd
	if last := chapters[len(chapters)-1]; last.End > 0 {
		end = min(mulDivSat(int64(last.End), chapterTimescale, int64(time.Second)), movieEnd)
	}

	c := &chapterTrack{chunkOff: chunkOff, startTicks: starts[0]}
	for i, ch := range chapters {
		next := end
		if i+1 < len(chapters) {
			next = starts[i+1]
		}
		// Every sample must advance the timeline. A zero stts delta stalls the
		// reader, which floors it to one tick regardless, so writing one here
		// keeps the table legal and the times monotonic rather than letting an
		// unordered or sub-tick chapter list read back disagreeing with itself.
		dur := min(max(next-starts[i], 1), math.MaxUint32)
		title := truncateRunes(ch.Title, maxChapterTitleBytes)
		c.data = append(c.data, u16(uint16(len(title)))...)
		c.data = append(c.data, title...)
		c.durs = append(c.durs, uint32(dur))
		c.sizes = append(c.sizes, uint32(2+len(title)))
		c.mediaDur += dur
	}
	return c
}

// trakBox renders the chapter track's trak. movieRate is the movie timescale
// (the progressive mvhd uses the audio sample rate) and movieDur the movie's
// length in it; the tkhd duration and the edit list are spelled in that scale,
// while everything under mdia uses chapterTimescale instead.
func (c *chapterTrack) trakBox(movieRate int, movieDur int64) []byte {
	empty, played := c.editDurs(movieRate, movieDur)
	// Flag 0x2 is track_in_movie without track_enabled: a chapter track is
	// read for its titles, never played, and an enabled text track renders as
	// burnt-in subtitles.
	// The track header states its duration on the movie's timeline, not the
	// chapter track's own, so it is the movie rate that decides the width:
	// this overflows 32 bits at the same 25-odd hours the audio track does,
	// not at the 49.7 days a millisecond tick would suggest.
	dur := empty + played
	tkhdVer := durVersion(dur)
	tkhd := makeFullBox("tkhd", tkhdVer, 0x000002,
		zeroTimes(tkhdVer), // creation, modification
		u32(chapterTrackID),
		u32(0),
		durField(tkhdVer, dur),
		u32(0), u32(0),
		u16(0), u16(0), // layer, alternate_group
		u16(0), u16(0), // volume: zero, the track carries no sound
		identityMatrix(),
		u32(0), u32(0))
	mdia := makeBox("mdia", c.mdhdBox(), chapterHdlrBox(), c.minfBox())
	if edts := c.edtsBox(empty, played); edts != nil {
		return makeBox("trak", tkhd, edts, mdia)
	}
	return makeBox("trak", tkhd, mdia)
}

// editDurs splits the chapter track's presentation into its two edit segment
// durations, in movie ticks: the leading empty edit holding the first chapter's
// start, and the edit playing the samples.
//
// Their sum is the track's presentation length, and both clamp so it cannot
// exceed the movie's own. A trak outlasting its mvhd is a file disagreeing with
// itself, and neither a rescale that rounded up nor a chapter list reaching past
// the end of the audio gets to cause that.
func (c *chapterTrack) editDurs(movieRate int, movieDur int64) (empty, played int64) {
	empty = min(mulDivSat(c.startTicks, int64(movieRate), chapterTimescale), movieDur)
	played = min(mulDivSat(c.mediaDur, int64(movieRate), chapterTimescale), movieDur-empty)
	return empty, played
}

// edtsBox delays the chapter track's presentation by an empty edit, which is
// the only thing that can carry the first chapter's start: stts spells deltas
// from zero, so a track without this reads back anchored at the movie's start
// and every chapter in it lands early by that first start.
//
// The empty edit (media_time -1) inserts blank presentation time consuming no
// media; the entry after it plays the samples from the top of their timeline.
// parseElst reads both back, and readTextChapters adds the blank time to every
// chapter it recovers.
//
// A first chapter at zero needs no delay and gets no edit list, which is the
// common case and keeps those files byte-identical to a muxer that never knew
// about empty edits.
func (c *chapterTrack) edtsBox(empty, played int64) []byte {
	if empty <= 0 {
		return nil
	}
	elst := makeFullBox("elst", 1, 0,
		u32(2),                                  // entry_count
		u64(uint64(empty)), u64(math.MaxUint64), // media_time -1: blank time
		u16(1), u16(0), // media_rate_integer, media_rate_fraction
		u64(uint64(played)), u64(0), // then the samples, from their start
		u16(1), u16(0))
	return makeBox("edts", elst)
}

// mdhdBox states the chapter track's duration on its own timeline, which
// ticks in milliseconds. That is the one duration here with genuine room
// (2^32 ms is 49.7 days), so the version is all but always 0; it is chosen
// rather than assumed so that this box and the track header above cannot
// disagree about how a duration is written.
func (c *chapterTrack) mdhdBox() []byte {
	ver := durVersion(c.mediaDur)
	return makeFullBox("mdhd", ver, 0,
		zeroTimes(ver), // creation, modification
		u32(chapterTimescale),
		durField(ver, c.mediaDur),
		u16(0x55C4), u16(0)) // language 'und'
}

// chapterHdlrBox declares the media a text handler, not a sound one: the
// demuxer matches a chapter track on this handler, and it is what tells every
// other reader that this trak is not audio to play.
func chapterHdlrBox() []byte {
	return makeFullBox("hdlr", 0, 0,
		u32(0),
		[]byte("text"),
		u32(0), u32(0), u32(0),
		append([]byte("SubtitleHandler"), 0))
}

func (c *chapterTrack) minfBox() []byte {
	// gmhd is QuickTime's base media header, the neither-audio-nor-video
	// counterpart of smhd that a text track takes instead: gmin's neutral
	// graphics mode and opcolor, then the text media's identity matrix.
	gmin := makeFullBox("gmin", 0, 0,
		u16(0x0040),                           // graphics mode: copy
		u16(0x8000), u16(0x8000), u16(0x8000), // opcolor
		u16(0), u16(0)) // balance, reserved
	gmhd := makeBox("gmhd", gmin, makeBox("text", identityMatrix()))
	dref := makeFullBox("dref", 0, 0, u32(1), makeFullBox("url ", 0, 0x000001))
	dinf := makeBox("dinf", dref)
	stbl := progStblBox(textSampleEntry(), c.durs, c.sizes, c.chunkOff)
	return makeBox("minf", gmhd, dinf, stbl)
}

// textSampleEntry builds the QuickTime 'text' sample entry. A chapter track is
// never drawn, so every display field is written at its neutral value and the
// font table is left empty.
//
// The bytes an AudioSampleEntry would spell samplerate (24..28) must stay
// zero, which the default text box covers: parseStsd reads the first entry of
// every stbl through parseAudioSampleEntry whatever the handler, and a nonzero
// rate there would leave t.fmt.Rate set and send parseStbl rescaling the
// chapter stts off its own timescale.
func textSampleEntry() []byte {
	return makeBox("text",
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u32(0),                 // displayFlags
		u32(0),                 // textJustification
		u16(0), u16(0), u16(0), // background color
		u16(0), u16(0), u16(0), u16(0), // default text box: top, left, bottom, right
		u64(0),         // reserved (scrpStartChar, scrpHeight, scrpAscent, scrpFont)
		u16(0), u16(0), // fontNumber, fontFace
		[]byte{0},              // reserved
		u16(0),                 // reserved
		u16(0), u16(0), u16(0), // foreground color
		[]byte{0}) // text name: an empty Pascal string
}
