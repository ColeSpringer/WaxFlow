package codecname

import "testing"

// TestWaveFormatNamesEveryTagInTheTable walks every entry, so a name edited on
// one side of the agreement with waxlabel fails here rather than in a message
// nobody compares.
func TestWaveFormatNamesEveryTagInTheTable(t *testing.T) {
	for tag, want := range map[uint16]string{
		0x0001: "PCM",
		0x0002: "ADPCM",
		0x0003: "IEEE float",
		0x0006: "A-law",
		0x0007: "mu-law",
		0x000A: "WMA Voice",
		0x0011: "IMA ADPCM",
		0x0050: "MP2",
		0x0055: "MP3",
		0x00FF: "AAC",
		0x0160: "WMA v1",
		0x0161: "WMA v2",
		0x0162: "WMA Pro",
		0x0163: "WMA Lossless",
		0xFFFE: "PCM (extensible)",
	} {
		if got := WaveFormat(tag); got != want {
			t.Errorf("WaveFormat(%#04x) = %q, want %q", tag, got, want)
		}
	}
	for _, tag := range []uint16{0x0000, 0x0004, 0x1234, 0xFFFF} {
		if got := WaveFormat(tag); got != "" {
			t.Errorf("WaveFormat(%#04x) = %q, want no name", tag, got)
		}
	}
}

func TestFourccNamesEveryEntryInTheTable(t *testing.T) {
	for fourcc, want := range map[string]string{
		"lpcm": "PCM",
		"ipcm": "PCM",
		"sowt": "PCM",
		"twos": "PCM",
		"in24": "PCM",
		"in32": "PCM",
		"raw ": "PCM",
		"NONE": "PCM",
		"fl32": "IEEE float",
		"fpcm": "IEEE float",
		"fl64": "IEEE float64",
		"ulaw": "mu-law",
		"alaw": "A-law",
		"ima4": "IMA ADPCM",
		"MAC3": "MAC3",
		"MAC6": "MAC6",
		".mp3": "MP3",
		".mp2": "MP2",
		".mp1": "MP1",
		// The two non-decodable MP4 audio fourccs that actually turn up, and
		// the spelling Matroska gives the second one.
		"ac-3": "AC-3",
		"ec-3": "E-AC-3",
		"eac3": "E-AC-3",
	} {
		if got := Fourcc(fourcc); got != want {
			t.Errorf("Fourcc(%q) = %q, want %q", fourcc, got, want)
		}
	}
	// AIFF-C and ffmpeg both fold case, so the table has to as well.
	for _, pair := range [][2]string{{"IMA4", "IMA ADPCM"}, {"SOWT", "PCM"}, {"mac3", "MAC3"}, {"None", "PCM"}} {
		if got := Fourcc(pair[0]); got != pair[1] {
			t.Errorf("Fourcc(%q) = %q, want %q", pair[0], got, pair[1])
		}
	}
	// "raw " carries a significant trailing space: without it, nothing.
	if got := Fourcc("raw"); got != "" {
		t.Errorf(`Fourcc("raw") = %q, want no name`, got)
	}
	for _, fourcc := range []string{"", "QDM2", "mp4a", "alac", "abcde"} {
		if got := Fourcc(fourcc); got != "" {
			t.Errorf("Fourcc(%q) = %q, want no name", fourcc, got)
		}
	}
}

// TestQuickTimeWaveTagReadsTheTwoBytesBehindMS pins the escape hatch and the
// one thing it must not do, which is match anything four bytes long.
func TestQuickTimeWaveTagReadsTheTwoBytesBehindMS(t *testing.T) {
	for fourcc, want := range map[string]uint16{
		"ms\x00\x55": 0x0055,
		"ms\x00\x02": 0x0002,
		"ms\x01\x63": 0x0163,
		"ms\xFF\xFE": 0xFFFE,
	} {
		got, ok := QuickTimeWaveTag(fourcc)
		if !ok || got != want {
			t.Errorf("QuickTimeWaveTag(%q) = %#04x, %v; want %#04x, true", fourcc, got, ok, want)
		}
	}
	for _, fourcc := range []string{"", "ms", "ms\x00", "MS\x00\x55", "mp4a", "ms\x00\x55\x00"} {
		if _, ok := QuickTimeWaveTag(fourcc); ok {
			t.Errorf("QuickTimeWaveTag(%q) matched", fourcc)
		}
	}
	// The tag names the codec, so Fourcc reaches the WAVE table through it.
	if got := Fourcc("ms\x00\x55"); got != "MP3" {
		t.Errorf(`Fourcc("ms\x00\x55") = %q, want "MP3"`, got)
	}
}

func TestLabel(t *testing.T) {
	for fourcc, want := range map[string]string{
		"ima4":       "IMA ADPCM (ima4)",
		"sowt":       "PCM (sowt)",
		"ms\x00\x55": "MP3 (ms 0x0055)",
		"ms\x00\x02": "ADPCM (ms 0x0002)",
		"ms\x12\x34": "ms 0x1234",
		// One word for one codec whichever case the file spelled it in: the
		// name alone, never "MAC3 (mac3)".
		"MAC3": "MAC3",
		"mac3": "MAC3",
		// No name: the fourcc as it stands, so a list of what a file held
		// reads as a list of codecs and not of quoted strings.
		"QDM2": "QDM2",
		// ac-3 folds onto its own canonical name, so it too is one word.
		"ac-3": "AC-3",
		"ec-3": "E-AC-3 (ec-3)",
		// Quoted only where it would otherwise vanish or corrupt the line.
		"\x00\x01\x02\x03": `"\x00\x01\x02\x03"`,
		"ab  ":             `"ab  "`,
		"":                 `""`,
	} {
		if got := Label(fourcc); got != want {
			t.Errorf("Label(%q) = %q, want %q", fourcc, got, want)
		}
	}
}
