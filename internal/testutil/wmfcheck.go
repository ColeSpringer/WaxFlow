package testutil

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

// Windows' WMA Voice encoder can return success with a valid but short file:
// drained too early, it drops the 1.8 seconds it holds. scripts/wmfenc/wmfenc.ps1
// drives that encoder itself for that reason, and says how. The other encoders
// and the decoder stay on MediaTranscoder, which delivered them whole.
//
// A short file still decodes, so a corpus would quietly lose its tails. Every
// encode and decode is held to its source's duration here, and one that runs a
// tenth or more short fails by name. A smaller loss passes this check: the Pro,
// Lossless and Voice cells also assert exact decoded counts, and the v2 cells
// rest on this floor alone.

// durationCheck compares a transcode's output duration against its input's
// and reports whether the output is long enough to be the whole thing.
// Only a shortfall matters: a decode that runs past the declared duration
// is the WMA delivery policy (docs/notes/wma-bitstream.md section 11), not
// damage.
func durationCheck(t testing.TB, in, out string) (ok bool, got, want float64) {
	t.Helper()
	want = mediaDuration(t, in)
	got = mediaDuration(t, out)
	if want <= 0 || got <= 0 {
		return true, got, want // nothing to compare; the caller's own checks stand
	}
	return got >= 0.9*want, got, want
}

// mediaDuration reads a duration in seconds out of a WAV or ASF header,
// returning 0 for anything else. It peeks rather than demuxing: this
// package sits below the containers.
func mediaDuration(t testing.TB, path string) float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	switch {
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return wavDuration(b)
	case len(b) > 30 && binary.LittleEndian.Uint32(b) == 0x75B22630:
		return asfDuration(b)
	}
	return 0
}

// wavDuration walks the chunk list for fmt and data.
func wavDuration(b []byte) float64 {
	var byteRate uint32
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		n := int(binary.LittleEndian.Uint32(b[p+4:]))
		if n < 0 || p+8+n > len(b) {
			n = len(b) - p - 8
		}
		switch {
		case id == "fmt " && n >= 16:
			byteRate = binary.LittleEndian.Uint32(b[p+16:])
		case id == "data" && byteRate > 0:
			return float64(n) / float64(byteRate)
		}
		p += 8 + n + n%2
	}
	return 0
}

// asfDuration reads the File Properties object's play duration, in 100 ns
// units, less the preroll it includes.
func asfDuration(b []byte) float64 {
	for p := 30; p+24 <= len(b); {
		// The size is the file's own 64-bit word, so it is bounded against
		// what is actually there before it moves the cursor: a truncated
		// header (which is the failure this whole file exists to catch) can
		// otherwise overflow p straight past the end and into a panic.
		n := int(binary.LittleEndian.Uint64(b[p+16:]))
		if n < 24 || n > len(b)-p {
			return 0
		}
		// The File Properties Object's GUID, 8CABDCA1-A947-11CF-8EE4-...
		if binary.LittleEndian.Uint32(b[p:]) == 0x8CABDCA1 && p+88 <= len(b) {
			play := binary.LittleEndian.Uint64(b[p+64:])
			preroll := binary.LittleEndian.Uint64(b[p+80:])
			d := float64(play)/1e7 - float64(preroll)/1e3
			if d < 0 {
				return 0
			}
			return d
		}
		p += n
	}
	return 0
}

// wmfShortf formats the message a short transcode fails with.
func wmfShortf(what string, got, want float64) string {
	return fmt.Sprintf("%s came back %.3fs of a %.3fs source: a valid file missing its tail (see wmfcheck.go)",
		what, got, want)
}
