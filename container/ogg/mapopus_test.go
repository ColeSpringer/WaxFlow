package ogg

import (
	"encoding/binary"
	"strings"
	"testing"
)

// opusHeadFamily1 builds an OpusHead with a family-1 channel-mapping block.
func opusHeadFamily1(channels, streams, coupled byte, table []byte) []byte {
	h := append([]byte("OpusHead"), 1, channels)
	h = binary.LittleEndian.AppendUint16(h, 312)
	h = binary.LittleEndian.AppendUint32(h, 48000)
	h = binary.LittleEndian.AppendUint16(h, 0)
	h = append(h, 1, streams, coupled)
	return append(h, table...)
}

// TestOpusFamily1Gate pins which family-1 layouts the mapping admits: only a
// single stream whose channels map onto its outputs in order (the family-0
// shape in disguise) is decodable by the single-stream Opus decoder. A
// multistream surround header previously flowed through to the decoder and
// panicked its 2-channel state.
func TestOpusFamily1Gate(t *testing.T) {
	cases := []struct {
		name    string
		head    []byte
		wantErr string // empty = accepted
	}{
		{"stereo identity", opusHeadFamily1(2, 1, 1, []byte{0, 1}), ""},
		{"mono identity", opusHeadFamily1(1, 1, 0, []byte{0}), ""},
		{"surround 5.1", opusHeadFamily1(6, 4, 2, []byte{0, 4, 1, 2, 3, 5}), "multistream"},
		{"stereo from two mono streams", opusHeadFamily1(2, 2, 0, []byte{0, 1}), "multistream"},
		{"swapped stereo table", opusHeadFamily1(2, 1, 1, []byte{1, 0}), "identity"},
		{"silent channel", opusHeadFamily1(2, 1, 1, []byte{0, 255}), "identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m opusMapping
			_, err := m.parseID(tc.head)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a layout the decoder cannot decode")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}
