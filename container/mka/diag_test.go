package mka

import (
	"bytes"
	"slices"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestUnlocalizedWarningHasNoOffset: a finding with no position, a track
// entry that does not parse, is listed at -1 whatever the source's base, and
// a strict refusal of it names no offset.
func TestUnlocalizedWarningHasNoOffset(t *testing.T) {
	var good []byte
	good = appendUint(good, idTrackNumber, 1)
	good = appendUint(good, idTrackUID, 1)
	good = appendUint(good, idTrackType, trackTypeAudio)
	good = appendString(good, idCodecID, "A_PCM/INT/LIT")
	var a []byte
	a = appendFloat(a, idSamplingFreq, 44100)
	a = appendUint(a, idChannels, 2)
	a = appendUint(a, idBitDepth, 16)
	good = appendElement(good, idAudio, a)
	var tracks []byte
	tracks = appendElement(tracks, idTrackEntry, good)
	tracks = appendElement(tracks, idTrackEntry, []byte{0, 0}) // no element ID parses
	block := blockBody(0, bytes.Repeat([]byte{0x10}, 64), 0x80)
	block[0] = 0x81
	var cluster []byte
	cluster = appendUint(cluster, idTimestamp, 0)
	cluster = appendElement(cluster, idSimpleBlock, block)
	var seg []byte
	seg = appendElement(seg, idInfo, appendUint(nil, idTimestampScale, 1_000_000))
	seg = appendElement(seg, idTracks, tracks)
	seg = appendElement(seg, idCluster, cluster)
	file := appendElement((&Muxer{opts: MuxerOptions{}}).ebmlHeader(), idSegment, seg)

	const msg = "skipping malformed track: mka: invalid element ID in master body"
	for _, base := range []int64{0, 4096} {
		d, err := NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Base: base})
		if err != nil {
			t.Fatal(err)
		}
		if want := (container.Warning{Offset: -1, Msg: msg}); !slices.Contains(d.Warnings(), want) {
			t.Errorf("base %d: listed %v, want %v", base, d.Warnings(), want)
		}
		_, err = NewDemuxer(container.BytesSource(file), &container.DemuxerOptions{Strict: true, Base: base})
		if want := "mka: " + msg; waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != want {
			t.Errorf("base %d: a strict open failed with %v, want %q", base, err, want)
		}
	}
}
