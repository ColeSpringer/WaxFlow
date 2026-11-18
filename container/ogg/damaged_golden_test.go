package ogg

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/testutil"
)

// TestGoldenDamagedRead pins what a tolerant read and seeks deliver past
// damage: where the page scan lands behind junk, and what it makes of a lone
// page. Regenerate with `make goldens` and review the diff.
func TestGoldenDamagedRead(t *testing.T) {
	open := func(src container.Source) (container.Demuxer, error) { return NewDemuxer(src, nil) }
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// audio is where each audio page of raw starts, then where the last ends.
	audio := func(t *testing.T, raw []byte) []int {
		d, err := NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		pages := pageStarts(t, raw)
		return pages[slices.Index(pages, int(d.firstData)):]
	}
	// take names the junk cases an input reads.
	type take struct {
		plain bool // junk-first and junk-mid
		more  bool // junk-window, decoy-mid and junk-island
		bound bool // junk-under-bound and junk-past-bound
		last  bool // junk-last
	}
	// Junk goes where two whole pages lie behind every page a resync lands
	// on, so a landing one page late still shows a page that held: that takes
	// five audio pages. The rule exempts junk-last, which has its own flag.
	cases := func(in take) func(t *testing.T, raw []byte) []testutil.Damage {
		return func(t *testing.T, raw []byte) []testutil.Damage {
			pages := audio(t, raw)
			var damages []testutil.Damage
			if in.last {
				// The landing that runs to the end of data.
				damages = append(damages, testutil.JunkAt("junk-last", raw, pages[len(pages)-2], 'O'))
			}
			if !in.plain && !in.more && !in.bound {
				return damages
			}
			if len(pages) < 6 {
				t.Fatalf("%d audio pages: junk needs two whole pages behind every landing", len(pages)-1)
			}
			first, mid, next := pages[0], pages[(len(pages)-4)/2], pages[(len(pages)-4)/2+1]
			if in.plain {
				damages = append(damages,
					testutil.JunkAt("junk-first", raw, first, 'O'),
					testutil.JunkAt("junk-mid", raw, mid, 'O'))
			}
			// The page scan steps scanStep bytes at a time.
			const window = scanStep
			if in.more {
				damages = append(damages,
					// The read's first window and the tail scan's both end
					// inside the capture pattern behind the junk.
					testutil.JunkOf("junk-window", raw, mid, window-2, 'O'),
					testutil.DecoyAt("decoy-mid", raw, mid, 'O'),
					// A lone page checks, and nothing page-shaped follows it.
					testutil.IslandAt("junk-island", raw, mid, next, 'O'))
			}
			if in.bound {
				// The bound is checked a window at a time, so the last page
				// found starts this far from where the scan did, which for a
				// read is one byte into the junk.
				horizon := maxResync/(window-3)*(window-3) + window - 4
				damages = append(damages,
					testutil.JunkOf("junk-under-bound", raw, mid, horizon+1, 'O'),
					testutil.JunkOf("junk-past-bound", raw, mid, horizon+2, 'O'))
			}
			return damages
		}
	}

	// The committed Vorbis and FLAC files hold two audio pages each, so their
	// other junk cases read a twin with a packet a page. The Opus file has
	// eleven and 740 packets, so all but its two plain cases read its first pages.
	for _, file := range []string{"testdata/golden-vorbis.ogg", "testdata/golden-flac.oga"} {
		testutil.GoldenDamagedRead(t, file, open, *update, cases(take{last: true}))
	}
	testutil.GoldenDamagedRead(t, "testdata/golden-opus.opus", open, *update, cases(take{plain: true}))
	vorbis, _ := repaginateRaw(t, read("golden-vorbis.ogg"), false)
	testutil.GoldenDamagedInput(t, "golden-vorbis.ogg-paged", "golden-vorbis.ogg rebuilt a packet a page",
		vorbis, open, *update, cases(take{plain: true, more: true, bound: true}))
	flac, _ := repaginateRaw(t, read("ref-flac.oga"), false)
	testutil.GoldenDamagedInput(t, "ref-flac.oga-paged", "ref-flac.oga rebuilt a packet a page",
		flac, open, *update, cases(take{plain: true, more: true, bound: true}))
	opus := read("golden-opus.opus")
	testutil.GoldenDamagedInput(t, "golden-opus.opus-head", "golden-opus.opus up to its sixth audio page",
		opus[:audio(t, opus)[5]], open, *update, cases(take{more: true, last: true}))
}
