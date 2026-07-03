package mp3

// Feature-coverage pins: the committed fixtures must keep exercising the
// side info features the differential suite claims to cover. A fixture
// regenerated with different encoder settings could otherwise silently
// stop reaching a code path (which is exactly how the scfsi-with-preflag
// interaction shipped untested: tonal fixtures set preflag but carry no
// energy in the pretab bands).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureFeatureCoverage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "noise-cbr64.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	preflag, scfsi, both, _, _ := scanFeatures(t, raw)
	if preflag == 0 || scfsi == 0 || both == 0 {
		t.Errorf("noise-cbr64.mp3 exercises preflag=%d scfsi=%d both=%d; the scalefactor-sharing regression needs all three",
			preflag, scfsi, both)
	}

	short := 0
	for _, name := range []string{"sine-vbr.mp3", "noise-cbr320.mp3", "noise-cbr64.mp3"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, s, _ := scanFeatures(t, raw)
		short += s
	}
	if short == 0 {
		t.Error("no committed fixture exercises short blocks")
	}
}

// scanFeatures walks a stream and counts side info feature use.
func scanFeatures(t *testing.T, raw []byte) (preflag, scfsi, both, short, mixed int) {
	t.Helper()
	off := 0
	for off+HeaderLen <= len(raw) {
		h, err := ParseHeader(raw[off:])
		if err != nil {
			off++
			continue
		}
		size := h.Size()
		if size == 0 || off+size > len(raw) {
			break
		}
		frame := raw[off : off+size]
		hoff := HeaderLen
		if h.Protected {
			hoff += 2
		}
		var si sideInfo
		if len(frame) >= hoff+h.SideInfoLen() && parseSideInfo(h, frame[hoff:hoff+h.SideInfoLen()], &si) {
			granules := 1
			if h.Version == MPEG1 {
				granules = 2
			}
			anyScfsi := false
			for ch := 0; ch < h.Channels; ch++ {
				for g := 0; g < 4; g++ {
					anyScfsi = anyScfsi || si.scfsi[ch][g]
				}
			}
			if anyScfsi {
				scfsi++
			}
			for gi := 0; gi < granules; gi++ {
				for ch := 0; ch < h.Channels; ch++ {
					gr := &si.gr[gi][ch]
					if gr.preflag {
						preflag++
					}
					if gr.blockType == blockShort {
						short++
						if gr.mixed {
							mixed++
						}
					}
					if gi == 1 && gr.preflag && anyScfsi {
						both++
					}
				}
			}
		}
		off += size
	}
	return
}
