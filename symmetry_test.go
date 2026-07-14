package waxflow

import (
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/hls"
)

// TestCodecContainerSymmetry is the read/write symmetry guard and the
// codec/container burn-down checklist in one. WaxFlow's read and write
// capabilities live in independent tables (format's drivers/decoders and the
// engine's outputs), so nothing structural stops the two from drifting: a
// codec can decode with no encoder, a container can demux with no muxer. This
// test reconciles the tables and fails on any asymmetry that is not recorded
// in the allowlist below.
//
// The allowlist is the burn-down list. Each entry names a known-open
// gap and the work that closes it. The check is bidirectional, so it is a
// ratchet, not just a filter:
//
//   - an open gap that is NOT allowlisted fails (new drift: fix it or record it);
//   - an allowlisted gap that has CLOSED fails (delete the stale entry).
//
// So the change that lands a capability must also delete its allowlist entry,
// and the list is empty once every codec encodes+decodes and every container
// demuxes+muxes.
func TestCodecContainerSymmetry(t *testing.T) {
	// symmetryGaps is the allowlist: the asymmetries that are known-open and
	// deliberately deferred. Each entry is deleted by the change that lands its
	// capability, so the list is the burn-down checklist and is empty once
	// every codec encodes+decodes and every container demuxes+muxes. Every gap
	// has since closed, so the list is now empty: the guard is a pure ratchet
	// that fails the moment a new codec or container ships half-symmetric.
	symmetryGaps := map[string]string{}

	decodes := map[codec.ID]bool{}
	for _, id := range format.Decoders() {
		decodes[id] = true
	}
	encodes := map[codec.ID]bool{}
	for _, o := range outputs {
		encodes[o.codecID] = true
	}

	// open reports, for each named gap, whether it is still open, computed from
	// the live tables so the predicate tracks the real code and cannot go stale.
	open := map[string]bool{
		// A codec with a decoder but no encoder-bearing outputs row.
		"vorbis-encode": decodes[codec.Vorbis] && !encodes[codec.Vorbis],
		// The mka package demuxes but has no muxer wired into any output row's
		// container override (opus/aac/flac/pcm reach it once the mka muxer lands).
		"mka-mux": !containerWritable("mka") && !containerWritable("webm"),
		// The ogg demuxer reads FLAC (mapflac.go) but the muxer writes Opus
		// only until the flac row gains an "ogg" container override.
		"ogg-flac-mux": !rowWritesContainer("flac", "ogg"),
		// The ogg demuxer reads Vorbis (mapvorbis.go) but no output row muxes
		// Vorbis into Ogg until the vorbis row lands.
		"ogg-vorbis-mux": !codecWritesContainer(codec.Vorbis, "ogg"),
		// The mp4 muxer/segmenter writes codecs the demuxer cannot read back
		// (Opus/FLAC fMP4); the demuxer learns the fragmented form to close it.
		"mp4-read-back": !mp4DemuxCoversMux(),
	}

	// Reconcile the live state against the allowlist, both directions.
	for name, isOpen := range open {
		_, listed := symmetryGaps[name]
		switch {
		case isOpen && !listed:
			t.Errorf("symmetry gap %q is open but not in the allowlist: close it or record it with the change that will", name)
		case !isOpen && listed:
			t.Errorf("symmetry gap %q has closed; delete its allowlist entry (%s)", name, symmetryGaps[name])
		}
	}
	// Every allowlist entry must correspond to a real, tracked predicate; a
	// typo'd or orphaned entry would silently excuse nothing.
	for name := range symmetryGaps {
		if _, ok := open[name]; !ok {
			t.Errorf("allowlist entry %q names no known symmetry predicate", name)
		}
	}

	// Codec-level reconciliation, independent of the named gaps above: every
	// decoder needs an encoder and every encoder a decoder, except where an
	// allowlisted codec gap explains the imbalance. This is the fully dynamic
	// half, so a new decoder-only or encoder-only codec is caught even if
	// nobody thought to add a named predicate for it.
	for id := range decodes {
		if !encodes[id] {
			t.Errorf("codec %q decodes but has no encoder (add an outputs row or an allowlist entry)", id)
		}
	}
	for id := range encodes {
		if !decodes[id] {
			t.Errorf("codec %q encodes but has no decoder", id)
		}
	}
}

// containerWritable reports whether any output row can mux into the named
// container through its Container override (the aac/adts, flac/ogg, and MKA
// wiring pattern).
func containerWritable(name string) bool {
	for i := range outputs {
		if outputs[i].container != nil {
			if _, err := outputs[i].container(name); err == nil {
				return true
			}
		}
	}
	return false
}

// rowWritesContainer reports whether the named output row can mux into the
// named container, by default or through a Container override.
func rowWritesContainer(row, container string) bool {
	for i := range outputs {
		o := &outputs[i]
		if o.name != row {
			continue
		}
		if rowDefaultContainer[o.name] == container {
			return true
		}
		if o.container != nil {
			if _, err := o.container(container); err == nil {
				return true
			}
		}
	}
	return false
}

// codecWritesContainer reports whether some output row producing the given
// codec muxes into the named container (default or override).
func codecWritesContainer(id codec.ID, container string) bool {
	for i := range outputs {
		o := &outputs[i]
		if o.codecID != id {
			continue
		}
		if rowDefaultContainer[o.name] == container {
			return true
		}
		if o.container != nil {
			if _, err := o.container(container); err == nil {
				return true
			}
		}
	}
	return false
}

// rowDefaultContainer maps an output row's name to the demuxer-side container
// name (the format.Inputs() vocabulary) its default muxer writes, so the
// write side reconciles against the read side. Rows reach other containers
// through their Container override, checked separately.
var rowDefaultContainer = map[string]string{
	"wav":    "wav",
	"opus":   "ogg",
	"aiff":   "aiff",
	"flac":   "flac",
	"mp3":    "mp3",
	"aac":    "mp4",
	"alac":   "mp4",
	"vorbis": "ogg",
}

// mp4DemuxCodecs is the set of codecs the mp4 demuxer reads. It is the one
// maintained datum in this test, edited exactly when the demuxer learns a
// codec, so the edit that closes the mp4-read-back gap (Opus and FLAC
// sample entries plus the fragmented branch) is the same edit that
// records the burn-down here.
var mp4DemuxCodecs = map[codec.ID]bool{
	codec.AACLC: true,
	codec.ALAC:  true,
	// The demuxer learned the fragmented form (moof/traf/trun) and
	// the dOps/dfLa sample entries the segmenter writes, so it reads back the
	// Opus and FLAC fMP4 it produces.
	codec.Opus: true,
	codec.FLAC: true,
}

// mp4DemuxCoversMux reports whether every codec the mp4 muxer or segmenter
// writes can be read back by the mp4 demuxer. The muxer writes ALAC/AAC
// progressively; the segmenter (any row with an hls form) writes Opus/FLAC/
// ALAC/AAC as fragmented CMAF, which the demuxer must also be able to read.
func mp4DemuxCoversMux() bool {
	for i := range outputs {
		o := &outputs[i]
		writesMP4 := rowDefaultContainer[o.name] == "mp4" || o.hls != nil
		if writesMP4 && !mp4DemuxCodecs[o.codecID] {
			return false
		}
	}
	return true
}

// TestHLSWriterHasParser is the plan's separate HLS check: the M3U8 writer in
// internal/hls has a matching parser, so what the segmenter advertises can be
// read back. HLS is playlist-driven and multi-resource, not a container.Muxer
// over one Source, so it is guarded here rather than in the table reconciliation.
func TestHLSWriterHasParser(t *testing.T) {
	master := hls.Master([]hls.MasterVariant{{URI: "v0.m3u8", Bandwidth: 128000, Codecs: "Opus"}})
	if _, err := hls.ParseMaster(master); err != nil {
		t.Errorf("ParseMaster of our own master playlist: %v", err)
	}
	media := hls.Media("init.mp4", []hls.MediaSegment{{URI: "0.m4s", Seconds: 4}})
	if _, err := hls.ParseMedia(media); err != nil {
		t.Errorf("ParseMedia of our own media playlist: %v", err)
	}
}
