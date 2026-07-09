package opus

import (
	"encoding/binary"
	"testing"
)

// opusHead builds an OpusHead packet with the given channel count and mapping
// family; family 1 appends a stream-count/coupled-count/table block.
func opusHead(channels, family int, streams, coupled byte, table []byte) []byte {
	h := make([]byte, 19)
	copy(h, "OpusHead")
	h[8] = 1
	h[9] = byte(channels)
	binary.LittleEndian.PutUint16(h[10:], 312) // pre-skip
	binary.LittleEndian.PutUint32(h[12:], 48000)
	h[18] = byte(family)
	if family != 0 {
		h = append(h, streams, coupled)
		h = append(h, table...)
	}
	return h
}

// TestMultichannelRejected pins that a valid RFC 7845 family-1 surround
// header parses (probe stays honest about the file) but NewDecoder refuses
// it cleanly: this decoder has no multistream de-framing, and its per-channel
// state is sized for mono/stereo, so 3+ channels must error, never panic.
func TestMultichannelRejected(t *testing.T) {
	head := opusHead(6, 1, 4, 2, []byte{0, 4, 1, 2, 3, 5})
	cfg, err := ParseOpusHead(head)
	if err != nil {
		t.Fatalf("ParseOpusHead(family-1 5.1): %v", err)
	}
	if cfg.Channels != 6 {
		t.Fatalf("channels = %d, want 6", cfg.Channels)
	}
	d, err := NewDecoder(cfg, cfg.Format())
	if err == nil {
		d.Release()
		t.Fatal("NewDecoder accepted a 6-channel config; it cannot decode multistream Opus")
	}
}
