package hls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// The live half of the HLS client: follow a live (non-ENDLIST) media playlist as
// it grows, fetching new fragmented-MP4 segments and decoding them as one
// continuous stream. Where the VOD reader loads a finite presentation up front,
// this reloads the playlist on an interval, tracks the media sequence to pick up
// only new segments, and blocks at the live edge until more arrive, retrying
// transient network failures at the client layer instead of failing the decode.
//
// The stream is exposed as a format.Media whose ReadChunk blocks for the next
// segment and returns io.EOF only when the playlist finally carries
// EXT-X-ENDLIST (a live event that completed) or the context is cancelled. A
// single decoder spans every segment, so decode stays continuous and gapless
// across segment boundaries exactly as a concatenated read would be.

// Defaults for the live refresh loop.
const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxRetries     = 3
	defaultLiveEdge       = 3 // segments back from the end a fresh live read starts
	minReloadInterval     = 200 * time.Millisecond
	backoffBase           = 250 * time.Millisecond
	backoffMax            = 8 * time.Second
)

// LiveOptions configures the live client.
type LiveOptions struct {
	// VariantIndex selects a master-playlist variant (0-based, default 0). The
	// client follows this one variant for the life of the read; the format-lock
	// rule (variants must agree on codec) keeps a future switch within a stable
	// output format.
	VariantIndex int
	// ReloadInterval overrides how often the media playlist is refetched. Zero
	// derives it from the playlist's EXT-X-TARGETDURATION (floored at 200 ms).
	ReloadInterval time.Duration
	// RequestTimeout bounds each individual fetch. Zero uses 30 s.
	RequestTimeout time.Duration
	// MaxRetries is how many times a failed fetch is retried with exponential
	// backoff before the read fails. Zero uses 3.
	MaxRetries int
	// LiveEdgeSegments is how many segments back from the live edge a fresh read
	// starts (the HLS convention keeps a small buffer). Zero uses 3.
	LiveEdgeSegments int
}

func (o *LiveOptions) or() LiveOptions {
	v := LiveOptions{}
	if o != nil {
		v = *o
	}
	if v.VariantIndex < 0 {
		v.VariantIndex = 0
	}
	if v.RequestTimeout <= 0 {
		v.RequestTimeout = defaultRequestTimeout
	}
	if v.MaxRetries <= 0 {
		v.MaxRetries = defaultMaxRetries
	}
	if v.LiveEdgeSegments <= 0 {
		v.LiveEdgeSegments = defaultLiveEdge
	}
	return v
}

// OpenLive follows a live HLS presentation and returns its audio as a
// format.Media. It resolves a master to one variant (format-locked so the
// variants agree on codec), fetches the init segment, and starts a refresh loop
// near the live edge; ReadChunk then blocks for new segments and returns io.EOF
// once the playlist carries EXT-X-ENDLIST or ctx is cancelled. A complete
// (ENDLIST) playlist is read fine too, so OpenLive also serves VOD when the
// caller wants streaming decode rather than OpenVOD's up-front load.
func OpenLive(ctx context.Context, f Fetcher, playlistURL string, opts *LiveOptions) (format.Media, error) {
	o := opts.or()
	mediaURL, raw, err := resolveLiveMedia(ctx, f, playlistURL, o)
	if err != nil {
		return nil, err
	}
	first, err := ParseMedia(string(raw))
	if err != nil {
		return nil, err
	}
	if first.InitURI == "" {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"hls: live playlist has no EXT-X-MAP init segment (only fragmented-MP4 is followed)")
	}
	initURL, err := resolveURL(mediaURL, first.InitURI)
	if err != nil {
		return nil, err
	}
	feed := &segmentFeed{ctx: ctx, f: f, mediaURL: mediaURL, opts: o, firstRaw: raw}
	init, err := feed.fetch(initURL)
	if err != nil {
		return nil, err
	}
	// The track (codec, config, format, front delay) comes from the init alone;
	// force an unknown length so the pipeline applies no back-trim to a stream
	// that is still growing.
	probe, err := mp4.NewFragmentedDemuxer(init, container.BytesSource(nil))
	if err != nil {
		return nil, err
	}
	tracks := probe.Tracks()
	if len(tracks) == 0 {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat, "hls: init segment declares no audio track")
	}
	track := tracks[0]
	track.Samples = -1
	track.SamplesExact = false
	return format.FromDemuxer("hls-live", &liveDemuxer{init: init, track: track, feed: feed})
}

// resolveLiveMedia fetches the playlist and, for a master, applies the
// format-lock and follows the selected variant, returning the media playlist URL
// and its bytes (reused as the feed's first load).
func resolveLiveMedia(ctx context.Context, f Fetcher, playlistURL string, o LiveOptions) (string, []byte, error) {
	raw, err := fetchRetry(ctx, f, playlistURL, o.RequestTimeout, o.MaxRetries)
	if err != nil {
		return "", nil, err
	}
	if !isMasterPlaylist(raw) {
		return playlistURL, raw, nil
	}
	master, err := ParseMaster(string(raw))
	if err != nil {
		return "", nil, err
	}
	if err := formatLock(master); err != nil {
		return "", nil, err
	}
	if o.VariantIndex >= len(master.Variants) {
		return "", nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("hls: variant index %d out of range (%d variants)", o.VariantIndex, len(master.Variants)))
	}
	mediaURL, err := resolveURL(playlistURL, master.Variants[o.VariantIndex].URI)
	if err != nil {
		return "", nil, err
	}
	media, err := fetchRetry(ctx, f, mediaURL, o.RequestTimeout, o.MaxRetries)
	return mediaURL, media, err
}

// formatLock enforces the ABR safety rule: every variant must declare the same
// codec, so following (or switching between) variants can never change the
// decode format the downstream pipeline was built from. Rate and channels are
// not in the master, so they are locked from the selected variant's init on
// open; codec is the piece the master can and must agree on.
func formatLock(master MasterPlaylist) error {
	codecs := master.Variants[0].Codecs
	for _, v := range master.Variants[1:] {
		if v.Codecs != codecs {
			return waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("hls: master variants disagree on codec (%q vs %q); a stable output format is required", codecs, v.Codecs))
		}
	}
	return nil
}

// liveDemuxer presents the growing segment feed as one container.Demuxer: each
// segment is a self-contained fragmented-MP4 read behind the shared init, and
// ReadPacket walks segment to segment, so a single downstream decoder spans them
// all. It exposes exactly one track (from the init) and returns io.EOF at the
// stream's true end.
type liveDemuxer struct {
	init  []byte
	track container.Track
	feed  *segmentFeed
	inner *mp4.Demuxer
}

func (d *liveDemuxer) Tracks() []container.Track { return []container.Track{d.track} }

func (d *liveDemuxer) ReadPacket(pkt *container.Packet) error {
	for {
		if d.inner == nil {
			seg, err := d.feed.next()
			if err != nil {
				return err // bare io.EOF at end of stream, or a ctx/fetch error
			}
			dem, err := mp4.NewFragmentedDemuxer(d.init, container.BytesSource(seg))
			if err != nil {
				return err
			}
			d.inner = dem
		}
		err := d.inner.ReadPacket(pkt)
		if errors.Is(err, io.EOF) {
			d.inner = nil // this segment is drained; pull the next
			continue
		}
		return err
	}
}

// segmentFeed drives the reload loop: it refetches the media playlist, tracks the
// media sequence so only new segments are taken, queues their URLs, and blocks
// at the live edge until the playlist grows or ends. Every fetch retries with
// exponential backoff under a per-request timeout.
type segmentFeed struct {
	ctx      context.Context
	f        Fetcher
	mediaURL string
	opts     LiveOptions

	firstRaw  []byte // the playlist bytes OpenLive already fetched, used once
	queue     []string
	nextSeq   int64
	reloadIvl time.Duration
	seeded    bool
	ended     bool
}

// next returns the next segment's bytes, reloading the playlist and blocking at
// the live edge as needed. It returns io.EOF once the playlist has ended and
// every segment has been delivered.
func (s *segmentFeed) next() ([]byte, error) {
	for {
		if len(s.queue) > 0 {
			url := s.queue[0]
			s.queue = s.queue[1:]
			return s.fetch(url)
		}
		if s.ended {
			return nil, io.EOF
		}
		grew, err := s.reload()
		if err != nil {
			return nil, err
		}
		if !grew && !s.ended {
			if err := s.wait(s.reloadInterval()); err != nil {
				return nil, err
			}
		}
	}
}

// reload refetches the media playlist and appends any newly appeared segments to
// the queue, returning whether the queue grew. The first reload seeds the start
// position near the live edge and consumes the playlist OpenLive already fetched.
func (s *segmentFeed) reload() (bool, error) {
	var raw []byte
	if s.firstRaw != nil {
		raw, s.firstRaw = s.firstRaw, nil
	} else {
		var err error
		if raw, err = s.fetch(s.mediaURL); err != nil {
			return false, err
		}
	}
	pl, err := ParseMedia(string(raw))
	if err != nil {
		return false, err
	}
	if s.reloadIvl == 0 && pl.TargetDuration > 0 {
		s.reloadIvl = time.Duration(pl.TargetDuration) * time.Second
	}
	// A live restart can reset EXT-X-MEDIA-SEQUENCE far below our cursor, leaving
	// every listed segment behind nextSeq so nothing ever matches and the read
	// hangs forever. Detect the backward jump (the whole playlist sits below the
	// cursor) and re-anchor to the new stream near its edge, accepting the
	// inherent discontinuity rather than stalling.
	if s.seeded && len(pl.Segments) > 0 && int64(pl.MediaSequence)+int64(len(pl.Segments)) < s.nextSeq {
		s.seeded = false
	}
	if !s.seeded {
		s.nextSeq = int64(pl.MediaSequence)
		if n := int64(len(pl.Segments)); n > int64(s.opts.LiveEdgeSegments) {
			s.nextSeq = int64(pl.MediaSequence) + n - int64(s.opts.LiveEdgeSegments)
		}
		s.seeded = true
	}
	before := len(s.queue)
	for i, seg := range pl.Segments {
		seq := int64(pl.MediaSequence) + int64(i)
		if seq < s.nextSeq {
			continue
		}
		url, err := resolveURL(s.mediaURL, seg.URI)
		if err != nil {
			return false, err
		}
		s.queue = append(s.queue, url)
		s.nextSeq = seq + 1
	}
	if pl.End {
		s.ended = true
	}
	return len(s.queue) > before, nil
}

// reloadInterval is the media-playlist reload cadence: the caller override, else
// the playlist's target duration, floored so a tiny or missing value cannot spin.
func (s *segmentFeed) reloadInterval() time.Duration {
	d := s.opts.ReloadInterval
	if d <= 0 {
		d = s.reloadIvl
	}
	return max(d, minReloadInterval)
}

// fetch retrieves url through the feed's context and options.
func (s *segmentFeed) fetch(url string) ([]byte, error) {
	return fetchRetry(s.ctx, s.f, url, s.opts.RequestTimeout, s.opts.MaxRetries)
}

// fetchRetry retrieves url, retrying transient failures with exponential backoff
// under a per-request timeout. A cancelled parent context aborts immediately
// (terminal, not a transient the backoff should paper over). Both the VOD and
// live paths route their fetches through it, so neither can stall on a hung
// server.
func fetchRetry(ctx context.Context, f Fetcher, url string, timeout time.Duration, maxRetries int) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := waitCtx(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		data, err := f.Fetch(reqCtx, url)
		cancel()
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "hls: fetch canceled", ctx.Err())
		}
		lastErr = err
	}
	return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable,
		fmt.Sprintf("hls: fetching %s failed after %d attempts", url, maxRetries+1), lastErr)
}

// wait sleeps for d, aborting if the feed's context is cancelled first.
func (s *segmentFeed) wait(d time.Duration) error { return waitCtx(s.ctx, d) }

// waitCtx sleeps for d, aborting if ctx is cancelled first.
func waitCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return waxerr.Wrap(waxerr.CodeCanceled, "hls: live read canceled", ctx.Err())
	case <-t.C:
		return nil
	}
}

// backoff is the exponential delay for retry attempt n (1-based), capped. Go
// defines large left shifts (no C-style UB: they saturate to 0), so the d<=0
// check already catches an overflow; the early n>30 return just avoids computing
// an absurd intermediate and states the intent.
func backoff(n int) time.Duration {
	if n > 30 {
		return backoffMax
	}
	d := backoffBase << (n - 1)
	if d > backoffMax || d <= 0 {
		d = backoffMax
	}
	return d
}
