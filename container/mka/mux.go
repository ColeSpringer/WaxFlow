package mka

import (
	"encoding/binary"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// Write-side element IDs, the header and track-entry elements the demuxer does
// not itself parse (or parses only on read) but a valid file needs. The
// read-side IDs live in mka.go.
const (
	idEBMLVersion        = 0x4286
	idEBMLReadVersion    = 0x42F7
	idEBMLMaxIDLength    = 0x42F2
	idEBMLMaxSizeLength  = 0x42F3
	idDocTypeVersion     = 0x4287
	idDocTypeReadVersion = 0x4285

	idMuxingApp  = 0x4D80
	idWritingApp = 0x5741

	idTrackUID   = 0x73C5
	idFlagLacing = 0x9C
)

// Deterministic identity, fixed so reproducible output is byte-identical the
// way the Ogg muxer pins oggOpusSerial. Matroska's Info also carries DateUTC
// and the app strings, which would otherwise leak wall-clock and build
// identity; DateUTC is omitted and the app strings are constant.
const (
	muxTrackUID = 1
	muxAppName  = "WaxFlow"
)

// Cluster bounds. Blocks carry a signed 16-bit cluster-relative timestamp in
// milliseconds, so a cluster cannot span more than 32767 ms; a much smaller
// span keeps clusters short and self-contained for simple players, and the
// byte target keeps a single cluster's buffer bounded.
const (
	maxClusterMs       = 4000
	clusterTargetBytes = 1 << 20
)

// MuxerOptions configures the Matroska/WebM muxer.
type MuxerOptions struct {
	// WebM selects the webm DocType and its strict codec subset (Opus and
	// Vorbis only); false writes matroska. The build closure sets it from the
	// requested container name, and Begin rejects a webm request for a codec
	// webm does not carry.
	WebM bool
	// Tags are accepted so the wiring matches the other muxers, but the muxer
	// does NOT emit Matroska Tags elements: metadata for .mka output is written
	// by the CLI's post-pass (cli/label, via waxlabel), which the CLI reaches
	// because Matroska is not in its embedsTags set. If this muxer is ever
	// taught to write Tags at Begin, add the Matroska containers to that
	// embedsTags check in cli/transcode.go so the tags are not written twice.
	Tags []container.Tag
}

// Muxer writes one audio track as Matroska (.mka/.mkv) or WebM. NeedsSeek
// reports false: a plain io.Writer gets a compliant stream with an unknown-size
// Segment, a Duration from the projected length, and a SeekHead naming Info and
// Tracks. An io.WriteSeeker also gets the Duration repatched to the trailer, a
// Cues index with a SeekHead entry pointing at it, and a definite Segment size.
// Output is deterministic (fixed TrackUID and app strings, no DateUTC).
//
// Cues are seekable-only because only the SeekHead pointer locates them, and on
// a plain writer that pointer cannot be filled.
//
// One codec packet is one SimpleBlock (no lacing). The final packet is held
// until End so its block can carry DiscardPadding (the trailing gapless trim),
// which a SimpleBlock cannot hold and a BlockGroup+Block can; CodecDelay on the
// track carries the front trim. These are the exact inverse of the demuxer's
// gapless read path.
type Muxer struct {
	w     io.Writer
	ws    io.WriteSeeker // nil when w cannot seek
	opts  MuxerOptions
	begun bool
	ended bool

	codecID codec.ID
	rate    int
	delay   int64 // track.Delay in samples, written as CodecDelay

	off int64 // bytes written so far

	// Back-patch anchors, absolute file offsets, -1 when absent.
	segDataOff int64
	segSizeOff int64
	durOff     int64
	cueSeekOff int64

	// Seek index, recorded only for a seekable writer.
	cues      []cuePoint
	clusters  int64 // clusters written, against which cueStride is measured
	cueStride int64 // record one cue every cueStride clusters

	rawSamples int64 // summed packet durations, the fallback for a -1 trailer

	// Cluster accumulator: the buffered child elements (Timestamp + blocks)
	// and the cluster's base time in milliseconds.
	cluster     []byte
	clusterMs   int64
	haveCluster bool

	// The held packet, kept back one step so End can turn the last one into a
	// BlockGroup carrying DiscardPadding.
	pending     codec.Packet
	havePending bool
}

// cuePoint is one cluster's timestamp and its position relative to the
// segment's data start, which is what CueClusterPosition means.
type cuePoint struct {
	timeMs int64
	pos    int64
}

// NewMuxer returns a Matroska/WebM muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{
		w:          w,
		segDataOff: -1,
		segSizeOff: -1,
		durOff:     -1,
		cueSeekOff: -1,
		cueStride:  1,
	}
	if ws, ok := w.(io.WriteSeeker); ok {
		// *os.File satisfies io.WriteSeeker for a pipe too, so probe rather
		// than trust the method set: a pipe would fail at the first patch,
		// after the Cues had already gone out. The probe also gives the
		// writer's starting offset, which the absolute patch offsets need.
		if at, err := ws.Seek(0, io.SeekCurrent); err == nil && at >= 0 {
			m.ws, m.off = ws, at
		}
	}
	if opts != nil {
		m.opts = *opts
	}
	return m
}

// NeedsSeek is false: Matroska streams with an unknown-size Segment and
// definite-size Clusters.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and writes the EBML header, Segment header,
// SeekHead, Info, and Tracks, reserving the slots End patches. The Segment is
// opened with an unknown size (streaming); Clusters follow from WritePacket.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.begun {
		return waxerr.New(waxerr.CodeInternal, "mka: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, "mka: muxers are single-track")
	}
	t := tracks[0]
	if err := t.Fmt.Valid(); err != nil {
		return err
	}
	codecID, err := matroskaCodecID(t.Codec, t.Fmt)
	if err != nil {
		return err
	}
	if m.opts.WebM && t.Codec != codec.Opus && t.Codec != codec.Vorbis {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			"mka: webm carries only Opus and Vorbis audio; use .mka for "+string(t.Codec))
	}
	priv, err := codecPrivate(t.Codec, t.CodecConfig)
	if err != nil {
		return err
	}
	m.codecID = t.Codec
	m.rate = t.Fmt.Rate
	m.delay = t.Delay
	if t.Codec == codec.Opus {
		// Opus signals its priming as the OpusHead pre-skip, not the track
		// Delay (the encoder reports it there and in the trailer, not through
		// a Delay method), so read it from the config for CodecDelay. This is
		// the front trim the demuxer's finalizeTrack reads back.
		m.delay = int64(binary.LittleEndian.Uint16(t.CodecConfig[10:12]))
	}
	m.begun = true

	header := m.ebmlHeader()
	header = m.appendSegmentHeader(header, t, codecID, priv)
	return m.write(header)
}

// WritePacket appends one codec packet as a SimpleBlock. The newest packet is
// held back one step (havePending) so End can emit the final one as a
// BlockGroup with DiscardPadding; every earlier packet is a plain SimpleBlock.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.begun || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mka: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, "mka: single-track muxer")
	}
	if m.havePending {
		if err := m.emitBlock(m.pending, 0); err != nil {
			return err
		}
	}
	// Copy the payload: the demuxer/engine may reuse pkt.Data after the call,
	// and this packet is held until the next WritePacket or End.
	m.pending = codec.Packet{
		Data: append([]byte(nil), pkt.Data...),
		PTS:  pkt.PTS,
		Dur:  pkt.Dur,
	}
	m.havePending = true
	if pkt.Dur > 0 {
		m.rawSamples += pkt.Dur
	}
	return nil
}

// End emits the held final packet (with DiscardPadding when the trailer carries
// end padding), flushes the last cluster, and finishes the stream.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.begun || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mka: End outside Begin")
	}
	m.ended = true
	if m.havePending {
		discardNS := samplesToNs(trailer.Padding, m.rate)
		if err := m.emitBlock(m.pending, discardNS); err != nil {
			return err
		}
		m.havePending = false
	}
	if err := m.flushCluster(); err != nil {
		return err
	}
	return m.finish(trailer)
}

// finish appends the Cues index and back-patches the slots Begin reserved. A
// plain io.Writer already has a complete stream and returns untouched.
func (m *Muxer) finish(trailer codec.Trailer) error {
	if m.ws == nil {
		return nil
	}
	cuesPos := int64(-1)
	if len(m.cues) > 0 {
		// Gated on seekability because the SeekHead pointer that finds it is.
		// An empty Cues element is not valid, so no cues means no element.
		cuesPos = m.off - m.segDataOff
		if err := m.write(m.cuesElement()); err != nil {
			return err
		}
	}
	if m.durOff >= 0 {
		// The trailer wins over the projection Begin wrote. A projection that
		// missed is not an error, unlike the FLAC and WAV muxers: a Matroska
		// Duration is advisory, so failing the file over a drift would be the
		// wrong trade.
		if n := finalSamples(trailer, m.rawSamples); n > 0 {
			if err := m.patch(m.durOff, appendFloat(nil, idDuration, durationTicks(n, m.rate))); err != nil {
				return err
			}
		}
	}
	if m.cueSeekOff >= 0 {
		if cuesPos >= 0 {
			var v [8]byte
			binary.BigEndian.PutUint64(v[:], uint64(cuesPos))
			if err := m.patch(m.cueSeekOff+seekPosValueOff, v[:]); err != nil {
				return err
			}
		} else if err := m.patch(m.cueSeekOff, appendVoid(nil, seekEntryLen)); err != nil {
			return err
		}
	}
	if size := m.off - m.segDataOff; m.segSizeOff >= 0 && size < (1<<56)-1 {
		// Width-stable: both forms are 8-byte vints, so nothing after moves.
		var v [8]byte
		binary.BigEndian.PutUint64(v[:], uint64(size))
		v[0] = 0x01
		if err := m.patch(m.segSizeOff, v[:]); err != nil {
			return err
		}
	}
	// Leave the writer at EOF; the jobs runner reopens the output for
	// post-passes.
	if _, err := m.ws.Seek(m.off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mka: seeking to end", err)
	}
	return nil
}

// finalSamples is the presentation length for Duration: the trailer when it
// declares one, else the summed packet durations less the trims.
// Trailer.Samples uses the -1-means-unknown sentinel, so zero means empty.
func finalSamples(trailer codec.Trailer, raw int64) int64 {
	if trailer.Samples >= 0 {
		return trailer.Samples
	}
	return raw - trailer.Delay - trailer.Padding
}

// cuesElement renders the recorded cluster positions as a Cues element.
func (m *Muxer) cuesElement() []byte {
	var body []byte
	for _, c := range m.cues {
		var pos []byte
		pos = appendUint(pos, idCueTrack, muxTrackNumber)
		pos = appendUint(pos, idCueClusterPosition, uint64(c.pos))
		var point []byte
		point = appendUint(point, idCueTime, uint64(c.timeMs))
		point = appendElement(point, idCueTrackPositions, pos)
		body = appendElement(body, idCuePoint, point)
	}
	return appendElement(nil, idCues, body)
}

// emitBlock places one packet into the current cluster, starting a fresh
// cluster when the packet's timestamp would leave the current one (byte target
// or the int16 millisecond span). discardNS > 0 wraps the block in a
// BlockGroup so it can carry DiscardPadding; otherwise it is a SimpleBlock.
func (m *Muxer) emitBlock(pkt codec.Packet, discardNS int64) error {
	ptsMs := msAt(pkt.PTS, m.rate)
	if !m.haveCluster || ptsMs-m.clusterMs > maxClusterMs || len(m.cluster) >= clusterTargetBytes {
		if err := m.flushCluster(); err != nil {
			return err
		}
		m.clusterMs = ptsMs
		m.cluster = appendUint(m.cluster[:0], idTimestamp, uint64(ptsMs))
		m.haveCluster = true
	}
	rel := int16(ptsMs - m.clusterMs)
	if discardNS > 0 {
		block := blockBody(rel, pkt.Data, 0x00) // Block: keyframe implied by BlockGroup
		var group []byte
		group = appendElement(group, idBlock, block)
		group = appendElement(group, idDiscardPadding, beIntBytes(discardNS))
		m.cluster = appendElement(m.cluster, idBlockGroup, group)
		return nil
	}
	block := blockBody(rel, pkt.Data, 0x80) // SimpleBlock: keyframe flag set (audio)
	m.cluster = appendElement(m.cluster, idSimpleBlock, block)
	return nil
}

// flushCluster writes the buffered cluster as a definite-size element and
// resets the accumulator. A cluster with no blocks is not written.
func (m *Muxer) flushCluster() error {
	if !m.haveCluster {
		return nil
	}
	m.haveCluster = false
	m.recordCue()
	out := appendElement(nil, idCluster, m.cluster)
	m.cluster = m.cluster[:0]
	return m.write(out)
}

// recordCue notes where the cluster about to be written begins. At the cap it
// drops every other entry and doubles the stride, so a long file gets a coarser
// index rather than an unbounded slice.
func (m *Muxer) recordCue() {
	if m.ws == nil {
		return
	}
	if m.clusters%m.cueStride == 0 {
		if len(m.cues) == maxCuePoints {
			for i := 0; i < maxCuePoints/2; i++ {
				m.cues[i] = m.cues[2*i]
			}
			m.cues = m.cues[:maxCuePoints/2]
			m.cueStride *= 2
		}
		m.cues = append(m.cues, cuePoint{timeMs: m.clusterMs, pos: m.off - m.segDataOff})
	}
	m.clusters++
}

// blockBody assembles a (Simple)Block payload: the track number as a vint, the
// 2-byte signed cluster-relative timestamp, the flags byte, then the frame.
func blockBody(rel int16, frame []byte, flags byte) []byte {
	b := appendVint(nil, muxTrackNumber)
	b = append(b, byte(uint16(rel)>>8), byte(uint16(rel)), flags)
	return append(b, frame...)
}

// muxTrackNumber is the single audio track's number, referenced by every block.
const muxTrackNumber = 1

// msAt converts a sample position to a whole-millisecond timestamp (the
// TimestampScale is 1 ms). Block timestamps are informational for the reader
// (it derives sample positions by frame-counting), so rounding here does not
// affect gapless accuracy.
func msAt(sample int64, rate int) int64 {
	if sample <= 0 || rate <= 0 {
		return 0
	}
	r := int64(rate)
	sec := sample / r
	rem := sample % r
	return sec*1000 + (rem*1000+r/2)/r
}

// ebmlHeader builds the EBML header declaring the DocType (matroska or webm)
// and the version levels the DiscardPadding/CodecDelay features need.
func (m *Muxer) ebmlHeader() []byte {
	docType, docVersion := "matroska", uint64(4)
	if m.opts.WebM {
		docType, docVersion = "webm", 2
	}
	var body []byte
	body = appendUint(body, idEBMLVersion, 1)
	body = appendUint(body, idEBMLReadVersion, 1)
	body = appendUint(body, idEBMLMaxIDLength, 4)
	body = appendUint(body, idEBMLMaxSizeLength, 8)
	body = appendString(body, idDocType, docType)
	body = appendUint(body, idDocTypeVersion, docVersion)
	body = appendUint(body, idDocTypeReadVersion, 2)
	return appendElement(nil, idEBML, body)
}

// appendSegmentHeader appends the Segment element opened with an unknown size,
// followed by its SeekHead, Info, and Tracks children. Recorded offsets are
// absolute: m.off is where the writer started, dst is what has been built since.
func (m *Muxer) appendSegmentHeader(dst []byte, t container.Track, codecID string, priv []byte) []byte {
	dst = appendID(dst, idSegment)
	m.segSizeOff = m.off + int64(len(dst))
	dst = append(dst, unknownSizeVint...)
	m.segDataOff = m.off + int64(len(dst))

	// Info and Tracks first, so the SeekHead can name their positions. No
	// circularity: entries are fixed-width, so the SeekHead's length does not
	// depend on the values in it.
	info, durSlot := m.infoElement(t)
	entry := appendElement(nil, idTrackEntry, m.trackEntry(t, codecID, priv))
	tracks := appendElement(nil, idTracks, entry)
	seekHead, cueSlot := m.seekHead(int64(len(info)))

	if durSlot >= 0 {
		m.durOff = m.segDataOff + int64(len(seekHead)+durSlot)
	}
	if cueSlot >= 0 {
		m.cueSeekOff = m.segDataOff + int64(cueSlot)
	}
	dst = append(dst, seekHead...)
	dst = append(dst, info...)
	return append(dst, tracks...)
}

// unknownSizeVint is the 8-byte all-ones EBML size marking an element as
// running to the end of the stream. End patches it to a definite size, which is
// width-stable because that form is eight bytes too.
var unknownSizeVint = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

const (
	// seekEntryLen is one Seek entry: 2 (ID) + 1 (size) + 7 (SeekID) + 11
	// (SeekPosition). Uniform, since every target has a 4-byte ID, which lets
	// an unused entry degrade to a Void of the same width.
	seekEntryLen = 21
	// seekPosValueOff is the SeekPosition value's offset within a Seek entry.
	seekPosValueOff = 13
	// durationLen is a Duration element: 2 (ID) + 1 (size) + 8 (the double).
	durationLen = 11
)

// seekHead builds the index: entries for Info and Tracks, plus a reserved one
// for Cues when the writer is seekable. cueSlot is that entry's index in the
// returned bytes, or -1. Positions are relative to the segment's data start.
func (m *Muxer) seekHead(infoLen int64) (elem []byte, cueSlot int) {
	n := 2
	if m.ws != nil {
		n = 3
	}
	// Measure from an empty body of the right size, rather than assuming the
	// size vint's width.
	shLen := int64(len(appendElement(nil, idSeekHead, make([]byte, n*seekEntryLen))))
	var body []byte
	body = appendSeekEntry(body, idInfo, uint64(shLen))
	body = appendSeekEntry(body, idTracks, uint64(shLen+infoLen))
	cueSlot = -1
	if m.ws != nil {
		cueSlot = len(body)
		body = appendSeekEntry(body, idCues, 0)
	}
	elem = appendElement(nil, idSeekHead, body)
	if cueSlot >= 0 {
		cueSlot += len(elem) - len(body)
	}
	return elem, cueSlot
}

// appendSeekEntry appends one SeekHead child. The position is a fixed-width
// 8-byte integer so a reserved slot can be patched without changing length.
func appendSeekEntry(dst []byte, target uint32, pos uint64) []byte {
	var body []byte
	body = appendElement(body, idSeekID, appendID(nil, target))
	body = appendUintFixed(body, idSeekPosition, pos, 8)
	return appendElement(dst, idSeek, body)
}

// infoElement builds the Info element and reports the index of its Duration
// slot, or -1 when there is none. DateUTC is omitted for reproducibility.
//
// Duration is the presentation length, after the gapless trims, which is what
// Track.Samples and Trailer.Samples carry. Samples <= 0 is unknown: the engine
// projects -1, and a zero-valued Track declares nothing.
func (m *Muxer) infoElement(t container.Track) (elem []byte, durSlot int) {
	var body []byte
	body = appendUint(body, idTimestampScale, defaultTimestampScale)
	body = appendString(body, idMuxingApp, muxAppName)
	body = appendString(body, idWritingApp, muxAppName)
	durSlot = -1
	switch {
	case t.Samples > 0:
		// From the projection, so a plain io.Writer carries a duration too. The
		// slot is still recorded, since End repatches it from the trailer.
		durSlot = len(body)
		body = appendFloat(body, idDuration, durationTicks(t.Samples, m.rate))
	case m.ws != nil:
		durSlot = len(body)
		body = appendVoid(body, durationLen)
	}
	elem = appendElement(nil, idInfo, body)
	if durSlot >= 0 {
		durSlot += len(elem) - len(body)
	}
	return elem, durSlot
}

// durationTicks converts a sample count to Info Duration units, samples*1000/rate
// at this muxer's fixed 1-ms scale. It is not rounded: the read path's
// nsToSamples then recovers the exact sample count, which a whole-millisecond
// Duration would lose by up to 47 samples at 48 kHz.
func durationTicks(samples int64, rate int) float64 {
	if samples <= 0 || rate <= 0 {
		return 0
	}
	const ticksPerSecond = 1e9 / defaultTimestampScale
	return float64(samples) * ticksPerSecond / float64(rate)
}

// trackEntry builds the single audio TrackEntry: number, UID, type, codec,
// CodecPrivate, the gapless CodecDelay/SeekPreRoll, and the Audio settings.
func (m *Muxer) trackEntry(t container.Track, codecID string, priv []byte) []byte {
	var e []byte
	e = appendUint(e, idTrackNumber, muxTrackNumber)
	e = appendUint(e, idTrackUID, muxTrackUID)
	e = appendUint(e, idTrackType, trackTypeAudio)
	e = appendUint(e, idFlagLacing, 0) // one frame per block, never laced
	e = appendString(e, idCodecID, codecID)
	if len(priv) > 0 {
		e = appendElement(e, idCodecPrivate, priv)
	}
	if m.delay > 0 {
		e = appendUint(e, idCodecDelay, uint64(samplesToNs(m.delay, m.rate)))
		// Opus carries an 80 ms seek pre-roll (RFC 7845); the demuxer reads it
		// to land a seek far enough ahead for the decoder to reconverge.
		if t.Codec == codec.Opus {
			e = appendUint(e, idSeekPreRoll, uint64(samplesToNs(3840, m.rate)))
		}
	}
	e = appendElement(e, idAudio, m.audioBody(t))
	return e
}

// audioBody builds the Audio element: sampling frequency and channel count for
// every codec, plus bit depth for PCM (where the Audio element is the only
// place the sample width is declared).
func (m *Muxer) audioBody(t container.Track) []byte {
	var a []byte
	a = appendFloat(a, idSamplingFreq, float64(t.Fmt.Rate))
	a = appendUint(a, idChannels, uint64(t.Fmt.Channels))
	if t.Codec == codec.PCM {
		a = appendUint(a, idBitDepth, uint64(pcmContainerBits(t.Fmt)))
	}
	return a
}

// write appends to the stream and advances m.off. The count is added before the
// error is checked: a short write would otherwise leave every later offset out
// by the shortfall.
func (m *Muxer) write(b []byte) error {
	n, err := m.w.Write(b)
	m.off += int64(n)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mka: write", err)
	}
	return nil
}

// patch rewrites bytes at an absolute offset. It leaves m.off alone; finish
// restores the stream position.
func (m *Muxer) patch(off int64, b []byte) error {
	if _, err := m.ws.Seek(off, io.SeekStart); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mka: seek for patch", err)
	}
	if _, err := m.ws.Write(b); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mka: patch", err)
	}
	return nil
}

// matroskaCodecID maps a waxflow codec to its Matroska CodecID string, the
// inverse of mkvCodecID. PCM resolves by the track's sample type.
func matroskaCodecID(id codec.ID, f audio.Format) (string, error) {
	switch id {
	case codec.Opus:
		return "A_OPUS", nil
	case codec.Vorbis:
		return "A_VORBIS", nil
	case codec.FLAC:
		return "A_FLAC", nil
	case codec.AACLC:
		return "A_AAC", nil
	case codec.PCM:
		if f.Type == audio.Float {
			return "A_PCM/FLOAT/IEEE", nil
		}
		return "A_PCM/INT/LIT", nil
	}
	return "", waxerr.New(waxerr.CodeUnsupportedFormat,
		"mka: cannot mux codec "+string(id)+" (opus, vorbis, flac, aac-lc, pcm)")
}

// codecPrivate builds the CodecPrivate blob for the codec from the encoder's
// codec config, the inverse of the demuxer's setup* functions. Opus, Vorbis,
// and AAC pass their config through unchanged; FLAC wraps STREAMINFO in the
// native "fLaC" magic plus a STREAMINFO metadata block; PCM has none.
func codecPrivate(id codec.ID, cfg []byte) ([]byte, error) {
	switch id {
	case codec.Opus:
		if len(cfg) < 19 || string(cfg[:8]) != "OpusHead" {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mka: Opus track config is not an OpusHead")
		}
		return cfg, nil
	case codec.Vorbis:
		if len(cfg) == 0 {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mka: Vorbis track has no codec config")
		}
		return cfg, nil
	case codec.AACLC:
		if len(cfg) == 0 {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mka: AAC track has no AudioSpecificConfig")
		}
		return cfg, nil
	case codec.FLAC:
		if len(cfg) != flac.StreamInfoLen {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mka: FLAC track config is not a STREAMINFO block")
		}
		priv := append([]byte("fLaC"), 0x80, 0x00, 0x00, byte(flac.StreamInfoLen))
		return append(priv, cfg...), nil
	case codec.PCM:
		return nil, nil
	}
	return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "mka: no CodecPrivate for codec "+string(id))
}

// pcmContainerBits is the Matroska BitDepth for a PCM track: the container word
// width in bits (bytes per sample times eight), which is what the demuxer's
// setupPCM reads back to size each frame.
func pcmContainerBits(f audio.Format) int {
	if f.Type == audio.Float {
		return 32
	}
	// Round the pipeline depth up to a whole-byte container word (8/16/24/32),
	// the width the PCM encoder actually packs.
	return (f.BitDepth + 7) / 8 * 8
}
