package audio

import (
	"errors"
	"testing"

	"github.com/colespringer/waxflow/waxerr"
)

func TestFormatValid(t *testing.T) {
	tests := []struct {
		name string
		f    Format
		ok   bool
	}{
		{"cd", Format{Rate: 44100, Channels: 2, Type: Int, BitDepth: 16}, true},
		{"hires", Format{Rate: 96000, Channels: 2, Type: Int, BitDepth: 24}, true},
		{"float", Format{Rate: 48000, Channels: 1, Type: Float, BitDepth: 32}, true},
		{"surround", Format{Rate: 48000, Channels: 6, Layout: DefaultLayout(6), Type: Int, BitDepth: 24}, true},
		{"one bit int", Format{Rate: 8000, Channels: 1, Type: Int, BitDepth: 1}, true},
		{"zero rate", Format{Channels: 2, Type: Int, BitDepth: 16}, false},
		{"negative rate", Format{Rate: -1, Channels: 2, Type: Int, BitDepth: 16}, false},
		{"zero channels", Format{Rate: 44100, Type: Int, BitDepth: 16}, false},
		{"too many channels", Format{Rate: 44100, Channels: 9, Type: Int, BitDepth: 16}, false},
		{"layout mismatch", Format{Rate: 44100, Channels: 2, Layout: FrontCenter, Type: Int, BitDepth: 16}, false},
		{"int depth 0", Format{Rate: 44100, Channels: 2, Type: Int}, false},
		{"int depth 33", Format{Rate: 44100, Channels: 2, Type: Int, BitDepth: 33}, false},
		{"float depth 64", Format{Rate: 44100, Channels: 2, Type: Float, BitDepth: 64}, false},
		{"bad sample type", Format{Rate: 44100, Channels: 2, Type: SampleType(9), BitDepth: 16}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.f.Valid()
			if tt.ok && err != nil {
				t.Errorf("Valid() = %v, want nil", err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatal("Valid() = nil, want error")
				}
				if !errors.Is(err, waxerr.ErrInvalidRequest) {
					t.Errorf("Valid() code = %v, want invalid-request", waxerr.CodeOf(err))
				}
			}
		})
	}
}

func TestDefaultLayout(t *testing.T) {
	for ch := 1; ch <= MaxChannels; ch++ {
		if got := DefaultLayout(ch).Count(); got != ch {
			t.Errorf("DefaultLayout(%d).Count() = %d", ch, got)
		}
	}
	if DefaultLayout(0) != 0 || DefaultLayout(9) != 0 {
		t.Error("DefaultLayout outside 1..8 must be zero")
	}
	if got := DefaultLayout(6); got != FrontLeft|FrontRight|FrontCenter|LowFrequency|BackLeft|BackRight {
		t.Errorf("DefaultLayout(6) = %v", got)
	}
}

func TestChannelMaskString(t *testing.T) {
	if got := (FrontLeft | FrontRight | LowFrequency).String(); got != "FL|FR|LFE" {
		t.Errorf("String() = %q, want FL|FR|LFE", got)
	}
	if got := ChannelMask(0).String(); got != "unknown" {
		t.Errorf("String() = %q, want unknown", got)
	}
}

func TestBufferChannelViews(t *testing.T) {
	f := Format{Rate: 48000, Channels: 3, Layout: DefaultLayout(3), Type: Int, BitDepth: 16}
	b := Get(f, 100)
	defer Put(b)
	if b.Cap() != 100 || b.N != 0 || b.Pos != 0 || b.Discont {
		t.Fatalf("fresh buffer: cap=%d n=%d pos=%d discont=%v", b.Cap(), b.N, b.Pos, b.Discont)
	}
	b.N = 10
	for c := 0; c < 3; c++ {
		view := b.ChanI(c)
		if len(view) != 10 || cap(view) != 100 {
			t.Fatalf("ChanI(%d): len=%d cap=%d, want 10, 100", c, len(view), cap(view))
		}
		for i := range view {
			view[i] = int32(c*1000 + i)
		}
	}
	// Views alias the flat backing at c*Stride.
	for c := 0; c < 3; c++ {
		for i := 0; i < 10; i++ {
			if got := b.I[c*b.Stride+i]; got != int32(c*1000+i) {
				t.Fatalf("backing[%d*stride+%d] = %d, want %d", c, i, got, c*1000+i)
			}
		}
	}
	// Appending past a channel view's capacity must reallocate, never
	// clobber the adjacent channel.
	b.N = 100
	v := b.ChanI(0)
	v = append(v, 7)
	v[0] = -1
	if b.I[0] == -1 {
		t.Error("append past Stride aliased the original backing")
	}
	if b.I[b.Stride] != int32(1000) {
		t.Error("channel 1 clobbered")
	}
}

func TestBufferDomainPanics(t *testing.T) {
	fInt := Format{Rate: 48000, Channels: 1, Type: Int, BitDepth: 16}
	fFloat := Format{Rate: 48000, Channels: 1, Type: Float, BitDepth: 32}
	bi := Get(fInt, 8)
	bf := Get(fFloat, 8)
	defer Put(bi)
	defer Put(bf)
	if bi.I == nil || bi.F != nil {
		t.Error("int buffer must populate I only")
	}
	if bf.F == nil || bf.I != nil {
		t.Error("float buffer must populate F only")
	}
	mustPanic(t, func() { bi.ChanF(0) })
	mustPanic(t, func() { bf.ChanI(0) })
	mustPanic(t, func() { Get(Format{}, 8) })
	mustPanic(t, func() { Get(fInt, 0) })
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	fn()
}

func TestPoolRoundTrip(t *testing.T) {
	f := Format{Rate: 48000, Channels: 2, Type: Float, BitDepth: 32}
	for range 100 {
		b := Get(f, StandardChunk)
		if len(b.F) != 2*StandardChunk {
			t.Fatalf("len(F) = %d, want %d", len(b.F), 2*StandardChunk)
		}
		b.N = StandardChunk
		b.ChanF(1)[StandardChunk-1] = 0.5
		Put(b)
	}
	// Oversized requests bypass the pool but still work.
	huge := Get(f, (1<<maxClassBits)/2+1)
	if len(huge.F) != 2*((1<<maxClassBits)/2+1) {
		t.Fatalf("oversized len(F) = %d", len(huge.F))
	}
	Put(huge)
	Put(nil) // must not panic
}

func TestCopyFrames(t *testing.T) {
	f := Format{Rate: 48000, Channels: 2, Layout: DefaultLayout(2), Type: Int, BitDepth: 16}
	src := Get(f, 16)
	dst := Get(f, 16)
	defer Put(src)
	defer Put(dst)
	src.N = 16
	dst.N = 16
	for c := 0; c < 2; c++ {
		s := src.ChanI(c)
		for i := range s {
			s[i] = int32(c*100 + i)
		}
	}
	CopyFrames(dst, 4, src, 2, 5)
	for c := 0; c < 2; c++ {
		for i := 0; i < 5; i++ {
			if got := dst.I[c*dst.Stride+4+i]; got != int32(c*100+2+i) {
				t.Fatalf("dst ch%d[%d] = %d, want %d", c, 4+i, got, c*100+2+i)
			}
		}
	}
	// Overlapping forward copy within one buffer (the compaction case).
	CopyFrames(src, 0, src, 2, 10)
	if src.ChanI(1)[0] != 102 || src.ChanI(1)[9] != 111 {
		t.Error("overlapping CopyFrames corrupted data")
	}
	CopyFrames(dst, 0, src, 0, 0) // n=0 is a no-op
	other := Get(Format{Rate: 48000, Channels: 2, Layout: DefaultLayout(2), Type: Float, BitDepth: 32}, 4)
	defer Put(other)
	mustPanic(t, func() { CopyFrames(other, 0, src, 0, 1) })
}

// TestPoolCycleAllocationFree pins the point of pooling whole Buffers:
// a warm Get/Put cycle performs no heap allocation at all (boxed slice
// headers would cost one per Put).
func TestPoolCycleAllocationFree(t *testing.T) {
	f := Format{Rate: 48000, Channels: 2, Layout: DefaultLayout(2), Type: Int, BitDepth: 16}
	Put(Get(f, 1024)) // warm the class
	allocs := testing.AllocsPerRun(200, func() {
		Put(Get(f, 1024))
	})
	if allocs != 0 {
		t.Errorf("warm Get+Put allocates %v per run, want 0", allocs)
	}
}

func TestClassBits(t *testing.T) {
	tests := []struct{ total, want int }{
		{1, minClassBits},
		{1 << minClassBits, minClassBits},
		{1<<minClassBits + 1, minClassBits + 1},
		{1 << 12, 12},
		{1<<12 + 1, 13},
		{1 << maxClassBits, maxClassBits},
		{1<<maxClassBits + 1, -1},
	}
	for _, tt := range tests {
		if got := classBits(tt.total); got != tt.want {
			t.Errorf("classBits(%d) = %d, want %d", tt.total, got, tt.want)
		}
	}
}
