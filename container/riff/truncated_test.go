package riff

import (
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
)

// cutTail is raw without its last n bytes.
func cutTail(raw []byte, n int) []byte { return raw[:len(raw)-n] }

// truncatedBy is how many samples raw's headers declare past its payload
// (container.Truncated).
func truncatedBy(t *testing.T, raw []byte) int64 {
	t.Helper()
	d, err := NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return container.TruncatedOf(d, d.Tracks()[0].ID)
}

// TestTruncatedWAVCountsWhatItsHeadersStillClaim: a data chunk declaring more
// bytes than the file holds, and a fact chunk declaring more samples than the
// data does, are each a count of the audio the file no longer has. A header
// that agrees with the payload, or claims less, counts none.
func TestTruncatedWAVCountsWhatItsHeadersStillClaim(t *testing.T) {
	cfg := pcm.Config{Encoding: pcm.SignedInt, Bits: 16}
	f := cfg.PCMFormat(48000, 2, audio.DefaultLayout(2))
	ws := &memWS{}
	muxWAV(t, ws, cfg, f, wireBytes(cfg, 2, 300, 21), 300, nil)
	frame := cfg.BytesPerFrame(2)
	const spb = 505 // samples per IMA ADPCM block at a 256-byte block align
	blocks := func(n int) []byte { return make([]byte, 256*n) }
	big := &memWS{}
	muxWAV(t, big, cfg, f, wireBytes(cfg, 2, 300, 21), 300, &MuxerOptions{SizeLimit: 128}) // forces RF64
	le.PutUint64(big.Buf[20+8:], 0xFFFFFFFFFFFFFFF0)                                       // the ds64 data size

	for _, c := range []struct {
		name string
		raw  []byte
		want int64
	}{
		{"whole", ws.Buf, 0},
		{"PCM cut after 100 frames", ws.Buf[:len(ws.Buf)-200*frame], 200},
		{"PCM cut inside a frame", ws.Buf[:len(ws.Buf)-200*frame+1], 200},
		{"a fact past the blocks", wavHeader(tagIMAADPCM, 1, 8000, 256, 4, imaExtra(spb), blocks(4), 4*spb+1000), 1000},
		{"a fact inside the last block", wavHeader(tagIMAADPCM, 1, 8000, 256, 4, imaExtra(spb), blocks(4), 3*spb+1), 0},
		{"a fact past byte-linear data", wavHeader(tagALaw, 1, 8000, 1, 8, nil, make([]byte, 10), 12), 2},
		{"a fact short of byte-linear data", wavHeader(tagALaw, 1, 8000, 1, 8, nil, make([]byte, 10), 7), 0},
		{"a ds64 size no file holds", big.Buf, (math.MaxInt64 - 300*int64(frame)) / int64(frame)},
		// A block codec's fact chunk is its length, so a file cut short of it
		// lost what the fact declares past the blocks held, which is less
		// than the blocks the data chunk sized where the fact ends inside one.
		{"blocks cut under a fact inside the last", cutTail(wavHeader(tagIMAADPCM, 1, 8000, 256, 4, imaExtra(spb), blocks(10), 9*spb+255), 4*256),
			9*spb + 255 - 6*spb},
		{"blocks cut with no fact to count them", cutTail(wavHeader(tagIMAADPCM, 1, 8000, 256, 4, imaExtra(spb), blocks(10), -1), 4*256), 4 * spb},
		{"blocks cut past a fact the rest still hold", cutTail(wavHeader(tagIMAADPCM, 1, 8000, 256, 4, imaExtra(spb), blocks(10), 5*spb+255), 4*256), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := truncatedBy(t, c.raw); got != c.want {
				t.Fatalf("%d samples truncated, want %d", got, c.want)
			}
		})
	}
}
