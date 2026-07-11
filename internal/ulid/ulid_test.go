package ulid

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func TestMakeKnownVectors(t *testing.T) {
	// Expected strings are hand-computed from the spec encoding: the
	// timestamp part of 1469918176385 is the ULID spec's own example.
	tests := []struct {
		name    string
		ms      int64
		entropy []byte
		want    string
	}{
		{
			name:    "spec timestamp, zero entropy",
			ms:      1469918176385,
			entropy: make([]byte, 10),
			want:    "01ARYZ6S410000000000000000",
		},
		{
			name:    "spec timestamp, patterned entropy",
			ms:      1469918176385,
			entropy: []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
			want:    "01ARYZ6S41VTPVXVR024H36H2N",
		},
		{
			name:    "epoch, zero entropy",
			ms:      0,
			entropy: make([]byte, 10),
			want:    "00000000000000000000000000",
		},
		{
			name:    "maximum timestamp and entropy",
			ms:      1<<48 - 1,
			entropy: bytes.Repeat([]byte{0xFF}, 10),
			want:    "7ZZZZZZZZZZZZZZZZZZZZZZZZZ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Make(time.UnixMilli(tt.ms), bytes.NewReader(tt.entropy))
			if err != nil {
				t.Fatalf("Make: %v", err)
			}
			if got != tt.want {
				t.Errorf("Make = %q, want %q", got, tt.want)
			}
			if !Valid(got) {
				t.Errorf("Valid(%q) = false, want true", got)
			}
		})
	}
}

func TestMakeReadsExactlyTenEntropyBytes(t *testing.T) {
	r := bytes.NewReader(make([]byte, 20))
	if _, err := Make(time.UnixMilli(0), r); err != nil {
		t.Fatalf("Make: %v", err)
	}
	if r.Len() != 10 {
		t.Errorf("entropy bytes left = %d, want 10", r.Len())
	}
}

func TestMakeErrors(t *testing.T) {
	tests := []struct {
		name    string
		t       time.Time
		entropy []byte
	}{
		{"time before epoch", time.UnixMilli(-1), make([]byte, 10)},
		{"time past 48 bits", time.UnixMilli(1 << 48), make([]byte, 10)},
		{"short entropy", time.UnixMilli(0), make([]byte, 5)},
		{"empty entropy", time.UnixMilli(0), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := Make(tt.t, bytes.NewReader(tt.entropy)); err == nil {
				t.Errorf("Make = %q, want error", got)
			}
		})
	}
}

func TestOrderingFollowsTime(t *testing.T) {
	base := time.UnixMilli(1_700_000_000_000)

	// Adversarial entropy: the earlier id gets the largest possible
	// entropy and the later one the smallest, so ordering can only come
	// from the timestamp prefix.
	earlier, err := Make(base, bytes.NewReader(bytes.Repeat([]byte{0xFF}, 10)))
	if err != nil {
		t.Fatal(err)
	}
	later, err := Make(base.Add(time.Millisecond), bytes.NewReader(make([]byte, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if earlier >= later {
		t.Errorf("ids out of order: %q >= %q", earlier, later)
	}

	// A sequence over random entropy stays sorted as time advances.
	prev := ""
	for i := range 20 {
		id, err := Make(base.Add(time.Duration(i)*time.Millisecond), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if id <= prev {
			t.Fatalf("id %d not after its predecessor: %q <= %q", i, id, prev)
		}
		prev = id
	}
}

func TestNew(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !Valid(id) {
			t.Fatalf("New produced invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("New repeated id %q", id)
		}
		seen[id] = true
	}

	// The timestamp prefix of a fresh id decodes to roughly now.
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var ms int64
	for i := range 10 {
		ms = ms<<5 | int64(strings.IndexByte(alphabet, id[i]))
	}
	if d := time.Since(time.UnixMilli(ms)); d < -time.Minute || d > time.Minute {
		t.Errorf("decoded timestamp off by %v", d)
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"known vector", "01ARYZ6S41VTPVXVR024H36H2N", true},
		{"all zeros", "00000000000000000000000000", true},
		{"maximum", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", true},
		{"empty", "", false},
		{"too short", strings.Repeat("0", 25), false},
		{"too long", strings.Repeat("0", 27), false},
		{"lowercase", "01aryz6s41vtpvxvr024h36h2n", false},
		{"excluded letter I", "01ARYZ6S41VTPVXVR024H36H2I", false},
		{"excluded letter L", "01ARYZ6S41VTPVXVR024H36H2L", false},
		{"excluded letter O", "01ARYZ6S41VTPVXVR024H36H2O", false},
		{"excluded letter U", "01ARYZ6S41VTPVXVR024H36H2U", false},
		{"first char overflows 128 bits", "8ZZZZZZZZZZZZZZZZZZZZZZZZZ", false},
		{"path separator", "01ARYZ6S41/TPVXVR024H36H2N", false},
		{"dot", "01ARYZ6S41.TPVXVR024H36H2N", false},
		{"space", "01ARYZ6S41 TPVXVR024H36H2N", false},
		{"traversal", "../x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Valid(tt.s); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}
