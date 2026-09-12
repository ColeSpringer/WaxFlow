// Package codecname names the audio codecs a container can declare, so a
// refusal says what the file holds rather than only that it holds something.
//
// The names are the ones waxlabel reports for the same bytes: a file scanned
// with one tool and refused by the other should be talking about the same
// codec, and two vocabularies for one format tag is how that stops being true.
// It is internal because it is a message vocabulary, not an API: a caller
// routing on a codec has codec.ID for that.
//
// container/asf keeps its own table and its longer "Windows Media Audio ..."
// names for the tags it already names; this is consulted only where nothing
// else has a name to offer.
package codecname

import (
	"fmt"
	"strconv"
	"strings"
)

// WaveFormat names a WAVEFORMATEX format tag, or returns "" for one no name is
// known for. A RIFF "fmt " chunk, an ASF Stream Properties object and a
// QuickTime "ms" sample entry all describe their audio with this one number,
// so one table serves all three.
func WaveFormat(tag uint16) string {
	switch tag {
	case 0x0001:
		return "PCM"
	case 0x0002:
		return "ADPCM"
	case 0x0003:
		return "IEEE float"
	case 0x0006:
		return "A-law"
	case 0x0007:
		return "mu-law"
	case 0x000A:
		return "WMA Voice"
	case 0x0011:
		return "IMA ADPCM"
	case 0x0050:
		return "MP2"
	case 0x0055:
		return "MP3"
	case 0x00FF:
		return "AAC"
	case 0x0160:
		return "WMA v1"
	case 0x0161:
		return "WMA v2"
	case 0x0162:
		return "WMA Pro"
	case 0x0163:
		return "WMA Lossless"
	case 0xFFFE:
		return "PCM (extensible)"
	}
	return ""
}

// Fourcc names a QuickTime/ISOBMFF sample-entry fourcc, or the AIFF-C
// compression type that spells the same codec, and returns "" for one no name
// is known for. Matched case-insensitively, since AIFF-C and ffmpeg's demuxers
// both accept either case for these. "RAW " carries a significant trailing
// space.
//
// Byte order, signedness and width are storage detail of one codec rather than
// different codecs, so the PCM family reads "PCM" whichever spelling carried
// it. MACE has no descriptive name, so its fourcc is its name, which is how
// ffmpeg reports it too.
func Fourcc(fourcc string) string {
	if tag, ok := QuickTimeWaveTag(fourcc); ok {
		return WaveFormat(tag)
	}
	switch strings.ToUpper(fourcc) {
	case "LPCM", "IPCM", "SOWT", "TWOS", "IN24", "IN32", "RAW ", "NONE":
		return "PCM"
	case "FL32", "FPCM":
		return "IEEE float"
	case "FL64":
		return "IEEE float64"
	case "ULAW":
		return "mu-law"
	case "ALAW":
		return "A-law"
	case "IMA4":
		return "IMA ADPCM"
	case "MAC3":
		return "MAC3"
	case "MAC6":
		return "MAC6"
	case "AC-3":
		return "AC-3"
	case "EC-3", "EAC3":
		return "E-AC-3"
	case ".MP3":
		return "MP3"
	case ".MP2":
		return "MP2"
	case ".MP1":
		return "MP1"
	}
	return ""
}

// QuickTimeWaveTag reports the WAVEFORMATEX tag a QuickTime "ms" sample entry
// spells, which is the escape hatch QuickTime uses for codecs that have a WAVE
// format tag and no fourcc of their own: the two bytes behind "ms", big-endian.
func QuickTimeWaveTag(fourcc string) (uint16, bool) {
	if len(fourcc) != 4 || fourcc[0] != 'm' || fourcc[1] != 's' {
		return 0, false
	}
	return uint16(fourcc[2])<<8 | uint16(fourcc[3]), true
}

// Label renders a fourcc for a message: the codec's name with the spelling
// that carried it, the name alone when the two are the same word, and the
// fourcc itself when there is no name.
//
// The fourcc is quoted only when it does not read as a word, because an "ms"
// fourcc's last two bytes are a format tag rather than text and a refusal
// listing what it found must not put a NUL in the middle of a sentence. A
// printable fourcc is left bare, so a list of what a file held reads as
// "ac-3, QDM2" rather than as a row of quoted strings.
func Label(fourcc string) string {
	if tag, ok := QuickTimeWaveTag(fourcc); ok {
		if name := WaveFormat(tag); name != "" {
			return fmt.Sprintf("%s (ms %#04x)", name, tag)
		}
		return fmt.Sprintf("ms %#04x", tag)
	}
	spelling := fourcc
	if !readable(fourcc) {
		spelling = strconv.Quote(fourcc)
	}
	switch name := Fourcc(fourcc); {
	case name == "":
		return spelling
	// EqualFold, since Fourcc folds: "mac3" and "MAC3" are one codec and must
	// not print two ways.
	case strings.EqualFold(name, fourcc):
		return name
	default:
		return fmt.Sprintf("%s (%s)", name, spelling)
	}
}

// readable reports whether a fourcc can go into a sentence as it stands:
// printable ASCII with no space, so a non-printable byte or an invisible
// leading or trailing space is quoted instead of vanishing.
func readable(fourcc string) bool {
	if fourcc == "" {
		return false
	}
	for i := 0; i < len(fourcc); i++ {
		if fourcc[i] <= ' ' || fourcc[i] > '~' {
			return false
		}
	}
	return true
}
