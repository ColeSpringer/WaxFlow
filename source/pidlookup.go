package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxflow/internal/flight"
	"github.com/colespringer/waxflow/internal/reftext"
	"github.com/colespringer/waxflow/waxerr"
)

// DefaultPIDLookupTimeout bounds one lookup when PIDLookupOptions.Timeout
// is zero.
const DefaultPIDLookupTimeout = 5 * time.Second

// ProbePID is the id `waxflow doctor` asks a lookup host for: the smallest
// ULID, which no catalog holds, so a reachable host answers it 404 (or 400).
const ProbePID = "00000000000000000000000000"

const (
	// pidCacheEntries bounds the remembered answers.
	pidCacheEntries = 4096
	// maxPIDCacheTTL caps how long a host may have an answer remembered.
	maxPIDCacheTTL = time.Hour
	// maxPIDAnswerBytes caps an answer body; a reference is a short path.
	maxPIDAnswerBytes = 64 << 10
	// maxPIDLen caps an id.
	maxPIDLen = 128
	// pidIdleConns is how many idle connections to the host are kept: the
	// playlist, init and segment fetches a player starts together resolve
	// together.
	pidIdleConns = 16
)

// PIDLookupOptions configures NewPIDLookup.
type PIDLookupOptions struct {
	// Key is sent as a bearer token with every lookup; empty sends none.
	// See CheckPIDLookupKey for what a key may hold.
	Key string
	// Client makes the requests. Nil builds one that follows no redirects
	// and consults no proxy; Close releases that one's idle connections.
	Client *http.Client
	// Timeout bounds one lookup; 0 means DefaultPIDLookupTimeout.
	Timeout time.Duration
	// Logger receives what the errors clients see leave out: why the host
	// is unreachable, once per change of its state, and what a broken
	// answer said. Nil discards.
	Logger *slog.Logger
}

// PIDLookup resolves pid:<id> references by asking a host over HTTP for the
// root-relative reference behind each one, then opening that through the
// resolver it wraps: GET <base>/<id> answers {"ref": "<root>/<path>"}, the
// reference in text form (see Ref), so a name that is not UTF-8 is written
// pct:. The opened file keeps the pid as its Ref, so cache keys and signed
// URLs survive a rename the host tracks. An answer is remembered only as long
// as its Cache-Control max-age allows (an hour at most) and only once its
// file opened; a remembered path that turns out gone is asked again once.
// Errors name the pid and a status, never the host or a path: a pid exists
// to be location-free, and the envelope reaches a signed URL's holder; the
// detail goes to the logger.
type PIDLookup struct {
	base    *url.URL
	key     string
	next    Resolver
	client  *http.Client
	own     *http.Transport // built here and closed by Close; nil for a caller's client
	timeout time.Duration
	now     func() time.Time
	log     *slog.Logger
	down    atomic.Bool
	flights flight.Group[pidAnswer]

	mu    sync.Mutex
	cache map[string]*pidEntry
	clock uint64
}

// pidAnswer is what one request to the host yields.
type pidAnswer struct {
	ref string
	ttl time.Duration
}

// pidEntry is one remembered answer; used orders entries for eviction.
type pidEntry struct {
	ref     string
	expires time.Time
	used    uint64
}

// ParsePIDLookupURL validates a lookup base: an absolute http or https URL
// with a host and no query or fragment. Errors carry
// waxerr.CodeInvalidRequest and never the URL's password.
func ParsePIDLookupURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error quotes the whole URL, password included; its reason is
		// what the operator needs.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, waxerr.Wrap(waxerr.CodeInvalidRequest, "source: pid lookup URL does not parse", err)
	}
	shown := u.Redacted()
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: pid lookup URL %q must be http or https", shown))
	case u.Host == "":
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: pid lookup URL %q has no host", shown))
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: pid lookup URL %q must carry no query or fragment", shown))
	}
	return u, nil
}

// CheckPIDLookupKey refuses a key no Authorization header can carry: a key
// is a bearer token, A-Z a-z 0-9 - . _ ~ + / and =. Empty is no key.
func CheckPIDLookupKey(key string) error {
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case '0' <= c && c <= '9', 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z',
			c == '-', c == '.', c == '_', c == '~', c == '+', c == '/', c == '=':
		default:
			return waxerr.New(waxerr.CodeInvalidRequest,
				"source: pid lookup key must be a bearer token (A-Z a-z 0-9 - . _ ~ + / =)")
		}
	}
	return nil
}

// NewPIDLookup returns a lookup asking the host at rawURL (see
// ParsePIDLookupURL), delegating every reference it does not claim to next.
func NewPIDLookup(rawURL string, next Resolver, opts PIDLookupOptions) (*PIDLookup, error) {
	base, err := ParsePIDLookupURL(rawURL)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return nil, waxerr.New(waxerr.CodeInternal, "source: pid lookup needs a resolver to delegate to")
	}
	if err := CheckPIDLookupKey(opts.Key); err != nil {
		return nil, err
	}
	client := opts.Client
	var own *http.Transport
	if client == nil {
		own = newPIDTransport()
		client = &http.Client{
			Transport: own,
			// A lookup endpoint has no business redirecting; a 3xx reads as
			// a host outside the contract, in ask.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultPIDLookupTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &PIDLookup{
		base:    base,
		key:     opts.Key,
		next:    next,
		client:  client,
		own:     own,
		timeout: timeout,
		now:     time.Now,
		log:     log,
		cache:   make(map[string]*pidEntry),
	}, nil
}

// newPIDTransport is the default transport: http.DefaultTransport's dial and
// handshake bounds, no proxy (the host is a service inside one deployment),
// and room for the lookups a player's parallel fetches start at once.
func newPIDTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          pidIdleConns,
		MaxIdleConnsPerHost:   pidIdleConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// URL is the lookup base with any password masked, for logs.
func (l *PIDLookup) URL() string { return l.base.Redacted() }

// Close releases the idle connections of the transport NewPIDLookup built;
// a caller's own client is left alone. Resolve still works afterwards.
func (l *PIDLookup) Close() error {
	if l.own != nil {
		l.own.CloseIdleConnections()
	}
	return nil
}

// Resolve implements Resolver for pid:<id> references.
func (l *PIDLookup) Resolve(ctx context.Context, ref string) (*File, error) {
	id, ok := strings.CutPrefix(ref, "pid:")
	if !ok {
		return l.next.Resolve(ctx, ref)
	}
	if !validPID(id) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("source: pid %q is not an id (1 to %d of A-Z a-z 0-9 _ -)", id, maxPIDLen))
	}
	target, remembered := l.cached(id)
	var ttl time.Duration
	var err error
	if !remembered {
		if target, ttl, err = l.lookup(ctx, id); err != nil {
			return nil, err
		}
	}
	f, err := l.open(ctx, id, ref, target)
	if err != nil && remembered && waxerr.CodeOf(err) == waxerr.CodeNotFound {
		// The remembered path is gone: a rename landed since. Ask once more;
		// a second not-found is real.
		l.drop(id)
		if target, ttl, err = l.lookup(ctx, id); err != nil {
			return nil, err
		}
		f, err = l.open(ctx, id, ref, target)
	}
	if err != nil {
		return nil, err
	}
	// Remembered only once it opened: an answer the roots refuse is the
	// host's mistake, not one to pin for its max-age.
	if ttl > 0 {
		l.remember(id, target, ttl)
	}
	return f, nil
}

// lookup asks the host, once for every caller asking for id at the same
// time (a player fetches a playlist, an init and a segment together).
func (l *PIDLookup) lookup(ctx context.Context, id string) (string, time.Duration, error) {
	a, err := l.flights.Join(ctx, id, func(ctx context.Context) (pidAnswer, error) {
		ref, ttl, err := l.ask(ctx, id)
		return pidAnswer{ref: ref, ttl: ttl}, err
	})
	if err != nil && ctx.Err() != nil {
		// The caller left while the shared request went on for the others.
		err = waxerr.Wrap(waxerr.CodeCanceled, fmt.Sprintf("source: pid lookup for %q", id), err)
	}
	return a.ref, a.ttl, err
}

// open resolves the host's answer through next and hands the file back under
// the pid reference. What the roots said about the path stays out of the
// error and goes to the logger: a reference refused as such (malformed,
// escaping its root, a directory) is the host's mistake, and any other
// refusal is reported by its code alone. A root that is down passes through
// whole: it names the root, not a path, and the CLI's hint reads its cause.
func (l *PIDLookup) open(ctx context.Context, id, ref, target string) (*File, error) {
	f, err := l.next.Resolve(ctx, target)
	if err == nil {
		f.Ref = ref
		return f, nil
	}
	code := waxerr.CodeOf(err)
	switch code {
	case waxerr.CodeRootUnavailable, waxerr.CodePayloadTooLarge, waxerr.CodeCanceled:
		return nil, err
	case waxerr.CodeInvalidRequest, waxerr.CodeUnsupportedSource:
		l.log.Warn("pid lookup answered an unusable reference", "pid", id, "ref", clip(Ref(target).Text()), "err", clip(err.Error()))
		return nil, pidBroken(id, "answered an unusable reference")
	case waxerr.CodeNotFound:
		level := slog.LevelDebug // a rename the host has not indexed yet
		if !errors.Is(err, fs.ErrNotExist) {
			level = slog.LevelWarn // a root this daemon does not have
		}
		l.log.Log(ctx, level, "pid lookup answered a reference with nothing behind it", "pid", id, "ref", clip(Ref(target).Text()), "err", clip(err.Error()))
		return nil, waxerr.New(code, fmt.Sprintf("source: nothing is behind pid %q", id))
	}
	l.log.Warn("the file behind a pid did not open", "pid", id, "ref", clip(Ref(target).Text()), "err", clip(err.Error()))
	return nil, waxerr.New(code, fmt.Sprintf("source: the file behind pid %q did not open: %s", id, code))
}

// pidStatus sorts the host's status into the contract.
type pidStatus int

const (
	pidAnswered pidStatus = iota // 200: the body is the answer
	pidGone                      // 404, 410: no such item
	pidRejected                  // 400, 422: the host refused the id
	pidDown                      // 401, 403, 408, 429, 5xx: down for now
	pidOutside                   // anything else: not this contract
)

// classify reads a status. A refused key (401, 403) counts as down rather
// than broken: it is an operator's to fix, the log says so once, and jobs
// wait for that fix as they wait for a mount. Redirects and the rest of the
// 4xx range are a host that does not serve this contract.
func classify(status int) pidStatus {
	switch {
	case status == http.StatusOK:
		return pidAnswered
	case status == http.StatusNotFound, status == http.StatusGone:
		return pidGone
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return pidRejected
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return pidDown
	}
	return pidOutside
}

// ask performs one GET and reads the host's answer: the reference and how
// long it may be remembered.
func (l *PIDLookup) ask(parent context.Context, id string) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(parent, l.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base.JoinPath(id).String(), nil)
	if err != nil {
		return "", 0, waxerr.Wrap(waxerr.CodeInternal, "source: pid lookup request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "waxflow")
	if l.key != "" {
		req.Header.Set("Authorization", "Bearer "+l.key)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return "", 0, l.unreachable(parent, id, err)
	}
	defer resp.Body.Close()
	// Read whole before judging: an unread body costs the connection, and a
	// host that stalls after its status line is still down.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPIDAnswerBytes+1))
	if err != nil {
		return "", 0, l.unreachable(parent, id, err)
	}
	switch classify(resp.StatusCode) {
	case pidAnswered:
		l.reachable()
	case pidGone:
		l.reachable()
		return "", 0, waxerr.New(waxerr.CodeNotFound, fmt.Sprintf("source: no item for pid %q", id))
	case pidRejected:
		l.reachable()
		return "", 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("source: the pid lookup rejected %q", id))
	case pidDown:
		l.wentDown(id, fmt.Errorf("the host answered %d", resp.StatusCode))
		return "", 0, waxerr.New(waxerr.CodeCatalogUnavailable,
			fmt.Sprintf("source: pid lookup for %q: the host answered %d", id, resp.StatusCode))
	default:
		l.log.Warn("pid lookup answered a status outside the contract", "pid", id, "status", resp.StatusCode)
		return "", 0, pidBroken(id, fmt.Sprintf("answered %d, outside the contract", resp.StatusCode))
	}
	if len(body) > maxPIDAnswerBytes {
		return "", 0, l.broken(id, body, fmt.Sprintf("more than %d bytes", maxPIDAnswerBytes))
	}
	// Ref decodes strictly, and only ref is held to it: the fields beside
	// it are ignored, whatever they hold.
	var answer struct {
		Ref Ref `json:"ref"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		why := "not JSON"
		if waxerr.CodeOf(err) == waxerr.CodeInvalidRequest {
			why = strings.TrimPrefix(err.Error(), "source: ") // the reference's own refusal
		}
		return "", 0, l.broken(id, body, why)
	}
	// The reference's shape is the roots' to judge (open recodes a refusal).
	return string(answer.Ref), maxAge(resp.Header), nil
}

// unreachable codes a failed request: the caller leaving is canceled,
// anything else means the host is down for now. The message says only that
// (a raw dial or DNS error names the host and the resolver, and the message
// reaches the daemon's clients); the cause goes to the logger.
func (l *PIDLookup) unreachable(parent context.Context, id string, err error) error {
	msg := fmt.Sprintf("source: pid lookup for %q", id)
	if parent.Err() != nil {
		return waxerr.Wrap(waxerr.CodeCanceled, msg, parent.Err())
	}
	what := "the host could not be reached"
	if errors.Is(err, context.DeadlineExceeded) {
		what = fmt.Sprintf("no answer within %v", l.timeout)
	}
	l.wentDown(id, err)
	return waxerr.New(waxerr.CodeCatalogUnavailable, msg+": "+what)
}

// wentDown logs why the host is unavailable, once per outage; reachable logs
// its return. Every lookup while it is down would otherwise log a line.
func (l *PIDLookup) wentDown(id string, cause error) {
	if !l.down.Swap(true) {
		l.log.Warn("pid lookup host unavailable", "url", l.base.Redacted(), "pid", id, "err", cause)
	}
}

func (l *PIDLookup) reachable() {
	if l.down.Swap(false) {
		l.log.Info("pid lookup host answering again", "url", l.base.Redacted())
	}
}

// broken logs a host's wrong answer, clipped, and returns the error clients
// see, which does not carry it.
func (l *PIDLookup) broken(id string, body []byte, why string) error {
	l.log.Warn("pid lookup answered badly", "pid", id, "why", why, "answer", clip(string(body)))
	return pidBroken(id, `answered something other than {"ref": "<root>/<path>"}`)
}

// pidBroken is the error for a host that answered, wrongly.
func pidBroken(id, what string) error {
	return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("source: pid lookup for %q %s", id, what))
}

// clip makes a logged answer or error legible and bounds it, cutting between
// characters and never inside a %XX or \xNN escape, so what is shown is the
// start of what was sent.
func clip(s string) string {
	const keep = 160
	s = reftext.EscapeStray(s)
	if len(s) <= keep {
		return s
	}
	cut := keep
	for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	if i := strings.LastIndexByte(s[cut-2:cut], '%'); i >= 0 {
		cut += i - 2
	}
	if i := strings.LastIndex(s[cut-3:cut], `\x`); i >= 0 {
		cut += i - 3
	}
	return s[:cut] + " (clipped)"
}

// validPID accepts 1 to maxPIDLen bytes of A-Z, a-z, 0-9, '_' and '-': one
// URL path segment that needs no escaping and can never be "." or "..".
func validPID(id string) bool {
	if id == "" || len(id) > maxPIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case '0' <= c && c <= '9', 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// maxAge reads how long an answer may be remembered from its Cache-Control:
// max-age=N, capped at maxPIDCacheTTL (a value past what an int64 holds is
// the cap too); 0 when absent, zero, negative, unparsable, or beside
// no-store or no-cache.
func maxAge(h http.Header) time.Duration {
	var ttl time.Duration
	for _, cc := range h.Values("Cache-Control") {
		for d := range strings.SplitSeq(cc, ",") {
			d = strings.ToLower(strings.TrimSpace(d))
			switch {
			case d == "no-store", d == "no-cache":
				return 0
			case strings.HasPrefix(d, "max-age="):
				n, err := strconv.ParseInt(strings.TrimPrefix(d, "max-age="), 10, 64)
				if (err != nil && !errors.Is(err, strconv.ErrRange)) || n <= 0 {
					continue
				}
				ttl = time.Duration(min(n, int64(maxPIDCacheTTL/time.Second))) * time.Second
			}
		}
	}
	return ttl
}

// cached returns the remembered reference for id, forgetting an expired one
// and marking a hit as the most recently used.
func (l *PIDLookup) cached(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.cache[id]
	if !ok {
		return "", false
	}
	if !l.now().Before(e.expires) {
		delete(l.cache, id)
		return "", false
	}
	l.clock++
	e.used = l.clock
	return e.ref, true
}

// remember stores an answer, making room at capacity.
func (l *PIDLookup) remember(id, ref string, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, ok := l.cache[id]; !ok && len(l.cache) >= pidCacheEntries {
		l.evictLocked(now)
	}
	l.clock++
	l.cache[id] = &pidEntry{ref: ref, expires: now.Add(ttl), used: l.clock}
}

// evictLocked drops an expired entry when there is one, else the entry used
// longest ago. One linear pass, run only on an insert at capacity.
func (l *PIDLookup) evictLocked(now time.Time) {
	var victim string
	oldest := uint64(math.MaxUint64)
	for k, e := range l.cache {
		if !now.Before(e.expires) {
			victim = k
			break
		}
		if e.used < oldest {
			oldest, victim = e.used, k
		}
	}
	delete(l.cache, victim)
}

// drop forgets id.
func (l *PIDLookup) drop(id string) {
	l.mu.Lock()
	delete(l.cache, id)
	l.mu.Unlock()
}
