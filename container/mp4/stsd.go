package mp4

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/codec/flac"
	"github.com/colespringer/waxflow/codec/opus"
)

// parseStsd parses the sample description box, reading the first audio
// sample entry into the track. ALAC, AAC-LC, Opus, and FLAC are wired; an
// entry of any other format leaves t.codec set to the format name so
// selectAudio can report it.
func (d *Demuxer) parseStsd(t *track, payload []byte, depth int) error {
	if depth > maxDepth {
		return malformed("box nesting deeper than %d", maxDepth)
	}
	_, _, rest, ok := fullBox(payload)
	if !ok || len(rest) < 4 {
		return malformed("stsd truncated")
	}
	entries := be32(rest)
	rest = rest[4:]
	if entries == 0 {
		return malformed("stsd has no sample entries")
	}
	done := false
	return walkBoxes(rest, func(format string, body []byte) error {
		if done {
			return nil // one audio sample description is enough
		}
		done = true
		return d.parseAudioSampleEntry(t, format, body, depth+1)
	})
}

// parseAudioSampleEntry reads the AudioSampleEntry header and dispatches on
// the sample format to extract the codec configuration.
func (d *Demuxer) parseAudioSampleEntry(t *track, format string, body []byte, depth int) error {
	// SampleEntry: reserved(6) + data_reference_index(2). AudioSampleEntry
	// (QTFF/ISO v0): version(2) revision(2) vendor(4) channelcount(2)
	// samplesize(2) compressionID(2) packetsize(2) samplerate(4, 16.16).
	if len(body) < 28 {
		return malformed("audio sample entry %q truncated", format)
	}
	version := be16(body[8:10])
	channels := int(be16(body[16:18]))
	bitsPerSample := int(be16(body[18:20]))
	sampleRate := int(be32(body[24:28]) >> 16)

	childOff := 28
	switch version {
	case 1:
		childOff = 28 + 16 // samplesPerPacket, bytesPerPacket, bytesPerFrame, bytesPerSample
	case 2:
		childOff = 28 + 36
	}
	if childOff > len(body) {
		childOff = len(body)
	}
	children := body[childOff:]

	// Unwrap a QuickTime 'wave' box, which nests the codec extension.
	if wave := findChild(children, "wave"); wave != nil {
		children = wave
	}

	switch format {
	case "alac":
		return d.setALAC(t, format, children, sampleRate, channels, bitsPerSample)
	case "mp4a":
		return d.setMP4A(t, children, sampleRate, channels)
	case "Opus":
		return d.setOpus(t, children)
	case "fLaC":
		return d.setFLAC(t, children)
	default:
		t.codec = codec.ID(format) // an unknown but named audio codec
		return nil
	}
}

// setOpus reads the 'dOps' box (Opus-in-ISOBMFF) and rebuilds the OpusHead
// codec config, the inverse of seg.go's opusSampleEntry. dOps is big-endian
// where OpusHead is little-endian, so the fields are byte-swapped; only channel
// mapping family 0 (mono and stereo single-stream) is read, matching the muxer.
func (d *Demuxer) setOpus(t *track, children []byte) error {
	dops := findChild(children, "dOps")
	if dops == nil {
		return malformed("Opus sample entry has no dOps box")
	}
	// Version(1) OutputChannelCount(1) PreSkip(2) InputSampleRate(4)
	// OutputGain(2) ChannelMappingFamily(1).
	if len(dops) < 11 {
		return malformed("dOps box truncated (%d bytes)", len(dops))
	}
	if family := dops[10]; family != 0 {
		return malformed("Opus channel mapping family %d unsupported", family)
	}
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1                                              // version
	head[9] = dops[1]                                        // channel count
	binary.LittleEndian.PutUint16(head[10:], be16(dops[2:])) // pre-skip
	binary.LittleEndian.PutUint32(head[12:], be32(dops[4:])) // input sample rate
	binary.LittleEndian.PutUint16(head[16:], be16(dops[8:])) // output gain
	head[18] = 0                                             // channel mapping family 0
	cfg, err := opus.ParseOpusHead(head)
	if err != nil {
		return err
	}
	t.codec = codec.Opus
	t.codecConfig = head
	t.fmt = cfg.Format()
	return nil
}

// setFLAC reads the 'dfLa' box (FLAC-in-ISOBMFF) and extracts the STREAMINFO,
// the inverse of seg.go's flacSampleEntry.
func (d *Demuxer) setFLAC(t *track, children []byte) error {
	dfla := findChild(children, "dfLa")
	if dfla == nil {
		return malformed("FLAC sample entry has no dfLa box")
	}
	// FullBox(4) then a metadata block: a 4-byte block header (last-flag|type,
	// 24-bit length) and the STREAMINFO body.
	_, _, rest, ok := fullBox(dfla)
	if !ok || len(rest) < 4+flac.StreamInfoLen {
		return malformed("dfLa box truncated")
	}
	if typ := rest[0] & 0x7F; typ != 0 {
		return malformed("dfLa first metadata block is type %d, want STREAMINFO", typ)
	}
	si := append([]byte(nil), rest[4:4+flac.StreamInfoLen]...)
	parsed, err := flac.ParseStreamInfo(si)
	if err != nil {
		return err
	}
	t.codec = codec.FLAC
	t.codecConfig = si
	t.fmt = parsed.PCMFormat()
	return nil
}

// setALAC extracts the ALAC magic cookie (the 'alac' extension box) and
// builds the track format from it.
func (d *Demuxer) setALAC(t *track, format string, children []byte, rate, channels, bits int) error {
	ext := findChild(children, "alac")
	if ext == nil {
		return malformed("ALAC sample entry has no magic cookie")
	}
	// The extension box carries a 4-byte version/flags prefix before the
	// 24-byte ALACSpecificConfig; older tools store the bare config.
	cookie := ext
	if len(ext) >= 28 {
		cookie = ext[4:]
	}
	cfg, err := alac.ParseMagicCookie(cookie)
	if err != nil {
		return err
	}
	t.codec = codec.ALAC
	t.codecConfig = append([]byte(nil), cfg.Cookie...)
	t.fmt = cfg.Format()
	return nil
}

// setMP4A extracts the AudioSpecificConfig from the esds descriptor and
// builds the track format. A non-AAC object type is named but left
// unwired.
func (d *Demuxer) setMP4A(t *track, children []byte, rate, channels int) error {
	esds := findChild(children, "esds")
	if esds == nil {
		return malformed("mp4a sample entry has no esds")
	}
	asc, objType, err := parseESDS(esds)
	if err != nil {
		return err
	}
	if !isAACObjectType(objType) {
		t.codec = codec.ID(objectTypeName(objType))
		return nil
	}
	if len(asc) == 0 {
		return malformed("mp4a esds carries no AudioSpecificConfig")
	}
	cfg, err := aac.ParseASC(asc)
	if err != nil {
		return err
	}
	// The ASC is authoritative for rate and channels; fall back to the
	// sample entry only when the ASC left channels implicit (config 0).
	if cfg.Channels == 0 {
		cfg.Channels = channels
	}
	// An esds carries a full ASC, so HE-AAC signals explicitly here and the
	// band limit is knowable at open. Record it on the track rather than
	// emitting it now: this runs for every mp4a track, and a file with an
	// alternate audio track we never select would otherwise warn about audio
	// nobody decodes. selectAudio emits it for the chosen track, which is what
	// mka does.
	t.note = cfg.SBRWarning()
	t.codec = codec.AACLC
	t.codecConfig = append([]byte(nil), asc...)
	t.fmt = cfg.Format()
	return nil
}

// findChild returns the payload of the first child box of the given type,
// or nil. It walks a single level.
func findChild(body []byte, typ string) []byte {
	var found []byte
	_ = walkBoxes(body, func(t string, payload []byte) error {
		if found == nil && t == typ {
			found = payload
		}
		return nil
	})
	return found
}

// parseESDS parses an esds box into its AudioSpecificConfig and the object
// type indication (ISO 14496-1 descriptors).
func parseESDS(payload []byte) (asc []byte, objType byte, err error) {
	_, _, rest, ok := fullBox(payload)
	if !ok {
		return nil, 0, malformed("esds truncated")
	}
	tag, body, _, ok := readDescriptor(rest)
	if !ok || tag != tagES {
		return nil, 0, malformed("esds has no ES_Descriptor")
	}
	if len(body) < 3 {
		return nil, 0, malformed("ES_Descriptor truncated")
	}
	flags := body[2]
	p := body[3:]
	if flags&0x80 != 0 { // streamDependenceFlag
		if len(p) < 2 {
			return nil, 0, malformed("ES_Descriptor truncated")
		}
		p = p[2:]
	}
	if flags&0x40 != 0 { // URL_Flag
		if len(p) < 1 || len(p) < 1+int(p[0]) {
			return nil, 0, malformed("ES_Descriptor URL truncated")
		}
		p = p[1+int(p[0]):]
	}
	if flags&0x20 != 0 { // OCRstreamFlag
		if len(p) < 2 {
			return nil, 0, malformed("ES_Descriptor truncated")
		}
		p = p[2:]
	}
	for len(p) >= 2 {
		dt, dbody, drest, ok := readDescriptor(p)
		if !ok {
			break
		}
		if dt == tagDecoderConfig {
			if len(dbody) < 13 {
				return nil, 0, malformed("DecoderConfigDescriptor truncated")
			}
			objType = dbody[0]
			q := dbody[13:]
			for len(q) >= 2 {
				st, sbody, srest, ok := readDescriptor(q)
				if !ok {
					break
				}
				if st == tagDecoderSpecific {
					asc = sbody
				}
				q = srest
			}
		}
		p = drest
	}
	return asc, objType, nil
}

// MPEG-4 descriptor tags (ISO 14496-1 section 7.2.6).
const (
	tagES              = 0x03
	tagDecoderConfig   = 0x04
	tagDecoderSpecific = 0x05
)

// readDescriptor parses one MPEG-4 descriptor: a tag byte, an expandable
// length (up to four 7-bit groups), then the body. A length overrunning
// the buffer is clamped so a crafted descriptor cannot induce a panic.
func readDescriptor(b []byte) (tag byte, body, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	tag = b[0]
	i := 1
	length := 0
	for n := 0; n < 4; n++ {
		if i >= len(b) {
			return 0, nil, nil, false
		}
		c := b[i]
		i++
		length = length<<7 | int(c&0x7F)
		if c&0x80 == 0 {
			break
		}
	}
	if length > maxDescriptorLen {
		length = maxDescriptorLen
	}
	if i+length > len(b) {
		length = len(b) - i
	}
	return tag, b[i : i+length], b[i+length:], true
}

// isAACObjectType reports whether an esds objectTypeIndication denotes AAC:
// MPEG-4 Audio (0x40) or MPEG-2 AAC main/LC/SSR (0x66-0x68). This mapping is
// an MP4/esds fact, so it lives here rather than in the codec layer.
func isAACObjectType(ot byte) bool {
	switch ot {
	case 0x40, 0x66, 0x67, 0x68:
		return true
	}
	return false
}

// objectTypeName names an object type indication for diagnostics.
func objectTypeName(ot byte) string {
	switch ot {
	case 0x69, 0x6B:
		return "mp3"
	case 0xA9:
		return "dts"
	case 0xA5, 0xA6:
		return "ac3"
	default:
		return "unknown"
	}
}
