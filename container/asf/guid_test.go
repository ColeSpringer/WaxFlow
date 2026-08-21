package asf

import (
	"testing"

	"github.com/colespringer/waxflow/codec"
)

// TestGUIDWireOrder pins each GUID literal against the canonical text its
// comment carries. The wire form reverses the first three fields and leaves
// the last eight bytes alone, which is exactly the transposition a hand-typed
// literal gets wrong, and a wrong GUID does not fail loudly: it makes the
// object it names invisible.
func TestGUIDWireOrder(t *testing.T) {
	for _, tc := range []struct {
		g    guid
		want string
	}{
		{guidHeader, "75B22630-668E-11CF-A6D9-00AA0062CE6C"},
		{guidData, "75B22636-668E-11CF-A6D9-00AA0062CE6C"},
		{guidSimpleIndex, "33000890-E5B1-11CF-89F4-00A0C90349CB"},
		{guidFileProperties, "8CABDCA1-A947-11CF-8EE4-00C00C205365"},
		{guidStreamProperties, "B7DC0791-A9B7-11CF-8EE6-00C00C205365"},
		{guidHeaderExtension, "5FBF03B5-A92E-11CF-8EE3-00C00C205365"},
		{guidContentDescription, "75B22633-668E-11CF-A6D9-00AA0062CE6C"},
		{guidExtendedContentDescription, "D2D0A440-E307-11D2-97F0-00A0C95EA850"},
		{guidContentEncryption, "2211B3FB-BD23-11D2-B4B7-00A0C955FC6E"},
		{guidExtendedContentEncryption, "298AE614-2622-4C17-B935-DAE07EE9289C"},
		{guidAdvancedContentEncryption, "43058533-6981-49E6-9B74-AD12CB86D58C"},
		{guidAudioMedia, "F8699E40-5B4D-11CF-A8FD-00805F5C442B"},
		{guidVideoMedia, "BC19EFC0-5B4D-11CF-A8FD-00805F5C442B"},
		{guidCommandMedia, "59DACFC0-59E6-11D0-A3AC-00A0C90348F6"},
		{guidBinaryMedia, "3AFB65E2-47EF-40F2-AC2C-70A90D71D343"},
	} {
		if got := tc.g.String(); got != tc.want {
			t.Errorf("guid %v renders %s, want %s", []byte(tc.g[:]), got, tc.want)
		}
	}
}

// TestCodecTable pins the format tags this build claims and the ones it names
// without decoding. A tag that silently fell out of the table would turn a
// refusal that says "Windows Media Audio Pro" into one that says a hex number.
func TestCodecTable(t *testing.T) {
	for _, tc := range []struct {
		tag  uint16
		id   codec.ID
		name string
	}{
		{0x0160, codec.WMA, "Windows Media Audio 1"},
		{0x0161, codec.WMA, "Windows Media Audio 2"},
		{0x0162, "", "Windows Media Audio Pro"},
		{0x0163, "", "Windows Media Audio Lossless"},
		{0x0164, "", "Windows Media Audio Pro"},
		{0x000A, "", "Windows Media Audio Voice"},
		{0x000B, "", "Windows Media Audio Voice"},
		{0x0055, "", ""}, // MP3 in ASF: recognized by nobody here, named by nobody
	} {
		id, name := asfCodecID(tc.tag)
		if id != tc.id || name != tc.name {
			t.Errorf("asfCodecID(%#04x) = %q, %q; want %q, %q", tc.tag, id, name, tc.id, tc.name)
		}
	}
}

// TestTimeConversions checks the two arithmetic paths that turn container time
// into sample positions, including the split that keeps a crafted timestamp
// from overflowing the product.
func TestTimeConversions(t *testing.T) {
	if got := msToSamples(1000, 44100); got != 44100 {
		t.Errorf("msToSamples(1000, 44100) = %d", got)
	}
	// 46 ms at 44100 rounds to nearest, not down: 2028.6 -> 2029.
	if got := msToSamples(46, 44100); got != 2029 {
		t.Errorf("msToSamples(46, 44100) = %d, want 2029", got)
	}
	if got, ok := hnsToSamples(20_420_000, 44100); !ok || got != 90052 {
		t.Errorf("hnsToSamples(2.042s, 44100) = %d, %v; want 90052, true", got, ok)
	}
	// The seek direction floors rather than rounding, so a target never moves
	// forward past the packet that holds it: 90052 samples is 2041.99 ms.
	if got := samplesToMS(90052, 44100); got != 2041 {
		t.Errorf("samplesToMS(90052, 44100) = %d, want 2041", got)
	}
	// A crafted duration must be refused, not wrapped: WAVEFORMATEX states
	// the rate in 32 bits and audio.Format only requires it positive, so the
	// two together reach past int64.
	if got, ok := hnsToSamples(1<<62, 1<<30); ok {
		t.Errorf("hnsToSamples(1<<62, 1<<30) = %d, want the overflow reported", got)
	}
	if got, ok := hnsToSamples(1<<62, 192000); !ok || got <= 0 {
		t.Errorf("hnsToSamples(1<<62, 192000) = %d, %v", got, ok)
	}
	if got := msToSamples(1<<48, 192000); got <= 0 {
		t.Errorf("msToSamples overflowed to %d", got)
	}
	for _, ms := range []int64{0, -1, -1 << 40} {
		if got := msToSamples(ms, 44100); got != 0 {
			t.Errorf("msToSamples(%d) = %d, want 0", ms, got)
		}
	}
}

// TestParseWaveFormat covers the clamp a writer that overstates cbSize needs,
// since the fixed fields are what select the codec.
func TestParseWaveFormat(t *testing.T) {
	b := []byte{
		0x61, 0x01, // wFormatTag: WMAV2
		0x02, 0x00, // channels
		0x44, 0xAC, 0x00, 0x00, // 44100
		0x80, 0x3E, 0x00, 0x00, // avg bytes/sec
		0xE7, 0x02, // block align 743
		0x10, 0x00, // bits
		0xFF, 0xFF, // cbSize, wildly overstated
		0x01, 0x02, 0x03,
	}
	w, ok := parseWaveFormat(b)
	if !ok {
		t.Fatal("parseWaveFormat rejected a well-formed structure")
	}
	if w.tag != 0x0161 || w.channels != 2 || w.rate != 44100 || w.blockAlign != 743 || w.bits != 16 {
		t.Errorf("waveFormat = %+v", w)
	}
	if len(w.extra) != 3 || len(w.raw) != len(b) {
		t.Errorf("extra %d bytes, raw %d bytes; want 3 and %d", len(w.extra), len(w.raw), len(b))
	}
	if _, ok := parseWaveFormat(b[:17]); ok {
		t.Error("parseWaveFormat accepted a structure shorter than its fixed fields")
	}
}
