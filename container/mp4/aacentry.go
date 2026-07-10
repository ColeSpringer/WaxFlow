package mp4

import (
	"fmt"

	"github.com/colespringer/waxflow/codec/aac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// aacSampleEntry builds the 'mp4a' AudioSampleEntry with an esds box
// wrapping the track's AudioSpecificConfig (the write-side inverse of
// stsd.go's parseESDS). AAC's sample entry carries no delay field; the
// encoder priming rides in the edit list instead, so t.Delay is not
// cross-checked here.
func aacSampleEntry(t container.Track) ([]byte, error) {
	cfg, err := aac.ParseASC(t.CodecConfig)
	if err != nil {
		return nil, err
	}
	if want := cfg.Format(); t.Fmt.Rate != want.Rate || t.Fmt.Channels != want.Channels ||
		t.Fmt.Type != want.Type || t.Fmt.BitDepth != want.BitDepth {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track format %v does not match the AudioSpecificConfig (%v)", t.Fmt, want))
	}
	return audioSampleEntry("mp4a", t.Fmt, esdsBox(t.CodecConfig)), nil
}

// esdsBox assembles the MPEG-4 descriptor chain: an ES_Descriptor
// holding a DecoderConfigDescriptor (objectTypeIndication 0x40, MPEG-4
// Audio) whose DecoderSpecificInfo is the AudioSpecificConfig, plus the
// mandatory SLConfigDescriptor. Lengths are single-byte (every payload
// here is far under 128 bytes); readDescriptor accepts that form and so
// does every demuxer.
func esdsBox(asc []byte) []byte {
	dsi := descriptor(tagDecoderSpecific, asc)
	decCfg := descriptor(tagDecoderConfig, concat(
		[]byte{0x40},        // objectTypeIndication: MPEG-4 Audio
		[]byte{0x05<<2 | 1}, // streamType audio, upStream 0, reserved 1
		[]byte{0, 0x18, 0},  // bufferSizeDB: the 6144-bit decoder buffer in bytes
		u32(0), u32(0),      // maxBitrate, avgBitrate: unknown
		dsi))
	es := descriptor(tagES, concat(
		u16(1),    // ES_ID
		[]byte{0}, // no stream dependence, no URL, no OCR
		decCfg,
		descriptor(0x06, []byte{0x02}))) // SLConfigDescriptor: MP4 predefined
	return makeFullBox("esds", 0, 0, es)
}

// descriptor frames one MPEG-4 descriptor with a single-byte length.
func descriptor(tag byte, body []byte) []byte {
	if len(body) > 127 {
		// Unreachable for anything this muxer produces; expandable
		// lengths exist but nothing here needs them.
		panic("mp4: descriptor body over 127 bytes")
	}
	out := make([]byte, 0, 2+len(body))
	out = append(out, tag, byte(len(body)))
	return append(out, body...)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
