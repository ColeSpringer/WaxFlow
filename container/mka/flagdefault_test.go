package mka

import (
	"bytes"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// trackFlags are the bodies of one track's FlagDefault and FlagEnabled
// elements: nil leaves the element out, and an empty body writes it with no
// octets.
type trackFlags struct {
	def, enabled []byte
}

// twoTrackFile is a Matroska file of two PCM tracks, the first at 44.1 kHz
// and the second at 48 kHz, with one block each.
func twoTrackFile(flags [2]trackFlags) (file []byte, frames [2][]byte) {
	rates := [2]int{44100, 48000}
	var tracks, cluster []byte
	cluster = appendUint(cluster, idTimestamp, 0)
	for i, rate := range rates {
		number := uint64(i + 1)
		var e []byte
		e = appendUint(e, idTrackNumber, number)
		e = appendUint(e, idTrackUID, number)
		e = appendUint(e, idTrackType, trackTypeAudio)
		if flags[i].def != nil {
			e = appendElement(e, idFlagDefault, flags[i].def)
		}
		if flags[i].enabled != nil {
			e = appendElement(e, idFlagEnabled, flags[i].enabled)
		}
		e = appendString(e, idCodecID, "A_PCM/INT/LIT")
		var a []byte
		a = appendFloat(a, idSamplingFreq, float64(rate))
		a = appendUint(a, idChannels, 2)
		a = appendUint(a, idBitDepth, 16)
		e = appendElement(e, idAudio, a)
		tracks = appendElement(tracks, idTrackEntry, e)

		frames[i] = bytes.Repeat([]byte{byte(0x10 * number)}, 64)
		block := blockBody(0, frames[i], 0x80)
		block[0] = 0x80 | byte(number) // the track number, as a one-byte vint
		cluster = appendElement(cluster, idSimpleBlock, block)
	}
	var seg []byte
	seg = appendElement(seg, idInfo, appendUint(nil, idTimestampScale, 1_000_000))
	seg = appendElement(seg, idTracks, tracks)
	seg = appendElement(seg, idCluster, cluster)
	return appendElement((&Muxer{opts: MuxerOptions{}}).ebmlHeader(), idSegment, seg), frames
}

// TestMissingFlagDefaultIsDefault: Matroska's FlagDefault defaults to 1, so
// a track that leaves it out is a default track, and one that writes 0 is
// not. Reading a missing flag as 0 picked the track its file marked as not
// the default. An element written with no octets is its default too, as
// EBML reads any empty element that has one.
//
// FlagEnabled is read the same way and ranks first: a track its file marks
// unusable is passed over while a usable one exists, whatever either says
// about being a default.
func TestMissingFlagDefaultIsDefault(t *testing.T) {
	zero, one, empty := []byte{0}, []byte{1}, []byte{}
	def := func(a, b []byte) [2]trackFlags { return [2]trackFlags{{def: a}, {def: b}} }
	for _, tc := range []struct {
		name  string
		flags [2]trackFlags
		want  int // the selected track, 0 or 1
	}{
		{"an explicit 0, then none", def(zero, nil), 1},
		{"none on either", def(nil, nil), 0},
		{"none, then an explicit 1", def(nil, one), 0},
		{"none, then an explicit 0", def(nil, zero), 0},
		{"an explicit 0, then an explicit 1", def(zero, one), 1},
		{"an explicit 0 on both", def(zero, zero), 0},
		{"an explicit 0, then an empty element", def(zero, empty), 1},
		{"an empty element, then an explicit 1", def(empty, one), 0},

		{"a disabled track, then a default one", [2]trackFlags{{enabled: zero}, {def: one}}, 1},
		{"a disabled default track, then an enabled one that is no default",
			[2]trackFlags{{def: one, enabled: zero}, {def: zero}}, 1},
		{"a track enabled by an empty element, then a default one", [2]trackFlags{{enabled: empty}, {def: one}}, 0},
		{"an enabled track, then a disabled default one", [2]trackFlags{{def: zero}, {def: one, enabled: zero}}, 0},
		// Nothing usable is still something to decode: the first, as before.
		{"both disabled", [2]trackFlags{{enabled: zero}, {enabled: zero}}, 0},
		{"both disabled, the second a default", [2]trackFlags{{def: zero, enabled: zero}, {enabled: zero}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, frames := twoTrackFile(tc.flags)
			d, err := NewDemuxer(container.BytesSource(file), &DemuxerOptions{Strict: true})
			if err != nil {
				t.Fatal(err)
			}
			wantRate := [2]int{44100, 48000}[tc.want]
			if got := d.Tracks()[0].Fmt.Rate; got != wantRate {
				t.Fatalf("selected the track at %d Hz, want the one at %d Hz", got, wantRate)
			}
			// The blocks read are that track's, and only that track's.
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(pkt.Data, frames[tc.want]) {
				t.Errorf("the first packet is not track %d's block", tc.want+1)
			}
		})
	}
}

// TestTrackNamesTheDemuxerRevision: the track carries DemuxerVersion, which
// is what puts this demuxer's revision in a cache key, and keeps it across
// the walk that settles its length.
func TestTrackNamesTheDemuxerRevision(t *testing.T) {
	file, _ := twoTrackFile([2]trackFlags{})
	d, err := NewDemuxer(container.BytesSource(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Tracks()[0].DemuxerVersion; got != DemuxerVersion || got == "" {
		t.Fatalf("the track names demuxer revision %q, want %q", got, DemuxerVersion)
	}
	if err := d.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.Tracks()[0].DemuxerVersion; got != DemuxerVersion {
		t.Errorf("after the walk the track names %q, want %q", got, DemuxerVersion)
	}
}
