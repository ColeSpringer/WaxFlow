package waxflow_test

// HLS live client (phase 6b): follow a growing (non-ENDLIST) media playlist,
// picking up new segments across reloads and decoding them as one continuous
// stream, until the playlist finally carries EXT-X-ENDLIST. Also the ABR safety
// guard (format-lock: a master whose variants disagree on codec is refused) and
// context cancellation unblocking a live read waiting at the edge.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/internal/hls"
)

// liveServer serves an init segment and media segments, plus a media playlist
// that reveals one more segment on each fetch and appends EXT-X-ENDLIST once all
// are exposed. It models a live encoder publishing segments over time.
type liveServer struct {
	mu       sync.Mutex
	init     []byte
	segs     []mp4.Segment
	rate     int
	revealed int
	noEnd    bool // when set, the playlist never carries EXT-X-ENDLIST

	seg0Once sync.Once
	seg0Got  chan struct{} // closed when the first media segment has been fetched
}

func (s *liveServer) mediaPlaylist() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revealed < len(s.segs) {
		s.revealed++ // each reload exposes one more segment
	}
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q\n", "init.mp4")...)
	for i := 0; i < s.revealed; i++ {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\nseg%d.m4s\n", float64(s.segs[i].Samples)/float64(s.rate), i)...)
	}
	if s.revealed == len(s.segs) && !s.noEnd {
		b = append(b, "#EXT-X-ENDLIST\n"...)
	}
	return string(b)
}

func newLiveServer(t *testing.T, e *waxflow.Engine, raw []byte, src *audio.Buffer, opts waxflow.TranscodeOptions, segSamples int) (*liveServer, *httptest.Server) {
	t.Helper()
	plan, err := e.PlanSegments(pcmTrack(src.Fmt, src.N), opts, float64(segSamples)/float64(src.Fmt.Rate))
	if err != nil {
		t.Fatal(err)
	}
	init, err := e.InitSegment(plan, opts)
	if err != nil {
		t.Fatal(err)
	}
	segs, _ := collectSegments(t, e, raw, opts, plan.SegmentSamples, 0)
	ls := &liveServer{init: init, segs: segs, rate: src.Fmt.Rate, seg0Got: make(chan struct{})}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/live.m3u8":
			w.Write([]byte(ls.mediaPlaylist()))
		case r.URL.Path == "/init.mp4":
			w.Write(ls.init)
		default:
			var idx int
			if _, err := fmt.Sscanf(r.URL.Path, "/seg%d.m4s", &idx); err == nil && idx < len(ls.segs) {
				if idx == 0 {
					ls.seg0Once.Do(func() { close(ls.seg0Got) })
				}
				w.Write(ls.segs[idx].Data)
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return ls, srv
}

// TestHLSLiveFollow follows a live FLAC playlist that grows one segment per
// reload to completion. The continuous decode across segment boundaries
// reconstructs the source bit for bit over its real length, and the read ends
// (io.EOF) once EXT-X-ENDLIST appears.
func TestHLSLiveFollow(t *testing.T) {
	const frames = 90000
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 71)
	defer audio.Put(src)
	e := waxflow.New()

	_, srv := newLiveServer(t, e, raw, src, waxflow.TranscodeOptions{Format: "flac"}, 24576)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	med, err := hls.OpenLive(ctx, hls.HTTPFetcher{}, srv.URL+"/live.m3u8", &hls.LiveOptions{
		// Start from the first segment (not the live edge) so the whole stream is
		// decoded, and reload fast so the test does not wait real target durations.
		LiveEdgeSegments: 1 << 20,
		ReloadInterval:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenLive: %v", err)
	}
	defer med.Close()

	// Read the whole live stream to EOF. The live path applies no back-trim
	// (unknown length), so the tail carries the encoder's frame padding past the
	// source length; the real samples are the leading `frames`.
	got := decodeMedia(t, med, frames+65536)
	defer audio.Put(got)
	if got.N < frames {
		t.Fatalf("live decode produced %d frames, want at least %d", got.N, frames)
	}
	for c := 0; c < src.Fmt.Channels; c++ {
		w, g := src.ChanI(c), got.ChanI(c)
		for i := 0; i < frames; i++ {
			if w[i] != g[i] {
				t.Fatalf("ch%d[%d] = %d, want %d (live decode not bit-exact across segments)", c, i, g[i], w[i])
			}
		}
	}
}

// TestHLSLiveFormatLock rejects a master whose variants declare different codecs,
// the ABR safety rule: the decode pipeline is built once, so a mid-stream switch
// to a different output format is refused up front.
func TestHLSLiveFormatLock(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-VERSION:7\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=128000,CODECS=\"mp4a.40.2\"\naac.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=200000,CODECS=\"fLaC\"\nflac.m3u8\n"
	f := staticFetcher{"http://h/master.m3u8": master}
	_, err := hls.OpenLive(context.Background(), f, "http://h/master.m3u8", nil)
	if err == nil {
		t.Fatal("a mixed-codec master must be refused (format-lock)")
	}
}

// TestHLSLiveCancel confirms a live read blocked at the edge (a playlist that
// never ends and stops growing) unblocks promptly when the context is cancelled,
// rather than hanging.
func TestHLSLiveCancel(t *testing.T) {
	const frames = 30000
	raw, src := makeWAV(t, pcm.Config{Bits: 16}, 2, frames, 72)
	defer audio.Put(src)
	e := waxflow.New()

	ls, srv := newLiveServer(t, e, raw, src, waxflow.TranscodeOptions{Format: "flac"}, 24576)
	// Pin the playlist so it never grows past its first segment and never ends,
	// so the client must block at the live edge waiting for more.
	ls.mu.Lock()
	ls.segs = ls.segs[:1]
	ls.noEnd = true
	ls.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	med, err := hls.OpenLive(ctx, hls.HTTPFetcher{}, srv.URL+"/live.m3u8", &hls.LiveOptions{
		LiveEdgeSegments: 1 << 20,
		ReloadInterval:   10 * time.Millisecond,
	})
	if err != nil {
		cancel()
		t.Fatalf("OpenLive: %v", err)
	}
	defer med.Close()

	done := make(chan error, 1)
	go func() {
		f := med.Info().Default().Fmt
		buf := audio.Get(f, audio.StandardChunk)
		defer audio.Put(buf)
		for {
			if err := med.ReadChunk(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	// Wait until the client has actually fetched the one available segment (a
	// real signal, not a sleep-to-coordinate), so it is decoding or blocked at
	// the live edge; then cancel. With no ENDLIST the read can only end in a
	// cancellation error, never EOF.
	select {
	case <-ls.seg0Got:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("client never fetched the first segment")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("blocked live read ended with %v, want a cancellation error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled live read did not unblock")
	}
}

// staticFetcher serves fixed bytes per URL for the tests that need no server.
type staticFetcher map[string]string

func (m staticFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	if v, ok := m[url]; ok {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("not found: %s", url)
}
