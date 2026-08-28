package meta_test

import (
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
)

// TestEmbeddableTagsTrimsProjection pins the split the muxers cannot make: a
// tag a caller named is refused when it does not fit, but a tag carried
// forward off a source is dropped here so the transcode still runs. The keys
// come back so the caller can say what went missing.
func TestEmbeddableTagsTrimsProjection(t *testing.T) {
	big := strings.Repeat("a line of the sheet\n", 4000) // ~80 KB
	kept, dropped := meta.EmbeddableTags([]container.Tag{
		{Key: "TITLE", Value: "Kept"},
		{Key: "LYRICS", Value: big},
		{Key: "ARTIST", Value: "Also kept"},
	})
	if len(kept) != 2 || kept[0].Key != "TITLE" || kept[1].Key != "ARTIST" {
		t.Errorf("kept %v, want the two small tags", kept)
	}
	if len(dropped) != 1 || dropped[0] != "LYRICS" {
		t.Errorf("dropped %v, want [LYRICS]", dropped)
	}
}

// TestEmbeddableTagsPassesOrdinaryTags is the other half: a descriptive set
// must come through untouched, or the trim would be quietly costing every
// transcode its metadata.
func TestEmbeddableTagsPassesOrdinaryTags(t *testing.T) {
	in := []container.Tag{
		{Key: "TITLE", Value: "A Title"},
		{Key: "ARTIST", Value: "WaxFlow"},
		{Key: "LYRICS", Value: strings.Repeat("a line\n", 100)},
	}
	kept, dropped := meta.EmbeddableTags(in)
	if len(kept) != len(in) || len(dropped) != 0 {
		t.Errorf("kept %d of %d and dropped %v, want everything kept", len(kept), len(in), dropped)
	}
}

// TestEmbeddableTagsBoundsTheWholeSet checks the budget is the block's, not one
// value's: many tags that each fit can still overflow together, and a per-value
// check would hand the muxer a set it must then refuse.
func TestEmbeddableTagsBoundsTheWholeSet(t *testing.T) {
	var in []container.Tag
	for i := range 20 {
		in = append(in, container.Tag{
			Key:   "COMMENT" + string(rune('A'+i)),
			Value: strings.Repeat("x", 8<<10), // 8 KB each: 160 KB in total
		})
	}
	kept, dropped := meta.EmbeddableTags(in)
	if len(dropped) == 0 {
		t.Fatal("20 tags of 8 KB fit a 48 KiB block, which cannot be")
	}
	used := 64
	for _, tag := range kept {
		used += 11 + len(tag.Key) + len(tag.Value)
	}
	if used > 48<<10 {
		t.Errorf("the kept set is %d bytes, over the smallest block's %d", used, 48<<10)
	}
}
