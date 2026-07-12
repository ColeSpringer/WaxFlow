package hls

import (
	"context"
	"errors"
	"testing"
	"time"
)

// flakyFetcher fails its first failFirst calls with a transient error, then
// serves body, counting every call.
type flakyFetcher struct {
	failFirst int
	calls     int
	body      string
}

func (f *flakyFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return nil, errors.New("transient network error")
	}
	return []byte(f.body), nil
}

// TestFetchRetriesTransient confirms the feed retries a transient fetch failure
// with backoff and returns the body once the fetch succeeds.
func TestFetchRetriesTransient(t *testing.T) {
	f := &flakyFetcher{failFirst: 2, body: "ok"}
	s := &segmentFeed{
		ctx:  context.Background(),
		f:    f,
		opts: LiveOptions{MaxRetries: 3, RequestTimeout: time.Second},
	}
	data, err := s.fetch("http://h/x")
	if err != nil {
		t.Fatalf("fetch after transient failures: %v", err)
	}
	if string(data) != "ok" || f.calls != 3 {
		t.Fatalf("got %q after %d calls, want \"ok\" after 3", data, f.calls)
	}
}

// TestFetchGivesUp confirms the feed stops after MaxRetries+1 attempts and
// surfaces the failure rather than retrying forever.
func TestFetchGivesUp(t *testing.T) {
	f := &flakyFetcher{failFirst: 100, body: "never"}
	s := &segmentFeed{
		ctx:  context.Background(),
		f:    f,
		opts: LiveOptions{MaxRetries: 2, RequestTimeout: time.Second},
	}
	if _, err := s.fetch("http://h/x"); err == nil {
		t.Fatal("fetch of a permanently failing URL must error")
	}
	if f.calls != 3 { // 1 initial + 2 retries
		t.Fatalf("made %d attempts, want 3 (MaxRetries+1)", f.calls)
	}
}

// scriptFetcher returns a scripted sequence of bodies, clamping at the last.
type scriptFetcher struct {
	bodies []string
	i      int
}

func (s *scriptFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	b := s.bodies[min(s.i, len(s.bodies)-1)]
	s.i++
	return []byte(b), nil
}

// TestFeedSequenceReset confirms the feed re-anchors when a live restart resets
// EXT-X-MEDIA-SEQUENCE below the cursor, instead of stranding the cursor ahead
// of every segment and hanging forever.
func TestFeedSequenceReset(t *testing.T) {
	hi := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:100\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.0,\ns100.m4s\n#EXTINF:1.0,\ns101.m4s\n#EXTINF:1.0,\ns102.m4s\n"
	reset := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.0,\ns0.m4s\n#EXTINF:1.0,\ns1.m4s\n"
	s := &segmentFeed{
		ctx:      context.Background(),
		f:        &scriptFetcher{bodies: []string{hi, reset}},
		mediaURL: "http://h/m.m3u8",
		opts:     (&LiveOptions{LiveEdgeSegments: 1 << 20}).or(), // start from the first segment
	}

	if _, err := s.reload(); err != nil {
		t.Fatalf("reload 1: %v", err)
	}
	if len(s.queue) != 3 || s.nextSeq != 103 {
		t.Fatalf("after reload 1: queue=%d nextSeq=%d, want 3 and 103", len(s.queue), s.nextSeq)
	}
	s.queue = s.queue[:0] // simulate the three segments being consumed

	// The reset playlist (MEDIA-SEQUENCE 0) sits entirely below nextSeq=103, so
	// the feed must re-anchor and queue the new stream rather than skip it all.
	if _, err := s.reload(); err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	if len(s.queue) != 2 {
		t.Fatalf("after reset: queue=%d, want 2 (re-anchored to the restarted stream)", len(s.queue))
	}
	if s.queue[0] != "http://h/s0.m4s" {
		t.Errorf("re-anchored queue[0] = %q, want the reset stream's first segment", s.queue[0])
	}
}

// TestBackoffGrowsAndCaps pins the exponential backoff: it doubles per attempt
// and saturates at the cap rather than overflowing.
func TestBackoffGrowsAndCaps(t *testing.T) {
	if backoff(1) != backoffBase {
		t.Errorf("backoff(1) = %v, want %v", backoff(1), backoffBase)
	}
	if backoff(2) != 2*backoffBase {
		t.Errorf("backoff(2) = %v, want %v", backoff(2), 2*backoffBase)
	}
	if backoff(100) != backoffMax {
		t.Errorf("backoff(100) = %v, want the %v cap", backoff(100), backoffMax)
	}
}
