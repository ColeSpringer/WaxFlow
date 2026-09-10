package mp4

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer   = (*Demuxer)(nil)
	_ container.Seeker    = (*Demuxer)(nil)
	_ container.Warner    = (*Demuxer)(nil)
	_ container.Chapterer = (*Demuxer)(nil)
	_ container.Tagger    = (*Demuxer)(nil)
)

// DemuxerOptions configures parsing.
type DemuxerOptions struct {
	// Strict turns tolerated damage (the Warnings list) into errors.
	Strict bool
}

// Chapter is one parsed chapter marker, timed in the movie timeline. It
// aliases the container-level type so demuxer chapters feed muxer options
// (and the metadata mapper) without conversion.
type Chapter = container.Chapter

// Demuxer reads one audio track from an ISO base media file. It selects
// the sound track, exposes it as a single track (ID 0), and reads sample
// packets from mdat on demand.
type Demuxer struct {
	src  container.Source
	opts DemuxerOptions
	size int64

	track    container.Track
	sel      *track // the selected audio track's parsed detail
	brands   []string
	chapters []Chapter
	tags     map[string][]string // ilst tags, canonical keys
	warnings []container.Warning

	movieTimescale int64     // mvhd timescale (ticks per second)
	chplChapters   []Chapter // Nero chpl markers, if present

	// iTunes iTunSMPB gapless fields, in samples; valid only when smpbOK.
	smpbDelay int64
	smpbPad   int64
	smpbTotal int64
	smpbOK    bool

	// seekPreroll is how many samples before the target SeekSample lands
	// so the decoder's inter-frame state (AAC's IMDCT overlap) converges;
	// format.Media discards the difference for a sample-exact seek.
	seekPreroll int64
	// mp3Overhead is what one Layer III frame spends ahead of its main data,
	// for the bit-reservoir half of the backoff. Set only for an MP3 track.
	mp3Overhead int64

	cur int64 // next sample index ReadPacket delivers

	// Fragmented (CMAF) reading state, populated when the movie carries an
	// mvex box; the samples then live in moof+mdat fragments rather than the
	// (empty) moov sample table. See fragdemux.go.
	fragmented bool
	trex       trexDefaults
	fragStart  int64        // offset of the first top-level box after moov
	fragOff    int64        // next top-level box the fragment iterator reads
	fragQueue  []fragSample // the current fragment's samples
	fragIdx    int
	fragDecode int64 // running decode time (samples) for the next sample's PTS

	// w is the shared read-ahead window over mdat sample data.
	w srcwin.Window
}

// NewDemuxer parses the movie header and positions on the first sample.
// The returned Demuxer implements container.Seeker and container.Warner.
func NewDemuxer(src container.Source, opts *DemuxerOptions) (*Demuxer, error) {
	d := &Demuxer{src: src, size: src.Size(),
		w: srcwin.New(src, src.Size(), "mp4: reading sample data")}
	if opts != nil {
		d.opts = *opts
	}
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
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: msg, Kind: container.Damage})
	return nil
}

// note records a Warning that Strict must not escalate: a limitation of this
// decoder against a file that is well formed, rather than damage in the file.
//
// warn is for mess, and Strict turns mess into an error for conformance runs.
// An HE-AAC config is not mess; it is a conformant file whose high band this
// decoder does not synthesize. Routing that through warn would make
// `probe --strict` reject valid HE-AAC files as malformed, which is why this
// path exists and why it cannot fail.
func (d *Demuxer) note(off int64, format string, args ...any) {
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...), Kind: container.Note})
}

// parse scans the top-level boxes, reads moov into memory, builds the
// track tree, and selects the audio track.
func (d *Demuxer) parse() error {
	var moov []byte
	sawFtyp := false
	off := int64(0)
	for off < d.size {
		b, err := readBox(d.src, off, d.size)
		if err != nil {
			// A damaged top-level chain is unrecoverable: unlike a page
			// stream there is no resync, so stop where we are. If moov was
			// already found, use it; otherwise report the damage.
			if moov != nil {
				break
			}
			return err
		}
		switch b.typ {
		case "ftyp":
			sawFtyp = true
			d.readBrands(b)
		case "moov":
			if b.payloadLen() > maxMoovBytes {
				return malformed("moov box of %d bytes exceeds the %d cap", b.payloadLen(), int64(maxMoovBytes))
			}
			moov = make([]byte, b.payloadLen())
			if err := container.ReadFull(d.src, moov, b.payloadOff()); err != nil {
				return err
			}
			// Fragments (moof+mdat) of a fragmented movie follow moov; record
			// where the fragment iterator starts scanning.
			d.fragStart = b.off + b.size
		}
		if b.toEnd {
			break
		}
		off = b.off + b.size
	}
	if !sawFtyp {
		// Reached only through an extension hint, since Match requires an ftyp
		// to sniff at all. So the caller has said this is an MP4-family file
		// and the first box is not ftyp: a QuickTime .mov, which the format
		// does not require one of, or a damaged .m4a. Nothing here can tell
		// them apart, and calling it damage refuses every conformant .mov, so
		// this reports what the demuxer saw and leaves the judgement out.
		d.note(0, "no ftyp box")
	}
	if moov == nil {
		return malformed("no moov box")
	}

	tracks, err := d.parseMoov(moov)
	if err != nil {
		return err
	}
	return d.selectAudio(tracks)
}

// readBrands records the ftyp major and compatible brands, for diagnostics.
func (d *Demuxer) readBrands(b box) {
	buf := make([]byte, min(b.payloadLen(), 256))
	if container.ReadFull(d.src, buf, b.payloadOff()) != nil {
		return
	}
	for i := 0; i+4 <= len(buf); i += 4 {
		if i == 4 {
			continue // minor_version, not a brand
		}
		if brand := trimBrand(buf[i : i+4]); brand != "" {
			d.brands = append(d.brands, brand)
		}
	}
}

// selectAudio picks the FIRST sound track carrying a codec we decode, builds
// its container.Track, and resolves gapless trims and chapters.
//
// First rather than best: there is no codec preference, so which track a
// multi-audio movie delivers is the file's ordering, and every codec added to
// decodableAudio can therefore change the answer for a movie that carries it
// ahead of one already decoded. That is the policy rather than an oversight —
// a preference order would be this package inventing one — and a caller that
// needs a particular track selects it itself.
func (d *Demuxer) selectAudio(tracks []*track) error {
	var audio *track
	var foundCodecs []string
	var stsdErr error
	named := 0 // entries in foundCodecs that are a codec name, not "unknown"
	for _, t := range tracks {
		if t.handler != "soun" {
			continue
		}
		if t.codec == "" {
			// A sound track with no codec failed to parse its sample
			// description or carried none we could read; parseStbl deferred
			// the reason here rather than rejecting a file whose other audio
			// track is fine.
			if stsdErr == nil {
				stsdErr = t.stsdErr
			}
			foundCodecs = append(foundCodecs, "unknown")
			continue
		}
		if !decodableAudio(t.codec) {
			foundCodecs = append(foundCodecs, string(t.codec))
			named++
			continue
		}
		if audio == nil {
			audio = t
		}
	}
	if audio == nil {
		// The deferred reason is the specific one: it says why this file is
		// refused ("aac: audio object type 1 is not AAC-LC"), where "found:
		// unknown" only says that something in a box we already read did not
		// come out. It replaces the list when the list holds nothing else,
		// and rides alongside it when the file also carries a codec we can
		// name, since "there is also an mp3 track in here" is the actionable
		// half of that case.
		switch {
		case stsdErr != nil && named == 0:
			return stsdErr
		case stsdErr != nil:
			// The deferred reason keeps its own code: a stsd this build
			// declines (an object type it has no decoder for) is
			// unsupported, a truncated one is damage, and the wrap must not
			// flatten the two into one answer.
			return waxerr.Wrap(waxerr.CodeOf(stsdErr),
				fmt.Sprintf("mp4: no decodable audio track (found: %s)", joinNames(foundCodecs)), stsdErr)
		case len(foundCodecs) > 0:
			return unsupported("no decodable audio track (found: %s)", joinNames(foundCodecs))
		}
		return unsupported("no audio track")
	}
	d.sel = audio
	if audio.note != "" {
		d.note(0, "%s", audio.note)
	}

	if audio.codec == codec.MP3 {
		// Before Valid, so the format that gets validated is the one that
		// will be decoded with.
		if err := d.mp3AdoptFrameFormat(audio); err != nil {
			return err
		}
	}
	if err := audio.fmt.Valid(); err != nil {
		return container.UnusableFormat("mp4", audio.fmt, err)
	}
	if audio.codec == codec.AACLC {
		d.seekPreroll = 1024 // one frame of IMDCT overlap history
	}
	if audio.codec == codec.HEAAC {
		// IMDCT overlap plus the SBR QMF/adjuster history rebuild in two
		// AUs, but the PS layer needs its header and parameter refresh
		// too: fdk repeats them about every half second, so a cold seek
		// pre-rolls that far. The samples-domain constant covers every AU
		// shape the ID spans (2048 dual-rate, 1024 downsampled). (The SBR
		// noise and sinusoid PHASE free-runs from the stream head in every
		// decoder and is not recoverable by any preroll; the v2 seek gate
		// documents that.)
		d.seekPreroll = aac.HESeekPreroll
	}
	if audio.codec == codec.MP3 {
		// Layer III frames are not independently decodable, whatever the
		// sample table says by carrying no stss: the filterbank keeps IMDCT
		// overlap and synthesis-window history, and the bit reservoir lets a
		// frame's main data begin up to 511 BYTES before its own header. The
		// first is a fixed number of samples and lives here; the second is
		// not (six frames at 32 kbit/s, one at 320), so SeekSample extends
		// the landing off the sample table's own byte sizes, which is what
		// container/mpa does with its index.
		d.seekPreroll = mp3SeekPreroll
		d.mp3Overhead = mp3FrameOverhead(audio.fmt)
	}

	var delay, padding, samples int64
	var exact, advisory bool
	if d.fragmented {
		// The fragmented sample tables are empty; gapless comes from the init
		// edit list, and the length is authoritative (SamplesExact) when the
		// edit list carries a segment duration.
		delay, samples, exact = d.fragmentedGapless(audio)
		d.fragOff = d.fragStart
	} else {
		delay, padding, samples, advisory = d.gapless(audio)
	}
	d.track = container.Track{
		Codec:           audio.codec,
		CodecConfig:     audio.codecConfig,
		Fmt:             audio.fmt,
		Samples:         samples,
		Delay:           delay,
		Padding:         padding,
		SamplesExact:    exact,
		SamplesAdvisory: advisory,
		Default:         true,
	}
	d.resolveChapters(tracks, audio)
	return nil
}

// decodableAudio reports whether the demuxer decodes a codec: ALAC and AAC-LC
// (progressive) plus Opus and FLAC (their sample entries are read for the
// fragmented path, and their decoders are registered), and MP3, which an mp4a
// entry carries under object type 0x69/0x6B and QuickTime writes as its own
// '.mp3' fourcc.
func decodableAudio(id codec.ID) bool {
	switch id {
	case codec.ALAC, codec.AACLC, codec.HEAAC, codec.Opus, codec.FLAC, codec.MP3:
		return true
	}
	return false
}

// Tracks returns the single selected audio track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// Chapters returns parsed chapter markers in start order, nil when the file
// carries none. The slice is the demuxer's own and must not be mutated.
func (d *Demuxer) Chapters() []Chapter { return d.chapters }

// Tags returns the ilst tags, nil when the file carries none. The map is
// the demuxer's own and must not be mutated; format.Info hands it on to
// read-only consumers.
func (d *Demuxer) Tags() map[string][]string { return d.tags }

// Brands returns the ftyp brands, for diagnostics.
func (d *Demuxer) Brands() []string { return d.brands }

// ReadPacket yields the next sample as a codec packet. Packet data aliases
// the read window and is reused across calls.
func (d *Demuxer) ReadPacket(pkt *container.Packet) error {
	if d.fragmented {
		return d.readFragmentedPacket(pkt)
	}
	st := &d.sel.st
	if d.cur >= st.total {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return io.EOF
	}
	off := st.offsets[d.cur]
	size := int(st.sizes[d.cur])
	d.w.Trim(off)
	data := d.w.BytesAt(off, size)
	if len(data) != size {
		if d.w.Err() != nil {
			return d.w.Err()
		}
		return waxerr.New(waxerr.CodeSourceUnreadable,
			fmt.Sprintf("mp4: sample %d truncated (want %d bytes at %d)", d.cur, size, off))
	}
	pts, dur := st.timeOf(d.cur)
	*pkt = container.Packet{
		Track: 0,
		Packet: codec.Packet{
			Data: data,
			PTS:  pts,
			Dur:  dur,
			Sync: st.isSync(d.cur),
		},
	}
	d.cur++
	return nil
}

// SeekSample lands on a sync sample at or before the target in the raw
// decoder timeline, backed off by seekPreroll samples so the decoder's
// inter-frame state converges. format.Media pre-rolls the remainder for a
// sample-exact landing.
func (d *Demuxer) SeekSample(track int, sample int64) (int64, error) {
	if track != 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: no track %d", track))
	}
	if sample < 0 {
		return 0, waxerr.New(waxerr.CodeInvalidRequest, "mp4: negative seek target")
	}
	// Both paths back the target off by the preroll in the sample domain,
	// so the fragmented walk pre-rolls the same distance the progressive
	// one does.
	if d.seekPreroll > 0 {
		sample = max(sample-d.seekPreroll, 0)
	}
	if d.fragmented {
		return d.seekFragmented(sample)
	}
	st := &d.sel.st
	if st.total == 0 {
		return 0, nil
	}
	idx := st.sampleAt(sample)
	idx = st.syncAtOrBefore(idx)
	if d.sel.codec == codec.MP3 {
		// The bytes half of the backoff, which seekPreroll cannot express.
		// The fragmented path has no sample table to measure it on and gets
		// the samples half alone; nothing writes MP3 into a fragmented movie,
		// and a landing there is still sample-exact through the pre-roll
		// format.Media runs, just with a few frames of settling audible in
		// the discarded region rather than before it.
		idx = st.mp3Backoff(idx, d.mp3Overhead)
	}
	d.cur = idx
	pts, _ := st.timeOf(idx)
	return pts, nil
}

// mp3AdoptFrameFormat lets the first frame's own header decide the track's
// format, and warns when it disagrees with the sample entry.
//
// For MPEG audio the frame header is what the ASC is for AAC: the
// authoritative statement of rate and channel count, where the sample entry
// is a field that can be wrong. And unlike every other codec here, nothing
// else can check it — an mp4a MP3 track's esds carries no
// DecoderSpecificInfo and a '.mp3' entry carries nothing at all — so without
// this the entry is adopted unread, and a file that misstates either opens,
// probes plausibly, and then fails at the first packet, because codec/mp3
// refuses a frame that disagrees with the track it was built for. That is the
// right refusal in the wrong place: the file plays in every other decoder.
//
// A read that does not deliver is left alone rather than reported. This is a
// cross-check on a track that already parsed from boxes, so a source that
// cannot serve the first frame has a problem ReadPacket states better, and
// the entry standing is exactly the behaviour that shipped before.
func (d *Demuxer) mp3AdoptFrameFormat(t *track) error {
	if d.fragmented || t.st.total == 0 {
		return nil
	}
	off := t.st.offsets[0]
	if int(t.st.sizes[0]) < mp3.HeaderLen {
		return nil
	}
	d.w.Trim(off)
	b := d.w.BytesAt(off, mp3.HeaderLen)
	if len(b) != mp3.HeaderLen {
		return nil
	}
	h, err := mp3.ParseHeader(b)
	if err != nil {
		return nil
	}
	got := h.PCMFormat()
	if got == t.fmt {
		return nil
	}
	// warn rather than note: an entry that disagrees with the frames it
	// describes is a file deviating from its format, which is what riff's
	// block-align and channel-mask disagreements are treated as too, so
	// `--strict` refuses it and an ordinary read carries on with the truth.
	if werr := d.warn(off, "sample entry says %v, the first frame says %v; the frame wins", t.fmt, got); werr != nil {
		return werr
	}
	t.fmt = got
	return nil
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// trimBrand renders a 4-byte brand, trimming trailing spaces and NULs, or
// "" when it holds no printable content.
func trimBrand(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == ' ' || b[end-1] == 0) {
		end--
	}
	for _, c := range b[:end] {
		if c < 0x20 || c > 0x7E {
			return ""
		}
	}
	return string(b[:end])
}
