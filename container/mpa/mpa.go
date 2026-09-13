// Package mpa demuxes the MP3 elementary stream: a bare sequence of
// Layer III frames, usually wrapped in ID3 tags, often led by a Xing,
// Info, or VBRI metadata frame.
//
// MP3 frames carry no position, so sample-exact seeking needs an exact
// frame index. The walk that builds one, its resyncs, its seek backoff and
// its persistable index live in container/internal/mpegframes, since a WAV
// data chunk and an AIFF-C SSND payload hold the same frames; what is this
// package's own is the tag baggage around them (leading ID3v2, a trailing
// ID3v1 or APEv2) and the track those frames describe.
package mpa

import (
	"github.com/colespringer/waxflow/codec/mp3"
	"github.com/colespringer/waxflow/container/internal/mpegframes"
)

// Match reports whether head begins with an MP3 elementary stream: a
// parsable Layer III header confirmed by a second one right behind it
// (the sync word alone false-positives on arbitrary binaries, which is
// also why this driver sniffs last). A leading ID3v2 tag was already
// skipped by the caller (format's sniff) or by NewDemuxer.
func Match(head []byte) bool {
	for off := 0; off < maxLeadingJunk && off+mp3.HeaderLen <= len(head); off++ {
		h, err := mp3.ParseHeader(head[off:])
		if err != nil || h.Size() == 0 {
			continue
		}
		next := off + h.Size()
		if next+mp3.HeaderLen > len(head) {
			// The confirming frame lies past the sniff window; a lone
			// candidate at the very start is convincing enough, junk
			// deeper in is not.
			return off == 0
		}
		if n, err := mp3.ParseHeader(head[next:]); err == nil && h.Kin(n) {
			return true
		}
	}
	return false
}

// maxLeadingJunk bounds the Match scan; the tolerant demuxer itself
// scans further (maxResync) once the driver is chosen.
const maxLeadingJunk = 8 << 10

// MatchNeed is the sniff window Match wants: room for leading junk plus
// two maximum-size frames.
const MatchNeed = maxLeadingJunk + 2*1441 + mp3.HeaderLen

// DecoderDelay is the fixed Layer III decoder latency in samples
// (528 plus 1) that gapless trims add to the encoder delay signaled in
// the LAME tag; every mainstream decoder applies the same constant, so
// trimmed output lines up across implementations.
//
// It is exported because it is the difference between the two conventions a
// caller of this package may be holding: the LAME tag states the encoder's
// share alone, while container.Track and codec.Trailer state the samples a
// decoder drops. MuxerOptions.DecodedTrims is how a caller says which it
// has.
const DecoderDelay = mpegframes.DecoderDelay
