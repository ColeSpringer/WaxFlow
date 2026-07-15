package mp4

import (
	"fmt"
	"io"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/srcwin"
	"github.com/colespringer/waxflow/waxerr"
)

var (
	_ container.Demuxer   = (*Demuxer)(nil)
	_ container.Seeker    = (*Demuxer)(nil)
	_ container.Warner    = (*Demuxer)(nil)
	_ container.Chapterer = (*Demuxer)(nil)
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
	warnings []container.Warning

	movieTimescale int64     // mvhd timescale (ticks per second)
	chplChapters   []Chapter // Nero chpl markers, if present

	// iTunes iTunSMPB gapless fields, in samples; valid only when smpbOK.
	smpbDelay int64
	smpbTotal int64
	smpbOK    bool

	// seekPreroll is how many samples before the target SeekSample lands
	// so the decoder's inter-frame state (AAC's IMDCT overlap) converges;
	// format.Media discards the difference for a sample-exact seek.
	seekPreroll int64

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
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: msg})
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
	d.warnings = append(d.warnings, container.Warning{Offset: off, Msg: fmt.Sprintf(format, args...)})
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
		if err := d.warn(0, "no ftyp box"); err != nil {
			return err
		}
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

// selectAudio picks the sound track carrying a codec we decode, builds its
// container.Track, and resolves gapless trims and chapters.
func (d *Demuxer) selectAudio(tracks []*track) error {
	var audio *track
	var foundCodecs []string
	for _, t := range tracks {
		if t.handler != "soun" {
			continue
		}
		if t.codec == "" {
			foundCodecs = append(foundCodecs, "unknown")
			continue
		}
		if !decodableAudio(t.codec) {
			foundCodecs = append(foundCodecs, string(t.codec))
			continue
		}
		if audio == nil {
			audio = t
		}
	}
	if audio == nil {
		if len(foundCodecs) > 0 {
			return malformed("no decodable audio track (found: %s)", joinNames(foundCodecs))
		}
		return malformed("no audio track")
	}
	d.sel = audio
	if audio.note != "" {
		d.note(0, "%s", audio.note)
	}

	if err := audio.fmt.Valid(); err != nil {
		return waxerr.Wrap(waxerr.CodeUnsupportedFormat, "mp4: unusable audio format", err)
	}
	if audio.codec == codec.AACLC {
		d.seekPreroll = 1 // one frame of IMDCT overlap history
	}

	var delay, padding, samples int64
	var exact bool
	if d.fragmented {
		// The fragmented sample tables are empty; gapless comes from the init
		// edit list, and the length is authoritative (SamplesExact) when the
		// edit list carries a segment duration.
		delay, samples, exact = d.fragmentedGapless(audio)
		d.fragOff = d.fragStart
	} else {
		delay, padding, samples = d.gapless(audio)
	}
	d.track = container.Track{
		Codec:        audio.codec,
		CodecConfig:  audio.codecConfig,
		Fmt:          audio.fmt,
		Samples:      samples,
		Delay:        delay,
		Padding:      padding,
		SamplesExact: exact,
		Default:      true,
	}
	d.resolveChapters(tracks, audio)
	return nil
}

// decodableAudio reports whether the demuxer decodes a codec: ALAC and AAC-LC
// (progressive) plus Opus and FLAC (their sample entries are read for the
// fragmented path, and their decoders are registered).
func decodableAudio(id codec.ID) bool {
	switch id {
	case codec.ALAC, codec.AACLC, codec.Opus, codec.FLAC:
		return true
	}
	return false
}

// Tracks returns the single selected audio track.
func (d *Demuxer) Tracks() []container.Track { return []container.Track{d.track} }

// Warnings returns damage tolerated during parsing.
func (d *Demuxer) Warnings() []container.Warning { return d.warnings }

// Chapters returns parsed chapter markers, nil when the file carries none.
func (d *Demuxer) Chapters() []Chapter { return d.chapters }

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
	if d.fragmented {
		return d.seekFragmented(sample)
	}
	st := &d.sel.st
	if st.total == 0 {
		return 0, nil
	}
	idx := st.sampleAt(sample)
	idx = st.syncAtOrBefore(idx)
	if d.seekPreroll > 0 {
		idx = st.syncAtOrBefore(max(idx-d.seekPreroll, 0))
	}
	d.cur = idx
	pts, _ := st.timeOf(idx)
	return pts, nil
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
