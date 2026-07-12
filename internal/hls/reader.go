package hls

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/waxerr"
)

// The read side of the segmenter: given a complete (EXT-X-ENDLIST) HLS
// presentation, follow the playlists, pull the init and media segments, and
// decode them through the fragmented-MP4 demuxer. HLS is multi-resource and
// playlist-driven, so this is a client that fetches many resources rather than a
// container.Demuxer over one Source; the result is exposed as a format.Media so
// the rest of the pipeline consumes it exactly like a local file. This is the
// piece that closes the "HLS is write-only" symmetry gap: the segmenter writes
// numbered fMP4 segments and M3U8 playlists, and this reads them back.

// Bounds on a fetched presentation. Playlists are attacker-influenced once the
// client fetches over the network, so the segment count and the concatenated
// media size are capped rather than trusted.
const (
	// maxSegments caps how many media segments one VOD playlist may list. At a
	// few seconds each this clears many hours; past it the playlist is refused.
	maxSegments = 1 << 20
	// maxMediaBytes caps the total media a VOD read stores. VOD read-back loads
	// the whole (finite) presentation to a temp file; a stream larger than this
	// is refused rather than filling the disk.
	maxMediaBytes = 1 << 30
	// maxResponseBytes bounds a single fetched resource (one playlist, init, or
	// media segment) so an oversized or hostile response cannot exhaust memory in
	// the fetch itself, before the aggregate caps get a chance to apply. Audio
	// segments are seconds of audio (a few MB); this is generous headroom while
	// still bounding one response.
	maxResponseBytes = 64 << 20
)

// Fetcher retrieves the bytes at an absolute URL. The reader fetches the
// playlist, the init segment, and each media segment through it, so tests serve
// from a local server and production uses HTTP.
type Fetcher interface {
	Fetch(ctx context.Context, rawurl string) ([]byte, error)
}

// HTTPFetcher fetches over HTTP with a caller-supplied client (for timeouts).
// A nil client uses http.DefaultClient.
type HTTPFetcher struct {
	Client *http.Client
	// MaxResponseBytes caps a single response body; 0 uses maxResponseBytes. A
	// response over the cap is refused, not truncated, so a hostile server cannot
	// exhaust memory or feed the demuxer a silently-cut segment.
	MaxResponseBytes int64
}

// Fetch GETs rawurl and returns its body, mapping transport and status errors
// to waxerr codes so the client's failures classify like the rest of the stack.
// The body is read through an io.LimitReader so no single response can exhaust
// memory regardless of the aggregate caps the caller enforces afterward.
func (h HTTPFetcher) Fetch(ctx context.Context, rawurl string) ([]byte, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalidRequest, "hls: building request", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "hls: fetching "+rawurl, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, waxerr.New(waxerr.CodeSourceUnreadable,
			fmt.Sprintf("hls: fetching %s: HTTP %d", rawurl, resp.StatusCode))
	}
	limit := h.MaxResponseBytes
	if limit <= 0 {
		limit = maxResponseBytes
	}
	// Read one byte past the limit so an over-cap body is detected, not truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeSourceUnreadable, "hls: reading "+rawurl, err)
	}
	if int64(len(body)) > limit {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("hls: %s response exceeds the %d-byte limit", rawurl, limit))
	}
	return body, nil
}

// ClientOptions configures the HLS reader.
type ClientOptions struct {
	// VariantIndex selects a master-playlist variant by position (0-based). A
	// negative index (the default) auto-selects the first variant. It composes
	// with the live client's format-lock rule.
	VariantIndex int
	// RequestTimeout bounds each individual fetch (playlist, init, segment). Zero
	// uses the default (30 s), so a hung server cannot stall the whole read.
	RequestTimeout time.Duration
	// MaxRetries is how many times a failed fetch is retried with exponential
	// backoff before the read fails. Zero uses the default (3).
	MaxRetries int
}

func (o *ClientOptions) variantIndex() int {
	if o == nil || o.VariantIndex < 0 {
		return 0
	}
	return o.VariantIndex
}

func (o *ClientOptions) timeout() time.Duration {
	if o != nil && o.RequestTimeout > 0 {
		return o.RequestTimeout
	}
	return defaultRequestTimeout
}

func (o *ClientOptions) maxRetries() int {
	if o != nil && o.MaxRetries > 0 {
		return o.MaxRetries
	}
	return defaultMaxRetries
}

// retryingFetcher pairs a Fetcher with a per-request timeout and retry budget so
// every fetch on the VOD path gets the same bounded, retrying behavior the live
// path already has (fetchRetry).
type retryingFetcher struct {
	f          Fetcher
	timeout    time.Duration
	maxRetries int
}

func (rf retryingFetcher) get(ctx context.Context, url string) ([]byte, error) {
	return fetchRetry(ctx, rf.f, url, rf.timeout, rf.maxRetries)
}

// OpenVOD reads a complete (EXT-X-ENDLIST) HLS presentation and returns its
// audio as a format.Media. playlistURL may name a master or a media playlist; a
// master is resolved to one variant (ClientOptions.VariantIndex). It fetches the
// init segment and every media segment up front (VOD is finite), streams the
// media to a temp file, and decodes it through the fragmented-MP4 demuxer with
// the init as the out-of-band codec config. Every fetch has a per-request
// timeout and retry. A live (non-ENDLIST) playlist is refused here; OpenLive
// follows it.
func OpenVOD(ctx context.Context, f Fetcher, playlistURL string, opts *ClientOptions) (format.Media, error) {
	rf := retryingFetcher{f: f, timeout: opts.timeout(), maxRetries: opts.maxRetries()}
	mediaURL, media, err := resolveMediaPlaylist(ctx, rf, playlistURL, opts.variantIndex())
	if err != nil {
		return nil, err
	}
	if !media.End {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			"hls: playlist has no EXT-X-ENDLIST (a live playlist); OpenVOD reads complete presentations only")
	}
	return assembleMedia(ctx, rf, mediaURL, media)
}

// resolveMediaPlaylist fetches playlistURL, follows a master to its selected
// variant, and returns the media playlist plus its own URL (the base for the
// init and segment URIs).
func resolveMediaPlaylist(ctx context.Context, rf retryingFetcher, playlistURL string, variant int) (string, MediaPlaylist, error) {
	raw, err := rf.get(ctx, playlistURL)
	if err != nil {
		return "", MediaPlaylist{}, err
	}
	if isMasterPlaylist(raw) {
		master, err := ParseMaster(string(raw))
		if err != nil {
			return "", MediaPlaylist{}, err
		}
		if variant >= len(master.Variants) {
			return "", MediaPlaylist{}, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("hls: variant index %d out of range (%d variants)", variant, len(master.Variants)))
		}
		mediaURL, err := resolveURL(playlistURL, master.Variants[variant].URI)
		if err != nil {
			return "", MediaPlaylist{}, err
		}
		raw, err = rf.get(ctx, mediaURL)
		if err != nil {
			return "", MediaPlaylist{}, err
		}
		media, err := ParseMedia(string(raw))
		return mediaURL, media, err
	}
	media, err := ParseMedia(string(raw))
	return playlistURL, media, err
}

// isMasterPlaylist reports whether raw is a master (variant) playlist, detected
// by an #EXT-X-STREAM-INF tag at the start of a line. The line anchor matters:
// a bare substring check would misroute a media playlist that merely mentions
// the tag text inside an #EXTINF title or comment.
func isMasterPlaylist(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#EXT-X-STREAM-INF") {
			return true
		}
	}
	return false
}

// assembleMedia fetches the init and media segments a media playlist lists,
// streams the media to a temp file, and wires the fragmented-MP4 demuxer behind
// a format.Media. Segments go to disk rather than a growing in-memory buffer so
// peak memory is one segment, not the whole (possibly large) presentation, and a
// concurrent server cannot be driven to exhaust RAM; the temp file is removed
// when the Media is closed.
func assembleMedia(ctx context.Context, rf retryingFetcher, baseURL string, media MediaPlaylist) (format.Media, error) {
	if media.InitURI == "" {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			"hls: playlist has no EXT-X-MAP init segment (only fragmented-MP4 presentations are read)")
	}
	if len(media.Segments) > maxSegments {
		return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("hls: playlist lists %d segments, over the %d cap", len(media.Segments), maxSegments))
	}
	initURL, err := resolveURL(baseURL, media.InitURI)
	if err != nil {
		return nil, err
	}
	init, err := rf.get(ctx, initURL)
	if err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp("", "waxflow-hls-*.m4s")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "hls: creating temp media file", err)
	}
	// Any failure before the Media takes ownership must not leak the temp file.
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	var total int64
	for _, seg := range media.Segments {
		segURL, err := resolveURL(baseURL, seg.URI)
		if err != nil {
			return nil, err
		}
		data, err := rf.get(ctx, segURL)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > maxMediaBytes {
			return nil, waxerr.New(waxerr.CodeUnsupportedFormat,
				fmt.Sprintf("hls: media exceeds the %d-byte cap", int64(maxMediaBytes)))
		}
		if _, err := tmp.Write(data); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeInternal, "hls: writing temp media", err)
		}
	}

	src, err := container.FileSource(tmp)
	if err != nil {
		return nil, err
	}
	demux, err := mp4.NewFragmentedDemuxer(init, src)
	if err != nil {
		return nil, err
	}
	med, err := format.FromDemuxer("hls", &closingDemuxer{Demuxer: demux, file: tmp})
	if err != nil {
		return nil, err
	}
	ok = true
	return med, nil
}

// closingDemuxer wires the temp file's lifetime to the Media: format.media.Close
// closes the demuxer as an io.Closer, which closes and removes the file. It
// embeds the concrete *mp4.Demuxer so the Seeker and Warner it implements stay
// promoted (a wrapper hiding them would silently make HLS input non-seekable).
type closingDemuxer struct {
	*mp4.Demuxer
	file *os.File
	once sync.Once
}

func (c *closingDemuxer) Close() error {
	c.once.Do(func() {
		c.file.Close()
		os.Remove(c.file.Name())
	})
	return nil
}

// resolveURL resolves a (possibly relative) playlist reference against a base
// URL, the standard RFC 3986 resolution HLS URIs use.
func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeInvalidRequest, "hls: bad base URL", err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeUnsupportedFormat, "hls: bad segment URI", err)
	}
	return b.ResolveReference(r).String(), nil
}
